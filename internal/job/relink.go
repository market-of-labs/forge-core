package job

import (
	"path"
	"regexp"
	"strings"
)

// ---- 把 README 里指向仓库内部的相对路径改写成指向源项目的绝对地址 --------------
//
// 为什么必须改：Release 正文是**在 release 页面那个 URL 下**渲染的，而 README 是
// 在仓库根这个 base 下渲染的。上游写 `![](./img/x.png)` 在它自己的仓库页面上完全
// 正确，原样搬进我们的正文就变成 `<release 页面>/img/x.png` —— 一张 404 的图。
//
// 两种目标给两种地址：图片要的是**字节**（raw），链接要的是**GitHub 那个文件页**
// （blob；目录链接也能落在目录树上）。两者都用 `HEAD` —— `gh.Readme` 取的就是默认
// 分支的 README，而 `HEAD` 解析到同一个分支，README 说 `./img/x.png` 时指的就是
// 那个分支里的它。

var (
	// 行内图片 `![alt](url "title")`。放在链接那一条**前面**跑：badge 那种
	// `[![build](badge.svg)](https://ci)` 嵌套形态下，链接那条会把整个
	// `![build](badge.svg` 当成链接文字吃掉，里面的图片就漏掉了。
	mdImage = regexp.MustCompile(`!\[([^\]]*)\]\(([^\s)]+)(\s+"[^"]*")?\)`)
	// 行内链接 `[text](url "title")`。
	mdLink = regexp.MustCompile(`\[([^\]]*)\]\(([^\s)]+)(\s+"[^"]*")?\)`)
	// HTML 属性。实测上游 README 里嵌 `<img>`/`<a>` 比 markdown 形态还多。
	htmlAttr = regexp.MustCompile(`(?i)\b(src|href)\s*=\s*("[^"]*"|'[^']*')`)
	// `scheme:` 前缀 —— 有它就是绝对地址（http: / https: / mailto: / data: …）。
	scheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)
)

// relinkReadme 把 markdown 里指向仓库内文件的相对路径改写成指向 repo（`owner/name`）
// 的绝对地址，其余原样返回。
//
// **幂等**：已经是绝对地址的目标一个都不碰，所以改写过的正文再跑一遍字节不变 ——
// syncReleaseBody 那句"正文相同就不 PATCH"照旧成立，不会每轮都重写一次 Release。
// （反过来说：本次上线后，已经发布过的正文里还是相对路径，下一轮对账会各 PATCH 一次
// 把它们升上来，之后就一直命中"相同"。）
//
// ⚠️ 已知边界，都不处理（代价只是少改几个链接，不会改坏东西）：
//   - 引用式链接 `[x][ref]` + `[ref]: ./a.png` 不认。裸的参考定义分不出是图片还是
//     链接，分不出就只能猜，而猜错的表现是一张图变成一条链接 —— 比不改更难看。
//   - fenced code block 里的 `src=`/`href=` 照改。要跳过得跟踪围栏状态，为一段
//     几乎不存在的输入养一个小状态机不值。ponytail: 真出现了再上围栏跟踪。
//   - 路径不做 percent-encoding：上游已经编过的（`%20`）再编一遍才是 bug。
//   - `<!-- -->` 注释里的链接照改（实测有上游这么干）。渲染器把注释整段丢掉，所以
//     改了看不见；万一哪天上游取消注释，改过的反而正好是对的。不用管它。
func relinkReadme(md, repo string) string {
	if md == "" || repo == "" {
		return md
	}
	md = mdImage.ReplaceAllStringFunc(md, func(m string) string {
		return relinkMD(m, mdImage, repo, "![", true)
	})
	md = mdLink.ReplaceAllStringFunc(md, func(m string) string {
		return relinkMD(m, mdLink, repo, "[", false)
	})
	return htmlAttr.ReplaceAllStringFunc(md, func(m string) string {
		g := htmlAttr.FindStringSubmatch(m)
		quoted := g[2]
		target := quoted[1 : len(quoted)-1]
		if isAbsoluteTarget(target) {
			return m
		}
		// `src=` 是图片/媒体（给字节），`href=` 是链接（给文件页）。
		return g[1] + "=" + quoted[:1] + linkURL(target, repo, strings.EqualFold(g[1], "src")) + quoted[:1]
	})
}

// relinkMD 处理一条 markdown 行内图片或链接。prefix 是 `![` 或 `[`，raw 决定目标类型。
func relinkMD(m string, re *regexp.Regexp, repo, prefix string, raw bool) string {
	g := re.FindStringSubmatch(m)
	if isAbsoluteTarget(g[2]) {
		return m
	}
	return prefix + g[1] + "](" + linkURL(g[2], repo, raw) + g[3] + ")"
}

// linkURL 把一个仓库内相对路径拼成绝对地址。raw 为真走 raw.githubusercontent.com。
func linkURL(target, repo string, raw bool) string {
	// 开头的 `/` 在 README 里指的就是**仓库根**（GitHub 把仓库根 README 渲染在仓库根
	// 这个 base 上），不是 github.com 的域根 —— 所以剥掉它再拼。
	//
	// 用 path 不用 filepath：这是 URL 路径，分隔符永远是 `/`（在 Windows 上跑测试时
	// filepath 会把它们变成 `\`）。path.Join 顺带把 `./` 与 `a/../b` 归掉。
	p := path.Join(".", strings.TrimPrefix(target, "/"))
	if raw {
		return "https://raw.githubusercontent.com/" + repo + "/HEAD/" + p
	}
	return "https://github.com/" + repo + "/blob/HEAD/" + p
}

// isAbsoluteTarget 报告一个链接目标本来就指到别处 —— 是就别碰它。
func isAbsoluteTarget(t string) bool {
	if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "//") || strings.HasPrefix(t, "?") {
		return true
	}
	return scheme.MatchString(t)
}
