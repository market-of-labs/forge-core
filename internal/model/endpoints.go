package model

import (
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"

	"github.com/market-of-labs/forge-core/internal/naming"
)

// 模板里允许出现的占位符（03 §2.3）。**这是全部** —— 未知占位符一律报错，
// 不静默留空，因为静默留空会让一个拼错的模板产出"看起来对"的地址。
//
// 曾经这里还有第四个 `fileName`，服务于 `assetUrlTemplate`：那时地址由 forge 拼好、
// 逐条写进 Obtainium 的清单。F-Droid 那条路把它整条去掉了（D58/D62）——
// 地址不再由我们产出，而是**客户端拿 `repo.address + "/" + 文件名` 自拼**，
// 于是"模板"这个概念在地址这一侧彻底没有了。剩下的两个模板都只描述**名字**。
const (
	VarAppID   = "appId"
	VarVersion = "version"
	VarABI     = "abi"
)

// Tag 按 tagTemplate 渲染 Release 的 tag（03 §3.1）。
func (e Endpoints) Tag(appID string) (string, error) {
	return render(e.TagTemplate, map[string]string{VarAppID: appID})
}

// AssetName 按 assetNameTemplate 渲染规范文件名（02 §2.4）。
func (e Endpoints) AssetName(appID, version, abi string) (string, error) {
	return render(e.AssetNameTemplate, map[string]string{
		VarAppID:   appID,
		VarVersion: version,
		VarABI:     abi,
	})
}

// render 做占位符替换。
//
// 刻意**不支持转义**（没有 `{{`）：模板是四个固定变量拼地址，不是通用模板引擎。
// 多一套转义规则就多一个"看起来像转义其实不是"的坑，而这里的收益是零。
func render(tmpl string, vars map[string]string) (string, error) {
	var b strings.Builder
	b.Grow(len(tmpl) + 32)

	for i := 0; i < len(tmpl); {
		if tmpl[i] != '{' {
			b.WriteByte(tmpl[i])
			i++
			continue
		}
		j := strings.IndexByte(tmpl[i:], '}')
		if j < 0 {
			return "", fmt.Errorf("模板 %q 里有未闭合的 '{'", tmpl)
		}
		key := tmpl[i+1 : i+j]
		v, ok := vars[key]
		if !ok {
			return "", fmt.Errorf("模板 %q 里的占位符 {%s} 不是已知变量（已知：%s）",
				tmpl, key, strings.Join(knownVars(), ", "))
		}
		// 空值一律报错：一个空 appId 渲染出的地址会指向仓库根而不是某个 Release，
		// 这种"能构造出来但必然 404"的地址不该被静默写进清单。
		if v == "" {
			return "", fmt.Errorf("模板 %q 的变量 {%s} 是空值", tmpl, key)
		}
		b.WriteString(v)
		i += j + 1
	}
	return b.String(), nil
}

func knownVars() []string {
	v := []string{VarAppID, VarVersion, VarABI}
	sort.Strings(v)
	return v
}

// Validate 检查这份地址配置是否自洽：①② 看两个名字模板，③ 看成品地址。
//
// **①② 不检查模板"写了什么"，而是检查模板"渲染出什么"** —— 用两个不同的探针值渲染，
// 断言结果与 naming 的实现逐字相等。这是本文件最重要的一条：
//
//	解析一侧（naming.Split）**无法**由模板驱动 —— 给定一个文件名，你得先知道形状才能切分。
//	所以形状的事实源只能是 naming，模板只是"对形状的声明"。
//
// 于是两者一旦分叉就是**静默故障**（索引建不出来 / 客户端折叠不中，两边都不报错，
// 02 §2.4 明说解析失败是静默降级），必须在读配置时就把它变成一条响亮的启动失败。
//
// 用两个探针而不是一个：单个探针挡不住"模板忽略了变量、直接返回常量"这种写法。
func (e Endpoints) Validate() error {
	if strings.TrimSpace(e.TagTemplate) == "" {
		return fmt.Errorf("endpoints.tagTemplate 为空")
	}
	if strings.TrimSpace(e.AssetNameTemplate) == "" {
		return fmt.Errorf("endpoints.assetNameTemplate 为空")
	}
	if strings.TrimSpace(e.RepoURL) == "" {
		return fmt.Errorf("endpoints.repoUrl 为空")
	}

	// ① tag 必须就是 appId 本身（03 §3.1：tag 不含版本段、无斜杠）。
	for _, probe := range []string{"com.example.probe", "dev.imranr.obtainium"} {
		got, err := e.Tag(probe)
		if err != nil {
			return fmt.Errorf("endpoints.tagTemplate 渲染失败：%w", err)
		}
		if got != probe {
			return fmt.Errorf("endpoints.tagTemplate 渲染 %q 得到 %q，但 tag 必须恒等于 appId（03 §3.1）",
				probe, got)
		}
	}

	// ② assetNameTemplate 必须与 naming 的实现一致。
	nameProbes := []struct{ id, version, abi string }{
		{"com.example.probe", "1.2.3", "universal"},
		{"dev.imranr.obtainium", "1.6.15", "arm64-v8a"},
	}
	for _, p := range nameProbes {
		want := naming.AssetName(p.id, p.version, p.abi)
		got, err := e.AssetName(p.id, p.version, p.abi)
		if err != nil {
			return fmt.Errorf("endpoints.assetNameTemplate 渲染失败：%w", err)
		}
		if got != want {
			return fmt.Errorf("endpoints.assetNameTemplate 渲染出 %q，但命名契约要求 %q。"+
				"文件名形状是硬依赖（02 §2.4），改模板等于改契约 —— 要改就得同时改 naming 与客户端折叠逻辑",
				got, want)
		}
	}

	// ③ repoUrl 必须是 https、有 host、且**不带尾斜杠**。
	//
	// 它与上面两条不同：那两条校验的是"模板渲染出来的形状"，这一条校验的是一个**成品地址**。
	// 而它值得校验的理由，是它同时是**客户端要用的那个地址**：fdroidserver 会把这个字符串
	// 逐字写进 `index-v2.json` 的 `repo.address`，客户端随后拿 `address + "/" + 文件名`
	// 去取每一个 APK（03 §4.4）。所以这个字符串里多一个字符就多在所有 APK 地址里。
	//
	// 尾斜杠正是那种多出来的字符：`.../repo/` + `/` + `x.apk` 是双斜杠。多数服务端会
	// 把双斜杠折掉，于是它**看起来能用** —— 而"看起来能用"的配置一旦发出去就会成为事实，
	// 之后想改回来就要所有人重新添加这个源（地址是用户添加源时存下来的）。
	u, err := url.Parse(e.RepoURL)
	if err != nil {
		return fmt.Errorf("endpoints.repoUrl 无法解析（%q）：%w", e.RepoURL, err)
	}
	if u.Scheme != "https" {
		// 豁免只有一格，且精确限制在回环上（见 isLoopbackHost）。
		if !(u.Scheme == "http" && isLoopbackHost(u.Hostname())) {
			return fmt.Errorf("endpoints.repoUrl 的协议是 %q（%q），必须是 https —— "+
				"客户端的源地址是明文可读的，而它是这套私有市场唯一对外暴露的地址；"+
				"唯一的例外是回环 http（本机验证用）", u.Scheme, e.RepoURL)
		}
	}
	if u.Host == "" {
		return fmt.Errorf("endpoints.repoUrl 没有 host：%q", e.RepoURL)
	}
	if strings.HasSuffix(e.RepoURL, "/") {
		return fmt.Errorf("endpoints.repoUrl 带尾斜杠（%q）—— 它会被逐字写进索引，"+
			"而客户端是拿 `address + \"/\" + 文件名` 取文件的，于是每个 APK 地址都是双斜杠", e.RepoURL)
	}
	return nil
}

// isLoopbackHost 报告 host 是不是回环地址（`localhost` / 127.0.0.0 段 / `::1`）。
//
// 存在的唯一理由是**本机验证**：规格 03 §7 #11 那条"整机装一个真客户端、添加自定义源、
// 装一个应用"必须走 http（`python -m http.server` 不做 TLS），而它连的必然是回环地址。
//
// 把豁免精确地卡在回环上，是为了让这条豁免**不可能**落到一件真要发布的东西上：
// 一个对外的 repo 地址不可能是 `localhost`。
//
// ⚠️ 注意参数是 `u.Hostname()` 而不是 `u.Host` —— 后者带端口（`localhost:8000`），
// 而带端口的串与任何固定值比对都会漏；`Hostname()` 已经把端口剥掉了。
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Validate 检查一份 sources/{appId}.json 是否符合 03 §2.2。
//
// fileName 是它在仓库里的实际文件名，用来核对"文件名 = {appId}.json"这条硬规则 ——
// 写错名字的后果不是报错而是**静默漏掉**（遍历时按文件名取 id，对不上就永远不被处理）。
func (s *Source) Validate(fileName string) error {
	if s.ID == "" {
		return fmt.Errorf("id 为空")
	}
	if fileName != "" && fileName != s.ID+".json" {
		return fmt.Errorf("文件名是 %q，但 id 是 %q —— 必须是 %s.json", fileName, s.ID, s.ID)
	}
	if s.ID == IncomingTag {
		return fmt.Errorf("id 不得为保留名 %q（03 §3.1：那是手动上传的暂存 Release，不是合法的 appId）", IncomingTag)
	}
	if strings.ContainsAny(s.ID, `/\`) {
		return fmt.Errorf("id %q 含斜杠：包名 charset 是 [a-zA-Z0-9_.]，含斜杠会让 tag 与路径都需要转义（03 §3.1）", s.ID)
	}
	if s.Name == "" {
		return fmt.Errorf("name 为空")
	}
	if s.Author == "" {
		return fmt.Errorf("author 为空")
	}
	// desc 是可选字段，但**手改文件这条入口必须自己把关**：issue 那条路会在
	// 解析时先裁到上限（issue 里不能拒，见 TruncateDesc），这里若也不管，
	// 一个手写的超长 desc 会一路渲染成 metadata 的 Summary，把客户端列表里那一行挤没。
	if n := len([]rune(s.Desc)); n > MaxDescRunes {
		return fmt.Errorf("desc 有 %d 个字，超过上限 %d（02 §2.5）—— "+
			"它渲染成 metadata 的 Summary，也就是客户端列表里应用名下面那一行小字", n, MaxDescRunes)
	}
	if strings.ContainsAny(s.Desc, "\r\n") {
		return fmt.Errorf("desc 含换行符 —— 它渲染成 metadata 的 Summary，而那一行在客户端是**单行**：" +
			"换行不会渲染成两行，只会被吞掉")
	}

	switch s.Source {
	case SourceGitHub:
		if s.Upstream == nil {
			return fmt.Errorf("source=github 时 upstream 必填")
		}
		if err := s.Upstream.Validate(); err != nil {
			return err
		}
	case SourceManual:
		if s.Upstream != nil {
			return fmt.Errorf("source=manual 时不该有 upstream（二进制走 _incoming 上传队列）")
		}
	default:
		return fmt.Errorf("source = %q，只能是 %q 或 %q", s.Source, SourceGitHub, SourceManual)
	}

	for _, a := range s.ABIWhitelist {
		if !naming.IsABI(a) {
			return fmt.Errorf("abiWhitelist 里的 %q 不在固定集 %v 内", a, naming.ABISet)
		}
	}
	return nil
}

// Validate 检查一个 github 上游自身，**不涉及 appId 与文件名**。
//
// 从 Source.Validate 里抽出来是为了让"还没有 appId 的那半条申请"也能复用同一批规则：
// issue 新增单里申请人只填 repo，id 要等对账从 APK 里读出来，而此刻
// `ID == ""` 会让 Source.Validate 直接判失败。规则只写一遍，两条入口才不可能漂移。
func (u *Upstream) Validate() error {
	if u.Type != UpstreamGitHubRelease {
		return fmt.Errorf("upstream.type = %q，v1 只支持 %q", u.Type, UpstreamGitHubRelease)
	}
	if err := validateRepoSlug(u.Repo); err != nil {
		return fmt.Errorf("upstream.repo：%w", err)
	}
	if u.AssetPattern != "" {
		// 提前在配置读取期就把正则编译一次：留到遍历上游时才发现写错，
		// 会在"某个上游恰好发版"时才炸，排查成本高得多。
		if _, err := compilePattern(u.AssetPattern); err != nil {
			return fmt.Errorf("upstream.assetPattern：%w", err)
		}
	}
	return nil
}

// validateRepoSlug 检查 "owner/name" 形状。上游 repo 是拼进 API URL 的东西，
// 提前挡掉畸形值，避免在上游请求里出现路径穿越或空段。
func validateRepoSlug(repo string) error {
	if repo == "" {
		return fmt.Errorf("为空")
	}
	parts := strings.Split(repo, "/")
	if len(parts) != 2 {
		return fmt.Errorf("%q 不是 owner/name 形状", repo)
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return fmt.Errorf("%q 含空段或相对路径段", repo)
		}
		if strings.ContainsAny(p, " \t?#") {
			return fmt.Errorf("%q 含空白或 URL 保留字符", repo)
		}
	}
	return nil
}
