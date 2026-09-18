package fdroid

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/market-of-labs/forge-core/internal/model"
)

// githubSrc 是一份最小但完整的 github 来源。`id` 用真实的包名形状 ——
// 校验里对它的 charset 是有要求的（不得含斜杠、不得是 `_incoming`）。
func githubSrc() *model.Source {
	return &model.Source{
		ID:         "dev.imranr.obtainium",
		Name:       "Obtainium",
		Author:     "Imran R",
		Desc:       "从任意来源安装并更新应用",
		Source:     model.SourceGitHub,
		Categories: []string{"工具", "效率"},
		Upstream: &model.Upstream{
			Type: model.UpstreamGitHubRelease,
			Repo: "ImranR98/Obtainium",
		},
	}
}

// parse 把我们渲染出来的 metadata 交给真的 YAML 解析器。
//
// 每个测试都从这里开始，而不是拿字符串做 `strings.Contains` —— 后者会让
// "引号写错但恰好包含那个子串"这种坏法一路绿灯。这份文件最终是**给 Python 读的**，
// 所以判据必须是"解析器读到了什么"，不是"文本里有没有那几个字"。
func parse(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		t.Fatalf("yaml.v3 解不开 Render 的输出：%v\n渲染结果是：\n%s", err, b)
	}
	return m
}

// TestRenderGitHubSourceRoundTrips 逐字段比对 §2.5 的映射表。
func TestRenderGitHubSourceRoundTrips(t *testing.T) {
	b, err := Render(githubSrc())
	if err != nil {
		t.Fatalf("Render 报错：%v", err)
	}
	m := parse(t, b)

	if got := m["Name"]; got != "Obtainium" {
		t.Errorf("Name：想 %q，实得 %v", "Obtainium", got)
	}
	if got := m["Summary"]; got != "从任意来源安装并更新应用" {
		t.Errorf("Summary：实得 %v", got)
	}
	if got := m["AuthorName"]; got != "Imran R" {
		t.Errorf("AuthorName：实得 %v", got)
	}
	if got := m["SourceCode"]; got != "https://github.com/ImranR98/Obtainium" {
		t.Errorf("SourceCode：实得 %v", got)
	}
	if got := m["IssueTracker"]; got != "https://github.com/ImranR98/Obtainium/issues" {
		t.Errorf("IssueTracker：实得 %v", got)
	}

	cats, ok := m["Categories"].([]any)
	if !ok {
		t.Fatalf("Categories 应当是一个序列，实得 %T（%v）", m["Categories"], m["Categories"])
	}
	if len(cats) != 2 || cats[0] != "System" || cats[1] != "Office" {
		t.Errorf("Categories：想 [System Office]，实得 %v", cats)
	}
}

// TestRenderPinsUpdateModesToNone 钉住那两行**契约**。
//
// 它们不是配置，是"把 fdroidserver 从自动追更新降级成纯索引生成器"的开关（03 §5.1）。
// 删掉它们不会有任何测试红、不会有任何报错，只会让 fdroidserver 开始去上游翻版本 ——
// 而那是一条我们完全控制不了的路径。所以这条测试是本文件里最该存在的那个。
func TestRenderPinsUpdateModesToNone(t *testing.T) {
	for _, src := range []*model.Source{githubSrc(), manualSrc()} {
		b, err := Render(src)
		if err != nil {
			t.Fatalf("Render(%s) 报错：%v", src.ID, err)
		}
		m := parse(t, b)
		for _, k := range []string{"AutoUpdateMode", "UpdateCheckMode"} {
			if got := m[k]; got != "None" {
				t.Errorf("来源 %s 的 %s = %v，必须是字符串 None —— "+
					"少了它 fdroidserver 会自己去上游找更新", src.ID, k, got)
			}
		}
	}
}

// TestRenderKeepsChineseExactly 是**转义决定**的端到端证明。
//
// 它跑通了整条链：Go 里的中文 → 我们的 quote（转成 \uXXXX）→
// 字节流（纯 ASCII）→ yaml.v3 解析 → 中文。任何一环漏了，这里都会走样。
//
// 为什么这条值得单独一个测试：本机**没有 fdroidserver**，所以"中文 metadata 到底能不能
// 被读回来"唯一的本地证据就是它。而真出事的环境（Python 在无 LANG 的容器里
// 按 ASCII 解码）我们连复现都复现不了。
func TestRenderKeepsChineseExactly(t *testing.T) {
	const name = "记事本 🚀"
	const desc = "含「全角」标点，和：冒号 # 井号"
	const author = "张三"

	src := githubSrc()
	src.Name, src.Desc, src.Author = name, desc, author
	src.Categories = []string{"媒体"}

	b, err := Render(src)
	if err != nil {
		t.Fatalf("Render 报错：%v", err)
	}

	// 先确认输出真的是纯 ASCII —— 这是转义那一步的成果，不是副产品。
	for i, c := range b {
		if c >= 0x80 {
			t.Fatalf("第 %d 字节是 0x%02X —— 输出里出现了非 ASCII：\n%s", i, c, b)
		}
	}

	m := parse(t, b)
	if m["Name"] != name {
		t.Errorf("Name 走样：想 %q，实得 %v", name, m["Name"])
	}
	if m["Summary"] != desc {
		t.Errorf("Summary 走样：想 %q，实得 %v", desc, m["Summary"])
	}
	if m["AuthorName"] != author {
		t.Errorf("AuthorName 走样：想 %q，实得 %v", author, m["AuthorName"])
	}
}

// TestRenderOmitsSummaryWhenDescEmpty 钉住"空就不写这个键"。
//
// 写一个空 `Summary: ""` 也能解析，但会让 §2.8 那条"summary 缺失"的软告警**永远不响** ——
// 它再也分不出"没写"和"写了但空"。
func TestRenderOmitsSummaryWhenDescEmpty(t *testing.T) {
	src := githubSrc()
	src.Desc = ""

	b, err := Render(src)
	if err != nil {
		t.Fatalf("Render 报错：%v", err)
	}
	if _, present := parse(t, b)["Summary"]; present {
		t.Errorf("desc 为空时不应当写出 Summary 键，实得：\n%s", b)
	}
}

// TestRenderOmitsCategoriesWhenNothingMaps 覆盖"分类全被丢掉"这条路。
//
// `其他` 是刻意不映射的，所以只带它的来源**不该有 Categories 键**（而不是一个空序列）。
func TestRenderOmitsCategoriesWhenNothingMaps(t *testing.T) {
	src := githubSrc()
	src.Categories = []string{"其他"}

	b, err := Render(src)
	if err != nil {
		t.Fatalf("Render 报错：%v", err)
	}
	if _, present := parse(t, b)["Categories"]; present {
		t.Errorf("分类全被丢掉时不应当写出 Categories 键，实得：\n%s", b)
	}
}

// manualSrc 是一份 manual 来源 —— 它走 `_incoming` 上传，没有上游仓库。
func manualSrc() *model.Source {
	return &model.Source{
		ID:     "com.example.manual",
		Name:   "手动来源",
		Author: model.AuthorUnknown,
		Source: model.SourceManual,
	}
}

// TestRenderManualSourceHasNoUpstreamFields 钉住"不给 manual 来源编一个上游地址"。
//
// `SourceCode`/`IssueTracker` 是**指向别人的**声明。manual 来源根本没有那两样东西，
// 编一个出来（比如拼 `https://github.com/` + 空 → 一个指向 GitHub 首页的链接）
// 是把客户端引到一个与这个应用无关的页面上。
func TestRenderManualSourceHasNoUpstreamFields(t *testing.T) {
	b, err := Render(manualSrc())
	if err != nil {
		t.Fatalf("Render 报错：%v", err)
	}
	m := parse(t, b)
	for _, k := range []string{"SourceCode", "IssueTracker"} {
		if v, present := m[k]; present {
			t.Errorf("manual 来源不应当有 %s，实得 %v", k, v)
		}
	}
}

// TestRenderNeverEmitsPaused 钉住"`paused` 不产生任何字段"。
//
// paused 的语义是"要不要渲染这个文件"，由调用方决定（§2.5）—— 把它写进 metadata
// 会给 fdroidserver 一个它不认识的键（它只警告），而真正的效果是零：
// 应用照样出现在索引里。要停就得**不写这个文件**。
func TestRenderNeverEmitsPaused(t *testing.T) {
	src := githubSrc()
	src.Paused = true

	b, err := Render(src)
	if err != nil {
		t.Fatalf("Render 报错：%v", err)
	}
	if _, present := parse(t, b)["Paused"]; present {
		t.Errorf("paused 不应当产生任何字段，实得：\n%s", b)
	}
}

// TestRenderNeverEmitsLicenseOrBuild 钉住那几条**刻意不写**的字段。
//
// 它们每一条都有理由（见 Render 的注释），但理由写在注释里是拦不住人的 ——
// "顺手补个 License" 看起来太像是在做好事了。这条测试让那个动作需要一次有意的删除。
func TestRenderNeverEmitsLicenseOrBuild(t *testing.T) {
	b, err := Render(githubSrc())
	if err != nil {
		t.Fatalf("Render 报错：%v", err)
	}
	m := parse(t, b)
	for _, k := range []string{"License", "RepoType", "Repo", "Builds", "AutoName", "CurrentVersion"} {
		if v, present := m[k]; present {
			t.Errorf("不应当写出 %s（实得 %v）—— 详见 Render 的注释：我们镜像现成 APK，"+
				"不构建、也没有许可证的来源", k, v)
		}
	}
}

// TestRenderRejectsBadInput 覆盖几类"输入非法就别渲染"。
//
// 尤其是 `_incoming`：它是手动上传的暂存 Release，不是合法 appId（03 §3.1）。
// 为它渲染一份 metadata 会让一个叫 `_incoming` 的应用出现在索引里。
func TestRenderRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*model.Source)
	}{
		{"id 为空", func(s *model.Source) { s.ID = "" }},
		{"id 是保留名 _incoming", func(s *model.Source) { s.ID = model.IncomingTag }},
		{"id 含斜杠", func(s *model.Source) { s.ID = "a/b" }},
		{"name 为空", func(s *model.Source) { s.Name = "" }},
		{"author 为空", func(s *model.Source) { s.Author = "" }},
		{"desc 含换行", func(s *model.Source) { s.Desc = "第一行\n第二行" }},
		{"source 取值不认识", func(s *model.Source) { s.Source = "gitlab" }},
		{"source=github 却没有 upstream", func(s *model.Source) { s.Upstream = nil }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := githubSrc()
			c.mut(src)
			b, err := Render(src)
			if err == nil {
				t.Fatalf("这条输入应当被拒绝，却渲染出了：\n%s", b)
			}
		})
	}

	t.Run("nil 来源", func(t *testing.T) {
		if _, err := Render(nil); err == nil {
			t.Error("nil 来源应当报错，而不是渲染出一份空文件")
		}
	})
}

// TestRenderErrorsMentionTheSourceID 是个**可操作性**要求，不是格式洁癖。
//
// 这条错误最终会出现在 CI 日志里，而一次对账要过十几个来源 ——
// 一句"name 为空"没法告诉人该去改哪个文件。
func TestRenderErrorsMentionTheSourceID(t *testing.T) {
	src := githubSrc()
	src.Name = ""

	_, err := Render(src)
	if err == nil {
		t.Fatal("应当报错")
	}
	if !strings.Contains(err.Error(), src.ID) {
		t.Errorf("错误信息里没有来源 id，改的人不知道该动哪个文件：%v", err)
	}
}

// TestMetadataFileName 钉住"文件名与 id 逐字一致"（§2.5 表格第一行）。
//
// fdroidserver 是**按文件名**把 APK 与元数据对上的，对不上的表现是**静默地少一个应用**。
func TestMetadataFileName(t *testing.T) {
	if got, want := MetadataFileName("dev.imranr.obtainium"), "dev.imranr.obtainium.yml"; got != want {
		t.Errorf("想 %q，实得 %q", want, got)
	}
	// 与 §2.5 的另一个方向也一致：文件名必须是 id + ".yml"，不能是 id 本身。
	if got := MetadataFileName("a.b"); strings.HasSuffix(got, ".json") {
		t.Errorf("metadata 是 yml 不是 json，实得 %q", got)
	}
}

// TestUnknownCategories 覆盖那个给 check-repo 用的出口。
func TestUnknownCategories(t *testing.T) {
	src := githubSrc()
	src.Categories = []string{"工具", "没见过的标签", "其他"}

	got := UnknownCategories(src)
	want := []string{"没见过的标签"}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("想 %q，实得 %q（注意 `其他` 不该出现在这里 —— 它是刻意不映射的）", want, got)
	}
}

// TestRenderIsDeterministic 是个不变式：同样的输入必须逐字节产出同样的输出。
//
// 它挡的是"哪天有人在渲染里用了 map 迭代" —— 那样 CI 每轮都会产生一个无意义的 diff，
// 而"这轮产物变了没有"这个问题的答案就没了。
func TestRenderIsDeterministic(t *testing.T) {
	first, err := Render(githubSrc())
	if err != nil {
		t.Fatalf("Render 报错：%v", err)
	}
	for i := 0; i < 20; i++ {
		again, err := Render(githubSrc())
		if err != nil {
			t.Fatalf("第 %d 次 Render 报错：%v", i, err)
		}
		if string(again) != string(first) {
			t.Fatalf("第 %d 次渲染的结果与第一次不同（map 迭代顺序泄漏进了输出？）\n第一次：\n%s\n这一次：\n%s",
				i, first, again)
		}
	}
}
