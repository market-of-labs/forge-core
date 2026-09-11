package model

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// APKRef 是 apkUrls 里的一项：`[name, url]`（02 §2.2）。
//
// name = 规范文件名 `{appId}-{version}-{abi}.apk`；url = 由 assetUrlTemplate 渲染的地址。
type APKRef struct {
	Name string
	URL  string
}

// MarshalAPKRefs 渲染 apkUrls 这个字段要存的 **JSON 字符串**。
//
// 注意两层编码：清单里 apkUrls 的值本身是一个字符串，内容是 `[["n","u"],...]`。
// Go 的 json.Marshal 会把内层引号转义 —— 这正是 02 §2.6 样例里
// `"[[\"name\",\"url\"]]"` 那个形态的来源，不是笔误。
//
// 空列表渲染成 `[]` 而不是 `null`：02 规则 8 明说空数组合法，而 null 会让
// 客户端侧的 `jsonDecode` 拿到 null 再 `.map` 就炸。
func MarshalAPKRefs(refs []APKRef) (string, error) {
	pairs := make([][2]string, 0, len(refs))
	for _, r := range refs {
		pairs = append(pairs, [2]string{r.Name, r.URL})
	}
	b, err := json.Marshal(pairs)
	if err != nil {
		return "", fmt.Errorf("渲染 apkUrls：%w", err)
	}
	return string(b), nil
}

// UnmarshalAPKRefs 解析 apkUrls 字段。
//
// 容错口径按 02 §2.9：客户端忽略未知字段。这里同样只取前两个元素，
// 多出来的忽略而不是报错 —— 将来若要扩成 [name,url,size] 不至于让旧解析器炸。
func UnmarshalAPKRefs(s string) ([]APKRef, error) {
	if s == "" {
		return nil, nil
	}
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, fmt.Errorf("apkUrls 不是合法 JSON 数组：%w", err)
	}

	out := make([]APKRef, 0, len(raw))
	for i, item := range raw {
		var pair []string
		if err := json.Unmarshal(item, &pair); err != nil {
			return nil, fmt.Errorf("apkUrls[%d] 不是字符串数组：%w", i, err)
		}
		if len(pair) < 2 {
			return nil, fmt.Errorf("apkUrls[%d] 少于两个元素（需要 [name,url]）", i)
		}
		out = append(out, APKRef{Name: pair[0], URL: pair[1]})
	}
	return out, nil
}

// APKRefs 解析本条目已渲染好的 apkUrls。
func (e *Entry) APKRefs() ([]APKRef, error) { return UnmarshalAPKRefs(e.APKUrls) }

// SetAPKRefs 渲染并写入 apkUrls。
func (e *Entry) SetAPKRefs(refs []APKRef) error {
	s, err := MarshalAPKRefs(refs)
	if err != nil {
		return err
	}
	e.APKUrls = s
	return nil
}

// Settings 解析 additionalSettings 这个 JSON 字符串 map（02 §2.3）。
//
// 空串按"没有设置"处理而不是报错：02 说该字段必填，但一个空 map 与缺失语义相同，
// 让调用方少写一处分支。
func (e *Entry) Settings() (map[string]any, error) {
	if e.AdditionalSettings == "" {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(e.AdditionalSettings), &m); err != nil {
		return nil, fmt.Errorf("additionalSettings 不是合法 JSON 对象：%w", err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// SetSettings 写回 additionalSettings。
//
// Go 的 json.Marshal 对 map 按 key 排序输出，所以同一个 map 永远渲染成同一个字符串 ——
// 这正是我们要的：**稳定 diff**。否则每次重建清单都会产生一行无意义的变更。
//
// 不用 SetEscapeHTML(false)：默认的 HTML 转义会把 `<` 写成 `<`，
// 虽然丑但在 JSON 里完全等价，而关掉它会带来别的转义差异。不值得。
func (e *Entry) SetSettings(m map[string]any) error {
	if m == nil {
		m = map[string]any{}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("渲染 additionalSettings：%w", err)
	}
	e.AdditionalSettings = string(b)
	return nil
}

// VersionCode 取 additionalSettings.versionCode（02 §2.3：v1 契约必填）。
//
// 返回值 ok 区分"字段不存在"与"是 0" —— 虽然规则 6 要求正整数，但解析层
// 不该替校验层做判断（否则"缺字段"与"值为 0"会共用一条错误信息，排查时看不出区别）。
//
// JSON 数字经 any 解出来是 float64，这里既接受 float64（真实解析路径）
// 也接受 int64（程序内构造的 map），避免调用方为了塞值去迁就类型。
func (e *Entry) VersionCode() (int64, bool) {
	m, err := e.Settings()
	if err != nil {
		return 0, false
	}
	v, ok := m["versionCode"]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
		return 0, false
	default:
		return 0, false
	}
}

// SetVersionCode 写入 additionalSettings.versionCode，**保留其余键**。
//
// 02 §2.3 允许该 map 里出现 sha256 / useVersionCodeAsOSVersion 等别的键，
// 并且「清单显式给出的值全部保留」。所以这里只覆盖一个键，不做整份替换。
func (e *Entry) SetVersionCode(code int64) error {
	m, err := e.Settings()
	if err != nil {
		return err
	}
	m["versionCode"] = code
	return e.SetSettings(m)
}

// String 让条目在日志里可读，且**只打印关键字段** —— 公有仓库的 Actions 日志
// 任何登录用户都能读，整条 JSON 里可能有我们不想外扩的东西（03 §6.2 不变量 3）。
func (e *Entry) String() string {
	kind := e.Kind
	if kind == "" {
		kind = "-"
	}
	return fmt.Sprintf("%s@%s(kind=%s)", e.ID, e.LatestVersion, kind)
}

// ParseVersionCode 把 sources/issue 里可能出现的各种数字形态收敛成 int64。
// issue 表单里的值一律是字符串，而文件里的值是数字 —— 两条入口共用这一处转换。
func ParseVersionCode(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("versionCode %q 不是整数：%w", s, err)
	}
	return n, nil
}
