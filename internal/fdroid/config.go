package fdroid

import (
	"fmt"
	"unicode/utf8"
)

// 仓库身份。**这三项不是秘密** —— 证书本身是公开的（每个客户端添加源时都会把指纹
// 摆在用户面前让人确认，02 §2.7），所以它们写成常量、不进 secret。
// 真正进 secret 的只有 keystore 的字节与两个口令。
//
// ⚠️ `KeyAlias` 必须与**生成那把 keystore 时用的 alias** 一致，否则
// `fdroid update` 会以"找不到条目"失败。它是一个跨仓库的硬耦合：
// 改这里等于要求重新生成 keystore，而 keystore **一旦发布就不可轮换**（D61）。
const (
	KeyAlias = "market-of-labs"
	// KeyDName 只在 fdroidserver **新建** key 时用得上；我们的 key 已经存在，
	// 它不会走到那条路。仍然保持与生成时一致，免得某天它真的去建了第三把钥匙。
	KeyDName = "CN=market-of-labs, OU=fdroid repo, O=market-of-labs, L=NA, ST=NA, C=CN"
	// RepoName 会进 `index-v2.json` 的 `repo.name`，是客户端"添加源"界面上显示的名字。
	RepoName = "market-of-labs"
	// RepoDescription 同上，进 `repo.description`。
	RepoDescription = "market-of-labs 内部应用市场"
)

// RenderConfig 渲染 fdroidserver 的 `config.yml`。
//
// 它**不写文件、也不 chmod** —— 落盘是调用方的事。但调用方有一条不可省的责任：
// **必须 `chmod 0600`**。fdroidserver 会检查 config.yml 的权限并打一句
// `unsafe permissions` 的警告（实测于 spike ①），而更重要的是这个文件里
// **明文躺着签名私钥的口令**，见下。
//
// ⚠️ 本函数收到的口令是**明文**，所以它返回的字节流是**敏感数据**：
//
//   - 不要回显（CI 里 `cat config.yml` 等于把口令打进公开日志）
//   - 不要进 artifact
//   - 落盘位置必须在**仓库之外**（`keystorePath` 同理）—— 这样私钥与口令在结构上
//     就不可能被 `git add` 到，而不是靠 `TrackedPaths()` 记得把它们排除掉
//
// `keystorePath` 应当是**绝对路径**：fdroidserver 是从 `cwd = <store>/store/fdroid/`
// 出发解析这个路径的，相对路径会随着 `fdroid update` 的工作目录而漂。
//
// 全部值都走 `doc.Str`（一律双引号）。口令尤其不能裸写：一个全是数字的口令
// （`123456`，或者说 32 位十六进制口令里那个万一）在 YAML 里会被读成**整数**，
// 于是 fdroidserver 拿一个 int 去开 keystore，报出来的错跟"口令不对"毫无关系。
func RenderConfig(repoURL, keystorePath, storePass, keyPass string) ([]byte, error) {
	for _, f := range []struct{ name, val string }{
		{"repoUrl", repoURL},
		{"keystorePath", keystorePath},
		{"storePass", storePass},
		{"keyPass", keyPass},
	} {
		if f.val == "" {
			return nil, fmt.Errorf("fdroid: config.yml 的 %s 为空", f.name)
		}
		if !utf8.ValidString(f.val) {
			return nil, fmt.Errorf("fdroid: config.yml 的 %s 不是合法 UTF-8", f.name)
		}
	}

	var d doc
	d.Str("repo_url", repoURL)
	d.Str("repo_name", RepoName)
	d.Str("repo_description", RepoDescription)
	d.Str("keystore", keystorePath)
	d.Str("repo_keyalias", KeyAlias)
	d.Str("keystorepass", storePass)
	d.Str("keypass", keyPass)
	d.Str("keydname", KeyDName)

	return d.Bytes(), nil
}
