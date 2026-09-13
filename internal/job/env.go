// Package job 是 forge 的编排层：把 03 §5 那几个脚本职责实现成一组动词，
// 每个动词都作用在一个"store 工作副本 + GitHub 客户端"的上下文上。
//
// 与下面几层的关系：
//
//	internal/{naming,apkmeta,model,issue,upstream}   纯逻辑，无 IO
//	internal/{gh,gitx,store}                         各自的 IO 封装
//	internal/job  ← 本包                             把上面两层拼成"一次维护动作"
//	cmd/forge                                        子命令分发
//
// 本包不碰 os.Args、不决定退出码、不打印 ::add-mask:: —— 那些是 cmd 的事。
// 这样每个动词都能在测试里被直接调用。
package job

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// 环境变量的名字。集中在这里，免得散落在各处对不上。
const (
	// EnvToken 是 PAT（03 §6.1：store 与 forge 各存一份，值相同）。
	EnvToken = "STORE_TOKEN"
	// EnvStoreRepo / EnvForgeRepo 是 owner/repo。
	EnvStoreRepo = "STORE_REPO"
	EnvForgeRepo = "FORGE_REPO"
	// EnvStoreDir 指向一个**已经 checkout 好**的 store 工作副本。
	// 为空时本包会自己浅克隆到一个临时目录（用完删掉）。
	EnvStoreDir = "STORE_DIR"
	// EnvAPIBase 覆盖 GitHub API 根地址。Actions 会自带 GITHUB_API_URL，
	// 我们优先用它；这个变量用于 GHES 与测试。
	EnvAPIBase = "FORGE_API_BASE"
	// EnvUploadBase 覆盖上传根地址。Actions 不提供对应的环境变量，
	// 所以默认由 API 地址推导（api.github.com → uploads.github.com）。
	EnvUploadBase = "FORGE_UPLOAD_BASE"

	// 以下是 repository_dispatch 的 client_payload 带过来的事件字段（03 §2.6）。
	//
	// 这里**没有** releaseTag / prerelease：它们只服务过 `release: published`
	// 那条自动发车的路（闸门靠 tag 判断"这次发布是不是 _incoming"）。队列改成
	// 常驻 draft + 显式触发之后，那类事件根本不会打到 forge，两个字段就没有输入了。
	EnvEvent = "EVENT"
	EnvIssue = "ISSUE"
	EnvSHA   = "SHA"
	EnvRef   = "REF"

	// 默认的仓库地址。
	DefaultStoreRepo = "market-of-labs/store"
	DefaultForgeRepo = "market-of-labs/forge"
	// 默认的 API 根地址。
	DefaultAPIBase = "https://api.github.com"
	// 默认的上传根地址。
	DefaultUploadBase = "https://uploads.github.com"
)

// Env 是一次运行的全部外部输入。
//
// 刻意做成一个显式结构体而不是到处 os.Getenv：这样"这次跑用了哪些输入"
// 在日志里可以一次打全，测试里也可以整份替换。
type Env struct {
	Token     string
	StoreRepo string
	ForgeRepo string
	// StoreDir 非空表示用调用方给的工作副本（不克隆、也不在结束时删除）。
	StoreDir string

	APIBase    string
	UploadBase string

	Event string
	Issue int
	SHA   string
	Ref   string

	// InCI 表示跑在 GitHub Actions 里。只影响 ::add-mask:: 要不要发。
	InCI bool
}

// FromEnv 从进程环境读一次运行的全部输入。
func FromEnv() (*Env, error) {
	e := &Env{
		Token:     os.Getenv(EnvToken),
		StoreRepo: defaulted(os.Getenv(EnvStoreRepo), DefaultStoreRepo),
		ForgeRepo: defaulted(os.Getenv(EnvForgeRepo), DefaultForgeRepo),
		StoreDir:  os.Getenv(EnvStoreDir),

		APIBase:    firstNonEmpty(os.Getenv(EnvAPIBase), os.Getenv("GITHUB_API_URL"), DefaultAPIBase),
		UploadBase: os.Getenv(EnvUploadBase),

		Event: strings.TrimSpace(os.Getenv(EnvEvent)),
		SHA:   os.Getenv(EnvSHA),
		Ref:   os.Getenv(EnvRef),
		InCI:  os.Getenv("GITHUB_ACTIONS") == "true",
	}

	if v := strings.TrimSpace(os.Getenv(EnvIssue)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("%s=%q 不是整数：%w", EnvIssue, v, err)
		}
		e.Issue = n
	}

	if e.UploadBase == "" {
		e.UploadBase = deriveUploadBase(e.APIBase)
	}
	if err := e.validate(); err != nil {
		return nil, err
	}
	return e, nil
}

// validate 检查一次运行的输入是否自洽。
//
// 检查得比"能不能跑"更严一点：这条流程有跨仓库写权限，**半配置状态下跑起来**
// 比直接拒绝危险得多 —— 比如没有 token 时会退化成匿名读，于是"什么都没发生"
// 被当成"一切正常"，而实际是一个版本都没镜像。
func (e *Env) validate() error {
	if e.StoreRepo == "" || e.ForgeRepo == "" {
		return fmt.Errorf("%s / %s 不能为空", EnvStoreRepo, EnvForgeRepo)
	}
	if !strings.Contains(e.StoreRepo, "/") || !strings.Contains(e.ForgeRepo, "/") {
		return fmt.Errorf("仓库地址应当是 owner/repo 形式，得到 %q / %q", e.StoreRepo, e.ForgeRepo)
	}
	if e.APIBase == "" {
		return fmt.Errorf("%s 为空", EnvAPIBase)
	}
	return nil
}

// RequireToken 断言有 token。写操作前调用。
func (e *Env) RequireToken(what string) error {
	if e.Token == "" {
		return fmt.Errorf("%s 需要 %s，但它是空的", what, EnvToken)
	}
	return nil
}

// IsOwnCheckout 报告工作副本是不是调用方给的（true）还是我们临时克隆的（false）。
func (e *Env) IsOwnCheckout() bool { return e.StoreDir != "" }

// AddMask 发出 GitHub 的 ::add-mask:: 掩码指令（03 §4.5 规则 9）。
//
// 为什么必须自己做：GitHub 的自动脱敏表里只有 `ghp_/gho_/ghu_/ghs_/ghr_` 前缀，
// **不含 `github_pat_`** —— 而我们用的正是 fine-grained PAT。不发这一行，
// token 会以明文出现在 Actions 日志里，而公有仓库的日志任何登录用户都能读。
//
// 只在 Actions 里发：本地跑时这一行的作用是把 token 打到自己的终端上，
// 那正是我们要避免的。返回值是实际发出的那一行（测试用），没发则为空。
func (e *Env) AddMask() string {
	if !e.InCI || e.Token == "" {
		return ""
	}
	line := "::add-mask::" + e.Token
	fmt.Println(line)
	return line
}

// Sanitized 返回一份可以安全打进日志的输入摘要。
//
// token 只以"有没有 / 多长"的形式出现 —— 长度足够用来判断"是不是把 token 和
// 别的变量搞混了"，又不足以还原它。
func (e *Env) Sanitized() string {
	tok := "未设置"
	if e.Token != "" {
		tok = fmt.Sprintf("已设置(%d 字符)", len(e.Token))
	}
	return fmt.Sprintf(
		"event=%q issue=%d sha=%q store=%s forge=%s api=%s token=%s",
		e.Event, e.Issue, shortSHA(e.SHA),
		e.StoreRepo, e.ForgeRepo, e.APIBase, tok)
}

func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func defaulted(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return strings.TrimSpace(v)
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// deriveUploadBase 从 API 根地址推出上传根地址。
//
// github.com 的两个域名是不对称的（api.github.com ↔ uploads.github.com），
// 而 GHES 上是 api.<host> ↔ <host>。推不出来时回落到 API 地址本身，
// 让请求以"404 + 明确的路径"失败，而不是连到一个不存在的主机。
func deriveUploadBase(apiBase string) string {
	b := strings.TrimSuffix(apiBase, "/")
	if b == DefaultAPIBase {
		return DefaultUploadBase
	}
	if rest, ok := strings.CutPrefix(b, "https://api."); ok {
		return "https://" + rest
	}
	return b
}
