package fdroid

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// hostile 是自由文本**可能长成的样子**，穷举它。
//
// 为什么值得这么长一张表：这些值里没有一个来自我们的代码 —— 全都来自 issue 表单里的
// 人手输入（`name`/`desc`/`author`）或上游 Release 的标题。而我们这边
// **没有 fdroidserver 可以试**（本机没有 Debian 环境，验证以 CI 为主），
// 所以"我发出的 YAML 到底能不能被解析回来"这件事，本地唯一的裁判就是
// `gopkg.in/yaml.v3` —— 一个与我们无关的、真的 YAML 实现。
//
// 失败模式的严重性也不对称：一个没引起来的 `:` 不会让 fdroidserver 报"你这行写错了"，
// 它会让整个 metadata 文件读不进来，于是**那个应用静默地从索引里消失**。
var hostile = []string{
	// -- 空与空白：YAML 里 `K: ` 是 null，不是空串 --
	"", " ", "  ", "\t", "\n", "\r\n", "a ", " a", "  a  ", "\t a \t",

	// -- 结构字符：冒号是键值分隔、`#` 起注释、`-` 起序列 --
	":", "a:", ":a", "a: b", "a:b", "Key: Value", "a: ", " : ",
	"#", "a#b", " # comment", "a #comment", "#a", "a #",
	"-", "- a", "-a", "--", "---", "...", "- -",

	// -- **首字节是指示符**：这一组是本包真实踩过的坑，见
	//    TestLeadingIndicatorsAreNeverPlain。`&` 必须待在字符集里（否则
	//    `Phone & SMS` 会被多余地引起来），但它出现在开头就是 anchor 指示符。 --
	"&", "&a", "&&", "&a b", "&a: b", "*a", "!a", "|a", ">a", "%a", "@a", "`a",
	"?", "?", ": ", ",", "[", "]", "{", "}", "'", "<a>", "=a",

	// -- 引号与反斜杠：转义写错会吃掉字符，或者干脆解析失败 --
	`"`, `""`, `"a"`, `"a`, `a"`, `'"'`,
	`\`, `\\`, `\n`, `\t`, `a\b`, `C:\path\to`, `"\\"`, `\u0041`,

	// -- 类型推断：不引起来就会被读成 bool / int / float / 时间 --
	"true", "True", "TRUE", "false", "yes", "no", "on", "off", "y", "n", "Y", "N",
	"null", "Null", "NULL", "~",
	"0", "1", "-1", "007", "0x1f", "0o17", "1e3", "1.5", ".5", "1_000", "+1",
	"2002-12-14", "2026-09-18",

	// -- 组合：结构字符 + 前后空格，两头最容易一起踩 --
	" a: b ", "  #x  ", " - x ", "\ta: b\n",

	// -- 非 ASCII：中文、全角标点、emoji、星平面、组字序列 --
	"记事本", "工具", "中文 名 字", "含「全角」标点，和：冒号",
	"emoji 🚀", "🚀", "👨👩👧👦", "𝄞", "\U0010FFFF", "ä", "Ünïcödé",
	"混合 mixed 中英 🚀: #x",
	"\x00", "a\x00b", "\x7f",
}

// TestStrRoundTrips 是**本包最重要的一条测试**：它拿一个真的 YAML 实现当裁判，
// 逐条确认 `Str` 发出去的每一个值都能原样解回来。
//
// 用 `map[string]string` 而不是 `map[string]any` 是有意的：解成 string 意味着
// 若某个值被 YAML 读成了 bool/int，这里的 `got["K"]` 与 `s` 就不是同一个类型能比的 ——
// 它会以"值走样"的形式炸出来，而不是被 `fmt.Sprint` 悄悄抹平成一个看着差不多的字符串。
func TestStrRoundTrips(t *testing.T) {
	for _, s := range hostile {
		var d doc
		d.Str("K", s)

		var got map[string]string
		if err := yaml.Unmarshal(d.Bytes(), &got); err != nil {
			t.Errorf("yaml.v3 解不开我们发出的 YAML（值 %q）：%v\n发出的原文是 %s", s, err, d.Bytes())
			continue
		}
		if got["K"] != s {
			t.Errorf("值走样：发出 %q，解回来是 %q", s, got["K"])
		}
	}
}

// TestPlainRoundTrips 对 `Plain` 做同样的事。
//
// 它**不保证**每个值都能裸写（`Plain` 的语义是"这是我自己写的常量，别引"），
// 但要保证：凡是 `Plain` 会裸写出去的值，YAML 都解得回来、而且解回**字符串**。
// 这条能挡住"某个常量其实是数字/Bool"这类错误 —— 比如 `AutoUpdateMode: None`
// 里万一哪天写了个 `None` 之外的取值。
func TestPlainRoundTrips(t *testing.T) {
	values := append([]string{}, FdroidCategories...)
	values = append(values, "None", "market-of-labs", "https://github.com/a/b")

	for _, s := range values {
		var d doc
		if !plainSafe(s) {
			continue // Plain 会退化成引号，那条路已由 TestStrRoundTrips 覆盖
		}
		d.Plain("K", s)

		var got map[string]string
		if err := yaml.Unmarshal(d.Bytes(), &got); err != nil {
			t.Errorf("yaml.v3 解不开我们裸写的 %q：%v（原文 %s）", s, err, d.Bytes())
			continue
		}
		if got["K"] != s {
			t.Errorf("裸写走样：发出 %q，解回来是 %q", s, got["K"])
		}
	}
}

// TestListRoundTrips 钉住 `List` 的两条路径。
func TestListRoundTrips(t *testing.T) {
	t.Run("空列表写成 []，不是留一行 null", func(t *testing.T) {
		var d doc
		d.List("K", nil)
		if got, want := string(d.Bytes()), "K: []\n"; got != want {
			t.Fatalf("空的 List 应当写 %q，实得 %q", want, got)
		}
		// 解成 []string 再看是不是 nil —— `K:` 那种写法会解成 nil，两者在
		// "有没有这个列表"上语义完全不同（null vs 空列表）。
		var got map[string][]string
		if err := yaml.Unmarshal(d.Bytes(), &got); err != nil {
			t.Fatalf("解不开：%v", err)
		}
		if got["K"] == nil {
			t.Errorf("`K: []` 被解成了 null —— 那它就与 `K:` 没有区别了")
		}
	})

	t.Run("非空列表逐项还原", func(t *testing.T) {
		want := []string{"System", "Office", "Phone & SMS", "工具", "a: b", " x "}
		var d doc
		d.List("K", want)

		var got map[string][]string
		if err := yaml.Unmarshal(d.Bytes(), &got); err != nil {
			t.Fatalf("解不开：%v\n原文 %s", err, d.Bytes())
		}
		if len(got["K"]) != len(want) {
			t.Fatalf("项数不对：想 %d 项，实得 %d 项（%q）", len(want), len(got["K"]), got["K"])
		}
		for i := range want {
			if got["K"][i] != want[i] {
				t.Errorf("第 %d 项走样：想 %q，实得 %q", i, want[i], got["K"][i])
			}
		}
	})

	t.Run("Items 不是被写在同一行", func(t *testing.T) {
		// `K: [a, b]` 也能被解析，但 diff 里永远是一整行 —— 而这个仓库的
		// 产物是要人肉 review 的，逐行才是它该有的样子。
		var d doc
		d.List("K", []string{"a", "b"})
		if got, want := string(d.Bytes()), "K:\n  - a\n  - b\n"; got != want {
			t.Errorf("想 %q，实得 %q", want, got)
		}
	})
}

// TestOutputIsPureASCII 钉住"**输出恒为纯 ASCII**"这条设计决定。
//
// metadata.go 的包注释里写了为什么要转义非 ASCII（fdroidserver 用 Python 读这些文件，
// 而 Python 的 `open()` 默认走 locale 编码；`debian:trixie` 容器里没有 `LANG`，
// 那就是 ASCII —— 一个中文 `Name` 要么抛解码错、要么被当 latin-1 读成乱码）。
//
// 这条测试的价值在于它是**穷举**的：只要有一个字符漏过了 `quote` 的转义表，
// 它就会在这里出现。
func TestOutputIsPureASCII(t *testing.T) {
	var d doc
	d.Str("Name", "记事本 🚀")
	d.Str("Summary", "中文简介")
	d.List("Categories", []string{"工具", "Phone & SMS"})
	d.Plain("AutoUpdateMode", "None")

	out := d.Bytes()
	for i, b := range out {
		if b >= 0x80 {
			t.Fatalf("第 %d 字节是 0x%02X —— 输出里出现了非 ASCII：\n%s", i, b, out)
		}
	}
}

// TestQuoteEscapesControlChars 钉住控制字符走的是 `\uXXXX` 而不是裸字节。
//
// 裸的控制字符（尤其是 NUL 与 ESC）在 YAML 里是**非法**的，而它们恰恰是
// 复制粘贴事故里最容易夹带进来的东西。
func TestQuoteEscapesControlChars(t *testing.T) {
	for _, s := range []string{"\x00", "\x1b", "\x07", "\x7f", "a\x00b"} {
		var d doc
		d.Str("K", s)
		if got := string(d.Bytes()); strings.ContainsAny(got, "\x00\x1b\x07\x7f") {
			t.Errorf("值 %q 里的控制字符被裸写出来了：%q", s, got)
		}
		var back map[string]string
		if err := yaml.Unmarshal(d.Bytes(), &back); err != nil {
			t.Errorf("值 %q 发出了解得不开的 YAML：%v", s, err)
			continue
		}
		if back["K"] != s {
			t.Errorf("值 %q 走样成 %q", s, back["K"])
		}
	}
}

// TestQuoteEscapesNonBMP 钉住星平面字符走 `\UXXXXXXXX` 而不是 UTF-16 代理对。
//
// YAML 里没有代理对这东西（那是 JSON/UTF-16 的包袱），写成 `\uD83D\uDE80`
// 会被解析成两个**孤立代理**，而不是 🚀。
func TestQuoteEscapesNonBMP(t *testing.T) {
	for _, s := range []string{"🚀", "𝄞", "\U0010FFFF"} {
		var d doc
		d.Str("K", s)
		if got := string(d.Bytes()); strings.Contains(strings.ToUpper(got), "\\UD8") {
			t.Errorf("值 %q 被写成了代理对（%s）—— YAML 不认这个", s, got)
		}
		var back map[string]string
		if err := yaml.Unmarshal(d.Bytes(), &back); err != nil {
			t.Errorf("值 %q 发出了解得不开的 YAML：%v", s, err)
			continue
		}
		if back["K"] != s {
			t.Errorf("值 %q 走样成 %q", s, back["K"])
		}
	}
}

// TestLeadingIndicatorsAreNeverPlain 是**本包唯一一个由真实缺陷催生的测试**。
//
// 起因：`plainSafe` 的字符集里必须放行 `&`（§2.6 的 `Phone & SMS` 要裸写），
// 但 `&` 出现在**节点开头**时是 YAML 的 anchor 指示符。于是
//
//	K: &a        →  值变成 null（anchor 定义），进 map[string]string 就是 ""
//	K: &a b      →  值变成 "b"，`&a` 被静默吃掉
//	K: -         →  解析直接失败（残缺的块序列）
//
// 三条都不报错、或者报在离现场很远的地方 —— 第一条尤其恶劣：**数据没了，日志干净**。
//
// ⚠️ 但下面这个测试**不是**那个缺陷的完整补丁 —— `TestPlainSafeAgreesWithTheParser`
// 才是。把 `&a`/`-` 这几个字符串硬编码进来，只是在补"我当时想到的那几个"；
// 而那条属性测试问的是"**凡是 `plainSafe` 说安全的，解析器是不是也同意**"，
// 它不需要我预先知道缺口在哪。
func TestLeadingIndicatorsAreNeverPlain(t *testing.T) {
	for _, s := range []string{"&", "&a", "&&", "&a b", "-", "--", "-a", "- a"} {
		if plainSafe(s) {
			t.Errorf("plainSafe(%q) = true，而它会以指示符开头 —— 裸写出去就是静默损坏", s)
		}
	}
}

// TestPlainSafeAgreesWithTheParser 把"安全"这个判断**交给解析器定义**。
//
// 这条测试替掉了穷举缺口的老办法。原先的写法是列一张"危险字符串"表 ——
// 那种测试只能验证我已经想到的东西，而 `&a` 这个坑恰恰是**我没想到**的
// （它长得太像 `Phone & SMS` 了）。现在换成一条不变式：
//
//	凡 plainSafe(s) 为真 ⇒ `K: <s>` 必须解析回恰好 s
//
// 这样"我想漏了什么"就不再重要了：只要 `hostile` 里有那个形状，
// 判定式与解析器不一致就会当场炸出来，而方向也明确 ——
// **永远是判定式太宽松**（太严格只会多引一对引号，不影响正确性）。
func TestPlainSafeAgreesWithTheParser(t *testing.T) {
	checked := 0
	for _, s := range hostile {
		if !plainSafe(s) {
			continue // 会被引起来，那条路已由 TestStrRoundTrips 覆盖
		}
		checked++

		var d doc
		d.Plain("K", s)
		var got map[string]string
		if err := yaml.Unmarshal(d.Bytes(), &got); err != nil {
			t.Errorf("plainSafe(%q)=true，但它裸写出去解析失败：%v", s, err)
			continue
		}
		if got["K"] != s {
			t.Errorf("plainSafe(%q)=true，但裸写出去值走样成 %q —— 判定式太宽松了", s, got["K"])
		}
	}
	if checked == 0 {
		t.Fatal("一条都没检查到 —— hostile 里的值全都不是 plainSafe，这条测试失去了意义")
	}
}

// TestPlainSafe 直接钉住那个判定式本身。它是 `Plain` 与 `List` 的分流开关，
// 判错的方向有两个，且都静默。
func TestPlainSafe(t *testing.T) {
	safe := []string{"None", "System", "Phone & SMS", "market-of-labs", "a.b+c_d", "a & b", "x &y"}
	unsafe := []string{
		"", " x", "x ",
		"true", "True", "FALSE", "yes", "no", "on", "off", "y", "n", "null", "~",
		"0", "1", "-1", "1.5", "1e3", "0x1f", "007",
		"2002-12-14",
		"a: b", "a#b", "#a", "*a", "!x", "|x", ">x", "%x", "@x", "`x",
		"{a}", "[a]", "a,b", "a?b", "a'b", `"a"`,
		"中文", "🚀",
		// 首字节是指示符：见 TestLeadingIndicatorsAreNeverPlain。
		"-a", "-", "&a", "&", "&&", "&a b",
		// 纯数字会被读成 int（`20002` 就是这么被挡下的）。
		"20002",
		// 冒号与斜杠过不了字符集，所以 URL 一律被引起来 —— 无害，只是别指望它裸写。
		"https://github.com/a/b",
	}
	for _, s := range safe {
		if !plainSafe(s) {
			t.Errorf("plainSafe(%q) = false，它应当是安全的（会被多余地引起来）", s)
		}
	}
	for _, s := range unsafe {
		if plainSafe(s) {
			t.Errorf("plainSafe(%q) = true，它**不**安全（会被裸写出去）", s)
		}
	}
}

// TestPlainSafeIsConservative 说明一个刻意的方向：这个判定式宁可**过度引用**，
// 也不放过。所以 `&`/`*` 这类只在**开头**才特殊的字符，出现在中间时我们允许裸写，
// 但只要它出现在首位就一律引用 —— 多一对引号没有任何代价，少一对有。
func TestPlainSafeIsConservative(t *testing.T) {
	// 中间出现是安全的：YAML 的指示符只在节点开头有效。
	if !plainSafe("Phone & SMS") {
		t.Errorf("`Phone & SMS` 应当可裸写 —— 它是 §2.6 枚举里的真值")
	}
	// 但同一批字符出现在开头就不赌了。
	for _, s := range []string{"&a", "*a", "!a"} {
		if plainSafe(s) {
			t.Errorf("%q 以指示符开头却判成了安全", s)
		}
	}
}

// TestLooksLikeDate 单独钉住日期形状。它是最容易被漏掉的一类：
// `2002-12-14` 在 YAML 1.1 里是一个 **timestamp**，不是字符串。
func TestLooksLikeDate(t *testing.T) {
	dates := []string{"2002-12-14", "0000-00-00", "9999-99-99"}
	notDates := []string{"", "2002-12-1", "02-12-14", "2002/12/14", "20021214", "abcd-ef-gh", "2002-12-14T00:00:00Z"}
	for _, s := range dates {
		if !looksLikeDate(s) {
			t.Errorf("looksLikeDate(%q) = false", s)
		}
	}
	for _, s := range notDates {
		if looksLikeDate(s) {
			t.Errorf("looksLikeDate(%q) = true", s)
		}
	}
}

// TestKeyPanicsOnUnsafeKey 守住那个 panic 分支。
//
// key 只可能是本包的字面量，所以这个 panic 在正常运行里永不触发 —— 正因如此，
// 它是那种"哪天有人写了个带空格的 key 才发现"的守卫。这里主动踩一次，
// 确认它真的会响，而不是安静地发出一个坏文件。
//
// ⚠️ 注意哪些 key **不会**触发它：`a b`（内部空格）与 `-x` 都是合法的 plain scalar，
// 所以 `plainSafe` 判真、不 panic。这不是漏网 —— `key` 的职责是挡住会**改变语法**的
// 字符，不是把所有非标识符都拒掉。列在这里是为了把这条边界写下来，
// 免得哪天有人看到 `a b` 通过了就以为守卫坏了。
func TestKeyPanicsOnUnsafeKey(t *testing.T) {
	for _, k := range []string{"", "a: b", "中文", "a\tb", "#a", "x ", "true", "0"} {
		t.Run(k, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("key %q 应当让 doc.key panic", k)
				}
			}()
			var d doc
			d.Plain(k, "v")
		})
	}
}
