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
	"github.com/market-of-labs/forge-core/internal/upstream"
)

// Plan 是一个待镜像的上游版本：从哪个源、哪个 Release、哪些 asset 候选。
type Plan struct {
	Source  model.Source
	Release gh.Release
	Picked  []gh.Asset
	Skipped []upstream.Skip
}

// versionAcc 是镜像过程中为一个 version token 攒起来的分片。
//
// 提到包级而不是留在 mirrorPlan 里，是因为 recordIndex 也要吃它 ——
// 本文件里所有的"每组数据"类型都放包级，免得重演一次参数类型对不上的编译错误。
type versionAcc struct {
	versionName string
	versionCode int32
	assets      []model.Asset
}

// ResolveOptions 调 resolve-upstream 的选项。
type ResolveOptions struct {
	// OnlyID 非空时只处理这一个 appId（handle-dispatch 的 push 分支用）。
	OnlyID string
}

// ResolveUpstream 算出"这一轮该镜像哪些上游版本"（03 §4.4 第 1–2 步）。
//
// 它**只做判断，不下载也不上传** —— 把"算"与"做"分开是为了让最难的那部分
// （该不该镜像）能被单测覆盖，而网络那部分退化成纯粹的搬运。
//
// # 该镜像哪些版本（水位线）
//
// §4.4 承诺"某天 runner 挂了、cron 被跳过、dispatch 丢了，第二天自然补齐"，
// 而 D33 又要求"收录时只镜像当刻的最新版本，更老的版本不追溯"。两句合起来只有一个解：
//
//	把上游 Release 按发布时间从新到旧排开，从最新那个往下走，直到撞见**已经镜像过的**
//	那个 Release 为止；这一段的全部版本都要镜像。一个都没镜像过 → 只取最新的那一个。
//
// 于是：跳过的那些天里发的版本会被补上（漏跑自愈），而收录之前的历史永远不会被翻出来
// （D33）。水位线用 upstreamTag 认，所以判定是纯字符串比较，一个字节都不用下。
func ResolveUpstream(ctx context.Context, c *Ctx, opts ResolveOptions) ([]Plan, *model.Report, error) {
	rep := &model.Report{}

	if opts.OnlyID != "" && c.Source(opts.OnlyID) == nil {
		return nil, rep, fmt.Errorf("sources/ 里没有 %q —— 无法为它解析上游", opts.OnlyID)
	}

	var plans []Plan
	for i := range c.Sources {
		src := c.Sources[i]
		if opts.OnlyID != "" && src.ID != opts.OnlyID {
			continue
		}
		if src.Source != model.SourceGitHub {
			// manual 源的二进制走 _incoming 上传队列（§5.5），没有上游可解析。
			continue
		}
		if src.Paused {
			// §4.4 第 1 步。**只停"追加新版本"**，不动已有 asset、也不从清单里删条目（§2.2）。
			c.Log("%s：paused=true，跳过（已有版本原样保留）", src.ID)
			continue
		}
		if src.Upstream == nil {
			// model.Validate 已经挡过，这里兜底：宁可能跑也不要在 nil 上崩。
			rep.Errorf(src.ID, "source=github 但没有 upstream 配置")
			continue
		}

		p, r, err := resolveOne(ctx, c, &src, opts)
		rep.Addf("", r)
		if err != nil {
			return nil, rep, err
		}
		if p != nil {
			plans = append(plans, *p)
		}
	}
	return plans, rep, nil
}

// resolveOne 处理一个 github 源。
func resolveOne(ctx context.Context, c *Ctx, src *model.Source, opts ResolveOptions) (*Plan, *model.Report, error) {
	rep := &model.Report{}
	repo := src.Upstream.Repo

	rels, err := c.GH.ListReleases(ctx, repo)
	if err != nil {
		// 单个上游拿不到（被删库、改名、网络抖动）不该让整次对账停摆 ——
		// 其余源照常收敛，这条以告警的形式留在日志里，下一轮再来。
		rep.Warnf(src.ID, "列上游 %s 的 Release 失败，本轮跳过：%v", repo, err)
		return nil, rep, nil
	}

	cands := upstream.Releasable(rels, src.Upstream.IncludePrerelease)
	if len(cands) == 0 {
		rep.Warnf(src.ID, "上游 %s 没有可镜像的发布（%s，或一个都没有）",
			repo, upstream.ExcludedNote(src.Upstream.IncludePrerelease))
		return nil, rep, nil
	}

	targets := pickTargets(c, src.ID, cands, rep)
	if len(targets) == 0 {
		return nil, rep, nil
	}

	pattern := src.Upstream.AssetPattern
	if pattern == "" {
		pattern = upstream.DefaultAssetPattern
	}
	re, err := upstream.CompilePattern(pattern)
	if err != nil {
		// sources 加载时已经编译过一次，这里是二道防线。
		rep.Errorf(src.ID, "assetPattern %q 非法：%v", pattern, err)
		return nil, rep, nil
	}

	// 最老的那个版本排在前面镜像：index 的位次就是"追加顺序"，
	// 而 IndexApp.Latest() 取最后一个 —— 顺序错了会把老版本当成最新。
	picked := make([]Plan, 0, len(targets))
	for _, rel := range targets {
		got, skipped := upstream.Match(rel.Assets, re.String())
		if len(got) == 0 {
			// §5.4：只有非独立安装物（纯 split / 只有 .aab）的版本**跳过并告警"需手动"**，
			// 不清空历史（D13）也不写半成品清单条目。
			rep.Warnf(src.ID, "上游 %s 的 %s 没有可独立安装的 APK（候选被全部过滤），"+
				"该版本需手动上传（§3.2 / §5.4）", repo, rel.TagName)
			continue
		}
		picked = append(picked, Plan{Source: *src, Release: rel, Picked: got, Skipped: skipped})
	}
	if len(picked) == 0 {
		return nil, rep, nil
	}

	// Plan.Skipped 只是给日志用的，不进报告 —— 一个上游 Release 里混着 .sha256、
	// .zip 是常态，每条都报一次会让真正需要人看的告警淹没在噪声里。
	c.Log("%s：%d 个版本待镜像（%s）", src.ID, len(picked), joinTags(picked))
	return &picked[0], rep, nil
}

// pickTargets 用 upstreamTag 水位线挑出本次要镜像的 Release，返回**从老到新**排列。
//
// 抽出来是因为它是对账里唯一有真正判断的一段，值得单独被测试钉住。
func pickTargets(c *Ctx, appID string, candsNewestFirst []gh.Release, rep *model.Report) []gh.Release {
	// 空候选返回 nil。调用方（resolveOne）在更早的地方就挡了"上游没有可镜像的发布"，
	// 但那是另一个函数里的防线 —— 而"一个 Release 都没有"是本函数最自然的输入之一，
	// 让它在下面的 `cands[:1]` 上 panic，等于把一个空集变成一个崩溃。
	if len(candsNewestFirst) == 0 {
		return nil
	}

	// 账本就在来源文件里（D48）。src 为 nil 的情形（上游有、sources 里没有）走不到这儿：
	// resolveOne 是从 c.Sources 出发的，`src` 就是它拿到的那个。
	src := c.Source(appID)

	// 从最新往下找第一个已镜像的 —— 它上面的（更新的）全都是漏掉的。
	firstMirrored := -1
	for i := range candsNewestFirst {
		if src != nil && src.MirroredUpstreamTag(candsNewestFirst[i].TagName) {
			firstMirrored = i
			break
		}
	}

	var targets []gh.Release
	switch {
	case firstMirrored == 0:
		// 上游最新那个已经镜像过了，无事可做。这是绝大多数轮次的正常结果。
		return nil
	case firstMirrored > 0:
		targets = candsNewestFirst[:firstMirrored]
	default:
		// 一个都没镜像过：可能是**首次收录**，也可能是我们记下的 upstreamTag
		// 对应的 Release 被上游删了。两种都只取最新的那一个 —— D33 明确
		// "收录时只镜像当刻的最新版本，更老的版本不追溯"，而误判成"首次收录"
		// 的代价只是少补几个老版本，不追溯本来就是既定口径。
		targets = candsNewestFirst[:1]
	}

	// 翻成从老到新。
	out := make([]gh.Release, len(targets))
	for i, r := range targets {
		out[len(targets)-1-i] = r
	}
	if len(out) > 1 {
		rep.Warnf(appID, "本轮要补 %d 个版本（%s）—— 通常是之前有几天没跑成，正在自动补齐（§4.4）",
			len(out), joinTagsReleases(out))
	}
	return out
}

// ---- 镜像 -------------------------------------------------------------------

// MirrorOptions 调 mirror-upstream 的选项。
type MirrorOptions struct {
	// DryRun 只走"下载 + 解析 + 命名"，不上传也不写 index。
	// 用来在上线前验证一个上游到底会被命名成什么。
	DryRun bool
}

// MirrorReport 是一条镜像结果摘要。
type MirrorReport struct {
	Uploaded int
	Skipped  int
	// Failed 是**整个应用**没镜像成的个数（区别于 Skipped：那个是"按规则不该镜像"，
	// 这个是"想镜像但出错了"）。它只用于摘要行 —— 真正的痕迹在 Problems 里。
	Failed int
	// Recorded 表示往索引里写过东西（哪怕一个字节都没上传）。
	// 它决定 index 要不要落盘：见 MirrorUpstream 末尾。
	Recorded bool
	Problems *model.Report
}

// MirrorUpstream 执行镜像：下载 → 按内容判定 → 改名 → 幂等上传（03 §4.4 第 3 步）。
//
// # 单个应用失败不中断整轮
//
// 一个应用的下载/上传出错（网络抖动、上游 asset 404、建 Release 失败）只让**它自己**
// 这一轮不镜像，其余应用照常走完，这一轮的 index 与 apps.json 照样重建并回写。
// 这跟 resolveOne 容忍"列上游失败"（upstream.go 里那句"不该让整次对账停摆"）是同一条
// 口径 —— 上一处一开始就写对了，这里当初漏了。
//
// 为什么必须容忍：apps.json 的重建在整轮**之后**（Reconcile 的 RebuildAndCheck），
// index 的落盘在本函数**末尾**。任一处失败就整体返回，等于"50 个应用里第 37 个碰上一次
// 下载 500，全市场当天都拿不到清单更新"；而水位线也没推进，下一轮会把前 36 个**已经成功**
// 的上游 APK 重新下载一遍（上传会被幂等挡下，但读元数据那趟下载省不掉）。
//
// 代价（明确接受）：**这一轮是绿的** —— 因为东西确实推上去了，符合 reconcile.yml 里
// "红 = 没推"那条既有约定。所以失败只以 ERROR 行的形式留在报告与日志里，不会让 workflow
// 变红；它也不阻断任何东西（res.Report 只被打印，唯一的阻断点是 check-manifest）。
// 漏掉的那个应用由下一轮幂等补齐。
//
// # ABI 的唯一权威是 APK 内容
//
// 文件名里的 ABI 只是**线索**，用来产生一条 mismatch 告警；决定改名成什么的永远是
// APK 里真实带了哪些原生库。这条来自用户对 §5.4 的裁决：信内容，改名按内容。
// 于是"上游把 arm64 的包命名成 x86"这种打包错误，结果是**我们按内容安放、
// 同时喊一声**，而不是把一个 x86 的名字安到一个 arm64 的包上。
func MirrorUpstream(ctx context.Context, c *Ctx, plans []Plan, opts MirrorOptions) (*MirrorReport, error) {
	out := &MirrorReport{Problems: &model.Report{}}
	if !opts.DryRun {
		if err := c.Env.RequireToken("上传 asset"); err != nil {
			return out, err
		}
	}

	for i := range plans {
		if err := c.mirrorPlan(ctx, &plans[i], opts, out); err != nil {
			// 记成**硬错误**而不是告警：整轮现在是绿的，报告与日志是它唯一的痕迹，
			// 不能让它看起来像一句无关紧要的提示。它不阻断任何东西 —— 见上面的说明。
			out.Problems.Errorf(plans[i].Source.ID,
				"镜像上游 %s 失败，本轮跳过这个应用（下一轮对账会补）：%v",
				plans[i].Release.TagName, err)
			out.Failed++
		}
	}

	// 账本在**往里面写过东西**时落盘。**必须先于 build-index** ——
	// 镜像这一步是唯一知道 versionName/versionCode/upstreamTag 的地方，
	// 而 build-index 只能从 Release 的 asset 名里读到 version 与 abi（§5.2）。
	//
	// 判据是 Recorded 而不是 Uploaded：一个字节都没上传也可能有东西要记 ——
	// 账本丢了之后重跑，asset 名让每次上传都被幂等挡下，但 upstreamTag 是这一轮
	// 重新认出来的，记下它下一轮才不会再去列一遍上游（并让水位线重新生效）。
	if out.Recorded && !opts.DryRun {
		ids := make([]string, 0, len(plans))
		for i := range plans {
			ids = append(ids, plans[i].Source.ID)
		}
		if err := writeLedgers(c, ids); err != nil {
			return out, err
		}
	}
	return out, nil
}

func (c *Ctx) mirrorPlan(ctx context.Context, p *Plan, opts MirrorOptions, out *MirrorReport) error {
	appID := p.Source.ID
	c.Log("%s：镜像上游 %s", appID, p.Release.TagName)

	var rel *gh.Release
	existing := map[string]gh.Asset{}
	if !opts.DryRun {
		var err error
		rel, err = c.EnsureRelease(ctx, appID, p.Source.Name)
		if err != nil {
			return fmt.Errorf("准备 %s 的 Release：%w", appID, err)
		}
		existing, err = c.ReleaseAssets(ctx, rel)
		if err != nil {
			return fmt.Errorf("列 %s 的 asset：%w", appID, err)
		}
		// 顺手把 Release 正文刷成上游 README（D51）。只告警不返回错误：正文是展示信息，
		// 不该让一个应用这一轮的镜像白跑（§4.4 的容忍口径），下一轮镜像时会再来一次。
		if err := c.syncReleaseBody(ctx, rel, p.Source.Upstream.Repo); err != nil {
			out.Problems.Warnf(appID, "%v", err)
		}
	}

	// 这一轮为这个上游 Release 攒出的分片。key 是 version token。
	got := map[string]*versionAcc{}
	var tokens []string

	for _, a := range p.Picked {
		meta, err := c.readAssetMeta(ctx, p.Source.Upstream.Repo, a)
		if err != nil {
			out.Problems.Warnf(appID, "解析 %s 失败，跳过：%v", a.Name, err)
			out.Skipped++
			continue
		}

		// 02 规则 7：条目 id 必须等于 APK 里的 package。
		// 这一条是**硬**的 —— 名字对不上意味着我们可能正在把 A 的包安到 B 的名字下，
		// 而客户端装上之后 id 与实际包名不符，后续一切都错位。
		if meta.Package != appID {
			out.Problems.Warnf(appID, "%s 的 package 是 %q，与 appId 不符（02 规则 7），跳过。"+
				"这是**上游打包错了**，不是我们改名改错 —— 请看上游 %s 的产物",
				a.Name, meta.Package, p.Source.Upstream.Repo)
			out.Skipped++
			continue
		}

		contentABI := meta.ABIToken()
		if mm, bad := upstream.CheckMismatch(a.Name, contentABI); bad {
			// 只告警，不改判。见本函数文档。
			out.Problems.Warnf(appID, "%v —— 已按内容判定为 %s（信内容，改名按内容）", mm, contentABI)
		}
		if len(meta.UnknownABIs()) > 0 {
			out.Problems.Warnf(appID, "%s 里有不在固定 ABI 集内的 lib 目录 %v，已保守判为 %s",
				a.Name, meta.UnknownABIs(), contentABI)
		}

		// abiWhitelist 是**镜像侧**的限制（控体积，D14），按内容判出的 ABI 来筛 ——
		// 按文件名筛的话，一个标着 arm64 实为 universal 的包会漏过 whitelist。
		if !abiAllowed(p.Source.ABIWhitelist, contentABI) {
			c.Log("  %s：内容判为 %s，不在 abiWhitelist %v 内，跳过",
				a.Name, contentABI, p.Source.ABIWhitelist)
			out.Skipped++
			continue
		}

		if !meta.HasVersionName() {
			// 拿不到 versionName 就**无法命名** —— {version} 这个 token 只能来自它。
			// 硬塞一个别的值（比如用 versionCode）会让 latestVersion 与文件名分家。
			out.Problems.Warnf(appID, "%s 里没有 versionName（拿不到就无法生成 {version} token），跳过", a.Name)
			out.Skipped++
			continue
		}
		version, err := naming.SanitizeVersion(meta.VersionName)
		if err != nil {
			out.Problems.Warnf(appID, "%s：%v", a.Name, err)
			out.Skipped++
			continue
		}

		target, err := c.Endpoints.AssetName(appID, version, contentABI)
		if err != nil {
			return fmt.Errorf("渲染 %s 的目标文件名：%w", appID, err)
		}

		// 幂等闸门（规则 3）：目标名已存在就跳过，**绝不 --clobber** ——
		// 覆盖会让正在下载的客户端拿到半个文件。
		if _, dup := existing[target]; dup {
			c.Log("  %s → %s：目标已存在，跳过（幂等）", a.Name, target)
			out.Skipped++
		} else if opts.DryRun {
			c.Log("  [dry-run] %s → %s（%d 字节，内容判 %s）", a.Name, target, a.Size, contentABI)
		} else {
			if err := c.uploadAsset(ctx, p.Source.Upstream.Repo, rel, target, a); err != nil {
				return err
			}
			// 本轮的 map 里也记上，免得同一个 Release 里两个 asset 撞到同一个目标名时
			// 第二次上传才发现名字被占了。
			existing[target] = a
			out.Uploaded++
		}

		b, ok := got[version]
		if !ok {
			b = &versionAcc{versionName: meta.VersionName, versionCode: meta.VersionCode}
			got[version] = b
			tokens = append(tokens, version)
		}
		b.assets = append(b.assets, model.Asset{ABI: contentABI, File: target, Size: a.Size})
	}

	if opts.DryRun || len(tokens) == 0 {
		return nil
	}
	c.recordIndex(appID, p, got, tokens)
	out.Recorded = true
	return nil
}

// recordIndex 把这一轮镜像出的版本写进内存里的账本。
//
// 已存在的 version token 走**合并**而不是追加：同一个版本可能分几次镜像完
// （比如上游先发 universal、几天后才补 arm64），追加会造出两条同 version 的记录，
// 而 apkUrls 只能指向其中一条。
func (c *Ctx) recordIndex(appID string, p *Plan, got map[string]*versionAcc, tokens []string) {
	// 一定有：resolveOne / pickTargets 是从 c.Sources 出发的，能走到镜像就说明
	// 这个 appID 有来源文件。所以这里不需要"凭空造一个"的分支。
	src := c.Source(appID)

	sort.Strings(tokens) // 只为日志稳定；真正的位次由追加顺序决定
	for _, token := range tokens {
		b := got[token]
		na := make([]model.Asset, 0, len(b.assets))
		for _, a := range b.assets {
			dup := false
			for _, e := range na {
				if e.ABI == a.ABI {
					dup = true
					break
				}
			}
			if !dup {
				na = append(na, a)
			}
		}
		assets := sortAssets(na)

		if v := src.FindVersion(token); v != nil {
			v.Assets = mergeAssets(v.Assets, assets)
			if v.VersionName == "" {
				v.VersionName = b.versionName
			}
			if v.VersionCode == 0 {
				v.VersionCode = b.versionCode
			}
			if v.ReleaseNote == "" {
				v.ReleaseNote = p.Release.Body
			}
			v.UpstreamTag = p.Release.TagName
			continue
		}

		src.Versions = append(src.Versions, model.Version{
			Version:     token,
			VersionName: b.versionName,
			VersionCode: b.versionCode,
			// PublishedAt 取**上游**的发布时间（不是我们的上传时间）：清单的 releaseDate
			// 是给用户看的"这个版本什么时候发的"，用镜像时间会系统性地偏晚。
			PublishedAt: p.Release.PublishedAt,
			UpstreamTag: p.Release.TagName,
			// 上游那一版的更新说明。只在这一轮真的镜像了它时才拿得到，所以它随
			// upstreamTag 一起落账本 —— 之后重建账本只能靠继承（见 index.go）。
			ReleaseNote: p.Release.Body,
			Assets:      assets,
		})
	}
}

// mergeAssets 把新的分片并进已有列表，同一 ABI 以新的为准。
//
// 为什么可能"同一个 ABI 换个文件名"：上游把一个包重新打包（比如从 arm64-v8a-config
// 变成 fat）会让同一个 ABI 落到不同的 {version} 上 —— 那是另一个 version token，
// 走不到这儿。真走到这儿的是"同一版本同一 ABI 但文件名变了"，极少见，
// 此时以本轮上传的为准（它是刚刚才验证过的）。
func mergeAssets(old, add []model.Asset) []model.Asset {
	byABI := map[string]model.Asset{}
	for _, a := range old {
		byABI[a.ABI] = a
	}
	for _, a := range add {
		byABI[a.ABI] = a
	}
	out := make([]model.Asset, 0, len(byABI))
	for _, a := range byABI {
		out = append(out, a)
	}
	return sortAssets(out)
}

// readAssetMeta 下载一个上游 asset 并读它的 APK 元数据。
//
// `repo` 是**上游**仓库（`a` 的出处），不是 store —— 见 downloadToTemp。
func (c *Ctx) readAssetMeta(ctx context.Context, repo string, a gh.Asset) (*apkmeta.Meta, error) {
	path, cleanup, err := c.downloadToTemp(ctx, repo, a.ID, "forge-upstream-*.apk")
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return apkmeta.Read(path)
}

// syncReleaseBody 把内部 Release 的正文刷成上游 README（D51）。
//
// # 为什么正文是 README，而版本说明进的是账本
//
// 内部 Release 是**一个 App 一个**（`tag = {appId}`，全部版本的 asset 都挂在同一个
// Release 下），正文因此只有一份 —— 而"这一版改了什么"是每版一份的东西，塞进这一份
// 正文里只会互相覆盖，所以它进账本（`model.Version.ReleaseNote`）。正文留给
// "这个 App 是干什么的"，那正好是 README：项目级、与版本无关。
//
// 正文相同就不 PATCH：README 是项目级的，绝大多数轮次里它一个字节都没变，
// 没必要每次镜像都写一次 Release（而 PATCH 改正文不产生状态跃迁，不会误发车）。
//
// 正文存的是**改写过的** README：仓库内的相对路径会被拼成指向源项目的绝对地址
// （理由见 relink.go —— 正文渲染在 release 页面那个 base 下，`./img/x.png` 在那儿
// 是 404）。那个改写是幂等的，所以上面"相同就不 PATCH"照旧成立。
func (c *Ctx) syncReleaseBody(ctx context.Context, rel *gh.Release, repo string) error {
	md, err := c.GH.Readme(ctx, repo)
	if err != nil {
		return fmt.Errorf("取上游 %s 的 README 失败，%s 的正文保持原样：%w", repo, rel.TagName, err)
	}
	md = relinkReadme(md, repo)
	if md == "" || md == rel.Body {
		return nil
	}
	body := md
	if _, err := c.GH.UpdateRelease(ctx, c.Env.StoreRepo, rel.ID, gh.ReleasePatch{Body: &body}); err != nil {
		return fmt.Errorf("写 %s 的 Release 正文失败：%w", rel.TagName, err)
	}
	c.Log("  %s 的正文已同步为上游 README（%d 字节）", rel.TagName, len(md))
	return nil
}

// uploadAsset 把一个上游 asset 以目标名上传到内部 Release。
//
// `repo` 是**上游**仓库：先从那儿把内容取下来，再传到 store（见 downloadToTemp）。
func (c *Ctx) uploadAsset(ctx context.Context, repo string, rel *gh.Release, target string, a gh.Asset) error {
	path, cleanup, err := c.downloadToTemp(ctx, repo, a.ID, "forge-upload-*.apk")
	if err != nil {
		return err
	}
	defer cleanup()

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	// 长度必须来自文件本身：UploadAsset 要如实报 Content-Length（见那里的 ⚠️），
	// 而 gh.Asset.Size 是上游自己报的数，不该拿它当我们的字节数。
	fi, err := f.Stat()
	if err != nil {
		return err
	}

	if _, err := c.GH.UploadAsset(ctx, c.Env.StoreRepo, rel.ID, target, f, fi.Size()); err != nil {
		return fmt.Errorf("上传 %s：%w", target, err)
	}
	c.Log("  %s → %s（%d 字节）", a.Name, target, a.Size)
	return nil
}

// downloadToTemp 把一个 asset 落到临时文件，返回路径与清理函数。
//
// 落盘而不是读进内存：APK 动辄上百 MB，而 apkmeta 要的是 io.ReaderAt，
// 留在内存里等于同时占两份。
//
// ⚠️ `repo` **必须由调用方给**（store 自己的 asset 传 StoreRepo，上游 asset 传
// 上游仓库）。asset id 是**仓库内**的编号，拿 A 仓库的 id 去 B 仓库要必然 404 ——
// 这里曾经写死过 StoreRepo，于是"上游 asset 一个也下不下来"，而失败在镜像侧
// 被 D44 容忍成一条 WARN，症状是"每天都是绿的、什么都没镜像"。
func (c *Ctx) downloadToTemp(ctx context.Context, repo string, assetID int64, pattern string) (string, func(), error) {
	rc, _, err := c.GH.DownloadAsset(ctx, repo, assetID)
	if err != nil {
		return "", func() {}, err
	}
	defer rc.Close()

	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", func() {}, err
	}
	name := f.Name()
	cleanup := func() { os.Remove(name) }

	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("落盘 asset %d：%w", assetID, err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return name, cleanup, nil
}

// abiAllowed 报告内容判出的 ABI 是否通过白名单。空白名单放行全部。
func abiAllowed(whitelist []string, abi string) bool {
	if len(whitelist) == 0 {
		return true
	}
	for _, w := range whitelist {
		if w == abi {
			return true
		}
	}
	return false
}

// ---- 日志小工具 -------------------------------------------------------------

func joinTags(plans []Plan) string {
	ts := make([]string, len(plans))
	for i := range plans {
		ts[i] = plans[i].Release.TagName
	}
	return strings.Join(ts, ", ")
}

func joinTagsReleases(rs []gh.Release) string {
	ts := make([]string, len(rs))
	for i := range rs {
		ts[i] = rs[i].TagName
	}
	return strings.Join(ts, ", ")
}

// DescribePlans 给 dry-run 输出一段人类可读的计划表。
func DescribePlans(plans []Plan) string {
	var b strings.Builder
	for _, p := range plans {
		fmt.Fprintf(&b, "%s ← %s @ %s\n", p.Source.ID, p.Source.Upstream.Repo, p.Release.TagName)
		for _, a := range p.Picked {
			fmt.Fprintf(&b, "    %s (%d 字节)\n", a.Name, a.Size)
		}
		for _, s := range p.Skipped {
			fmt.Fprintf(&b, "    [跳过] %s —— %s\n", s.Name, s.Reason)
		}
	}
	if b.Len() == 0 {
		return "（没有待镜像的版本）\n"
	}
	return b.String()
}
