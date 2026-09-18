package job

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/market-of-labs/forge-core/internal/model"
	"github.com/market-of-labs/forge-core/internal/store"
)

// ReconcileOptions 调 reconcile 的选项。
type ReconcileOptions struct {
	// OnlyID 非空时只收敛这一个 appId（03 §4.3 的 push 分支）。
	OnlyID string
	// DryRun 只解析、不下载不推送不写盘。
	DryRun bool
}

// ReconcileResult 是一次对账的结果摘要。
type ReconcileResult struct {
	Plans    int
	Uploaded int
	Skipped  int
	// Failed 是"想镜像但出错了"的应用个数。它存在的唯一理由是：**容忍之后这一轮是绿的**
	// （见 MirrorUpstream），摘要行就成了"这一轮是不是全都镜像成了"唯一的一眼可见处。
	Failed   int
	Commited bool
	Report   *model.Report
}

// Reconcile 是 03 §4.4 的幂等全量对账，也是**唯一的"日常收敛"入口**。
//
// 它把 §4.4 的四步串起来：解析上游 → 镜像缺失版本 → 重建索引（并重跑一次仓库生成）→ 回写。
// `handle-dispatch` 的 push / workflow_dispatch 分支最终都调它，区别只在 OnlyID；
// `issues` 分支在收录完一张新增单之后也调它一次 —— 那次只带刚收录的那一个 appId，
// 好让新应用在设备端"一分钟内可装"，而不是等到明天。
//
// # 它只做这一件事
//
// 它**不扫 issue、不认领待办**。以前有过这样的第 0 步（扫「待收录 / 待补充」标签的单），
// 已经删掉了：新增单现在在它自己的 issue 事件里一路跑完（见 newsource.go），而**落盘之后**
// 的重试本来就由下面的幂等性兜着，跟队列无关。于是每日 cron 的全部语义就是标题那句话 ——
// 扫 `sources/` 里需要自动更新的源。
//
// # 幂等 = 漏跑自愈
//
// 它**不依赖"上一次跑到哪"**，每次都是"拉上游现状 → 与 Release 现状比对 → 补齐"。
// 所以某天 runner 挂了、cron 被跳过、dispatch 丢了，第二天自然补齐，
// 不需要任何补偿逻辑、不需要记账文件、也不需要知道上次是什么时候跑的。
//
// # 为什么"重建"这一步不能省
//
// 镜像只动 Release 与账本，而**对外那一份产物是另一回事** —— 它由 sources（账本）+
// endpoints 渲染出来，再由 fdroidserver 扫目录扫成索引。改了任一输入却不重建，
// 产物就与事实漂移了；而漂移的产物在设备端表现为"点进去装的是旧版本"或者
// "某个应用干脆没出现"，两个都没人会想到来查对账的症状。
//
// 反过来也有一层：这一轮的 APK 是**从 Release 取**的（见 downloadNewest），
// 所以"镜像"与"生成仓库"是两个独立的消费者，谁都不能替谁把对方的活干了。
func Reconcile(ctx context.Context, c *Ctx, opts ReconcileOptions) (*ReconcileResult, error) {
	res := &ReconcileResult{Report: &model.Report{}}

	// 报告在**每一个出口**都打出来，包括中途失败的那些。
	//
	// 为什么放在这儿而不是让调用方打：res.Report 是这一轮唯一的诊断载体，而五个调用点里
	// 原先只有独立的 `reconcile` 动词会打它 —— 偏偏 CI 入口 `handle-dispatch` 走的不是那个。
	// 于是镜像里记录的硬 ERROR（"上传 asset …：HTTP 400 Bad Content-Length"）整整一路
	// 不留痕迹：workflow 是绿的，日志里只看到下游那句"apps 为空"，像另一件事。
	// 失败被容忍（D44）本来就该靠报告被人看见，报告再没人打就等于没有。
	defer func() { c.Reportf(res.Report) }()

	// 1–3 步：解析 + 镜像。
	plans, rep, err := ResolveUpstream(ctx, c, ResolveOptions{OnlyID: opts.OnlyID})
	if err != nil {
		return res, err
	}
	res.Report.Addf("", rep)
	res.Plans = len(plans)

	if len(plans) == 0 {
		c.Log("上游没有待镜像的版本")
	} else {
		c.Log("待镜像 %d 个版本：\n%s", len(plans), DescribePlans(plans))
		if !opts.DryRun {
			mr, err := MirrorUpstream(ctx, c, plans, MirrorOptions{})
			if err != nil {
				return res, err
			}
			res.Report.Addf("", mr.Problems)
			res.Uploaded, res.Skipped, res.Failed = mr.Uploaded, mr.Skipped, mr.Failed
		}
	}

	if opts.DryRun {
		c.Log("[dry-run] 到此为止：不重建索引、不重建清单、不回写")
		return res, nil
	}

	if err := RebuildAndCheck(ctx, c); err != nil {
		return res, err
	}

	// 4 步：回写。**只有 check-repo 过了才有这一步**（见 RebuildAndCheck）。
	committed, err := c.CommitBack(ctx, commitMessage(opts, res))
	if err != nil {
		return res, err
	}
	res.Commited = committed
	return res, nil
}

func commitMessage(opts ReconcileOptions, res *ReconcileResult) string {
	scope := "对账"
	if opts.OnlyID != "" {
		scope = "对账 " + opts.OnlyID
	}
	if res.Uploaded == 0 {
		// 没镜像任何东西也提交，是因为"重建产物"本身可能就是改动（比如某个来源
		// 被 paused 了、或者上一轮缺的分片这一轮从 cache 里补上了，索引都会跟着变）。
		// CommitBack 在没有实际改动时会安静地不提交，所以这里无需自己判断。
		return fmt.Sprintf("%s：重建索引", scope)
	}
	return fmt.Sprintf("%s：镜像 %d 个新 asset", scope, res.Uploaded)
}

// RebuildAndCheck 依次跑 build-index → build-repo → check-repo。
//
// 三步必须按这个顺序，且**必须一起跑**：
//
//	build-index  事实来自 Release、元数据来自旧账本 → 必须先于后面两步：
//	             repo 该摆哪些文件，完全由这一步重建出来的账本决定
//	build-repo   把 sources（自带账本）+ endpoints 渲染成 fdroid 的输入，
//	             跑一次 `fdroid update`，再把产物搬进根 `repo/`
//	check-repo   **读回磁盘上的**产物跑 02 §2.8，是唯一的阻断点
//
// ⚠️ 与它的前身（build-manifest + check-manifest）有一处结构性差别：那两步之间
// 只传一份 JSON，自检读的就是刚写下的那份字节。这里中间多了**一个外部工具**，
// 索引是 `fdroid update` 扫目录扫出来的 —— 所以自检读的是"别人写的东西"，
// 而它可能因为无数种与 Go 代码无关的原因不合规格。这正是自检在这一条路上
// 比在清单那条路上更重要的原因。
//
// # 自检不过就不回写
//
// 03 §5.3 说 check-repo「除硬错误外只告警不阻断」。这里的实现是：
// 告警全部打进日志、**硬错误返回 error**，于是调用方在 CommitBack 之前就停下了。
// 代价是这一轮"什么都没有落地"——包括已经镜像好的 asset（它们在 Release 里，
// 不依赖提交）。这个取舍是对的：把一份客户端会拒绝的索引推上去，会让**所有**
// 客户端立刻同步失败；而不推，最坏情况是"多跑一轮"。
func RebuildAndCheck(ctx context.Context, c *Ctx) error {
	// 账本：事实来自 Release 现状（D23：账本是派生数据，可以随时从 Release 重建）。
	// 它**就地改写 c.Sources 并落盘**，所以下一步 build-repo 拿到的已经是新账本 ——
	// 这里不需要"把新值替换回内存"这一步，那正是合并（D48）消掉的东西。
	ixRep, err := BuildIndex(ctx, c)
	if err != nil {
		return err
	}
	c.Reportf(ixRep)

	// 产物：渲染 metadata + 凑齐 APK + `fdroid update` + 拷进根 `repo/`。
	rRep, err := BuildRepo(ctx, c)
	if err != nil {
		return err
	}
	c.Reportf(rRep)

	// 自检：读回磁盘上的产物。这一步是"产出对不对"的唯一判据。
	chkRep, err := CheckRepo(ctx, c)
	if err != nil {
		return err
	}
	if c.Reportf(chkRep) {
		return fmt.Errorf("自检发现硬错误，已放弃本次回写（03 §5.3）：%w", chkRep.Err())
	}
	return nil
}

// ---- handle-dispatch：事件分派（03 §4.3） -----------------------------------

// DispatchResult 是一次事件处理的摘要。
type DispatchResult struct {
	Event     string
	IssueNo   int
	Intake    *IntakeDecision
	Incoming  *IncomingResult
	Reconcile *ReconcileResult
	Commited  bool
}

// HandleDispatch 按 `client_payload.event` 分派（03 §4.3）。
//
// payload 只被当作**信标**：它带来的 event 名、issue 号、sha 都是标识而非内容，
// 内容一律由 forge 自己用 API 去 store 读（§2.6 的切面）。于是"外部字符串进入
// 执行环境"这条路径在这里依然是不通的。
//
// 四个分支：
//
//	issues            → 处理 issue（§2.5 入口乙）：新增单**当场**收录并只同步它自己
//	intake-incoming   → 搬 _incoming（§3.2）：人点手动按钮，或上传 CI 发一个信标
//	push              → 只对该 appId 收敛（§4.3）
//	workflow_dispatch → 全量对账（同 §4.4）
func HandleDispatch(ctx context.Context, c *Ctx) (*DispatchResult, error) {
	res := &DispatchResult{Event: c.Env.Event}
	switch c.Env.Event {

	case "issues":
		res.IssueNo = c.Env.Issue
		if res.IssueNo <= 0 {
			// payload 里的 issue 号缺失。这不是攻击，是 store 侧转发写错了；
			// 猜一个号比报错危险得多（可能去改一张无关的单）。
			return res, fmt.Errorf("event=issues 但 client_payload.issue 是 %d，无法定位 issue", c.Env.Issue)
		}
		// 整条链（新增单含落盘与单项目同步）在 IntakeIssue 里一次跑完，
		// 提交与否也随之确定：变更单看 CommitBack 的返回，新增单看落盘与后续同步的或。
		d, r, err := IntakeIssue(ctx, c, res.IssueNo)
		res.Intake, res.Reconcile = d, r
		if d != nil {
			res.Commited = d.Committed
		}
		return res, err

	case "intake-incoming":
		// §3.2 的搬运。**没有闸门要过**：这条路只有两种来源 —— 人在 Actions 页点了
		// 手动按钮（store 的 forward-to-forge 或 forge 的 on-dispatch，都要仓库写
		// 权限），或上传 CI 发的信标。两者都明确指名了"搬 _incoming"，不像从前那个
		// `release: published` 事件，什么 Release 发布都会打进来、必须自己筛
		// （旧闸门就是为了筛它）。
		inc, err := IntakeIncoming(ctx, c)
		res.Incoming = inc
		if err != nil {
			return res, err
		}
		// 搬完必须重建清单与索引，否则新版本在 Release 里但清单看不见（§3.2 的流程）。
		// 什么都不用重建时（空队列）就跳过，免得制造一个无意义的提交。
		if inc == nil || (len(inc.Moved) == 0 && len(inc.Kept) == 0) {
			return res, nil
		}
		if err := RebuildAndCheck(ctx, c); err != nil {
			return res, err
		}
		msg := fmt.Sprintf("搬运 _incoming：%d 个 asset", len(inc.Moved))
		if len(inc.Kept) > 0 {
			msg += fmt.Sprintf("（%d 个未安置，保留待人工）", len(inc.Kept))
		}
		committed, err := c.CommitBack(ctx, msg)
		res.Commited = committed
		return res, err

	case "push":
		// 只收敛被改动的那一个来源。改动文件列表不在 payload 里，只能按 sha 反查。
		only, err := affectedAppID(ctx, c)
		if err != nil {
			// 反查失败**退化到全量**而不是报错：全量对账是幂等的，多跑一遍的代价
			// 只是多打几个上游的 API；而"这次什么都不做"会让一次配置改动
			// 一直不生效，直到下一天的 cron —— 那是个难查得多的症状。
			c.Log("无法从提交反查改动的来源（%v），退化为全量对账", err)
			only = ""
		}
		if only == "" {
			c.Log("这次 push 没有改动任何单个来源文件，做全量对账")
		}
		r, err := Reconcile(ctx, c, ReconcileOptions{OnlyID: only})
		res.Reconcile = r
		if r != nil {
			res.Commited = r.Commited
		}
		return res, err

	case "workflow_dispatch", "reconcile":
		// 手动按钮的另一个选项（verb=reconcile）。和 workflow_dispatch 同义：
		// 两者的意思都是"人主动要一次全量对账"，没必要分成两条路。
		r, err := Reconcile(ctx, c, ReconcileOptions{})
		res.Reconcile = r
		if r != nil {
			res.Commited = r.Commited
		}
		return res, err

	default:
		// 不认识的 event 一律拒绝而不是"当作全量对账"：store 侧将来新增事件类型时，
		// 静默地按全量处理会掩盖"forge 还没支持这个事件"这件事。
		return res, fmt.Errorf("不认识的事件 %q（03 §4.3 只定义了 issues / intake-incoming / push / workflow_dispatch / reconcile）",
			c.Env.Event)
	}
}

// affectedAppID 从 `push` 的 sha 反查被改动的来源 id。
//
// 返回 "" 表示"应该按全量处理"：没改任何 sources/ 文件，或者改了**多个**
// （批量改动时逐个收敛反而更慢、更容易撞上并发上传）。
//
// 只看 `sources/{appId}.json` 这一种形状 —— 根 `repo/` 与 `store/fdroid/metadata/`
// 都是 forge 自己的产物，它们的 push 只会来自 forge 自己的回写，而那已经带着
// [skip-dispatch] 被 store 侧挡掉了（规则 7）。真收到了，说明有人在手改产物，
// 那更该全量重算把它盖回去。
//
// sources/ 里的文件现在**一半是输入、一半是产物**（`versions` 账本是 forge 写的，
// D48），所以"改了 sources"既可能是人改了元数据、也可能是 forge 回写了账本 ——
// 两者都该收敛到这**一个** app，所以这里不需要区分。
func affectedAppID(ctx context.Context, c *Ctx) (string, error) {
	if c.Env.SHA == "" {
		return "", fmt.Errorf("payload 里没有 sha")
	}
	files, err := c.GH.CommitFiles(ctx, c.Env.StoreRepo, c.Env.SHA)
	if err != nil {
		return "", err
	}

	var hit []string
	for _, f := range files {
		// 正斜杠是 API 返回的固定形状（即使在 Windows 上跑）。
		if filepath.Dir(filepath.ToSlash(f)) != store.SourcesDirName {
			continue
		}
		base := filepath.Base(filepath.ToSlash(f))
		if !strings.HasSuffix(base, ".json") {
			continue
		}
		hit = append(hit, strings.TrimSuffix(base, ".json"))
	}
	switch len(hit) {
	case 0:
		return "", nil
	case 1:
		if c.Source(hit[0]) == nil {
			// 提交删掉了这个来源文件。没有上游要解析，全量重算把它的 metadata 清掉、
			// 索引里那个包自然就没了（见 BuildRepo 的 metadata 清理）。
			c.Log("来源 %s 已被删除，走全量重算把它从清单里去掉", hit[0])
			return "", nil
		}
		return hit[0], nil
	default:
		c.Log("这次 push 改了 %d 个来源文件，走全量对账", len(hit))
		return "", nil
	}
}
