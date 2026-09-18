package job

import (
	"context"
	"fmt"

	"github.com/market-of-labs/forge-core/internal/model"
)

// ReconcileOptions 调 reconcile 的选项。
type ReconcileOptions struct {
	// OnlyID 非空时只收敛这一个 appId。
	//
	// 今天只有两个来源：`forge reconcile -only`，以及 newsource 收录完一张新增单之后的
	// 那一次。从前它还服务过 §4.3 的 `push` 分支（一次提交改了哪个源就只收敛那一个），
	// 那条路已随分派器一起删掉。
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
// 调用它的只有两处：`forge reconcile` 动词（可带 `-only`），以及 newsource 在收录完一张
// 新增单之后的那一次 —— 那次只带刚收录的那一个 appId，好让新应用在设备端"一分钟内可装"，
// 而不是等到明天。从前 `handle-dispatch` 的三个分支也调它，那个分派器已随 §4.7 第 5 步删掉。
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
