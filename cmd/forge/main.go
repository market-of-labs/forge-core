// Command forge 是市场维护侧的单一可执行文件。
//
// 03 §4.1 列的是九个 `scripts/*.sh`，这里把它们实现成**一个二进制 + 十个子命令**。
// 换成 Go 之后"安装依赖"这一步整个消失了（纯 Go 解析 APK，不需要 aapt），
// 所以每个 workflow 的 "Install deps" 步骤也随之删掉 —— 那不是省事，
// 而是少了一个"runner 镜像换了、aapt 装不上了"的故障面。
//
// 本文件只做三件事：读环境 → 分发子命令 → 决定退出码。
// 一切业务逻辑都在 internal/job 里，这样它能在测试里被直接调用，
// 不需要把二进制跑起来、也不需要伪造 os.Args。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/market-of-labs/forge-core/internal/job"
)

// 退出码。2 专门留给"命令本身用错了"（参数缺失、环境不全）——
// workflow 里区分"配置错了"与"跑到一半失败了"很有用：前者要人改配置，
// 后者往往是上游/网络的瞬时问题，重跑一次就好。
const (
	exitOK      = 0
	exitFailed  = 1
	exitUsage   = 2
	exitRefused = 3
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return exitUsage
	}
	verb := args[0]
	switch verb {
	case "help", "-h", "--help":
		usage()
		return exitOK
	case "verbs":
		// 给 workflow 与补全用：一行一个动词，稳定顺序。
		for _, v := range verbNames() {
			fmt.Println(v)
		}
		return exitOK
	}

	env, err := job.FromEnv()
	if err != nil {
		// 环境不全时**什么都不做**。半配置跑起来的危害在于它会用默认值
		// （比如默认仓库名）去操作一个真实的远端 —— 那比直接失败危险得多。
		fmt.Fprintf(os.Stderr, "配置错误：%v\n", err)
		return exitUsage
	}

	log := func(format string, a ...any) { fmt.Printf(format+"\n", a...) }

	// 掩码必须在**任何其它输出之前**发出去。
	//
	// 规则 9：GitHub 自动脱敏的模式表里只有 ghp_/gho_/ghu_/ghs_/ghr_，**不含
	// github_pat_** —— 而我们用的正是 fine-grained PAT，所以这一行不是可选的。
	// 而 forge 是公有仓库，它的 Actions 日志任何登录用户都能读（§6.2 不变量 3）。
	if line := env.AddMask(); line != "" {
		fmt.Println(line)
	}

	log("forge %s | %s", verb, env.Sanitized())

	ctx := context.Background()
	c, err := job.Open(ctx, env, log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "准备运行环境失败：%v\n", err)
		return exitFailed
	}
	defer c.Close()

	code, err := dispatch(ctx, c, verb, args[1:], log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "失败：%v\n", err)
		return code
	}
	return exitOK
}

// dispatch 跑一个动词。返回的 code 在 err 非 nil 时才是退出码。
func dispatch(ctx context.Context, c *job.Ctx, verb string, args []string, log func(string, ...any)) (int, error) {
	switch verb {

	case "handle-dispatch":
		res, err := job.HandleDispatch(ctx, c)
		if res != nil {
			log("事件 %s 处理完毕（committed=%v）", res.Event, res.Commited)
		}
		return exitFailed, err

	case "intake-issue":
		fs := newFlagSet(verb)
		number := fs.Int("issue", c.Env.Issue, "issue 编号（默认取 $ISSUE）")
		if err := fs.Parse(args); err != nil {
			return exitUsage, err
		}
		if *number <= 0 {
			return exitUsage, errors.New("缺少 issue 编号：用 -issue N 或设置 $ISSUE")
		}
		d, r, err := job.IntakeIssue(ctx, c, *number)
		if d != nil {
			log("#%d %s", *number, d.Summary)
		}
		if r != nil {
			log("单项目同步：上传 %d，跳过 %d，失败 %d，committed=%v",
				r.Uploaded, r.Skipped, r.Failed, r.Commited)
		}
		return exitFailed, err

	case "intake-incoming":
		fs := newFlagSet(verb)
		force := fs.Bool("force", false, "跳过 §4.6 闸门（本地调试用；CI 里绝不要加）")
		if err := fs.Parse(args); err != nil {
			return exitUsage, err
		}
		if !*force {
			if err := job.CheckIncomingGate(c.Env); err != nil {
				// 闸门拦住**不是错误**：store 的 forward.yml 转发所有 published
				// 事件，所以"收到一个不该处理的 release"是正常流量，安静退出。
				log("%v", err)
				return exitOK, nil
			}
		} else {
			log("⚠️ -force：跳过了 §4.6 闸门")
		}
		inc, err := job.IntakeIncoming(ctx, c)
		if err != nil {
			return exitFailed, err
		}
		log("搬运 %d 个，保留 %d 个", len(inc.Moved), len(inc.Kept))
		if err := job.RebuildAndCheck(ctx, c, job.RebuildOptions{}); err != nil {
			return exitFailed, err
		}
		_, err = c.CommitBack(ctx, fmt.Sprintf("搬运 _incoming：%d 个 asset", len(inc.Moved)))
		return exitFailed, err

	case "resolve-upstream":
		fs := newFlagSet(verb)
		only := fs.String("only", "", "只处理这一个 appId")
		if err := fs.Parse(args); err != nil {
			return exitUsage, err
		}
		plans, rep, err := job.ResolveUpstream(ctx, c, job.ResolveOptions{OnlyID: *only})
		if rep != nil {
			c.Reportf(rep)
		}
		if err != nil {
			return exitFailed, err
		}
		// 只打印计划，不下载不上传 —— 这是"上线前先看一眼会被命名成什么"的手段。
		fmt.Print(job.DescribePlans(plans))
		return exitFailed, nil

	case "mirror-upstream":
		fs := newFlagSet(verb)
		only := fs.String("only", "", "只处理这一个 appId")
		dry := fs.Bool("dry-run", false, "只解析与命名，不上传、不写 index")
		if err := fs.Parse(args); err != nil {
			return exitUsage, err
		}
		plans, rep, err := job.ResolveUpstream(ctx, c, job.ResolveOptions{OnlyID: *only})
		if rep != nil {
			c.Reportf(rep)
		}
		if err != nil {
			return exitFailed, err
		}
		mr, err := job.MirrorUpstream(ctx, c, plans, job.MirrorOptions{DryRun: *dry})
		if err != nil {
			return exitFailed, err
		}
		if mr.Problems != nil {
			c.Reportf(mr.Problems)
		}
		log("上传 %d 个，跳过 %d 个", mr.Uploaded, mr.Skipped)
		return exitFailed, nil

	case "build-index":
		fs := newFlagSet(verb)
		fetch := fs.Bool("fetch-missing", false, "对缺元数据的版本下载一个分片补齐 versionCode（自愈路径）")
		if err := fs.Parse(args); err != nil {
			return exitUsage, err
		}
		ix, rep, err := job.BuildIndex(ctx, c, job.BuildIndexOptions{FetchMissing: *fetch})
		if rep != nil {
			c.Reportf(rep)
		}
		if err != nil {
			return exitFailed, err
		}
		if err := job.WriteIndex(c, ix); err != nil {
			return exitFailed, err
		}
		return exitFailed, nil

	case "build-manifest":
		_, rep, err := job.BuildManifest(c)
		if rep != nil {
			c.Reportf(rep)
		}
		return exitFailed, err

	case "check-manifest":
		_, rep, err := job.CheckManifest(c)
		if err != nil {
			return exitFailed, err
		}
		// §5.3：除硬错误外只告警不阻断。硬错误 = HasErrors()。
		if c.Reportf(rep) {
			return exitRefused, fmt.Errorf("自检失败：%d 个硬错误", len(rep.Errors()))
		}
		return exitFailed, nil

	case "reconcile":
		fs := newFlagSet(verb)
		only := fs.String("only", "", "只收敛这一个 appId")
		dry := fs.Bool("dry-run", false, "只解析，不下载不上传不回写")
		if err := fs.Parse(args); err != nil {
			return exitUsage, err
		}
		// 报告由 Reconcile 自己在出口打（放这儿只有这一个动词能打，handle-dispatch 打不着）。
		res, err := job.Reconcile(ctx, c, job.ReconcileOptions{OnlyID: *only, DryRun: *dry})
		if err != nil {
			return exitFailed, err
		}
		if res != nil {
			// 失败数一定要报出来：镜像失败是**容忍**的（见 MirrorUpstream），
			// 所以这一轮照样是绿的，这一行是"有没有应用没镜像成"最省事的判据。
			log("对账完成：%d 个待镜像版本，上传 %d，跳过 %d，失败 %d，committed=%v",
				res.Plans, res.Uploaded, res.Skipped, res.Failed, res.Commited)
		}
		return exitFailed, nil

	case "commit-back":
		fs := newFlagSet(verb)
		msg := fs.String("m", "手工回写", "提交信息（会自动追加 [skip-dispatch]）")
		if err := fs.Parse(args); err != nil {
			return exitUsage, err
		}
		committed, err := c.CommitBack(ctx, *msg)
		if err != nil {
			return exitFailed, err
		}
		log("committed=%v", committed)
		return exitFailed, nil

	default:
		usage()
		return exitUsage, fmt.Errorf("不认识的子命令 %q", verb)
	}
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("forge "+name, flag.ContinueOnError)
	// 出错时不要打两遍用法（flag 自己会打一遍）。
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {}
	return fs
}

// verbDoc 是子命令表。用有序切片而不是 map，好让 usage 的输出稳定 ——
// 一份每次顺序都不同的帮助文本没法被 diff、也没法被文档引用。
var verbDoc = []struct{ name, doc string }{
	{"handle-dispatch", "按 client_payload.event 分派（03 §4.3）。CI 的入口"},
	{"intake-issue", "读 issue → 校验 → 落盘 → （新增单）探身份并同步它自己 → 回评关单（§2.5）"},
	{"intake-incoming", "搬 _incoming 的 asset 到正式 Release 并清场（§3.2 / §4.6）"},
	{"resolve-upstream", "只算出该镜像哪些版本，打印计划（不下载不上传）"},
	{"mirror-upstream", "下载 → 按内容判 ABI → 改名 → 幂等上传（§4.4 第 3 步）"},
	{"build-index", "从 Release 现状重建 store/index.json（§5.2）"},
	{"build-manifest", "由 sources + index + endpoints 合成 apps.json（§5.1）"},
	{"check-manifest", "对 apps.json 跑 02 §2.8 自检 + 阈值告警（§5.3）"},
	{"reconcile", "幂等全量对账：解析 → 镜像 → 重建 → 回写（§4.4）"},
	{"commit-back", "把工作副本的改动按固定路径提交并推送（§5.5，自动加 [skip-dispatch]）"},
	{"verbs", "列出全部子命令（供脚本消费）"},
	{"help", "显示这份帮助"},
}

func verbNames() []string {
	out := make([]string, 0, len(verbDoc))
	for _, v := range verbDoc {
		out = append(out, v.name)
	}
	sort.Strings(out)
	return out
}

func usage() {
	w := os.Stderr
	fmt.Fprintf(w, "forge —— 私密市场维护侧的执行库\n\n")
	fmt.Fprintf(w, "用法：forge <子命令> [选项]\n\n子命令：\n")
	for _, v := range verbDoc {
		fmt.Fprintf(w, "  %-17s %s\n", v.name, v.doc)
	}
	fmt.Fprintf(w, "\n环境变量：\n")
	fmt.Fprintf(w, "  STORE_TOKEN         fine-grained PAT（03 §6.1）。只读动词可省略\n")
	fmt.Fprintf(w, "  STORE_REPO          store 仓库 owner/name（默认 %s）\n", job.DefaultStoreRepo)
	fmt.Fprintf(w, "  FORGE_REPO          forge 仓库 owner/name（默认 %s）\n", job.DefaultForgeRepo)
	fmt.Fprintf(w, "  STORE_DIR           已 checkout 的 store 工作副本；为空则自己浅克隆到临时目录\n")
	fmt.Fprintf(w, "  FORGE_API_BASE      API 根地址（默认 %s；Actions 里自动取 $GITHUB_API_URL）\n", job.DefaultAPIBase)
	fmt.Fprintf(w, "  EVENT / ISSUE / RELEASE_TAG / PRERELEASE / SHA / REF\n")
	fmt.Fprintf(w, "                      repository_dispatch 带过来的事件字段（03 §2.6）\n")
	fmt.Fprintf(w, "\n退出码：0 成功 · 1 执行失败 · 2 用法或配置错误 · 3 自检发现硬错误\n")
}
