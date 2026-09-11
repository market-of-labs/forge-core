package job

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/market-of-labs/forge-core/internal/apkmeta"
	"github.com/market-of-labs/forge-core/internal/gh"
	"github.com/market-of-labs/forge-core/internal/issue"
	"github.com/market-of-labs/forge-core/internal/model"
	"github.com/market-of-labs/forge-core/internal/upstream"
)

// ---- 待办 issue 扫描：登记的申请 → sources/{appId}.json（03 §2.6） --------------
//
// 新增申请不在 intake 那一刻落盘，因为申请人只填了 repo —— appId 要从上游 APK 里
// 读出来，而那是网络动作，DecideIntake 是纯函数、不许联网。于是切成两拍：
//
//	拍 1（秒级，纯本地）intake：本地校验 → 打「待收录」标签 → 回评「已收到」
//	拍 2（每日，本文件）对账：扫这两个标签 → 去上游读出身份 → 落盘 → 回评 → 关单
//
// # issue 本身就是队列
//
// 不另建数据库：标签就是状态，而**标签跳变**就是"该回评了"的信号。于是回评天然幂等 ——
// 状态没变的单，一条评论都不加。这一条很重要：扫描每天跑一次，靠"每次都说一遍"
// 会把回评刷成没人看的墙纸。
//
// 早先想过让申请人在**评论里**补正则（一行 `assetPattern: <正则>`）。放弃了：
// 那等于给"评论里的任意文字"开一条进入执行环境的路径（§5.5），而编辑正文能达到
// 完全相同的效果、且走的是**已有**的解析路径（DecideIntake 原样复用）。
// 于是整个模块不需要读评论，外部输入面一个字节都没扩大。

// PendingResult 是一次待办扫描的结果。
type PendingResult struct {
	Landed  []string // 已落地并关单的 appId
	Held    []int    // 仍待补充的 issue 号（下一轮还会看）
	Skipped []int    // 不是新增单、被跳过的 issue 号
}

// ScanPendingIssues 扫描待办队列：去上游确认身份，落盘，回评，关单。
//
// 它写文件、提交、回评、关单 —— 是**有副作用**的，dry-run 时调用方要跳过它。
func ScanPendingIssues(ctx context.Context, c *Ctx) (*PendingResult, error) {
	if err := c.Env.RequireToken("扫描待办 issue"); err != nil {
		return nil, err
	}

	queue, err := c.pendingQueue(ctx)
	if err != nil {
		return nil, err
	}
	if len(queue) == 0 {
		c.Log("待办队列是空的")
		return &PendingResult{}, nil
	}
	c.Log("待办队列有 %d 张单", len(queue))

	res := &PendingResult{}
	for _, is := range queue {
		if err := c.resolvePending(ctx, is, res); err != nil {
			// 一张单失败不该让整轮停摆 —— 它留在队列里，下一轮（或下次标签跳变）
			// 重来。而其余单照常处理，那些多半是无关的。
			c.Log("#%d 处理失败，本轮跳过：%v", is.Number, err)
			res.Held = append(res.Held, is.Number)
		}
	}
	return res, nil
}

// pendingQueue 拉出两个待办标签下的开着的 issue，按号排序。
//
// 两个标签各拉一次再去重：一张单理论上只该带其中一个，但手工改标签是可能的，
// 去重比"相信它"便宜。
func (c *Ctx) pendingQueue(ctx context.Context) ([]gh.Issue, error) {
	seen := map[int]bool{}
	var queue []gh.Issue
	for _, lb := range queueLabels {
		issues, err := c.GH.ListOpenIssues(ctx, c.Env.StoreRepo, lb)
		if err != nil {
			return nil, fmt.Errorf("列出带「%s」标签的 issue：%w", lb, err)
		}
		for _, is := range issues {
			if !seen[is.Number] {
				seen[is.Number] = true
				queue = append(queue, is)
			}
		}
	}
	// 按号排序：先开单的先生效，日志也可复现（map 遍历顺序是随机的）。
	sort.Slice(queue, func(i, j int) bool { return queue[i].Number < queue[j].Number })
	return queue, nil
}

// resolvePending 处理一张待办单。
func (c *Ctx) resolvePending(ctx context.Context, is gh.Issue, res *PendingResult) error {
	// 本地重评。申请人可能刚**编辑过正文**（回评就是这么请他的），而
	// "这份申请现在合法了吗"必须有同一个答案 —— 所以原样复用 DecideIntake，
	// 而不是在这里另写一套判断。
	d := DecideIntake(c, is.Body)
	if d.Kind != issue.KindAdd {
		// 变更单永远不会被打上这两个标签，所以走到这里只可能是手工加的。
		c.Log("#%d 不是新增申请（标签是手工打上去的？），跳过", is.Number)
		res.Skipped = append(res.Skipped, is.Number)
		return nil
	}
	if !d.Accept {
		// 本地就不合法（repo 形状、正则、词表）。这不是"缺信息"，
		// 而是申请人得改正文 —— 同一个出口。
		return c.holdForInfo(ctx, is, d.Reply)
	}

	src, tag, err := c.probeIdentity(ctx, d.Source)
	if err != nil {
		return c.holdForInfo(ctx, is, fmt.Sprintf(
			"**没能从上游确定这个应用的身份**，本次收录没有完成。\n\n%s\n\n---\n%s",
			err, retryHint))
	}

	// 已经有同 appId 的来源。appId 现在是从 APK 里读出来的，所以这一撞
	// 一定有实据（不是"名字像"），值得直接拒绝。
	if old := c.Source(src.ID); old != nil {
		return c.holdForInfo(ctx, is, fmt.Sprintf(
			"解析出来的包名是 `%s`，而 `sources/` 里**已经有**这个应用了"+
				"（来自 `%s`）。\n\n"+
				"Obtainium 用包名当安装身份，所以同一个包名在本市场里只能有一条记录。\n\n"+
				"要改它的元数据、暂停或移除，请用 **`change-source.yml`**（03 §2.5 规则 5）。",
			src.ID, old.Upstream.Repo))
	}

	if err := c.Repo.WriteSource(src); err != nil {
		return fmt.Errorf("写 sources/%s.json：%w", src.ID, err)
	}
	if _, err := c.CommitBack(ctx, fmt.Sprintf("收录 %s（#%d）", src.ID, is.Number)); err != nil {
		return err
	}
	res.Landed = append(res.Landed, src.ID)
	// 立刻并进内存里的工作副本，**不等 Reconcile 收尾时的 Load()**。
	// 否则同一轮里两张单指向同一个包名时，上面那句 `c.Source(src.ID) != nil`
	// 对第二张单不成立（它只看得到开轮时的快照），于是两张都报「已收录」，
	// 而第二份文件把第一份**静默覆盖**掉 —— 申请人收到的回评是假的。
	//
	// c.Source / SourcesByRepo 返回指向本切片的指针，所以这里 append 会让它们失效；
	// 调用点都只在本轮内即时使用、不跨这次 append 持有，故安全。
	c.Sources = append(c.Sources, *src)

	// 先落盘再回评（与 IntakeIssue 同一个顺序）：反过来会把"已收录"留在一条
	// 实际上没落地的单上，而之后的补跑会因"已存在"而拒绝它，那句假话就永远在了。
	reply := fmt.Sprintf(
		"已收录 `%s`。\n\n"+
			"| 字段 | 值 |\n|---|---|\n"+
			"| 包名（appId） | `%s` |\n| 显示名（列表里显示的） | %s |\n| 作者 / 组织 | %s |\n"+
			"| 上游仓库 | `%s` |\n| 资产正则 | `%s` |\n| 分类标签 | %s |\n| 只镜像 ABI | %s |\n\n"+
			"**包名与显示名是从 APK 里读出来的**（上游发布 `%s`），作者取仓库 owner —— "+
			"都不是申请时填的。显示名不对的话请另开一张 `change-source.yml`。\n"+
			"显示名里中点后面那段是**你填的简介**（超过 %d 个字会在这里显示成裁过的样子）。\n\n"+
			"下一轮对账会把上游当前最新的版本镜像进来（03 §4.4）。在此之前它不会出现在 "+
			"`apps.json` 里 —— 一个还没有任何版本的条目在设备端是个点不动的空壳，"+
			"所以刻意不出（03 §5.4）。",
		src.ID, src.ID, src.DisplayName(), src.Author, src.Upstream.Repo,
		orDefault(src.Upstream.AssetPattern, DefaultAssetPatternNote),
		orDefault(strings.Join(src.Categories, " / "), "未勾选"),
		orDefault(strings.Join(src.ABIWhitelist, " / "), "全部"),
		tag, model.MaxDescRunes)

	if err := c.GH.CommentIssue(ctx, c.Env.StoreRepo, is.Number, reply); err != nil {
		return fmt.Errorf("回评 issue #%d：%w", is.Number, err)
	}
	if err := c.GH.CloseIssue(ctx, c.Env.StoreRepo, is.Number); err != nil {
		return fmt.Errorf("关闭 issue #%d：%w", is.Number, err)
	}
	c.Log("#%d 已收录 %s（%s），回评并关闭", is.Number, src.ID, src.Name)
	return nil
}

// holdForInfo 把一张单置为「待补充」并回评。
//
// **只在标签真的要变的时候回评**：这是整套幂等性的落点 —— 状态没变的单一条评论都不加。
func (c *Ctx) holdForInfo(ctx context.Context, is gh.Issue, reply string) error {
	if is.HasLabel(LabelNeedInfo) {
		c.Log("#%d 仍待在补充，状态未变，不重复回评", is.Number)
		return nil
	}
	if err := c.GH.AddLabels(ctx, c.Env.StoreRepo, is.Number, LabelNeedInfo); err != nil {
		return fmt.Errorf("给 issue #%d 打「%s」标签：%w", is.Number, LabelNeedInfo, err)
	}
	if is.HasLabel(LabelPending) {
		if err := c.GH.RemoveLabel(ctx, c.Env.StoreRepo, is.Number, LabelPending); err != nil {
			return fmt.Errorf("摘掉 issue #%d 的「%s」标签：%w", is.Number, LabelPending, err)
		}
	}
	c.Log("#%d 标签跳变为「%s」", is.Number, LabelNeedInfo)
	if err := c.GH.CommentIssue(ctx, c.Env.StoreRepo, is.Number, reply); err != nil {
		return fmt.Errorf("回评 issue #%d：%w", is.Number, err)
	}
	return nil
}

// probeIdentity 去上游读出这个来源的**身份三件套**，产出一份可以落盘的完整 Source。
//
// # 只探最新那个有匹配资产的 Release
//
// D33 说收录只镜像当刻最新的版本，而身份应当来自**实际会被镜像的那一份** ——
// 所以从最新往下找到第一个真能取出元数据的 Release 就停。上游最新那个 Release
// 只放了源码包（没有 APK）是常见情况，所以要能往下退。
//
// # 多包名 = 拒绝（appId 自动派生之后唯一剩下的把关点）
//
// 同一个 Release 里出现两个不同 package，意味着光凭 repo 定位不到"哪一个应用"。
// **不能随便挑一个**：挑错了就是静默地收录了一个申请人没想要的应用，而
// 这个错误唯一的表现是设备上多了一行 —— 没人会去核对。
//
// 返回第二个值是该 Release 的 tag，只用于回评里写清"身份是从哪次发布读出来的"。
func (c *Ctx) probeIdentity(ctx context.Context, half *model.Source) (*model.Source, string, error) {
	repo := half.Upstream.Repo

	rels, err := c.GH.ListReleases(ctx, repo)
	if err != nil {
		return nil, "", fmt.Errorf("列上游 `%s` 的 Release 失败（仓库名写错了？私有库？）：%w", repo, err)
	}
	cands := upstream.Releasable(rels)
	if len(cands) == 0 {
		return nil, "", fmt.Errorf("上游 `%s` 没有可镜像的发布（全是 draft/prerelease，或一个都没有）", repo)
	}

	pattern := half.Upstream.AssetPattern
	if pattern == "" {
		pattern = upstream.DefaultAssetPattern
	}
	re, err := upstream.CompilePattern(pattern)
	if err != nil {
		// 二道防线：DecideIntake 已经编译过一次。
		return nil, "", fmt.Errorf("资产正则 %q 非法：%w", pattern, err)
	}

	return pickIdentityRelease(half, pattern, re, cands, func(rel gh.Release, picked []gh.Asset) []*apkmeta.Meta {
		return c.probeMetas(ctx, rel, picked)
	})
}

// pickIdentityRelease 在候选 Release 里从新到旧找第一个**真能定出身份**的，定出它。
//
// 抽成独立函数（而不是留在 probeIdentity 里）只有一个理由：让"走几层才停"这件事
// 可以在没有真 APK、也没有网络的情况下被测试 —— 而它恰恰是本模块里最容易写错的一段。
// readMetas 是唯一的注入点，生产路径传的就是 probeMetas。
func pickIdentityRelease(half *model.Source, pattern string, re *regexp.Regexp,
	cands []gh.Release, readMetas func(gh.Release, []gh.Asset) []*apkmeta.Meta,
) (*model.Source, string, error) {
	for _, rel := range cands {
		picked, _ := upstream.Match(rel.Assets, re.String())
		if len(picked) == 0 {
			continue
		}
		metas := readMetas(rel, picked)
		if len(metas) == 0 {
			continue
		}
		src, err := buildIdentity(half, rel, metas)
		if err != nil {
			// 定不出身份**不往下退**：多包名是"这个仓库同时发布了几个应用"，
			// 换一个更老的 Release 只会换一批包名，问题一模一样。直接把它报上去。
			return nil, "", err
		}
		return src, rel.TagName, nil
	}
	return nil, "", fmt.Errorf(
		"上游 `%s` 最近的发布里没有能解析出包名的 APK（匹配 `%s`）。\n\n"+
			"如果它的资产命名特殊，请在「资产匹配正则」里写一条能匹配到的正则，"+
			"然后**编辑本单正文**重填一次。", half.Upstream.Repo, pattern)
}

// probeMetas 读出这批 asset 的元数据。
//
// 读不动的跳过并记日志，**不中断**：一个 Release 里混着 .zip/.sha256/源码包是常态，
// 为它们放弃整张申请不值得。而"一个都读不出来"由调用方判空处理。
func (c *Ctx) probeMetas(ctx context.Context, rel gh.Release, picked []gh.Asset) []*apkmeta.Meta {
	out := make([]*apkmeta.Meta, 0, len(picked))
	for _, a := range picked {
		m, err := c.readAssetMeta(ctx, a)
		if err != nil {
			c.Log("  %s/%s：读元数据失败，跳过（%v）", rel.TagName, a.Name, err)
			continue
		}
		if m.Package == "" {
			c.Log("  %s/%s：包里没有 package 名，跳过", rel.TagName, a.Name)
			continue
		}
		out = append(out, m)
	}
	return out
}

// buildIdentity 从元数据定出身份，填进那份半成品 Source。
func buildIdentity(half *model.Source, rel gh.Release, metas []*apkmeta.Meta) (*model.Source, error) {
	// 一个 Release 里的多个 asset 是**同一个应用的分片**（按 ABI 拆），
	// 包名相同 —— 所以按包名归并，剩下的条数才是"这个仓库同时有几个应用"。
	byPkg := make(map[string]*apkmeta.Meta, len(metas))
	for _, m := range metas {
		if _, ok := byPkg[m.Package]; !ok {
			byPkg[m.Package] = m
		}
	}
	if len(byPkg) > 1 {
		pkgs := make([]string, 0, len(byPkg))
		for p := range byPkg {
			pkgs = append(pkgs, p)
		}
		sort.Strings(pkgs)
		return nil, fmt.Errorf(
			"上游 `%s` 的 `%s` 里有 **%d 个不同的包名**：`%s`。\n\n"+
				"光凭仓库定位不到你要的是哪一个，请在「资产匹配正则」里写一条只匹配目标 APK 的"+
				"正则（例如 `(?i)myapp.*\\.apk$`），然后**编辑本单正文**重填一次。",
			half.Upstream.Repo, rel.TagName, len(pkgs), strings.Join(pkgs, "`、`"))
	}

	src := *half
	for _, m := range byPkg { // 只剩一个
		src.ID = m.Package
		src.Name = m.Label
	}
	if src.Name == "" {
		// label 读不到（属性缺失，或它指向 @string 而包里没带 resources.arsc）。
		// 退回仓库名而不是留空：空显示名在 Obtainium 里是一行**没有标题的条目**，
		// 比"名字不完美"糟得多。
		src.Name = repoOwner(half.Upstream.Repo)
	}
	// 作者取仓库 owner。**这是 D30 口径下的唯一可派生值** —— APK 里没有作者字段，
	// 而上游仓库的 owner 至少是个有实据的归属（不是从应用名猜的）。
	src.Author = repoOwner(half.Upstream.Repo)
	return &src, nil
}

// repoOwner 取 owner/repo 的 owner 段。
//
// 不做任何"美化"（去公司后缀、转大小写）：作者是要写进 sources/ 当事实的，
// 猜出来的作者会在设备上显示成 `by X`，而 X 是编的。
func repoOwner(repo string) string {
	if i := strings.IndexByte(repo, '/'); i > 0 {
		return repo[:i]
	}
	return repo
}
