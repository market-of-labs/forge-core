package fdroid

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	testRepoURL  = "https://example.invalid/fdroid/repo"
	testKS       = "/tmp/keys/repo-signing.jks"
	testStoreKey = "correct horse battery staple"
	testKeyPass  = "hunter2"
)

// TestRenderConfigGolden 是 config.yml 的**逐字节**契约。
//
// 用 golden 而不是逐键断言，是因为这个文件有两条互相独立的性质要同时满足：
// ① 键名对（fdroidserver 只认它自己那套键名）；② 顺序稳定（它在 CI 里被生成，
// 一个抖动的文件会让每轮都产生无意义的 diff，而它**又不该进 git**，
// 所以 diff 一次都不会被人看见，只会让"产物变没变"这个问题没法回答）。
//
// ⚠️ `repo_description` 那一行的值被**抹成了占位符**再比 —— 这不是偷懒。
//
// 它的真值是一串 `\uXXXX`（因为 RepoDescription 是中文，而输出恒为纯 ASCII，
// 见上），把那串转义逐字抄进这份 golden 只会得到一条"抄错了就红、
// 而红的时候你还得对着两份转义找出第几个字节不同"的测试 —— 它验证的是我抄得准不准，
// 不是代码对不对。
//
// 它的内容由 `TestRenderConfigRoundTrips` 用真解析器做**语义**比对
// （`m["repo_description"] == RepoDescription`），那比逐字节强：
// 逐字节只证明"我抄的和它发的一样"，语义比对证明"fdroidserver 读到的就是那句话"。
// 而"这一行确实是转义的"由 `TestRenderConfigIsPureASCII` 覆盖。
//
// 于是这条 golden 只管它最擅长的事：**键名与顺序**。
func TestRenderConfigGolden(t *testing.T) {
	b, err := RenderConfig(testRepoURL, testKS, testStoreKey, testKeyPass)
	if err != nil {
		t.Fatalf("RenderConfig 报错：%v", err)
	}

	want := `repo_url: "https://example.invalid/fdroid/repo"
repo_name: "market-of-labs"
repo_description: <转义后的中文>
keystore: "/tmp/keys/repo-signing.jks"
repo_keyalias: "market-of-labs"
keystorepass: "correct horse battery staple"
keypass: "hunter2"
keydname: "CN=market-of-labs, OU=fdroid repo, O=market-of-labs, L=NA, ST=NA, C=CN"
`

	// 抹值：只认前缀，值的部分整段丢掉 —— 连"里面有没有引号"都不在这里管。
	got := string(b)
	var kept []string
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if strings.HasPrefix(line, "repo_description: ") {
			line = "repo_description: <转义后的中文>"
		}
		kept = append(kept, line)
	}

	if strings.Join(kept, "\n")+"\n" != want {
		t.Errorf("config.yml 与契约不符（描述行的值已抹平，这里只管键名与顺序）。\n"+
			"想：\n%s\n实得：\n%s", want, strings.Join(kept, "\n")+"\n")
	}
}

// TestRenderConfigRoundTrips 用真解析器确认那 8 个键的类型都是**字符串**。
//
// 这条不是走形式：`doc.Str` 一律加引号正是为了这里。fdroidserver 拿这些值去开 keystore、
// 去拼地址，一个被读成 int 的口令会让它报一个与"口令不对"毫无关系的错。
func TestRenderConfigRoundTrips(t *testing.T) {
	b, err := RenderConfig(testRepoURL, testKS, testStoreKey, testKeyPass)
	if err != nil {
		t.Fatalf("RenderConfig 报错：%v", err)
	}

	var m map[string]string // ← 解成 string：若哪个值被读成了 int，这里会直接报类型错
	if err := yaml.Unmarshal(b, &m); err != nil {
		t.Fatalf("yaml.v3 解不开 config.yml：%v\n原文：\n%s", err, b)
	}

	for k, want := range map[string]string{
		"repo_url":         testRepoURL,
		"repo_name":        RepoName,
		"repo_description": RepoDescription,
		"keystore":         testKS,
		"repo_keyalias":    KeyAlias,
		"keystorepass":     testStoreKey,
		"keypass":          testKeyPass,
		"keydname":         KeyDName,
	} {
		if got := m[k]; got != want {
			t.Errorf("%s：想 %q，实得 %q", k, want, got)
		}
	}

	// 反向：不该有我们没打算写的键（比如某个 fdroidserver 会当真的开关）。
	if len(m) != 8 {
		t.Errorf("config.yml 有 %d 个键，应当是 8 个：%v", len(m), m)
	}
}

// TestRenderConfigKeepsNumericPasswordAString 针对一个**具体的**坏法。
//
// 一个全数字的口令（或者 32 位十六进制口令里恰好全数字的那个万一）在 YAML 里裸写
// 就会被读成整数。这条测试把它钉死：口令走的是 `doc.Str`，永远带引号。
//
// 为什么值得单独一条：真发生时，报错会指向"keystore 打不开"，而**没有人**
// 会想到去看 YAML 的类型推断。现场离原因太远。
func TestRenderConfigKeepsNumericPasswordAString(t *testing.T) {
	for _, pass := range []string{"123456", "0", "007", "1e3", "0x1f"} {
		b, err := RenderConfig(testRepoURL, testKS, pass, pass)
		if err != nil {
			t.Fatalf("RenderConfig 报错：%v", err)
		}

		var m map[string]any
		if err := yaml.Unmarshal(b, &m); err != nil {
			t.Fatalf("解不开：%v", err)
		}
		for _, k := range []string{"keystorepass", "keypass"} {
			if _, isString := m[k].(string); !isString {
				t.Errorf("口令 %q 的 %s 被读成了 %T —— 它必须是字符串", pass, k, m[k])
			}
			if m[k] != pass {
				t.Errorf("口令 %q 走样成 %v", pass, m[k])
			}
		}
	}
}

// TestRenderConfigRejectsEmpty 覆盖四个必填项。
//
// 空值的后果是**在下游**才炸的：fdroidserver 会拿一个空口令去开 keystore，
// 报出来的错指向"口令错误"。在这里拦，错误就指向真正的原因。
func TestRenderConfigRejectsEmpty(t *testing.T) {
	cases := []struct {
		name                            string
		repoURL, ks, storePass, keyPass string
	}{
		{"repoUrl 为空", "", testKS, testStoreKey, testKeyPass},
		{"keystorePath 为空", testRepoURL, "", testStoreKey, testKeyPass},
		{"storePass 为空", testRepoURL, testKS, "", testKeyPass},
		{"keyPass 为空", testRepoURL, testKS, testStoreKey, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if b, err := RenderConfig(c.repoURL, c.ks, c.storePass, c.keyPass); err == nil {
				t.Fatalf("这条输入应当被拒绝，却渲染出了：\n%s", b)
			}
		})
	}
}

// TestRenderConfigIsPureASCII 与 metadata 的那条同理 —— 但这里的赌注更大：
// 这个文件里躺着**明文口令**，而解码出错时的常见处理是把它打进日志。
func TestRenderConfigIsPureASCII(t *testing.T) {
	b, err := RenderConfig(testRepoURL, testKS, testStoreKey, testKeyPass)
	if err != nil {
		t.Fatalf("RenderConfig 报错：%v", err)
	}
	for i, c := range b {
		if c >= 0x80 {
			t.Fatalf("第 %d 字节是 0x%02X —— config.yml 里出现了非 ASCII", i, c)
		}
	}
	// 中文在 RepoDescription 里，所以上面这条只有在转义生效时才过得去 ——
	// 顺带确认它真的被写进去了（否则这条测试会在一个空描述上白白通过）。
	if !strings.Contains(string(b), `\u`) {
		t.Error("config.yml 里一个 \\u 转义都没有 —— RepoDescription 是中文，应当被转义")
	}
}

// TestRenderConfigKeepsPasswordsOffSeparateLines 是个很朴素的守卫，但值得有：
//
// 口令一旦出现在**自己单独的一行**（比如被人"顺手"重排成 `passwords:` 块），
// `cat` 一下或者一条 `grep -v` 就更容易把它漏进日志。这里钉住"它们始终是某个键的值"。
func TestRenderConfigKeepsPasswordsOffSeparateLines(t *testing.T) {
	b, err := RenderConfig(testRepoURL, testKS, testStoreKey, testKeyPass)
	if err != nil {
		t.Fatalf("RenderConfig 报错：%v", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !strings.Contains(line, ":") {
			t.Errorf("这一行没有键名，孤零零地躺着一个值：%q", line)
		}
	}
}
