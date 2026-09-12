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
//	### 上游 GitHub 仓库
//
//	ImranR98/Obtainium
//
//	### 分类标签（可选）
//
//	- [X] 工具
//	- [ ] 效率
//
// 要点：
//   - 标题行是 `### ` + 表单里的 **label**（不是 id！id 不出现在正文里）。
//   - 未填的可选字段渲染成 `_No response_`，语义等于"空"。
//   - `checkboxes` 字段渲染成**一整张任务列表**（勾中的 `- [X]`、未勾的 `- [ ]`），
//     不是"只列出勾中的那几个" —— 取值走 Checked，不能走 List。
//   - 用户在正文里可以随手改任何东西 —— 包括伪造出重复的 `### 上游 GitHub 仓库`。
//     所以这里的取值规则必须**确定**：同一个 label 出现多次时取**第一个非空值**，
//     而不是随便挑一个。（取第一个 = 取人在渲染出来的正文里最先看到的那一个。）
package issue

import (
	"fmt"
	"regexp"
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

// checkboxItem 匹配 checkboxes 字段里的一项。
//
// **不锚定行首**，因为 cleanValue 已经把换行折叠成空格了：到这里的值长这样
//
//   - [X] arm64-v8a - [ ] armeabi-v7a - [X] universal
//
// 锚定 `^` 只会取到第一项。所以改成认 `[X]` 这个标记本身，再取紧跟其后的一个词。
// 代价是**选项标签不能含空格**（`[^\s\]]+` 会在空格处停）—— 这正是
// `TestTemplateVocabularyMatchesGo` 顺带断言"选项里没有空格"的原因。
var checkboxItem = regexp.MustCompile(`\[([xX ])\]\s*([^\s\]]+)`)

// Form 是一份解析好的 issue 正文。
type Form struct {
	// fields 按出现顺序保存 label → 值。
	fields map[string]string
}

// Parse 解析一份 issue 表单正文。
//
// 对无法识别的输入是**宽容**的（不报错，只是取不到值）：调用方拿 Get 取不到自然会
// 报"缺字段"，那条错误信息比"格式不对"有用得多。
func Parse(body string) *Form {
	f := &Form{fields: map[string]string{}}

	idx := heading.FindAllStringSubmatchIndex(body, -1)

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
		// 同一个 label 出现多次时取第一个非空值：模板渲染的那个在前，人工插的在后面。
		if v, ok := f.fields[label]; !ok || v == "" {
			f.fields[label] = cleanValue(body[start:end])
		}
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

// Get 取一个字段的值，取不到时返回空串。
func (f *Form) Get(label string) string {
	if f == nil {
		return ""
	}
	return f.fields[label]
}

// Required 与 Get 相同，但取不到时报错。错误信息里点出**该看到的那个标题** ——
// 申请人据此才知道自己缺的是哪一段。
func (f *Form) Required(label string) (string, error) {
	if v := f.Get(label); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("issue 正文里找不到「%s」字段（期待的标题是 `### %s`）—— "+
		"请用仓库里的 issue 模板重开一个，不要手改正文结构", label, label)
}

// Checked 取一个 checkboxes 字段里**被勾中**的项，按出现顺序。
//
// 认的是 GitHub 渲染出来的勾选标记，所以用户没法靠打字伪造出没勾的项。
// 返回 nil 表示"一项都没勾" —— 调用方把这个状态解释成"没限制"
// （与模板里"一个都不勾 = 全部"的文案一致）。
func (f *Form) Checked(label string) []string {
	raw := f.Get(label)
	if raw == "" {
		return nil
	}
	var out []string
	for _, m := range checkboxItem.FindAllStringSubmatch(raw, -1) {
		// m[1] 是勾选标记：`x`/`X` 为勾中，空格为未勾。
		if m[1] != " " {
			out = append(out, m[2])
		}
	}
	return out
}
