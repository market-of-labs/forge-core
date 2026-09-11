package model

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/market-of-labs/forge-core/internal/naming"
)

// 模板里允许出现的占位符（03 §2.3）。**这是全部** —— 未知占位符一律报错，
// 不静默留空，因为静默留空会让一个拼错的模板产出"看起来对"的地址。
const (
	VarAppID    = "appId"
	VarVersion  = "version"
	VarABI      = "abi"
	VarFileName = "fileName"
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

// AssetURL 按 assetUrlTemplate 渲染下载地址（03 §2.3）。
//
// `fileName` 是独立变量而不是"由另外三个拼出来"：第一期模板是
// `.../download/{appId}/{fileName}`，于是文件名必须能独立代入；
// 若模板改成 `.../{appId}/{version}/{fileName}`，同一份调用也照样成立。
func (e Endpoints) AssetURL(appID, version, fileName string) (string, error) {
	return render(e.AssetURLTemplate, map[string]string{
		VarAppID:    appID,
		VarVersion:  version,
		VarFileName: fileName,
	})
}

// AssetURLForABI 是 AssetName + AssetURL 的组合，返回清单里 apkUrls 需要的那一对。
func (e Endpoints) AssetURLForABI(appID, version, abi string) (APKRef, error) {
	name, err := e.AssetName(appID, version, abi)
	if err != nil {
		return APKRef{}, err
	}
	u, err := e.AssetURL(appID, version, name)
	if err != nil {
		return APKRef{}, err
	}
	return APKRef{Name: name, URL: u}, nil
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
	v := []string{VarAppID, VarVersion, VarABI, VarFileName}
	sort.Strings(v)
	return v
}

// Validate 检查模板自身是否自洽。
//
// **它不检查模板"写了什么"，而是检查模板"渲染出什么"** —— 用两个不同的探针值渲染，
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
	if strings.TrimSpace(e.AssetURLTemplate) == "" {
		return fmt.Errorf("endpoints.assetUrlTemplate 为空")
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

	// ③ assetUrlTemplate 必须是 https，且真的用到了 {fileName}。
	if !strings.Contains(e.AssetURLTemplate, "{"+VarFileName+"}") {
		return fmt.Errorf("endpoints.assetUrlTemplate 里没有 {%s}：模板 %q 无法定位到具体文件",
			VarFileName, e.AssetURLTemplate)
	}
	probeURL, err := e.AssetURL("com.example.probe", "1.2.3", "com.example.probe-1.2.3-universal.apk")
	if err != nil {
		return fmt.Errorf("endpoints.assetUrlTemplate 渲染失败：%w", err)
	}
	u, err := url.Parse(probeURL)
	if err != nil {
		return fmt.Errorf("endpoints.assetUrlTemplate 渲染出的地址无法解析（%q）：%w", probeURL, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("endpoints.assetUrlTemplate 渲染出的地址协议是 %q（%q），必须是 https —— "+
			"明文 http 会让客户端拒绝下载，而哨兵 url 的 `.invalid` 约束只约束哨兵、不约束这里",
			u.Scheme, probeURL)
	}
	if u.Host == "" {
		return fmt.Errorf("endpoints.assetUrlTemplate 渲染出的地址没有 host：%q", probeURL)
	}
	return nil
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

	switch s.Source {
	case SourceGitHub:
		if s.Upstream == nil {
			return fmt.Errorf("source=github 时 upstream 必填")
		}
		if s.Upstream.Type != UpstreamGitHubRelease {
			return fmt.Errorf("upstream.type = %q，v1 只支持 %q", s.Upstream.Type, UpstreamGitHubRelease)
		}
		if err := validateRepoSlug(s.Upstream.Repo); err != nil {
			return fmt.Errorf("upstream.repo：%w", err)
		}
		if s.Upstream.AssetPattern != "" {
			// 提前在配置读取期就把正则编译一次：留到遍历上游时才发现写错，
			// 会在"某个上游恰好发版"时才炸，排查成本高得多。
			if _, err := compilePattern(s.Upstream.AssetPattern); err != nil {
				return fmt.Errorf("upstream.assetPattern：%w", err)
			}
		}
	case SourceManual:
		if s.Upstream != nil {
			return fmt.Errorf("source=manual 时不该有 upstream（二进制走 _incoming 上传队列）")
		}
	default:
		return fmt.Errorf("source = %q，只能是 %q 或 %q", s.Source, SourceGitHub, SourceManual)
	}

	if s.Kind != "" && s.Kind != KindObtainium && s.Kind != KindCompanion {
		return fmt.Errorf("kind = %q，只能是 %q 或 %q（02 规则 9）", s.Kind, KindObtainium, KindCompanion)
	}

	for _, a := range s.ABIWhitelist {
		if !naming.IsABI(a) {
			return fmt.Errorf("abiWhitelist 里的 %q 不在固定集 %v 内", a, naming.ABISet)
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
