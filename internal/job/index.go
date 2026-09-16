package job

import (
	"context"
	"fmt"
	"sort"

	"github.com/market-of-labs/forge-core/internal/gh"
	"github.com/market-of-labs/forge-core/internal/model"
	"github.com/market-of-labs/forge-core/internal/naming"
)

// assetGroup 是 Release 里的一个分片，连同从它名字里解析出的坐标。
type assetGroup struct {
	version string
	abi     string
	asset   gh.Asset
}

// BuildIndex 从**本市场仓库的 Release 现状**重建每个来源的版本账本（03 §2.4）。
//
// 账本就是 `sources/{appId}.json` 里的 `versions` 块（D48），所以本函数**就地改写
// c.Sources** 并把改过的文件落盘 —— 调用方接着拿 c.Sources 去合成清单，看到的就已经是
// 新账本。于是"两份数据要对齐"这件事连同它的整类失败模式一起消失了。
//
// # 为什么事实源是 Release 而不是现有账本
//
// 账本是派生数据（D23），它可能被清空、被改坏、或与 Release 漂移（有人手工传了 asset）。
// Release 是唯一不可辩驳的事实 —— asset 名里的 `{version}` 与 `{abi}` 就是 02 §2.4
// 那份硬依赖的全部载体。
//
// 但**元数据**（versionName / versionCode / publishedAt / upstreamTag / releaseNote）
// 在 Release 里没有，只存在于账本自己、或上游的 Release 里。所以本函数对它们的策略是：
//
//	现有账本里有   → 原样保留（它记的是镜像**当时**读到的事实）
//	现有账本里没有 → 如实留空并告警（见 buildLedger 末尾那两条）
//
// 留空不等于"永远回不来"，只是**不在这里**回来：账本里没有 upstreamTag 的**最新**那个
// 版本，下一轮对账会被镜像那一步重新计划（水位线以 upstreamTag 为准，pickTargets），
// 那时它重读上游 APK 就把元数据填回账本了（recordIndex 的合并分支）。所以本函数
// **零下载**，也不提供"下载我们自己的 Release 里的 APK 来补齐"那条路 —— 那条路只在
// 上游也读不到时才有意义，而那时这个来源也不会有新版本了；何况账本本身在 git 里
// （每次回写一个 commit），回滚比下载更准、给得还更多（releaseNote 与 upstreamTag
// 只有 git 里有）。
func BuildIndex(ctx context.Context, c *Ctx) (*model.Report, error) {
	if err := c.Env.RequireToken("列 Release"); err != nil {
		return nil, err
	}
	rep := &model.Report{}

	rels, err := c.GH.ListReleases(ctx, c.Env.StoreRepo)
	if err != nil {
		return nil, fmt.Errorf("列 %s 的 Release：%w", c.Env.StoreRepo, err)
	}

	byApp := map[string][]assetGroup{}
	for i := range rels {
		rel := &rels[i]
		if rel.TagName == model.IncomingTag {
			// 手动上传暂存队列（03 §3.2），不是版本资产。
			continue
		}
		if rel.Draft {
			// 内部 Release 正常都是 published（EnsureRelease 就是这么建的）。能走到这里
			// 的 draft 有两种，**都在说同一件事：这个 Release 此刻没有主人**。
			//
			//   - 清场路径上的正常停留（规则 6：只 unpublish 不删除），下一轮会被重新武装；
			//   - **队列被发布过一次** —— published → draft 会把它的 tag_name 降级成
			//     `untagged-<sha>`，于是上面那句 `== model.IncomingTag` 认不出它，它就从
			//     队列滑进了这个分支。这一种**不会自愈**：下一次搬运照样认不出，人传上去的
			//     APK 就停在队列里不动（2026-09-16 实际撞上，症状是"传了却没反应"）。
			//
			// 两者在 API 层面分不出来（都只是"一个 draft"，也没有 ref 可查），所以不猜，
			// 只把两条成因和那一条手工出路一起写出来：`untagged-` 开头的 tag 名本身就是
			// 后一种的指纹，照着它就能在 Releases 页上找到那个 Release。
			rep.Warnf(rel.TagName, "Release 处于 draft，其 asset 不计入账本。"+
				"若它本该是手动上传的队列（tag 名显示为 `untagged-*` 即是此兆）：队列被发布过一次，"+
				"published → draft 把 tag 名降级了，要把它改回 `%s` 才能再次搬运（03 §3.2）",
				model.IncomingTag)
			continue
		}
		assets, err := c.GH.ListAssets(ctx, c.Env.StoreRepo, rel.ID)
		if err != nil {
			return nil, fmt.Errorf("列 Release %s 的 asset：%w", rel.TagName, err)
		}
		for _, a := range assets {
			version, abi, err := naming.Split(rel.TagName, a.Name)
			if err != nil {
				// 手传的、或改名改坏了的 asset。跳过而不是猜 ——
				// 猜错的后果是清单里出现一个客户端解析不了的条目（02 §2.4 静默降级）。
				rep.Warnf(rel.TagName, "asset %q 不符合命名契约（02 §2.4），未计入账本：%v", a.Name, err)
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

	var touched []string
	for _, id := range ids {
		src := c.Source(id)
		if src == nil {
			// Release 里有 asset 但**没有来源文件** —— 账本无处可写。
			//
			// 这不丢东西：移除一个来源只删文件，Release 与它的 asset 按 D13 全保留，
			// 重新收录之后下一次 build-index 会把它们再认回来。但仍然要说一声，
			// 免得有人以为清单漏了。
			rep.Warnf(id, "Release 里有它的 asset，但 sources/ 里没有这个条目：账本无处可写，"+
				"清单也不会收录它。若这是刚被「移除」的来源，属正常（D13 全保留，资产不删）")
			continue
		}
		versions, appRep, err := buildLedger(src, byApp[id])
		if err != nil {
			return nil, err
		}
		rep.Addf("", appRep)
		// 写回内存。**必须在 buildLedger 返回之后**：它是拿旧账本当"上一轮的事实"
		// 来继承元数据的（见 findOldVersion），反过来就继承不到任何东西。
		src.Versions = versions
		touched = append(touched, id)
	}

	if err := writeLedgers(c, touched); err != nil {
		return nil, err
	}
	return rep, nil
}

// writeLedgers 把指定来源的账本落盘。
//
// 写的是整个来源文件（见 store.WriteSource 的说明），所以元数据也被一并写回去 ——
// 它来自本次 Load 的磁盘快照，内容一致，不会出现"写账本把元数据冲掉"。
//
// 只写列表里这些：绝大多数轮次某个来源的账本一个字都没变，那时写下去的字节与磁盘上
// 的完全一致，git 不会因此记一笔。
func writeLedgers(c *Ctx, ids []string) error {
	versions, assets := 0, 0
	for _, id := range ids {
		src := c.Source(id)
		if src == nil {
			continue
		}
		if err := c.Repo.WriteSource(src); err != nil {
			return err
		}
		versions += len(src.Versions)
		for i := range src.Versions {
			assets += len(src.Versions[i].Assets)
		}
	}
	c.Log("写入 %d 个来源的版本账本：%d 个版本 / %d 个 asset", len(ids), versions, assets)
	return nil
}

// buildLedger 重建一个来源的账本。返回新值，由调用方写回 src.Versions。
func buildLedger(src *model.Source, groups []assetGroup) ([]model.Version, *model.Report, error) {
	rep := &model.Report{}
	id := src.ID
	old := src.Versions

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

	// 定序。**这个顺序就是 Source.Latest() 的判据**（"最后一个 = 最新"），
	// 所以它必须确定且随时间单调：
	//
	//	旧账本里已有的版本 → 保持它原来的位次（不动历史）
	//	新出现的版本       → 按其分片最早的 created_at 追加到末尾
	//
	// 用版本号或版本名排都不行：两者都是上游自由文本，回退发布（2.0 之后发 1.9）
	// 是真实存在的，而"最新"只能由时间定义。
	type placed struct {
		version string
		rank    int    // 0 = 旧账本里有；1 = 新版本
		key     string // rank=1 时的排序键
		pos     int    // rank=0 时是旧位次；rank=1 时是首次出现序（兜底）
	}
	items := make([]placed, 0, len(versions))
	for i, v := range versions {
		if _, oldPos := findOldVersion(old, v); oldPos >= 0 {
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

	out := make([]model.Version, 0, len(items))
	for _, it := range items {
		b := buckets[it.version]
		oldVer, _ := findOldVersion(old, it.version)

		v := model.Version{Version: it.version}
		if oldVer != nil {
			// 老版本的元数据全部保留。upstreamTag 尤其重要：它是"已镜像"的判据
			// （§4.4），丢了会让下一轮对账白下一次（然后被 asset 名幂等挡住）。
			v.VersionName = oldVer.VersionName
			v.VersionCode = oldVer.VersionCode
			v.PublishedAt = oldVer.PublishedAt
			v.UpstreamTag = oldVer.UpstreamTag
			// releaseNote 同属这一串：它只在上游的 Release 里，我们自己的 Release 元数据
			// 里没有 —— 忘了继承，症状是"每轮重建之后更新说明就没了"，而且是静默的
			// （清单照样合法、check-manifest 照样过）。
			v.ReleaseNote = oldVer.ReleaseNote
		}

		// **只对 github 源告警**没有 upstreamTag：manual 来源本来就没有上游 tag
		// （它的二进制走 _incoming 上传队列，§3.2），对它告警是纯粹的噪音 ——
		// 而噪音会让真正需要人看的告警失效。
		//
		// 这两条告警就是"账本缺元数据"的**全部**出口：没有自愈开关可开了（删了，
		// 见 BuildIndex 的说明）。所以措辞要说到点子上：缺的是"哪一条"、能不能自己好。
		if v.UpstreamTag == "" && src.Source == model.SourceGitHub {
			rep.Warnf(id, "版本 %s 没有 upstreamTag —— 水位线认不出它已经镜像过；若它是最新的那个版本，"+
				"下一轮对账会重新镜像一遍并把元数据填回来（重读上游 APK），更老的版本不会", it.version)
		}
		if v.VersionCode == 0 {
			rep.Warnf(id, "版本 %s 没解析出 versionCode：清单会缺该字段，check-manifest 将判**失败**（规则 6），"+
				"而这一轮的回写会连同**别的应用**一起放弃。它要么来自「上传成功但账本没落盘」"+
				"（最新的那个版本下一轮镜像会补回），要么来自上游那个 APK 里真的没有 versionCode"+
				"（不会自己好：得改 sources/%s.json 那条记录，或回滚它 —— 改完要点一次 store 的手动"+
				"按钮 verb=reconcile 才会重算。人手改 sources/ 不再自动触发：forward.yml 那条 push "+
				"触发器已经删了）", it.version, id)
		}

		assets := make([]model.Asset, 0, len(b.assets))
		for i := range b.assets {
			assets = append(assets, model.Asset{ABI: b.abis[i], File: b.assets[i].Name, Size: b.assets[i].Size})
		}
		v.Assets = sortAssets(assets)
		out = append(out, v)
	}
	return out, rep, nil
}

// findOldVersion 在旧账本里按 version token 找一个版本，返回它与其位次（找不到时位次为 -1）。
func findOldVersion(old []model.Version, version string) (*model.Version, int) {
	for i := range old {
		if old[i].Version == version {
			return &old[i], i
		}
	}
	return nil, -1
}

// sortAssets 按契约顺序排列分片，并去掉同一 ABI 的重复项（保留先出现的那份）。
//
// 去重不是洁癖：同一 ABI 出现两次意味着 Release 里真有两个不同名的文件指向同一个架构
// （比如改名前后各留了一份），而清单里一个 ABI 只能出现一次 —— 留着两个会让客户端
// 按 ABI 折叠时结果取决于遍历顺序。
func sortAssets(as []model.Asset) []model.Asset {
	seen := make(map[string]bool, len(as))
	out := make([]model.Asset, 0, len(as))
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
