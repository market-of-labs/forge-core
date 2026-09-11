// Package issue 把 GitHub Issue Form 渲染出来的正文解析成"字段 → 值"。
//
// 为什么要单独一个包：这是**外部输入唯一进入系统的地方**（03 §5.5）。issue 正文由
// 陌生人书写、由 GitHub 渲染，任何解析失误都会直接变成一个来源文件的字段值。所以
// 这里只做**纯文本解析** —— 不 eval、不执行、不把值当代码，且整体有单测覆盖。
//
// # GitHub Issue Form 的渲染格式
//
// 表单正文（`.github/ISSUE_TEMPLATE/*.yml`）被 GitHub 渲染成这样的 markdown：
//
//	### 应用包名（appId）
//
//	dev.imranr.obtainium
//
//	### 显示名
//
//	Obtainium
//
// 要点：
//   - 标题行是 `### ` + 表单里的 **label**（不是 id！id 不出现在正文里）。
//   - 未填的可选字段渲染成 `_No response_`，语义等于"空"。
//   - 用户在正文里可以随手改任何东西 —— 包括伪造出重复的 `### 应用包名（appId）`。
//     所以这里的取值规则必须**确定**：同一个 label 出现多次时取**第一个**非空值，
//     并把重复这件事报出来（见 Duplicates），而不是随便挑一个。
package issue

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// noResponse 是 GitHub 对"可选字段留空"的渲染结果。
const noResponse = "_No response_"

// heading 匹配表单字段的标题行。
//
// 只认行首的 `### `（一到三级都见过，用 `#{2,4}` 兜住），且要求后面有内容。
// 不认 `#### ` 之外的任意标题层级：issue 正文里用户自己写的 `# 标题` 很常见，
// 把它当成字段会让取值莫名其妙地错位。
var heading = regexp.MustCompile(`(?m)^#{2,4}[ \t]+(.+?)[ \t]*$`)

// Form 是一份解析好的 issue 正文。
type Form struct {
	// fields 按出现顺序保存 label → 值。
	fields map[string]string
	// Duplicates 记录出现次数 >1 的 label。调用方应当据此拒绝 ——
	// 重复字段几乎只可能来自"有人在正文里手工插了一段"，而那正是要警惕的输入。
	Duplicates []string
	// order 保留字段出现顺序，仅用于报错信息可读。
	order []string
}

// Parse 解析一份 issue 表单正文。
//
// 对无法识别的输入是**宽容**的（不报错，只是取不到值）：调用方拿 Get 取不到自然会
// 报"缺字段"，那条错误信息比"格式不对"有用得多。
func Parse(body string) *Form {
	f := &Form{fields: map[string]string{}}

	idx := heading.FindAllStringSubmatchIndex(body, -1)
	seen := map[string]int{}

	for i, m := range idx {
		label := strings.TrimSpace(body[m[2]:m[3]])
		if label == "" {
			continue
		}

		// 值从本行末尾到下一个标题行之间。
		start := m[1]
		end := len(body)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		value := cleanValue(body[start:end])

		seen[label]++
		if seen[label] > 1 {
			f.Duplicates = append(f.Duplicates, label)
			// 取第一个非空的：第一个是模板渲染出来的那个，后面的是人工插的。
			if f.fields[label] != "" {
				continue
			}
		}
		if _, ok := f.fields[label]; !ok {
			f.order = append(f.order, label)
		}
		f.fields[label] = value
	}

	return f
}

// cleanValue 把一段原始文本变成字段值。
func cleanValue(raw string) string {
	raw = strings.TrimSpace(raw)
	// `_No response_` 只在整段就是它本人时才算空 —— 否则一个填入
	// "见 _No response_ 说明" 的值会被误判成空。
	if raw == noResponse {
		return ""
	}
	// 折叠内部换行：多行字段（textarea）里的换行在来源文件里没有意义，
	// 而保留换行会让 JSON 里出现 \n，徒增 diff 噪声。
	return strings.Join(strings.Fields(raw), " ")
}

// Labels 返回出现过的全部 label（按出现顺序）。
func (f *Form) Labels() []string {
	if f == nil {
		return nil
	}
	out := make([]string, len(f.order))
	copy(out, f.order)
	return out
}

// Get 取一个字段的值，按顺序尝试多个候选 label（用于模板改过名的情况）。
// 都取不到时返回空串。
func (f *Form) Get(labels ...string) string {
	if f == nil {
		return ""
	}
	for _, l := range labels {
		if v, ok := f.fields[l]; ok && v != "" {
			return v
		}
	}
	return ""
}

// Required 与 Get 相同，但取不到时报错。errName 用于拼错误信息（写"应用包名"而不是
// 那一长串带括号的 label）。
func (f *Form) Required(errName string, labels ...string) (string, error) {
	if v := f.Get(labels...); v != "" {
		return v, nil
	}
	if len(labels) == 0 {
		return "", fmt.Errorf("内部错误：Required(%q) 没有给任何 label", errName)
	}
	return "", fmt.Errorf("issue 正文里找不到「%s」字段（期待的标题是 `### %s`）—— "+
		"请用仓库里的 issue 模板重开一个，不要手改正文结构", errName, labels[0])
}

// List 取一个逗号分隔的列表字段。中英文逗号、顿号都当分隔符（表单说明里写的是
// "逗号分隔"，但中文输入法下打出全角逗号太常见了，为此报错不值得）。
func (f *Form) List(labels ...string) []string {
	raw := f.Get(labels...)
	if raw == "" {
		return nil
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '，' || r == '、' || r == ';' || r == '；'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Int 取一个整数字段。
func (f *Form) Int(labels ...string) (int, bool) {
	raw := f.Get(labels...)
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, false
	}
	return n, true
}
