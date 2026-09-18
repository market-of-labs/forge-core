package model

import (
	"fmt"
	"regexp"
	"strings"
)

// Severity 区分"阻断"与"提示"。
//
// 03 §5.3 的口径：**除硬错误外只告警不阻断**。所以这个区分不是装饰 ——
// 它决定 check-repo 的退出码，也就决定 forge 会不会把这一轮的产物推回 store。
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
// 而 check-repo 本来就是给人看的诊断工具。
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

// ---- 03 §5.3 其余阈值 -------------------------------------------------------

// CheckSourceSet 校验整个 sources/ 集合，并检查跨条目的规则（id 唯一）。
//
// 它曾经还有第二条跨条目规则：`kind:"companion"` 至多一条 —— 那是为"自研伴侣应用"
// 那条路准备的**全局唯一**闸门（D58 作废）。companion 这条路整条没了，
// 于是这里只剩 id 唯一这一条，而它仍然是必须的：两条同 id 的来源会让
// `sources/{appId}.json` 这个文件名约定本身失效（先写的赢，后写的不报错）。
func CheckSourceSet(sources []Source) *Report {
	r := &Report{}
	seen := make(map[string]int, len(sources))

	for i := range sources {
		s := &sources[i]
		if err := s.Validate(""); err != nil {
			r.Errorf(s.ID, "sources 条目非法：%v", err)
		}
		seen[s.ID]++
	}
	for id, n := range seen {
		if n > 1 {
			r.Errorf(id, "sources/ 里出现了 %d 个 id 相同的条目", n)
		}
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
