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

	// EnvAPKCacheDir 指向一个**跨轮持久**的 APK 目录，为空表示没有 cache。
	//
	// 它是"索引里保留老版本"这条需求的**唯一实现手段**：CI 里每轮 store 都是全新浅克隆
	// （见 Ctx.Open），所以一个不落在克隆之外的目录活不过一轮。工作流那边由一个
	// `actions/cache` 步骤维护它（用的是滚动 key，**不能** key 在 sources 的哈希上 ——
	// 那样每次改来源 cache 就整个失效，老版本一起丢）。
	//
	// 语义只有一条：**里面躺着以前下过的 APK 文件，文件名就是 Release asset 名**。
	// 没有清单、没有元数据 —— 那正是 `sources/*.json` 的 `versions` 账本存在的理由
	// （见 model.Version），cache 自己回答不了"我有哪些版本"。
	EnvAPKCacheDir = "APK_CACHE_DIR"

	// 签名密钥三件套（03 §5.1）。keystore 的字节走 base64（它能装进一个 secret），
	// 两个口令分开存：JKS 允许 storepass 与 keypass 不同，虽然我们生成时用的是一个值。
	//
	// ⚠️ 它们**不是**"跑起来才需要的输入"，而是**每次 build-repo 都必须有**的：
	// `fdroid update` 没有密钥就签不出 `entry.jar`，而 `entry.jar` 缺失不是"少一个文件"，
	// 是**所有客户端拒绝这个源**。所以缺密钥时要在读配置那一层就拒绝（见 Keystore）。
	EnvKeystoreB64  = "REPO_KEYSTORE_B64"
	EnvKeystorePass = "REPO_KEYSTORE_PASS"
	EnvKeyPass      = "REPO_KEY_PASS"

	// 以下是 repository_dispatch 的 client_payload 带过来的事件字段（03 §2.6）。
	//
	// 这里**没有** releaseTag / prerelease：它们只服务过 `release: published`
	// 那条自动发车的路（闸门靠 tag 判断"这次发布是不是 _incoming"）。队列改成
	// 常驻 draft + 显式触发之后，那类事件根本不会打到 forge，两个字段就没有输入了。
	//
	// `EVENT` 是同理的第三样（2026-09-18 删，03 §4.7 第 5 步）：它只服务过按这个
	// 字符串分派的 `handle-dispatch`。事件类型现在由 `repository_dispatch` 的
	// `types:` 在**编译期**表达，执行体这边不再有"事件名"这个输入 —— 随之消失的
	// 还有"store 打过来一个 forge 不认识的事件"这条路径（从前靠 `default:` 硬错报警，
	// 现在它根本到不了 workflow 里）。
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

	Issue int
	SHA   string
	Ref   string

	// APKCacheDir 见 EnvAPKCacheDir。为空 = 这一轮**没有** cache：
	// 于是本轮只有最新版本会进索引，老版本一个都不会有。
	APKCacheDir string

	// 签名密钥（见上面那三个环境变量）。它们是**秘密**，与 Token 同级。
	KeystoreB64  string
	KeystorePass string
	KeyPass      string

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

		SHA:  os.Getenv(EnvSHA),
		Ref:  os.Getenv(EnvRef),
		InCI: os.Getenv("GITHUB_ACTIONS") == "true",

		APKCacheDir: strings.TrimSpace(os.Getenv(EnvAPKCacheDir)),

		// 口令**不 TrimSpace**：JKS 的口令是任意字节串，首尾空格是合法且常见的一部分，
		// 而"顺手 trim 一下"会把一个正确的口令变成错的 —— 且症状是"keystore was tampered
		// with, or password was incorrect"，跟口令错一模一样，根本指不回这里。
		// base64 那一段可以 trim（它由 `base64 -w0` 产出，换行只会来自粘贴）。
		KeystoreB64:  strings.TrimSpace(os.Getenv(EnvKeystoreB64)),
		KeystorePass: os.Getenv(EnvKeystorePass),
		KeyPass:      os.Getenv(EnvKeyPass),
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

// RequireKeystore 断言签名密钥齐全。`build-repo` 之前调用。
//
// 失败信息里点名**它存哪**（仓库级 secret），因为这条配置错误的唯一修法是去仓库设置里加一个，
// 而报错发生在容器深处、离那个设置页很远。
func (e *Env) RequireKeystore() error {
	if e.KeystoreB64 == "" {
		return fmt.Errorf("%s 为空 —— build-repo 靠它签 `entry.jar`，而 entry.jar 缺失或验签失败"+
			"不是「少一个文件」，是**所有客户端拒绝这个源**（02 §2.2）。它是 `base64 -w0 <keystore>` "+
			"的结果，存在 %s 的仓库级 secret 里（03 §5.1）", EnvKeystoreB64, e.ForgeRepo)
	}
	if e.KeystorePass == "" || e.KeyPass == "" {
		return fmt.Errorf("%s / %s 为空 —— 它们分别是 keystore 与私钥的口令。"+
			"JKS 允许两者不同（我们生成时用的是同一个值），所以两个都要设",
			EnvKeystorePass, EnvKeyPass)
	}
	return nil
}

// AddMask 发出 GitHub 的 ::add-mask:: 掩码指令（03 §4.5 规则 9）。
//
// 为什么必须自己做：GitHub 的自动脱敏表里只有 `ghp_/gho_/ghu_/ghs_/ghr_` 前缀，
// **不含 `github_pat_`** —— 而我们用的正是 fine-grained PAT。不发这一行，
// token 会以明文出现在 Actions 日志里，而公有仓库的日志任何登录用户都能读。
//
// 掩的是**全部秘密**，不是只有 token：签名密钥的两个口令与 keystore 的 base64 与 PAT 同级。
// keystore 字节 + 口令 = 能签出任何东西，而这把 key **一旦发布不可轮换**（D61）——
// 泄了就是所有客户端删源重加。所以这里的规则是"凡是秘密一律掩"，而不是逐个判断谁需要。
//
// 只在 Actions 里发：本地跑时这一行的作用是把秘密打到自己的终端上，那正是我们要避免的。
// 返回值是实际发出的那些行（测试用），没发则为 nil。
func (e *Env) AddMask() []string {
	if !e.InCI {
		return nil
	}
	var lines []string
	for _, secret := range []string{e.Token, e.KeystoreB64, e.KeystorePass, e.KeyPass} {
		if secret == "" {
			continue
		}
		line := "::add-mask::" + secret
		fmt.Println(line)
		lines = append(lines, line)
	}
	return lines
}

// Sanitized 返回一份可以安全打进日志的输入摘要。
//
// 秘密只以"有没有 / 多长"的形式出现 —— 长度足够用来判断"是不是把 token 和
// 别的变量搞混了"，又不足以还原它。
//
// `cache=` 要照实打出来（它是路径不是秘密）：**"这一轮有没有 cache"直接决定索引里
// 会不会有老版本**，而它出问题时的症状是"某个应用的旧版本在某天之后就不见了"——
// 一个没人会想到去看环境变量的症状。
func (e *Env) Sanitized() string {
	secret := func(name, v string) string {
		if v == "" {
			return name + "=未设置"
		}
		return fmt.Sprintf("%s=已设置(%d 字符)", name, len(v))
	}
	cache := e.APKCacheDir
	if cache == "" {
		cache = "无（老版本不会进索引）"
	}
	return fmt.Sprintf(
		"issue=%d sha=%q store=%s forge=%s api=%s %s %s %s cache=%s",
		e.Issue, shortSHA(e.SHA),
		e.StoreRepo, e.ForgeRepo, e.APIBase,
		secret("token", e.Token), secret("keystore", e.KeystoreB64),
		secret("pass", e.KeystorePass), cache)
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
