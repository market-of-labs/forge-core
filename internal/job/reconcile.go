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
// 它把 §4.4 的四步串起来：解析上游 → 镜像缺失版本 → 重建清单与索引 → 回写。
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
// 镜像只动 Release 与 index；清单（apps.json）是**另一个**产物，它由 sources +
// endpoints + index 三者合成。改了任一输入却不重建，清单就与事实漂移了 ——
// 而漂移的清单在设备端表现为"点进去装的是旧版本"，一个没人会想到来查对账的症状。
func Reconcile(ctx context.Context, c *Ctx, opts ReconcileOptions) (*ReconcileResult, error) {
	res := &ReconcileResult{Report: &model.Report{}}

	// **在镜像之前**就把"索引是不是空的"记下来。镜像会往 c.Index 里追加版本，
	// 在这里之后再看就已经不是"加载时的状态"了 —— 而自愈要判断的恰恰是后者。
	//
	// index 空了（被清空、丢失、或第一次跑）就开自愈：versionCode / versionName
	// **只存在于 APK 内部**，Release 的元数据里没有，所以拿回它们的唯一途径就是
	// 下载。代价是流量，收益是"不用人介入就能从一份空索引恢复"。
	//
	// 正常路径下它是关着的，但开着的代价也只是"对确实缺元数据的版本下一份"
	// （BuildIndex 只对 VersionName 为空的版本下载），所以宁可开。
	hadIndex := len(c.Index.Apps) > 0

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

	fetch := !hadIndex
	if fetch {
		c.Log("索引为空 —— 走自愈重建，会按需下载 APK 补齐 versionCode（03 §5.2）")
	}
	if err := RebuildAndCheck(ctx, c, RebuildOptions{FetchMissing: fetch}); err != nil {
		return res, err
	}

	// 4 步：回写。**只有 check-manifest 过了才有这一步**（见 RebuildAndCheck）。
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
		// 没镜像任何东西也提交，是因为"重建清单/索引"本身可能就是改动
		// （比如上游把某个 asset 改名了，index 会跟着变）。CommitBack 在没有
		// 实际改动时会安静地不提交，所以这里无需自己判断。
		return fmt.Sprintf("%s：重建清单与索引", scope)
	}
	return fmt.Sprintf("%s：镜像 %d 个新 asset", scope, res.Uploaded)
}

// RebuildOptions 调 RebuildAndCheck 的选项。
type RebuildOptions struct {
	// FetchMissing 见 BuildIndexOptions.FetchMissing。日常对账关着（镜像时已写好
	// 元数据，零下载）；`rebuild-index` 的自愈路径要打开。
	FetchMissing bool
}

// RebuildAndCheck 依次跑 build-index → build-manifest → check-manifest。
//
// 三步必须按这个顺序，且**必须一起跑**：
//
//	build-index     事实来自 Release，元数据来自 index 自己 → 必须先于清单
//	build-manifest  把 sources + index + endpoints 合成 apps.json
//	check-manifest  **读回磁盘上的** apps.json 跑 02 §2.8，是唯一的阻断点
//
// # 自检不过就不回写
//
// 03 §5.3 说 check-manifest「除硬错误外只告警不阻断」。这里的实现是：
// 告警全部打进日志、**硬错误返回 error**，于是调用方在 CommitBack 之前就停下了。
// 代价是这一轮"什么都没有落地"——包括已经镜像好的 asset（它们在 Release 里，
// 不依赖提交）。这个取舍是对的：把一份违反契约的 apps.json 推上去，会让**所有**
// 客户端立刻拿到坏数据；而不推，最坏情况是"多跑一轮"。
func RebuildAndCheck(ctx context.Context, c *Ctx, opts RebuildOptions) error {
	// 索引：事实来自 Release 现状（D23：index 是派生数据，可以随时从 Release 重建）。
	ix, ixRep, err := BuildIndex(ctx, c, BuildIndexOptions{FetchMissing: opts.FetchMissing})
	if err != nil {
		return err
	}
	// 注意顺序：先把 build-index 的报告打出来，再用新索引替换内存里的那份。
	// BuildIndex 是拿 c.Index 当"旧索引"来继承元数据的，所以替换必须发生在
	// 它返回**之后** —— 反过来会让它继承到自己刚生成的东西。
	c.Reportf(ixRep)
	c.Index = ix
	if err := WriteIndex(c, ix); err != nil {
		return err
	}

	// 清单：只需要 sources + endpoints + index。
	if _, mRep, err := BuildManifest(c); err != nil {
		return err
	} else {
		c.Reportf(mRep)
	}

	// 自检：读回磁盘上的 apps.json。这一步是"产出对不对"的唯一判据。
	_, chkRep, err := CheckManifest(c)
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
// payload 只被当作**信标**：它带来的 event 名、issue 号、release tag、sha 都是
// 标识而非内容，内容一律由 forge 自己用 API 去 store 读（§2.6 的切面）。
// 于是"外部字符串进入执行环境"这条路径在这里依然是不通的。
//
// 四个分支：
//
//	issues            → 处理 issue（§2.5 入口乙）：新增单**当场**收录并只同步它自己
//	release           → 过 §4.6 闸门后搬 _incoming（§3.2）
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

	case "release":
		// §4.6 的闸门（规则 2）先过，再动手。
		if err := CheckIncomingGate(c.Env); err != nil {
			c.Log("%v", err)
			return res, nil
		}
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
		if err := RebuildAndCheck(ctx, c, RebuildOptions{}); err != nil {
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

	case "workflow_dispatch":
		r, err := Reconcile(ctx, c, ReconcileOptions{})
		res.Reconcile = r
		if r != nil {
			res.Commited = r.Commited
		}
		return res, err

	default:
		// 不认识的 event 一律拒绝而不是"当作全量对账"：store 侧将来新增事件类型时，
		// 静默地按全量处理会掩盖"forge 还没支持这个事件"这件事。
		return res, fmt.Errorf("不认识的事件 %q（03 §4.3 只定义了 issues / release / push / workflow_dispatch）",
			c.Env.Event)
	}
}

// affectedAppID 从 `push` 的 sha 反查被改动的来源 id。
//
// 返回 "" 表示"应该按全量处理"：没改任何 sources/ 文件，或者改了**多个**
// （批量改动时逐个收敛反而更慢、更容易撞上并发上传）。
//
// 只看 `sources/{appId}.json` 这一种形状 —— `apps.json` / `store/index.json`
// 是 forge 自己的产物，它们的 push 只会来自 forge 自己的回写，而那已经带着
// [skip-dispatch] 被 store 侧挡掉了（规则 7）。真收到了，说明有人在手改产物，
// 那更该全量重算把它盖回去。
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
			// 提交删掉了这个来源文件。没有上游要解析，全量重算把清单里的条目去掉即可。
			c.Log("来源 %s 已被删除，走全量重算把它从清单里去掉", hit[0])
			return "", nil
		}
		return hit[0], nil
	default:
		c.Log("这次 push 改了 %d 个来源文件，走全量对账", len(hit))
		return "", nil
	}
}
