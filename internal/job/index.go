package job

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/market-of-labs/forge-core/internal/apkmeta"
	"github.com/market-of-labs/forge-core/internal/gh"
	"github.com/market-of-labs/forge-core/internal/model"
	"github.com/market-of-labs/forge-core/internal/naming"
	"github.com/market-of-labs/forge-core/internal/store"
)

// BuildIndexOptions 调 build-index 的选项。
type BuildIndexOptions struct {
	// FetchMissing 决定"Release 里有、但现有 index 里没有元数据的版本"要不要下载一个
	// APK 把它补全。
	//
	// 关掉（默认）时这种版本只拿到文件名（version/abi/size），versionName 用 token 兜底、
	// versionCode 缺失 —— 而 versionCode 缺失会让 check-manifest 判**失败**（规则 6），
	// 于是整份清单推不出去。所以真要重建时得开这个开关：它是**唯一**能拿回 versionCode
	// 的途径（那两个字段只在 APK 内部，Release 的元数据里没有）。代价是下载。
	FetchMissing bool
}

// assetGroup 是 Release 里的一个分片，连同从它名字里解析出的坐标。
type assetGroup struct {
	version string
	abi     string
	asset   gh.Asset
}

// BuildIndex 从**本市场仓库的 Release 现状**重建 `store/index.json`（03 §2.4）。
//
// 为什么事实源是 Release 而不是现有 index：index 是派生数据（D23），它可能被清空、
// 被改坏、或与 Release 漂移（有人手工传了 asset）。Release 是唯一不可辩驳的事实 ——
// asset 名里的 `{version}` 与 `{abi}` 就是 02 §2.4 那份硬依赖的全部载体。
//
// 但**元数据**（versionName / versionCode / publishedAt / upstreamTag）在 Release 里没有，
// 只存在于 index 自己、或 APK 内部。所以本函数对它们的策略是：
//
//	现有 index 里有   → 原样保留（它记的是镜像**当时**读到的事实）
//	现有 index 里没有 → 用 APK 内容补齐（需 FetchMissing），或如实留空并告警
//
// 于是日常路径（镜像时已写好元数据）**零下载**，而灾难恢复路径能自愈。
func BuildIndex(ctx context.Context, c *Ctx, opts BuildIndexOptions) (*model.Index, *model.Report, error) {
	if err := c.Env.RequireToken("列 Release"); err != nil {
		return nil, nil, err
	}
	rep := &model.Report{}

	rels, err := c.GH.ListReleases(ctx, c.Env.StoreRepo)
	if err != nil {
		return nil, nil, fmt.Errorf("列 %s 的 Release：%w", c.Env.StoreRepo, err)
	}

	byApp := map[string][]assetGroup{}
	for i := range rels {
		rel := &rels[i]
		if rel.TagName == model.IncomingTag {
			// 手动上传暂存队列（03 §3.2），不是版本资产。
			continue
		}
		if rel.Draft {
			// 内部 Release 正常都是 published（EnsureRelease 就是这么建的）。
			// draft 只出现在"清场"路径上（规则 6：只 unpublish 不删除），
			// 那种状态下的 asset 不该进清单。
			rep.Warnf(rel.TagName, "Release 处于 draft，其 asset 不计入索引（清场后的状态，非正常路径）")
			continue
		}
		assets, err := c.GH.ListAssets(ctx, c.Env.StoreRepo, rel.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("列 Release %s 的 asset：%w", rel.TagName, err)
		}
		for _, a := range assets {
			version, abi, err := naming.Split(rel.TagName, a.Name)
			if err != nil {
				// 手传的、或改名改坏了的 asset。跳过而不是猜 ——
				// 猜错的后果是清单里出现一个客户端解析不了的条目（02 §2.4 静默降级）。
				rep.Warnf(rel.TagName, "asset %q 不符合命名契约（02 §2.4），未计入索引：%v", a.Name, err)
				continue
			}
			byApp[rel.TagName] = append(byApp[rel.TagName], assetGroup{version, abi, a})
		}
	}
	c.Log("扫过 %d 个 Release，%d 个 App 有可识别的 asset", len(rels), len(byApp))

	ids := make([]string, 0, len(byApp))
	for id := range byApp {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	ix := &model.Index{GeneratedAt: model.NowISO(), Apps: make([]model.IndexApp, 0, len(ids))}
	for _, id := range ids {
		app, appRep, err := buildIndexApp(ctx, c, id, byApp[id], opts)
		if err != nil {
			return nil, nil, err
		}
		rep.Addf("", appRep)
		ix.Apps = append(ix.Apps, *app)
	}
	return ix, rep, nil
}

// buildIndexApp 重建一个 App 的版本账本。
func buildIndexApp(ctx context.Context, c *Ctx, id string, groups []assetGroup, opts BuildIndexOptions) (*model.IndexApp, *model.Report, error) {
	rep := &model.Report{}
	oldApp := c.Index.Find(id)

	// 先按 version 归并分片。
	type bucket struct {
		assets   []gh.Asset
		abis     []string
		earliest string // 分片里最早的 created_at，给新版本定序用
	}
	buckets := map[string]*bucket{}
	var versions []string
	for _, g := range groups {
		b, ok := buckets[g.version]
		if !ok {
			b = &bucket{}
			buckets[g.version] = b
			versions = append(versions, g.version)
		}
		if g.asset.CreatedAt != "" && (b.earliest == "" || g.asset.CreatedAt < b.earliest) {
			b.earliest = g.asset.CreatedAt
		}
		b.assets = append(b.assets, g.asset)
		b.abis = append(b.abis, g.abi)
	}

	// 定序。**这个顺序就是 IndexApp.Latest() 的判据**（"最后一个 = 最新"），
	// 所以它必须确定且随时间单调：
	//
	//	旧 index 里已有的版本 → 保持它原来的位次（不动历史）
	//	新出现的版本         → 按其分片最早的 created_at 追加到末尾
	//
	// 用版本号或版本名排都不行：两者都是上游自由文本，回退发布（2.0 之后发 1.9）
	// 是真实存在的，而"最新"只能由时间定义。
	type placed struct {
		version string
		rank    int    // 0 = 旧索引里有；1 = 新版本
		key     string // rank=1 时的排序键
		pos     int    // rank=0 时是旧位次；rank=1 时是首次出现序（兜底）
	}
	items := make([]placed, 0, len(versions))
	for i, v := range versions {
		if _, oldPos := findOldVersion(oldApp, v); oldPos >= 0 {
			items = append(items, placed{version: v, rank: 0, pos: oldPos})
			continue
		}
		items = append(items, placed{version: v, rank: 1, key: buckets[v].earliest + "\x00" + v, pos: i})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].rank != items[j].rank {
			return items[i].rank < items[j].rank
		}
		if items[i].key != items[j].key {
			return items[i].key < items[j].key
		}
		return items[i].pos < items[j].pos
	})

	app := &model.IndexApp{ID: id, Tag: id, Versions: make([]model.IndexVersion, 0, len(items))}
	for _, it := range items {
		b := buckets[it.version]
		oldVer, _ := findOldVersion(oldApp, it.version)

		v := model.IndexVersion{Version: it.version}
		if oldVer != nil {
			// 老版本的元数据全部保留。upstreamTag 尤其重要：它是"已镜像"的判据
			// （§4.4），丢了会让下一轮对账白下一次（然后被 asset 名幂等挡住）。
			v.VersionName = oldVer.VersionName
			v.VersionCode = oldVer.VersionCode
			v.PublishedAt = oldVer.PublishedAt
			v.UpstreamTag = oldVer.UpstreamTag
		}

		if v.VersionName == "" && opts.FetchMissing {
			meta, err := c.fetchVersionMeta(ctx, id, b.assets)
			switch {
			case err != nil:
				// 下载失败不该让整次重建失败：已经拿到的部分仍然有价值，
				// 而这个版本会被下面的告警点名。
				rep.Warnf(id, "版本 %s 补元数据失败：%v", it.version, err)
			case meta == nil:
				rep.Warnf(id, "版本 %s 补元数据：读取结果为空", it.version)
			default:
				v.VersionName = meta.VersionName
				v.VersionCode = meta.VersionCode
			}
		}

		if v.UpstreamTag == "" {
			// **只对 github 源告警**：manual 来源本来就没有上游 tag（它的二进制走
			// _incoming 上传队列，§3.2），对它告警是纯粹的噪音 —— 而噪音会让
			// 真正需要人看的告警失效。
			//
			// 另一半是"Release 里有 asset 但 sources/ 里没这个条目"：移除一个来源
			// 只删文件，Release 与它的 asset 全都留着（D13 全保留），于是 index 里
			// 会留有它的版本账本。清单不会收录它（manifest.Build 只遍历 sources），
			// 所以这是正常的、可解释的状态 —— 但值得说一声，免得有人以为清单漏了。
			switch src := c.Source(id); {
			case src == nil:
				rep.Warnf(id, "Release 里有它的 asset，但 sources/ 里没有这个条目：清单不会收录它。"+
					"若这是刚被「移除」的来源，属正常（D13 全保留，资产不删）")
			case src.Source == model.SourceGitHub:
				rep.Warnf(id, "版本 %s 没有 upstreamTag —— 下一轮对账会当成未镜像，多下一次后被 asset 名幂等挡住",
					it.version)
			}
		}
		if v.VersionCode == 0 {
			rep.Warnf(id, "版本 %s 没解析出 versionCode：清单会缺该字段，check-manifest 将判**失败**（规则 6）。"+
				"用 build-index --fetch-missing 可从 APK 内容补回来", it.version)
		}

		assets := make([]model.IndexAsset, 0, len(b.assets))
		for i := range b.assets {
			assets = append(assets, model.IndexAsset{ABI: b.abis[i], File: b.assets[i].Name, Size: b.assets[i].Size})
		}
		v.Assets = sortAssets(assets)
		app.Versions = append(app.Versions, v)
	}
	return app, rep, nil
}

// findOldVersion 在旧索引里按 version token 找一个版本，返回它与其位次（找不到时位次为 -1）。
func findOldVersion(app *model.IndexApp, version string) (*model.IndexVersion, int) {
	if app == nil {
		return nil, -1
	}
	for i := range app.Versions {
		if app.Versions[i].Version == version {
			return &app.Versions[i], i
		}
	}
	return nil, -1
}

// fetchVersionMeta 下载该版本的一个分片并读它的 APK 元数据。
//
// 挑**最小**的那个分片：同一版本的所有分片共享 versionName/versionCode，
// 下最小的那份能少传几 MB —— 而这是重建路径，可能有几十个版本要补。
func (c *Ctx) fetchVersionMeta(ctx context.Context, appID string, assets []gh.Asset) (*apkmeta.Meta, error) {
	if len(assets) == 0 {
		return nil, fmt.Errorf("该版本没有任何分片")
	}
	best := assets[0]
	for _, a := range assets[1:] {
		if a.Size > 0 && (best.Size == 0 || a.Size < best.Size) {
			best = a
		}
	}

	rc, _, err := c.GH.DownloadAsset(ctx, c.Env.StoreRepo, best.ID)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	// 落临时文件而不是读进内存：APK 动辄上百 MB，而 apkmeta.ReadZip 要的是
	// io.ReaderAt —— 读进内存还得再留一整份。
	f, err := os.CreateTemp("", "forge-apkmeta-*.apk")
	if err != nil {
		return nil, err
	}
	name := f.Name()
	defer os.Remove(name)

	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		return nil, fmt.Errorf("把 %s 落盘：%w", best.Name, err)
	}
	// 先关再读：apkmeta.Read 会按路径重新打开这个文件，
	// 在 Windows 上让同一份文件同时处于"我们写一半"与"它来读"的状态没有好处。
	if err := f.Close(); err != nil {
		return nil, err
	}

	c.Log("  下载 %s 以补 %s/%s 的元数据", best.Name, appID, mustVersion(best.Name, appID))
	return apkmeta.Read(name)
}

// mustVersion 只为日志服务，尽力从文件名里取版本段；取不到就返回文件名。
func mustVersion(fileName, appID string) string {
	if v, _, err := naming.Split(appID, fileName); err == nil {
		return v
	}
	return fileName
}

// sortAssets 按契约顺序排列分片，并去掉同一 ABI 的重复项（保留先出现的那份）。
//
// 去重不是洁癖：同一 ABI 出现两次意味着 Release 里真有两个不同名的文件指向同一个架构
// （比如改名前后各留了一份），而清单里一个 ABI 只能出现一次 —— 留着两个会让客户端
// 按 ABI 折叠时结果取决于遍历顺序。
func sortAssets(as []model.IndexAsset) []model.IndexAsset {
	seen := make(map[string]bool, len(as))
	out := make([]model.IndexAsset, 0, len(as))
	for _, a := range as {
		if seen[a.ABI] {
			continue
		}
		seen[a.ABI] = true
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool { return abiRank(out[i].ABI) < abiRank(out[j].ABI) })
	return out
}

// abiRank 是命名契约的顺序键。未知 ABI 排在已知的后面（与 naming.SortABIs 一致）。
func abiRank(abi string) int {
	for i, a := range naming.ABISet {
		if a == abi {
			return i
		}
	}
	return len(naming.ABISet)
}

// WriteIndex 把索引落盘。
func WriteIndex(c *Ctx, ix *model.Index) error {
	if err := store.WriteJSON(c.Repo.IndexPath(), ix); err != nil {
		return err
	}
	c.Log("写入 %s：%s", c.Repo.IndexPath(), IndexSummary(ix))
	return nil
}

// IndexSummary 给日志一行摘要。
func IndexSummary(ix *model.Index) string {
	versions, assets := 0, 0
	for i := range ix.Apps {
		versions += len(ix.Apps[i].Versions)
		for j := range ix.Apps[i].Versions {
			assets += len(ix.Apps[i].Versions[j].Assets)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d 个 App / %d 个版本 / %d 个 asset", len(ix.Apps), versions, assets)
	return b.String()
}
