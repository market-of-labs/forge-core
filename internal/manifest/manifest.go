// Package manifest 实现 03 §5.1：把 `sources/`（静态元数据）+ `store/index.json`（版本事实）
// + `store/endpoints.json`（地址模板）合成 `apps.json`。
//
// 三个输入的角色严格分开，本包**只读不写**其中任何一个：
//
//	sources/{appId}.json  人维护 → 谁在清单里、叫什么、kind 是什么
//	index.json            派生态 → 有哪些版本、每个版本有哪些 ABI 分片
//	endpoints.json        配置   → 地址长什么样
//
// 本包不碰网络、不读文件：三个输入由调用方准备好（这样它整条都能被单测覆盖）。
package manifest

import (
	"fmt"
	"sort"
	"time"

	"github.com/market-of-labs/forge-core/internal/model"
	"github.com/market-of-labs/forge-core/internal/naming"
)

// GeneratedBy 写进清单的 `generatedBy` 字段（02 §2.1，可选字段，用于标记维护来源）。
const GeneratedBy = "forge"

// Input 是合成的全部输入。
type Input struct {
	Sources     []model.Source
	Index       *model.Index
	Endpoints   model.Endpoints
	GeneratedBy string
}

// Build 合成清单。
//
// 返回的 Report 只承载"跳过 / 孤儿 / 数据退化"这类**关于合成过程**的告警；
// 02 §2.8 的条目级校验由 check-manifest 独立跑一遍（那是产出后的自检，
// 与"怎么产出"是两件事，混在一起会让两边的失败原因难以区分）。
func Build(in Input) (*model.Manifest, *model.Report, error) {
	if in.Index == nil {
		return nil, nil, fmt.Errorf("index.json 未提供：它是版本事实的唯一来源")
	}
	// 先验模板：模板与命名契约分叉时必须**在产出前**就失败。
	// 否则会生成一份"文件名符合模板但客户端解析不了"的清单 —— 而那是静默故障（02 §2.4）。
	if err := in.Endpoints.Validate(); err != nil {
		return nil, nil, fmt.Errorf("endpoints 非法，拒绝产出清单：%w", err)
	}

	rep := &model.Report{}
	generatedBy := in.GeneratedBy
	if generatedBy == "" {
		generatedBy = GeneratedBy
	}

	// 按 id 排序产出：清单顺序不影响语义，但**稳定顺序 = 稳定 diff**。
	// 不排序的话每次重建都可能因为 map 遍历顺序产生整份重排，让 review 失去意义。
	sources := make([]model.Source, len(in.Sources))
	copy(sources, in.Sources)
	sort.Slice(sources, func(i, j int) bool { return sources[i].ID < sources[j].ID })

	// 反向检查：index 里有、sources 里没有的 App。那意味着有人删了 sources 文件
	// 但 Release 还在（D13 全保留）—— 不算错误，但要让维护者看见"这些还在被镜像着"。
	want := make(map[string]bool, len(sources))
	for i := range sources {
		want[sources[i].ID] = true
	}
	for i := range in.Index.Apps {
		if !want[in.Index.Apps[i].ID] {
			rep.Warnf(in.Index.Apps[i].ID,
				"index.json 里有这个 App 但 sources/ 里没有：Release 与历史 asset 仍按 D13 保留，"+
					"清单不会收录它。若这是有意的移除，属预期（02 §2.9 移除不传播）")
		}
	}

	apps := make([]model.Entry, 0, len(sources))
	for i := range sources {
		src := &sources[i]
		entry, ok, r := buildEntry(src, in.Index, in.Endpoints)
		rep.Addf("", r)
		if !ok {
			continue
		}
		apps = append(apps, *entry)
	}

	m := &model.Manifest{
		SchemaVersion: model.SchemaVersion,
		ExportedAt:    model.NowISO(),
		GeneratedBy:   generatedBy,
		Apps:          apps,
	}
	return m, rep, nil
}

// buildEntry 合成单条。ok=false 表示"这条不该进清单"。
func buildEntry(src *model.Source, index *model.Index, ep model.Endpoints) (*model.Entry, bool, *model.Report) {
	rep := &model.Report{}

	idxApp := index.Find(src.ID)
	if idxApp == nil || len(idxApp.Versions) == 0 {
		// 03 §5.4 的原则：**不写半成品清单条目**。
		//
		// 一个刚收录、还没镜像出任何版本的 App，若照样出一条 apkUrls 为空的清单，
		// 后果不是"设备端看到空条目"这么轻 —— 02 规则 4 会判它失败，
		// 于是 check-manifest 阻断**整次回写**，连别的 App 的正常更新都推不出去。
		// 所以宁可这一条暂时缺席（下一轮 reconcile 补齐后自然出现）。
		rep.Warnf(src.ID, "sources/ 里有它但 index.json 里没有任何版本 —— 本轮不产出该条目（待镜像）。"+
			"source=%s%s", src.Source, pausedHint(src))
		return nil, false, rep
	}

	// 这里不再判 latest == nil：唯一能让 Latest() 返回 nil 的输入（Versions 为空）
	// 已经被上面那条 guard 挡掉了，再判一次就是一条永远走不到的分支。
	// 上面的 guard 必须留着 —— 它的告警带 source= 与 paused 提示，能定位；这条不能。
	latest := idxApp.Latest()
	if len(latest.Assets) == 0 {
		rep.Warnf(src.ID, "最新版本 %q 没有任何 asset，跳过（不写半成品条目）", latest.Version)
		return nil, false, rep
	}

	e := &model.Entry{
		ID:              src.ID,
		Name:            src.Name,
		Author:          src.Author,
		URL:             model.SentinelURL(src.ID),
		OverrideSource:  model.OverrideSource,
		LatestVersion:   latest.DisplayVersion(),
		OtherAssetUrls:  "[]",
		PreferredAPKIdx: 0,
		// changeLog **故意留空**：它会原样进 deep-link 的 URI（01 §3.6）。
		//
		// 清单是**全量单次推送**，条目共用同一个 URI 预算；上游的 release body 动辄几 KB，
		// 一百条 App 就是几百 KB —— 那会直接把客户端顶到系统 URI 上限上。而 02 §2.8
		// 规则 8 明说空 changeLog 合法。所以这里取"牺牲一个展示字段，换推送可靠性"。
		// 真要看更新说明，去上游 Release 看，那里没有长度约束。
		ChangeLog:  "",
		Categories: src.Categories,
		Kind:       src.Kind,
	}
	if e.Categories == nil {
		// 02 §2.8 规则 8 明说空 categories 合法 —— 但要渲染成 `[]` 而不是 `null`，
		// 否则客户端侧的解码会拿到 null。
		e.Categories = []string{}
	}

	// apkUrls：按 02 §2.4 的约定顺序列出**该版本的全部变体**。
	// 这里自己排一遍而不是信 index 里的顺序：index 是派生数据，可能被手工改过或由
	// rebuild-index 从 Release 的 asset 顺序重建（那个顺序是 API 返回顺序，无契约意义）。
	refs, err := apkRefs(src.ID, latest, ep)
	if err != nil {
		rep.Errorf(src.ID, "%v", err)
		return nil, false, rep
	}
	if err := e.SetAPKRefs(refs); err != nil {
		rep.Errorf(src.ID, "渲染 apkUrls：%v", err)
		return nil, false, rep
	}

	// additionalSettings.versionCode（02 §2.3 v1 契约必填）。
	// 拿不到就**不写**而不是写 0 —— 02 §2.8 规则 6 会因此判失败，那正是我们想要的：
	// 一份缺 versionCode 的清单应当被 check-manifest 拦住，而不是带着 0 推给设备。
	if latest.VersionCode > 0 {
		if err := e.SetVersionCode(int64(latest.VersionCode)); err != nil {
			rep.Errorf(src.ID, "写 versionCode：%v", err)
			return nil, false, rep
		}
	} else {
		rep.Warnf(src.ID, "版本 %q 没有解析出 versionCode，清单将缺该字段 → check-manifest 会判失败（规则 6）",
			latest.Version)
	}

	// releaseDate：可选字段（02 §2.2）。有就填，解析不了就留空并告警 ——
	// 绝不因为一个展示字段而阻断整条链。
	if latest.PublishedAt != "" {
		if t, err := time.Parse(time.RFC3339, latest.PublishedAt); err == nil {
			e.ReleaseDate = model.ReleaseDate(t)
		} else {
			rep.Warnf(src.ID, "publishedAt = %q 不是 RFC3339，releaseDate 留空：%v", latest.PublishedAt, err)
		}
	}
	return e, true, rep
}

// apkRefs 渲染某版本的全部 ABI 变体，顺序按 02 §2.4（universal 在前）。
func apkRefs(appID string, v *model.IndexVersion, ep model.Endpoints) ([]model.APKRef, error) {
	seen := make(map[string]bool, len(v.Assets))
	abis := make([]string, 0, len(v.Assets))
	for _, a := range v.Assets {
		if seen[a.ABI] {
			continue
		}
		seen[a.ABI] = true
		abis = append(abis, a.ABI)
	}

	refs := make([]model.APKRef, 0, len(abis))
	for _, abi := range naming.SortABIs(abis) {
		ref, err := ep.AssetURLForABI(appID, v.Version, abi)
		if err != nil {
			return nil, fmt.Errorf("渲染 %s/%s/%s 的地址：%w", appID, v.Version, abi, err)
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

func pausedHint(src *model.Source) string {
	if src.Paused {
		return "（该条目 paused=true，本轮不会有新版本；恢复更新改回这一个布尔值即可，02 §2.9）"
	}
	return ""
}
