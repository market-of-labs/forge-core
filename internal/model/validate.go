package model

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/market-of-labs/forge-core/internal/naming"
)

// Severity 区分"阻断"与"提示"。
//
// 03 §5.3 的口径：**除硬错误外只告警不阻断**。所以这个区分不是装饰 ——
// 它决定 check-manifest 的退出码，也就决定 forge 会不会把这份清单推回 store。
type Severity int

const (
	// SeverityError 会阻断回写。
	SeverityError Severity = iota
	// SeverityWarn 只打印，不阻断。
	SeverityWarn
)

func (s Severity) String() string {
	if s == SeverityError {
		return "ERROR"
	}
	return "WARN"
}

// Problem 是一条校验结论。AppID 可能为空（如信封级规则）。
type Problem struct {
	Sev   Severity
	AppID string
	Msg   string
}

func (p Problem) String() string {
	if p.AppID == "" {
		return fmt.Sprintf("%s  %s", p.Sev, p.Msg)
	}
	return fmt.Sprintf("%s  [%s] %s", p.Sev, p.AppID, p.Msg)
}

// Report 累积一个校验过程里的全部结论。
//
// 刻意**不早退**：一次跑完把问题全列出来，比"修一个跑一次"省事得多 ——
// 而 check-manifest 本来就是给人看的诊断工具。
type Report struct {
	Problems []Problem
}

func (r *Report) Errorf(appID, format string, a ...any) {
	r.Problems = append(r.Problems, Problem{Sev: SeverityError, AppID: appID, Msg: fmt.Sprintf(format, a...)})
}

func (r *Report) Warnf(appID, format string, a ...any) {
	r.Problems = append(r.Problems, Problem{Sev: SeverityWarn, AppID: appID, Msg: fmt.Sprintf(format, a...)})
}

// Addf 把另一份报告的结论并进来（带上来源标签，便于在合并后的报告里定位）。
func (r *Report) Addf(prefix string, other *Report) {
	if other == nil {
		return
	}
	for _, p := range other.Problems {
		m := p.Msg
		if prefix != "" {
			m = prefix + m
		}
		r.Problems = append(r.Problems, Problem{Sev: p.Sev, AppID: p.AppID, Msg: m})
	}
}

// Errors 只取阻断项。
func (r *Report) Errors() []Problem {
	var out []Problem
	for _, p := range r.Problems {
		if p.Sev == SeverityError {
			out = append(out, p)
		}
	}
	return out
}

// Warnings 只取提示项。
func (r *Report) Warnings() []Problem {
	var out []Problem
	for _, p := range r.Problems {
		if p.Sev == SeverityWarn {
			out = append(out, p)
		}
	}
	return out
}

// HasErrors 报告是否存在阻断项。
func (r *Report) HasErrors() bool {
	for _, p := range r.Problems {
		if p.Sev == SeverityError {
			return true
		}
	}
	return false
}

// Err 把阻断项收成一个 error，供"失败即退出"的调用点直接用。
// 无阻断项时返回 nil —— 提示项**不**产生 error（03 §5.3）。
func (r *Report) Err() error {
	errs := r.Errors()
	if len(errs) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d 条校验失败：", len(errs))
	for i, p := range errs {
		if i > 0 {
			b.WriteString("；")
		}
		b.WriteString(p.String())
	}
	return fmt.Errorf("%s", b.String())
}

// ---- 02 §2.8 清单自检 -------------------------------------------------------

// Validate 跑 02 §2.8 的全部规则，外加 03 §5.3 的阈值告警。
//
// ep 是渲染 apkUrls 的模板：规则 3 要求"清单里所有地址都指向本市场仓库的 Release
// asset"，而**唯一可信的判据**就是"每个 url 恰好等于该 name 按模板渲染出来的结果"。
// 这一条同时把"不得出现上游直链"也涵盖了 —— 上游直链不可能等于模板渲染值。
func (m *Manifest) Validate(ep Endpoints) *Report {
	r := &Report{}

	// 规则 1
	if m.SchemaVersion != SchemaVersion {
		r.Errorf("", "schemaVersion = %d，必须恒为 %d（02 §2.1）", m.SchemaVersion, SchemaVersion)
	}
	if len(m.Apps) == 0 {
		r.Errorf("", "apps 为空：清单至少要有条目，否则客户端拉到的是一份空市场")
	}
	if m.ExportedAt == "" {
		r.Errorf("", "exportedAt 为空")
	}

	seen := make(map[string]int, len(m.Apps))
	companions := 0

	for i := range m.Apps {
		e := &m.Apps[i]
		// 规则 1：id 全局唯一。这里不用 map[string]bool 而是记次数，
		// 好让报错说出"重复了几次"，而不是只报第二次出现的位置。
		seen[e.ID]++
		r.addf(validateEntry(e, ep))

		if e.Kind == KindCompanion {
			companions++
		}
	}
	for id, n := range seen {
		if n > 1 {
			r.Errorf(id, "id 重复出现了 %d 次：id 是安装身份，重复会让客户端按 id 互相覆盖", n)
		}
	}

	// 规则 9：kind:"companion" 至多一条。规格说客户端"取第一条并日志告警"，
	// 所以这里也是告警而非阻断 —— 多出来的那条不会让设备端坏掉，只是行为不确定。
	if companions > 1 {
		r.Warnf("", "有 %d 条 kind=%q 的条目，至多应有一条（02 规则 9）", companions, KindCompanion)
	}

	// 03 §5.3：条目数软告警。全量推送把市场规模直接暴露为设备端单次 URI 的大小。
	if len(m.Apps) > EntryCountWarn {
		r.Warnf("", "条目数 %d 超过 %d：deep-link 是**单次 URI 全量推送**（01 §3.6），"+
			"超限时设备端会抛异常，需评估拆分方案", len(m.Apps), EntryCountWarn)
	}
	return r
}

func (r *Report) addf(other *Report) { r.Addf("", other) }

// validateEntry 是单条目校验，独立成函数以便被 build-manifest 逐条复用
// （产出前自检时不必先拼出整份信封）。
func validateEntry(e *Entry, ep Endpoints) *Report {
	r := &Report{}
	id := e.ID

	// 保留名（03 §3.1）
	if id == IncomingTag {
		r.Errorf(id, "%q 是保留名（手动上传的暂存 Release），不得作为 appId", IncomingTag)
	}
	// 规则 2：必填字段
	if id == "" {
		r.Errorf("", "id 为空")
	}
	if e.Name == "" {
		r.Errorf(id, "name 为空")
	}
	if e.Author == "" {
		r.Errorf(id, "author 为空")
	}
	if e.LatestVersion == "" {
		r.Errorf(id, "latestVersion 为空（规则 4）")
	}
	// 规则 8
	if want := SentinelURL(id); e.URL != want && id != "" {
		r.Errorf(id, "url = %q，必须是哨兵地址 %q（规则 8：哨兵永不联网，仅作唯一键）", e.URL, want)
	}
	if e.OverrideSource != OverrideSource {
		r.Errorf(id, "overrideSource = %q，必须恒为 %q（规则 8）", e.OverrideSource, OverrideSource)
	}
	// 规则 9
	if e.Kind != "" && e.Kind != KindObtainium && e.Kind != KindCompanion {
		r.Errorf(id, "kind = %q，只能是 %q 或 %q（规则 9）", e.Kind, KindObtainium, KindCompanion)
	}

	// 规则 2 / 4：apkUrls
	refs, err := e.APKRefs()
	if err != nil {
		r.Errorf(id, "apkUrls 无法解析：%v（规则 2）", err)
		return r
	}
	if len(refs) == 0 {
		r.Errorf(id, "apkUrls 为空（规则 4）")
	}

	abis := make([]string, 0, len(refs))
	for i, ref := range refs {
		version, abi, err := naming.Split(id, ref.Name)
		if err != nil {
			r.Errorf(id, "apkUrls[%d].name = %q 不符合命名契约：%v（02 §2.4 —— 客户端靠它折叠 ABI，"+
				"解析失败是静默降级，不会报错，只会让该架构的设备拿不到这个 App）", i, ref.Name, err)
			continue
		}
		abis = append(abis, abi)

		// 规则 3：地址必须恰好是模板渲染值。
		want, err := ep.AssetURL(id, version, ref.Name)
		if err != nil {
			r.Errorf(id, "apkUrls[%d] 渲染模板失败：%v", i, err)
			continue
		}
		if ref.URL != want {
			r.Errorf(id, "apkUrls[%d].url = %q，按 endpoints.assetUrlTemplate 必须是 %q（规则 3：清单内不得出现上游直链）",
				i, ref.URL, want)
			continue
		}
		u, err := url.Parse(ref.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			r.Errorf(id, "apkUrls[%d].url 不是合法 https 地址：%q（规则 4）", i, ref.URL)
		}
	}

	// 02 §2.4 列表顺序约定：universal 在前，其后按固定集。
	if len(abis) > 1 {
		if want := naming.SortABIs(abis); !equalStrings(abis, want) {
			r.Warnf(id, "apkUrls 的 ABI 顺序是 %v，约定顺序是 %v（02 §2.4）", abis, want)
		}
	}

	// 规则 2：otherAssetUrls 也必须是合法 JSON 数组
	if e.OtherAssetUrls != "" {
		if _, err := UnmarshalAPKRefs(e.OtherAssetUrls); err != nil {
			r.Errorf(id, "otherAssetUrls 无法解析：%v（规则 2）", err)
		}
	}

	// 规则 6：versionCode 必填且为正整数
	vc, ok := e.VersionCode()
	switch {
	case !ok:
		r.Errorf(id, "additionalSettings 里缺 versionCode（规则 6 + 02 §2.3：v1 契约必填）")
	case vc <= 0:
		r.Errorf(id, "versionCode = %d，必须是正整数（规则 6）", vc)
	}

	// 一致性：latestVersion 与 apkUrls 的 version token 应当指同一个版本。
	// 只告警不阻断：02 §2.4 说 token 是清洗后的 versionName，所以两者**允许不同**
	// （`1.0 beta` → `1.0-beta`），但清洗后仍对不上就意味着清单指向了一个不存在的版本。
	if e.LatestVersion != "" && len(refs) > 0 {
		if token, err := naming.SanitizeVersion(e.LatestVersion); err == nil {
			hit := false
			for i := range refs {
				if v, _, err := naming.Split(id, refs[i].Name); err == nil && v == token {
					hit = true
					break
				}
			}
			if !hit {
				r.Warnf(id, "latestVersion = %q（清洗后 token = %q）在 apkUrls 里找不到对应版本 —— "+
					"清单声称的最新版没有任何 asset 指向它", e.LatestVersion, token)
			}
		}
	}

	// preferredApkIndex 不落在 [0, len(refs)) 里是可疑的，但 02 §2.8 没有这条规则，
	// 所以只告警（03 §5.3：除硬错误外只告警不阻断）。
	if len(refs) > 0 && (e.PreferredAPKIdx < 0 || e.PreferredAPKIdx >= len(refs)) {
		r.Warnf(id, "preferredApkIndex = %d 超出 apkUrls 的索引范围 [0,%d)", e.PreferredAPKIdx, len(refs))
	}
	return r
}

// ---- 03 §5.3 其余阈值 -------------------------------------------------------

// CheckSourceSet 校验整个 sources/ 集合，并检查跨条目的规则
// （id 唯一、kind:"companion" 至多一条）。
func CheckSourceSet(sources []Source) *Report {
	r := &Report{}
	seen := make(map[string]int, len(sources))
	companions := 0

	for i := range sources {
		s := &sources[i]
		if err := s.Validate(""); err != nil {
			r.Errorf(s.ID, "sources 条目非法：%v", err)
		}
		seen[s.ID]++
		if s.Kind == KindCompanion {
			companions++
		}
	}
	for id, n := range seen {
		if n > 1 {
			r.Errorf(id, "sources/ 里出现了 %d 个 id 相同的条目", n)
		}
	}
	if companions > 1 {
		r.Errorf("", "sources/ 里有 %d 条 kind=%q 的条目，至多一条（02 规则 9）", companions, KindCompanion)
	}
	return r
}

// CheckReleaseAssetCounts 跑 03 §3.3 的 asset 数阈值告警。
//
// 硬上限 1000 是 GitHub 官方明文，撞顶后唯一的出路是删最老版本的 asset ——
// 那要破 D13 的"全保留"，所以宁可提前在 800 就吵起来（只告警，不阻断）。
func CheckReleaseAssetCounts(counts map[string]int) *Report {
	r := &Report{}
	for tag, n := range counts {
		if n > ReleaseAssetCountWarn {
			r.Warnf(tag, "Release 有 %d 个 asset，超过 %d：GitHub 硬上限是 1000（03 §3.3），"+
				"撞顶后唯一出路是删最老版本的 asset（那会破 D13 全保留）", n, ReleaseAssetCountWarn)
		}
	}
	return r
}

// ---- 小工具 -----------------------------------------------------------------

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// compilePattern 编译 sources 里的 assetPattern（03 §2.2）。
//
// 用 Go 的 regexp 而不是 glob：`assetPattern` 在规格里的例子是 `app.*\.apk$`，
// 那是正则语法而非 glob。编译失败时把原样与错误一起给出，避免只看到
// "error parsing regexp" 却不知道是哪个字段写错了。
func compilePattern(pattern string) (*regexp.Regexp, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("正则 %q 编译失败：%w", pattern, err)
	}
	return re, nil
}

// CompilePattern 导出给上游解析用（internal/gh 与命令层要复用它来筛 asset）。
func CompilePattern(pattern string) (*regexp.Regexp, error) { return compilePattern(pattern) }
