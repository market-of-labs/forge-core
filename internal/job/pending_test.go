package job

import (
	"regexp"
	"strings"
	"testing"

	"github.com/market-of-labs/forge-core/internal/apkmeta"
	"github.com/market-of-labs/forge-core/internal/gh"
	"github.com/market-of-labs/forge-core/internal/model"
	"github.com/market-of-labs/forge-core/internal/upstream"
)

// 这一组钉的是待办扫描的**决策**部分：从哪个 Release 读身份、读出来算不算数。
// 它们都不发请求、也不下载 —— readMetas 被换成了一个返回手工 Meta 的桩，
// 这正是 pickIdentityRelease 把那一层抽出来的原因（真 APK 有 8 MB，不能进仓库）。

// halfSource 造一份"身份还没定"的半成品 Source，形状与 decideAdd 产出的完全一致。
func halfSource(repo string) *model.Source {
	return &model.Source{
		Source: model.SourceGitHub,
		Upstream: &model.Upstream{
			Type: model.UpstreamGitHubRelease,
			Repo: repo,
		},
		Categories:   []string{"工具"},
		ABIWhitelist: []string{"arm64-v8a"},
	}
}

// release 造一个候选 Release。assets 只给名字就够了 —— 不匹配的会被 Match 跳过。
func release(tag string, assetNames ...string) gh.Release {
	as := make([]gh.Asset, 0, len(assetNames))
	for i, n := range assetNames {
		as = append(as, gh.Asset{ID: int64(i + 1), Name: n})
	}
	return gh.Release{TagName: tag, Name: tag, Assets: as}
}

// metasFor 造一个"每个 asset 名对应什么元数据"的桩。
func metasFor(m map[string]*apkmeta.Meta) func(gh.Release, []gh.Asset) []*apkmeta.Meta {
	return func(_ gh.Release, picked []gh.Asset) []*apkmeta.Meta {
		var out []*apkmeta.Meta
		for _, a := range picked {
			if m[a.Name] != nil {
				out = append(out, m[a.Name])
			}
		}
		return out
	}
}

func mustCompile(t *testing.T, pattern string) *regexp.Regexp {
	t.Helper()
	re, err := upstream.CompilePattern(pattern)
	if err != nil {
		t.Fatalf("编译 %q：%v", pattern, err)
	}
	return re
}

// 上游最新那个 Release 只放了源码包是常见情况（也很常见地只放了 .zip）。
// 走不到就往下退一层 —— 退到哪一层必须停，是这一组测试的主要内容。
func TestPickIdentityRelease_WalksPastReleasesWithoutAPKs(t *testing.T) {
	pattern := upstream.DefaultAssetPattern
	cands := []gh.Release{
		release("v3.0", "source.tar.gz"), // 一个 apk 都没有
		release("v2.0", "app-v2.0.zip"),  // 有资产，但不是 apk
		release("v1.0", "app-v1.0.apk"),  // ← 应该在这里停下
		release("v0.9", "app-v0.9.apk"),
	}
	p, re := pattern, mustCompile(t, pattern)

	got, tag, err := pickIdentityRelease(halfSource("example/app"), p, re, cands,
		metasFor(map[string]*apkmeta.Meta{
			"app-v1.0.apk": {Package: "com.example.app", Label: "Example"},
		}))
	if err != nil {
		t.Fatalf("应当成功：%v", err)
	}
	if tag != "v1.0" {
		t.Fatalf("应当在 v1.0 停下（那是第一个能读出包名的），得到 %q", tag)
	}
	if got.ID != "com.example.app" || got.Name != "Example" {
		t.Fatalf("身份没定对：%+v", got)
	}
}

// 资产匹配上了、但一个都读不出包名（下载失败、或根本不是 APK）。
// 这种情况必须继续往下退，而不是就地失败。
func TestPickIdentityRelease_SkipsReleasesWhoseAssetsAreUnreadable(t *testing.T) {
	pattern := upstream.DefaultAssetPattern
	cands := []gh.Release{
		release("v2.0", "app-v2.0.apk"),
		release("v1.0", "app-v1.0.apk"),
	}
	p, re := pattern, mustCompile(t, pattern)

	// 桩里**没有** v2.0 的条目 = probeMetas 全军覆没。
	got, tag, err := pickIdentityRelease(halfSource("example/app"), p, re, cands,
		metasFor(map[string]*apkmeta.Meta{
			"app-v1.0.apk": {Package: "com.example.app", Label: "Example"},
		}))
	if err != nil {
		t.Fatalf("应当退到 v1.0 并成功：%v", err)
	}
	if tag != "v1.0" || got.ID != "com.example.app" {
		t.Fatalf("tag=%q id=%q", tag, got.ID)
	}
}

// 一个都走不出来时，错误必须**点名上游与正则** —— 申请人要照着回评改正文，
// 而他能改的只有这两样。
func TestPickIdentityRelease_NothingUsable(t *testing.T) {
	pattern := `(?i)release\.apk$`
	cands := []gh.Release{release("v1.0", "app-v1.0.apk")}
	p, re := pattern, mustCompile(t, pattern)

	_, _, err := pickIdentityRelease(halfSource("example/app"), p, re, cands,
		metasFor(nil))
	if err == nil {
		t.Fatal("一条候选都用不上时该报错")
	}
	// 错误里要出现仓库名与正则，且要告诉人怎么补 —— 这三点缺一，回评就成了"失败了"。
	for _, want := range []string{"example/app", `release\.apk$`, "编辑本单正文"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误里找不到 %q：%v", want, err)
		}
	}
}

// 没有任何可镜像的 Release（全是 draft/prerelease 或一个都没有）由 probeIdentity
// 判掉，pickIdentityRelease 拿到空列表时只要不 panic、并且报出"没有可用的"。
func TestPickIdentityRelease_EmptyCandidateList(t *testing.T) {
	pattern := upstream.DefaultAssetPattern
	p, re := pattern, mustCompile(t, pattern)
	if _, _, err := pickIdentityRelease(halfSource("example/app"), p, re, nil, metasFor(nil)); err == nil {
		t.Fatal("空候选列表该报错")
	}
}

// ---- buildIdentity：身份三件套 ------------------------------------------------

func TestBuildIdentity_ReadsPackageAndLabelFromAPK(t *testing.T) {
	half := halfSource("ExampleOrg/ExampleApp")
	src, err := buildIdentity(half, release("v1.0"), []*apkmeta.Meta{
		{Package: "com.example.app", Label: "示例应用", ABIs: []string{"arm64-v8a"}},
	})
	if err != nil {
		t.Fatalf("buildIdentity：%v", err)
	}
	if src.ID != "com.example.app" {
		t.Fatalf("id 必须来自 APK 的 package（02 规则 7）：%q", src.ID)
	}
	if src.Name != "示例应用" {
		t.Fatalf("显示名必须来自 APK 的 label：%q", src.Name)
	}
	// 作者没有别的可派生来源 —— 取仓库 owner，且刻意不做任何"美化"。
	if src.Author != "ExampleOrg" {
		t.Fatalf("作者应当取仓库 owner：%q", src.Author)
	}
	// 申请人勾的那两样必须原样带上：它们不是从 APK 派生的。
	if len(src.Categories) != 1 || src.Categories[0] != "工具" {
		t.Fatalf("分类丢了：%v", src.Categories)
	}
	if len(src.ABIWhitelist) != 1 || src.ABIWhitelist[0] != "arm64-v8a" {
		t.Fatalf("ABI 白名单丢了：%v", src.ABIWhitelist)
	}
	if src.Source != model.SourceGitHub {
		t.Fatalf("source 应当是 %q：%q", model.SourceGitHub, src.Source)
	}
}

// 同一个应用的多个分片（按 ABI 拆）包名相同 —— 它们**不是**多包名。
func TestBuildIdentity_ShardsOfOneAppAreOneApp(t *testing.T) {
	src, err := buildIdentity(halfSource("o/app"), release("v1.0"), []*apkmeta.Meta{
		{Package: "com.example.app", Label: "示例", ABIs: []string{"arm64-v8a"}},
		{Package: "com.example.app", Label: "示例", ABIs: []string{"armeabi-v7a"}},
		{Package: "com.example.app", Label: "示例", ABIs: []string{"x86_64"}},
	})
	if err != nil {
		t.Fatalf("同包名的多分片不该被判成多应用：%v", err)
	}
	if src.ID != "com.example.app" {
		t.Fatalf("id = %q", src.ID)
	}
}

// 读不出 label 时退回仓库 owner，而不是留空。
//
// 空显示名在 Obtainium 里是一行**没有标题的条目**，比"名字不完美"糟得多；
// 而"猜一个名字"更糟 —— 它会被写进 sources/ 当成事实。
func TestBuildIdentity_MissingLabelFallsBackToRepoOwner(t *testing.T) {
	src, err := buildIdentity(halfSource("ExampleOrg/ExampleApp"), release("v1.0"), []*apkmeta.Meta{
		{Package: "com.example.app"}, // 没有 Label
	})
	if err != nil {
		t.Fatalf("buildIdentity：%v", err)
	}
	if src.Name != "ExampleOrg" {
		t.Fatalf("label 缺失时应当退回仓库 owner，得到 %q", src.Name)
	}
	// 只有 label 那条链断掉时才会发生；这正是 HasLabel 的用途。
	if (apkmeta.Meta{Label: "x"}).HasLabel() != true || (apkmeta.Meta{}).HasLabel() != false {
		t.Fatal("HasLabel 的判据错了")
	}
}

// appId 自动派生之后，**多包名是唯一剩下的把关点**。
//
// 不能随便挑一个：挑错了就是静默地收录了一个申请人没想要的应用，
// 而这个错误唯一的表现是设备上多了一行 —— 没人会去核对。
func TestBuildIdentity_RejectsMultiplePackages(t *testing.T) {
	_, err := buildIdentity(halfSource("o/multi"), release("v1.0"), []*apkmeta.Meta{
		{Package: "com.example.two", Label: "Two"},
		{Package: "com.example.one", Label: "One"},
	})
	if err == nil {
		t.Fatal("一个 Release 里出现两个包名时必须拒绝，不能挑一个")
	}
	msg := err.Error()
	// 回评要给人**够改的**信息：两个包名都列出来，并指出唯一的消歧手段是正则。
	for _, want := range []string{"com.example.one", "com.example.two", "2 个不同的包名", "编辑本单正文"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误里找不到 %q：%s", want, msg)
		}
	}
	// 列出来的包名顺序必须稳定，否则同一条申请每次回评的文字都不一样
	// （Go map 遍历是随机的，而回评是**对外**的文字）。
	multi := []*apkmeta.Meta{
		{Package: "com.example.two"}, {Package: "com.example.one"}, {Package: "com.example.zero"},
	}
	_, first := buildIdentity(halfSource("o/multi"), release("v1.0"), multi)
	for i := 0; i < 20; i++ {
		_, e := buildIdentity(halfSource("o/multi"), release("v1.0"), multi)
		if e.Error() != first.Error() {
			t.Fatalf("第 %d 次的消息不一样：\n%s\n%s", i, first, e)
		}
	}
}

// 定出身份时**不能就地改那份半成品** —— 它是调用方传进来的，
// 后面还要拿去写别的东西（现在没有，但共享可变状态是这类函数的经典坑）。
func TestBuildIdentity_DoesNotMutateInput(t *testing.T) {
	half := halfSource("o/app")
	_, err := buildIdentity(half, release("v1.0"), []*apkmeta.Meta{{Package: "com.example.app", Label: "X"}})
	if err != nil {
		t.Fatalf("buildIdentity：%v", err)
	}
	if half.ID != "" || half.Name != "" || half.Author != "" {
		t.Fatalf("入参被改了：%+v", half)
	}
}

func TestRepoOwner(t *testing.T) {
	cases := map[string]string{
		"ImranR98/Obtainium": "ImranR98",
		"justaname":          "justaname", // 形状错的 repo 在校验那一步就被拒了，这里不 panic 即可
		"/leading":           "/leading",  // IndexByte 命中 0，取不出一段 —— 返回原样而不是空串
	}
	for in, want := range cases {
		if got := repoOwner(in); got != want {
			t.Errorf("repoOwner(%q) = %q，期望 %q", in, got, want)
		}
	}
}
