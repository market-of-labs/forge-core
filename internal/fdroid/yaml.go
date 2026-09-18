package fdroid

import (
	"fmt"
	"strconv"
	"strings"
)

// ---- 极简 YAML 发射器 --------------------------------------------------------
//
// 为什么手搓而不是引 yaml.v3：本包要写出去的 YAML 只有三种形状 —— 「本包自己的常量」
// 「自由文本」「字符串列表」—— 而 go.mod 至今只有 androidbinary 一个依赖。为一个
// **只写不读**的发射器引一整套 YAML 实现（解析器 + 锚点 + 流式语法 + 节点树）不划算。
//
// ⚠️ 手搓的代价是必须自己保证正确性，而这个文件里唯一真正难的地方是**转义**：
// name / desc / author 是任意 UTF-8（可能含 `:`、`#`、引号、反斜杠、emoji、首尾空格），
// 写坏一个标量**不会报错** —— fdroidserver 会安安静静地把它读成另一个字符串。
// 所以本文件的正确性由 `yaml_test.go` 的穷举用例兜底，而不是靠"看起来对"。

// doc 是一份按写入顺序落地的 YAML 文档。
//
// **有序**是刻意的：fdroidserver 读到的是一个 map，字段顺序对它毫无意义；但 store 里那份
// yml 要进 git，只有顺序恒定才有稳定的 diff —— 而"每轮对账都会重写这些文件"意味着
// 顺序不稳会让每一轮都产生一份纯噪声的 diff。
type doc struct{ b strings.Builder }

// Plain 写一个**本包自己的常量**（`None`、`Internet`……）：能裸写就裸写。
//
// 它只该被本包的字面量调用。值若不适合裸写会自动退化成双引号（见 scalarText），
// 所以它是**永远正确**的，只是好看不好看的问题。
func (d *doc) Plain(key, val string) { d.scalar(key, val, false) }

// Str 写一个**自由文本**（name / desc / author / 口令 / 路径……）：一律双引号。
//
// 它存在的全部理由见 plainSafe 上面那段 —— 一句话：任何"看起来安全就裸写"的判断
// 都是在复刻 YAML 的隐式类型规则，而复刻一定会漏。
func (d *doc) Str(key, val string) { d.scalar(key, val, true) }

// List 写一个字符串列表。
func (d *doc) List(key string, vals []string) {
	d.key(key)
	if len(vals) == 0 {
		// 空列表写成 `Key: []` 而不是留一行 `Key:` —— 后者在 YAML 里是 **null**，
		// 而 fdroidserver 对 null 与空列表的处理不同（前者常常直接 `.append` 崩）。
		d.b.WriteString(" []\n")
		return
	}
	d.b.WriteString("\n")
	for _, v := range vals {
		d.b.WriteString("  - ")
		d.b.WriteString(scalarText(v, false))
		d.b.WriteByte('\n')
	}
}

// Bytes 返回渲染结果。每一行都以 '\n' 收尾，所以结果**总是**以换行结束 ——
// 一份不以换行结尾的文件在 diff 里会带一个 `\ No newline at end of file`。
func (d *doc) Bytes() []byte { return []byte(d.b.String()) }

// key 写 `key:`。
//
// 这里 panic 是刻意的：key **只可能是本包里的字面量**，所以 `!plainSafe(key)` 意味着
// 有人在本包里写了一个畸形的 key，那是一个编程错误而不是一个输入错误。而它的后果是
// 静默的 —— 一个含空格的 key 会被 fdroidserver 当成另一个字段名，读到的是"这个字段没写"。
// 宁可在写的时候炸，也不要让它在索引里表现为"某个字段莫名其妙丢了"。
func (d *doc) key(key string) {
	if !plainSafe(key) {
		panic(fmt.Sprintf("fdroid: YAML key %q 不是安全标量；key 必须是本包的字面量", key))
	}
	d.b.WriteString(key)
	d.b.WriteByte(':')
}

func (d *doc) scalar(key, val string, forceQuote bool) {
	d.key(key)
	d.b.WriteByte(' ')
	d.b.WriteString(scalarText(val, forceQuote))
	d.b.WriteByte('\n')
}

// scalarText 把一个字符串渲染成 YAML 标量文本。
func scalarText(s string, forceQuote bool) string {
	if !forceQuote && plainSafe(s) {
		return s
	}
	return quote(s)
}

// plainSafe 报告一个值能不能**裸写**。
//
// 它只接受一个很小的字符集，并额外排掉三类"长得像普通字符串、其实是别的类型"的值 ——
// 这三类都是 PyYAML（fdroidserver 用的解析器，YAML **1.1**）会隐式转换的：
//
//	· 布尔 / 空：`yes` `no` `on` `off` `true` `false` `null` `~` ……
//	· 数字：`1.0` `0x10` `1e3` `1_0` ……
//	· **时间戳：`2020-01-01`** —— 这条最阴。它长得完全像一个普通字符串，
//	  而 YAML 1.1 会把它解析成 `datetime.date` 而不是 `str`。
//
// 最后那条正是"自由文本必须一律加引号"的实证理由：把 PyYAML 的隐式解析器实现一遍
// 一定会漏，而漏掉的那一条不会报错、只会让某个字段以错误的类型出现在索引里。
//
// 所以裸写被限制在**我们自己的常量**上（`None`、`Internet`、`Phone & SMS`），
// 而任何来自 sources/ 的字符串都走 doc.Str。
func plainSafe(s string) bool {
	if s == "" {
		return false
	}
	// 首尾空白在裸标量里会被吃掉（`a ` 读回来是 `a`），所以一律判不安全。
	if s != strings.TrimSpace(s) {
		return false
	}

	// ⚠️ **首字节是指示符的一律不裸写**，哪怕这个字符在中间是安全的。
	//
	// 这条是实测出来的（`yaml_test.go` 的 `TestLeadingIndicatorsAreNeverPlain`），
	// 起因是一个**静默**的坑：字符集里必须放行 `&`（否则 §2.6 的 `Phone & SMS`
	// 会被多余地引起来，那倒无害），但 `&` 出现在**节点开头**时是 anchor 指示符 ——
	// 于是 `K: &a` 被解析成"定义一个叫 a 的锚点"，**值变成 null**，
	// 而 null 进 `map[string]string` 就是空串。**不报错，只是数据没了。**
	//
	// 同一个探针还测出 `-` 单独一个也会炸（`K: -` 是残缺的块序列，直接解析错），
	// 而 `-a` 其实是安全的 —— 但区分"`-` 后面跟没跟东西"要盯着整个标量看，
	// 而按首字节一刀切**多引一对引号的代价是零**。这正是本函数一贯的方向：
	// 宁可过度引用，也不放过一个。
	//
	// YAML 的指示符里只有 `-` 与 `&` 落进了上面的字符集（`?:,[]{}#*!|>'"%@\`` 全都
	// 过不了字符集那一关），所以这两条就是全部的缺口。
	switch s[0] {
	case '-', '&':
		return false
	}

	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == ' ' || c == '_' || c == '.' || c == '+' || c == '&' || c == '-':
		default:
			return false
		}
	}
	return !looksTyped(s)
}

// looksTyped 报告这个（已经过了字符集的）值会不会被 YAML 读成非字符串。
func looksTyped(s string) bool {
	switch strings.ToLower(s) {
	case "y", "n", "yes", "no", "true", "false", "on", "off", "null", "~":
		return true
	}
	// ParseFloat 同时接住 `nan` / `inf` / `.inf` / `1e3`；ParseInt 接住 `0x10` / `0o7`
	// / `0b1` 与 YAML 1.1 也认的 `1_0`（Go 的 base=0 同样接受下划线）。
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return true
	}
	if _, err := strconv.ParseInt(s, 0, 64); err == nil {
		return true
	}
	return looksLikeDate(s)
}

// looksLikeDate 认 YAML 1.1 的日期形状 `2002-12-14`。
//
// 只认这一种：带时间的形状（`2002-12-14 21:59:43`）含 `:` 与空格以外的分隔符，
// 到不了这里 —— `:` 不在 plainSafe 的字符集里，早就被判成不安全了。
func looksLikeDate(s string) bool {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if i == 4 || i == 7 {
			continue
		}
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// quote 把一个任意 UTF-8 字符串渲染成 YAML 双引号标量。
//
// ⚠️ **非 ASCII 一律写成 `\uXXXX` / `\UXXXXXXXX`，所以输出永远是纯 ASCII。**
//
// 这一条不是"更安全"这种空话，而是一个具体风险：fdroidserver 在容器里由 Python 读这个
// 文件，而 Python 的 `open()` **默认按 locale 编码解码** —— 容器里没有 LANG 时那就是
// ASCII。若它没显式指定 encoding，一个中文 Name 有两种坏法：
//
//	· 解码报错（好情况，能在日志里看见）
//	· 按 latin-1 读成乱码（坏情况，**不报错**，名字就那么错着进索引）
//
// 而本机没有 Debian 环境（计划第 3 条：验证以 CI 为主），这个反馈环只在 CI 里闭合。
// 所以这里选那条**不依赖任何假设**的写法：纯 ASCII 的文件，无论被当成哪种编码解码，
// 读回来都是对的。
//
// 代价是 store 里那份 yml 的可读性 —— 而它是纯机器产物、人改的一直是 sources/*.json，
// 所以这个代价落在了一个没人在看的地方。
func quote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case 0:
			b.WriteString(`\0`)
		default:
			switch {
			case r >= 0x20 && r <= 0x7E:
				b.WriteRune(r)
			case r > 0xFFFF:
				// YAML 的双引号转义只有 `\xXX` / `\uXXXX` / `\UXXXXXXXX` 三种宽度。
				// 把 BMP 外的字符写成一对 `\uXXXX` **看起来**像 UTF-16 代理对，
				// 但 YAML 不认代理对 —— 读回来是两个孤立字符，不是那个 emoji。
				fmt.Fprintf(&b, `\U%08X`, r)
			default:
				fmt.Fprintf(&b, `\u%04X`, r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
