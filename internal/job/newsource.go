package job

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/market-of-labs/forge-core/internal/apkmeta"
	"github.com/market-of-labs/forge-core/internal/gh"
	"github.com/market-of-labs/forge-core/internal/model"
	"github.com/market-of-labs/forge-core/internal/upstream"
)

// ---- 新增申请 → sources/{appId}.json（03 §2.6） -------------------------------
//
// 新增申请**不在** `DecideIntake` 里落盘，因为申请人只填了 repo —— appId 要从上游 APK
// 里读出来，而那是网络动作，DecideIntake 是纯函数、不许联网。所以裁决之后，收录这条链
// 在**同一个 issue 事件里**一路走完：
//
//	探身份 → 落盘 → 只同步这一个应用 → 回评 → 关单
//
// # 没有队列，也没有标签
//
// 单**本身就是**那条待办记录：任何一步没成，本文件都不关单，只回评写明原因。于是重试
// 有两个入口，都够用：
//
//	申请人改完正文 → store 的 forward.yml 监听 issues[edited] → 整条链重新发车
//	维护者手工 `forge intake-issue <N>` → 同一棵树、同一个函数
//
// 曾经这里有一套「待收录 / 待补充」标签 + 每日扫标签的补漏路径。删掉它，是因为它实际
// 只覆盖了一个很窄的窗口 —— 同一次运行里、落盘之后、收尾（提交/回评）之前崩了。事件
// 整体丢失（PAT 过期、dispatch 掉包）时标签还没来得及打上，照样救不了；而**落盘之后**
// 的重试本来就由每日对账的幂等性兜着（§4.4 的漏跑自愈），与标签无关。
//
// # 不读评论
//
// 早先想过让申请人在**评论里**补正则（一行 `assetPattern: <正则>`）。放弃了：那等于给
// "评论里的任意文字"开一条进入执行环境的路径（§5.5），而编辑正文能达到完全相同的效果、
// 且走的是**已有**的解析路径（DecideIntake 原样复用）。于是整个模块不需要读评论，
// 外部输入面一个字节都没扩大。

// intakeNewSource 是新增单的收录链，由 `IntakeIssue` 在裁决之后紧接着调用。
//
// 返回这次单项目同步的结果（**没落地时为 nil**）。落盘失败与同步失败是两件事：前者由
// err 交给调用方（该红 —— 工作副本可能已经脏了），后者已经写进回评里了，它不影响本单
// 的结论（来源是真的，只是版本晚一点到）。
func (c *Ctx) intakeNewSource(ctx context.Context, is gh.Issue, d *IntakeDecision) (*ReconcileResult, error) {
	src, tag, committed, err := c.landNewSource(ctx, is, d)
	d.Committed = committed
	if err != nil || src == nil {
		// src == nil 表示这张单**没有落地**（本地不合法 / 身份定不出 / 撞了别的来源），
		// 而这三个出口该说的话 askForInfo 已经在里面说过了 —— 这里再说话就是重复。
		return nil, err
	}

	// 只收敛这一个 appId（03 §4.3）：进来的是一张单，不是"该全局收敛了"。全量的成本是
	// 遍历所有上游，而申请人只关心他提交的那一个。这一步顺带把 apps.json 重建出来，
	// 所以新应用是"一分钟内可装"，而不是"最多一天"。
	r, rerr := Reconcile(ctx, c, ReconcileOptions{OnlyID: src.ID})
	if r != nil {
		d.Committed = d.Committed || r.Commited
	}
	if rerr != nil {
		// 不冒泡：来源已经落盘，那是不可逆的事实，这一轮的同步成不成改不了它。
		// 该说的话写进回评（见 syncedNote），本单照样关。
		c.Log("收录 %s 之后的单项目同步没跑完（不影响落盘，下一轮对账会补）：%v", src.ID, rerr)
	}
	return r, c.announceLanded(ctx, is, src, landedReply(src, tag, d.DescNote, r, rerr))
}

// landNewSource 把一张新增单落成 `sources/{appId}.json`，返回落地的来源、读出身份的
// 那个 tag、以及**这一次有没有真的提交**（内容没变时 CommitBack 会安静地不提交）。
//
// 返回 `src == nil` 表示**没有落地**，且该说的话已经说过了。调用方此时唯一要做的是
// **别再往下走** —— 尤其别去镜像一个并不存在的来源。
func (c *Ctx) landNewSource(ctx context.Context, is gh.Issue, d *IntakeDecision) (*model.Source, string, bool, error) {
	if !d.Accept {
		// 本地就不合法（repo 形状、正则、词表）。这不是"缺信息"，而是申请人得改正文 ——
		// 同一个出口，原样复用 DecideIntake 写好的那句拒绝理由。
		return nil, "", false, c.askForInfo(ctx, is, d.Reply)
	}

	src, tag, err := c.probeIdentity(ctx, d.Source)
	if err != nil {
		return nil, "", false, c.askForInfo(ctx, is, fmt.Sprintf(
			"**没能从上游确定这个应用的身份**，本次收录没有完成。\n\n%s\n\n---\n%s",
			err, retryHint))
	}

	// 已经有同 appId 的来源。appId 现在是从 APK 里读出来的，所以这一撞一定有实据
	// （不是"名字像"），值得直接拒绝 —— **除非撞的是它自己**。
	//
	// 撞到自己的场合：上一次跑已经落盘了，而收尾（提交、回评）没走完。那时文件在
	// `sources/` 里、单还开着，重跑会走到这里；对一张**完全合法**的申请回一句"已经有
	// 这个应用了，请用 change-source.yml"，那句话是错的。见 sameRequest。
	if old := c.Source(src.ID); old != nil {
		if !sameRequest(old, d.Source) {
			// 真的撞了：另一份来源占着这个包名。
			// （`old.Upstream` 可能是 nil —— 手动来源没有上游，那不是"名字像"。）
			from := "（一个 `source: \"manual\"` 的手动来源，没有上游）"
			if old.Upstream != nil {
				from = fmt.Sprintf("（来自 `%s`）", old.Upstream.Repo)
			}
			return nil, "", false, c.askForInfo(ctx, is, fmt.Sprintf(
				"解析出来的包名是 `%s`，而 `sources/` 里**已经有**这个应用了"+
					"%s。\n\n"+
					"Obtainium 用包名当安装身份，所以同一个包名在本市场里只能有一条记录。\n\n"+
					"要改它的元数据、暂停或移除，请用 **`change-source.yml`**（03 §2.5 规则 5）。",
				src.ID, from))
		}
		// 是同一份申请（或别人重复提交了同一件事）：文件早就在了，只是上一次的收尾
		// 没走完。当作"已落地"继续往下 —— 重写一遍是幂等的（内容没变时 CommitBack
		// 自己会安静地不提交），而申请人需要的是那句"已收录"和关单，不是一句拒绝。
		c.Log("#%d：`%s` 已由这份申请落地过（上一轮回评/关单没走完），本次补上收尾",
			is.Number, src.ID)
	}

	if err := c.Repo.WriteSource(src); err != nil {
		return nil, "", false, fmt.Errorf("写 sources/%s.json：%w", src.ID, err)
	}
	committed, err := c.CommitBack(ctx, fmt.Sprintf("收录 %s（#%d）", src.ID, is.Number))
	if err != nil {
		return nil, "", false, err
	}
	// 立刻并进内存里的工作副本，**不等 Reconcile 收尾时的 Load()**。这一步是**载荷**
	// 而不是优化：紧接着的 `Reconcile(OnlyID)` 会去 c.Sources 里找这个 appId，
	// 找不到就直接报"`sources/` 里没有 X —— 无法为它解析上游"，于是刚收录的应用
	// 永远等不到它的第一个版本。
	//
	// c.Source 返回指向本切片的指针，所以这里 append 会让它失效；调用点只在本次 append
	// 之后重新取，不跨它持有旧的，故安全。
	c.Sources = append(c.Sources, *src)
	return src, tag, committed, nil
}

// askForInfo 把"这次没能收录"的原因回评出去。**单不关。**
//
// 关掉它就等于把这张单丢了：申请人改完正文要靠 `issues[edited]` 重新发车，而一张关着的
// 单不会再有人去动它。留开着也是这套设计里"重试"的全部实现 —— 没有队列、没有标签。
//
// 每次都回评，不做"状态没变就不说话"的节流：重跑只有编辑正文与手工跑动词这两个入口，
// 都是人主动发起的，每一次都该看到*当前*最新的原因。
func (c *Ctx) askForInfo(ctx context.Context, is gh.Issue, reply string) error {
	if err := c.GH.CommentIssue(ctx, c.Env.StoreRepo, is.Number, reply); err != nil {
		return fmt.Errorf("回评 issue #%d：%w", is.Number, err)
	}
	c.Log("#%d 未收录，回评原因并留开（改完正文会重新发车）", is.Number)
	return nil
}

// landedReply 组装「已收录」回评，末段如实交代**这一轮**镜像发生了什么。
//
// note 是裁决阶段攒下的提醒（目前只有简介截断那一条，见 decideAdd）—— 它只能从这里
// 出去：被接受的新增单在裁决阶段没有 Reply，而原始正文只在 decideAdd 里露过一面。
func landedReply(src *model.Source, tag, note string, r *ReconcileResult, rerr error) string {
	return fmt.Sprintf(
		"已收录 `%s`。\n\n"+
			"| 字段 | 值 |\n|---|---|\n"+
			"| 包名（appId） | `%s` |\n| 显示名（列表里显示的） | %s |\n| 作者 / 组织 | %s |\n"+
			"| 上游仓库 | `%s` |\n| 资产正则 | `%s` |\n| 分类标签 | %s |\n| 只镜像 ABI | %s |\n\n"+
			"%s**包名与显示名是从 APK 里读出来的**（上游发布 `%s`），作者取仓库 owner —— "+
			"都不是申请时填的，所以**请核对一下上面那个包名确实是你要的那个应用**："+
			"仓库填错时会静默收错一个应用，而这条回评是唯一的发现机会。\n"+
			"显示名不对的话请另开一张 `change-source.yml`。\n"+
			"显示名里中点后面那段是**你填的简介**（超过 %d 个字会在这里显示成裁过的样子）。\n\n"+
			"%s",
		src.ID, src.ID, src.DisplayName(), src.Author, src.Upstream.Repo,
		orDefault(src.Upstream.AssetPattern, DefaultAssetPatternNote),
		orDefault(strings.Join(src.Categories, " / "), "未勾选"),
		orDefault(strings.Join(src.ABIWhitelist, " / "), "全部"),
		note, tag, model.MaxDescRunes, syncedNote(r, rerr))
}

// announceLanded 给一张已落地的单回评并关单。
//
// 顺序仍是**先落盘、后回评**（与变更单同一条规矩）：被调用时文件已经在 `sources/` 里了，
// 所以"已收录"是真话。反过来写，就会在一条没落地的单上留一句假话，而之后的补跑会以
// "已存在"为由拒绝它，于是那句假话永远留在那儿。
func (c *Ctx) announceLanded(ctx context.Context, is gh.Issue, src *model.Source, reply string) error {
	if err := c.GH.CommentIssue(ctx, c.Env.StoreRepo, is.Number, reply); err != nil {
		return fmt.Errorf("回评 issue #%d：%w", is.Number, err)
	}
	if err := c.GH.CloseIssue(ctx, c.Env.StoreRepo, is.Number); err != nil {
		return fmt.Errorf("关闭 issue #%d：%w", is.Number, err)
	}
	c.Log("#%d 已收录 %s（%s），回评并关闭", is.Number, src.ID, src.Name)
	return nil
}

// sameRequest 判断一份已落地的来源是不是**这份申请自己**写出来的。
//
// 只比两个字段：上游仓库 + 资产正则。它们一起决定了"是哪个仓库的哪个 APK"，正好是这份
// 申请能决定的那一半（appId / 显示名 / 作者都是从 APK 与仓库里探出来的，不是申请人的
// 主张，所以不参与比较）。
//
// **拿整份文件比对是不行的**：显示名是从 APK 里读的，上游改过一次 label 就会让两份不再
// 相等 —— 那会把"自家补跑"误判成"别人撞了包名"，正是这个函数要防的那件事。
func sameRequest(old, half *model.Source) bool {
	if old == nil || half == nil || old.Upstream == nil || half.Upstream == nil {
		return false
	}
	return old.Upstream.Repo == half.Upstream.Repo &&
		old.Upstream.AssetPattern == half.Upstream.AssetPattern
}

// syncedNote 如实交代**这一轮**镜像发生了什么，作为「已收录」回评的收尾段。
//
// 三种结局都**照样关单**，理由是同一个 —— 来源已经落盘，那是不可逆的事实，本单开不开
// 都改不了它；而"版本什么时候能装"的答案也不取决于本单的状态。把失败写成提示、而不是
// 把人留在一张开着的单里，是因为每日对账本来就会补上（幂等 = 漏跑自愈）。
func syncedNote(r *ReconcileResult, err error) string {
	switch {
	case err != nil:
		return "⚠️ **来源已落盘，但紧接着的这次同步没有跑完。**\n\n" +
			"```\n" + err.Error() + "\n```\n\n" +
			"`sources/` 里这条记录是真的、不会丢；而每日对账是幂等的 —— " +
			"下一轮会把它补齐（03 §4.4 的漏跑自愈）。"
	case r != nil && r.Failed > 0:
		// 这一条在镜像改成**容忍单点失败**（D44）之后才成为必需：以前镜像出错会冒泡成
		// error 走到上面那一支，现在它被吞进 Failed 里、Reconcile 正常返回，于是会掉到
		// 下面那一支去说"上游没有匹配到资产"—— 那是**说错原因**，而"说错原因"正是
		// 这一整轮改动想消灭的东西。
		//
		// 带 OnlyID 的对账只看得见这一个应用，所以 Failed>0 时不可能同时有 Uploaded>0。
		return "⚠️ **来源已落盘，但这一轮的同步没成功**（镜像出错，对账日志里有那条 `ERROR`）。\n\n" +
			"`sources/` 里这条记录是真的、不会丢；每日对账是幂等的，下一轮会继续试（03 §4.4）。"
	case r == nil || r.Uploaded == 0:
		return "这一轮**没有版本被镜像进来**（上游没有匹配到资产？）。\n\n" +
			"来源本身已经落盘，每日对账会继续试（03 §4.4）。"
	default:
		return fmt.Sprintf("**这一轮已经把上游最新的版本镜像进来了**（新上传 %d 个 asset）—— "+
			"它现在就在 `apps.json` 里，打开 Obtainium 刷新一下就能装。", r.Uploaded)
	}
}

// probeIdentity 去上游读出这个来源的**身份三件套**，产出一份可以落盘的完整 Source。
//
// # 只探最新那个有匹配资产的 Release
//
// D33 说收录只镜像当刻最新的版本，而身份应当来自**实际会被镜像的那一份** —— 所以从最新
// 往下找到第一个真能取出元数据的 Release 就停。上游最新那个 Release 只放了源码包
// （没有 APK）是常见情况，所以要能往下退。
//
// # 多包名 = 拒绝（appId 自动派生之后唯一剩下的把关点）
//
// 同一个 Release 里出现两个不同 package，意味着光凭 repo 定位不到"哪一个应用"。
// **不能随便挑一个**：挑错了就是静默地收录了一个申请人没想要的应用，而这个错误唯一的
// 表现是设备上多了一行 —— 没人会去核对。
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
		return c.probeMetas(ctx, repo, rel, picked)
	})
}

// pickIdentityRelease 在候选 Release 里从新到旧找第一个**真能定出身份**的，定出它。
//
// 抽成独立函数（而不是留在 probeIdentity 里）只有一个理由：让"走几层才停"这件事可以在
// 没有真 APK、也没有网络的情况下被测试 —— 而它恰恰是本模块里最容易写错的一段。
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
func (c *Ctx) probeMetas(ctx context.Context, repo string, rel gh.Release, picked []gh.Asset) []*apkmeta.Meta {
	out := make([]*apkmeta.Meta, 0, len(picked))
	for _, a := range picked {
		m, err := c.readAssetMeta(ctx, repo, a)
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
