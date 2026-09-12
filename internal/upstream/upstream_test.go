package upstream_test

import (
	"strings"
	"testing"

	"github.com/market-of-labs/forge-core/internal/gh"
	"github.com/market-of-labs/forge-core/internal/naming"
	"github.com/market-of-labs/forge-core/internal/upstream"
)

func assets(names ...string) []gh.Asset {
	out := make([]gh.Asset, len(names))
	for i, n := range names {
		out[i] = gh.Asset{ID: int64(i + 1), Name: n, Size: 1000}
	}
	return out
}

func pickedNames(as []gh.Asset) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.Name
	}
	return out
}

// TestNotInstaller 覆盖"什么不该被镜像"。
//
// 这一组错一个的后果是：清单里多出一个**点了装不上**的条目（split 分片），
// 或者一个版本整体被误判为"没有可镜像的包"。两者都不是崩溃，而是静默的坏数据。
func TestNotInstaller(t *testing.T) {
	cases := []struct {
		name    string
		wantBad bool
	}{
		// 可独立安装 —— 必须放行
		{"app-1.0-universal.apk", false},
		{"app-1.0-arm64-v8a.apk", false},
		{"Obtainium-1.2.3.apk", false},
		{"app_release_build.APK", false}, // 大写扩展名也认（大小写不敏感）

		// 不可独立安装
		{"app-1.0.apks", true},
		{"app.xapk", true},
		{"app.apkm", true},
		{"app.aab", true},
		{"main.1.com.foo.obb", true},
		{"release.zip", true},
		{"src.tar.gz", true},
		{"notes.txt", true},
		{"app-1.0-universal.apk.sha256", true}, // 校验文件，落在"不是 .apk"上

		// split 配置分片 —— 扩展名是 .apk，但不独立可装
		{"app-1.0-arm64-v8a-config.zh.apk", true},
		{"app-1.0-config.xxhdpi.apk", true},
		{"base-config.arm64_v8a.apk", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, bad := upstream.NotInstaller(tc.name)
			if bad != tc.wantBad {
				t.Errorf("NotInstaller(%q) = (%q, %v)，期望 bad=%v", tc.name, reason, bad, tc.wantBad)
			}
			// 跳过必须给得出原因 —— 日志里只说"跳过了"等于没跳。
			if bad && reason == "" {
				t.Errorf("NotInstaller(%q) 判为跳过但没给原因", tc.name)
			}
		})
	}
}

// TestMatchKeepsOnlyInstallers 是上面那条的组合版：一批混合 asset 里，
// 只有真正可装的留下，且每个被跳过的都带原因。
func TestMatchKeepsOnlyInstallers(t *testing.T) {
	got, skipped := upstream.Match(assets(
		"app-1.0-universal.apk",
		"app-1.0.apks",
		"app-1.0-arm64-v8a.apk",
		"app-1.0-arm64-v8a-config.zh.apk",
		"checksums.txt",
	), "")

	want := []string{"app-1.0-universal.apk", "app-1.0-arm64-v8a.apk"}
	names := pickedNames(got)
	if len(names) != len(want) {
		t.Fatalf("选中 %v，期望 %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			// 顺序必须保持上游给的顺序 —— 改名的 ABI 排序在下一步做，
			// 这里重排会让"哪个分片对应哪个 asset ID"对不上。
			t.Errorf("第 %d 项 = %q，期望 %q", i, names[i], want[i])
		}
	}
	if len(skipped) != 3 {
		t.Errorf("跳过 %d 个，期望 3 个：%+v", len(skipped), skipped)
	}
}

func TestMatchHonoursPattern(t *testing.T) {
	// 上游同时放了 debug 与 release 两种产物，用正则只要 release。
	got, _ := upstream.Match(assets(
		"app-debug.apk",
		"app-release.apk",
	), `(?i)release.*\.apk$`)

	if names := pickedNames(got); len(names) != 1 || names[0] != "app-release.apk" {
		t.Errorf("选中 %v，期望只有 app-release.apk", names)
	}
}

func TestMatchBadPatternFallsBackNotToEmpty(t *testing.T) {
	// 坏模式退化成默认，而不是返回空。
	// 返回空的表现是"这个源一个版本都镜像不了"，比一个明确的报错难查得多。
	got, _ := upstream.Match(assets("app-1.0.apk"), `[unclosed`)
	if len(got) != 1 {
		t.Errorf("坏模式时应当退化成默认模式，得到 %v", pickedNames(got))
	}
}

func TestCompilePatternRejectsBadRegex(t *testing.T) {
	if _, err := upstream.CompilePattern(`[unclosed`); err == nil {
		t.Error("坏正则应当报错")
	}
	if _, err := upstream.CompilePattern(""); err != nil {
		t.Errorf("空模式应当走默认而不是报错：%v", err)
	}
}

// TestFilenameABI 钉住文件名线索。
//
// 这些值只用于**告警**，但线索判错的后果是"该报的冲突没报"，
// 也就是"信内容"这条规矩失去了唯一的可见性。
func TestFilenameABI(t *testing.T) {
	cases := []struct {
		file string
		want string
		ok   bool
	}{
		{"app-1.0-arm64-v8a.apk", naming.ABIARM64, true},
		{"app-1.0-armeabi-v7a.apk", naming.ABIARMv7, true},
		{"app-1.0-x86_64.apk", naming.ABIX86_64, true},
		{"app-1.0-x86.apk", naming.ABIX86, true},
		{"app-1.0-universal.apk", naming.ABIUniversal, true},
		{"app-1.0-all.apk", naming.ABIUniversal, true},
		{"app-1.0-aarch64.apk", naming.ABIARM64, true},
		// 上游常见的简写
		{"app-1.0-arm64.apk", naming.ABIARM64, true},
		{"app-1.0-arm.apk", naming.ABIARMv7, true},
		// 认不出来：必须如实返回 false，不能瞎猜一个
		{"app-release.apk", "", false},
		{"Obtainium-1.2.3.apk", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			got, ok := upstream.FilenameABI(tc.file)
			if ok != tc.ok || got != tc.want {
				t.Errorf("FilenameABI(%q) = (%q, %v)，期望 (%q, %v)", tc.file, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestFilenameABIPrefersLongestMatch 是上面最容易写错的一条单独拎出来：
// `x86_64` 里含 `x86`，`armeabi-v7a` 里含 `arm`。匹配顺序错了就会把
// 64 位的包标成 32 位，于是"内容说 x86_64、文件名说 x86"这类**假冲突**被报出来。
func TestFilenameABIPrefersLongestMatch(t *testing.T) {
	if got, _ := upstream.FilenameABI("app-x86_64.apk"); got != naming.ABIX86_64 {
		t.Errorf("x86_64 被认成了 %q", got)
	}
	if got, _ := upstream.FilenameABI("app-armeabi-v7a.apk"); got != naming.ABIARMv7 {
		t.Errorf("armeabi-v7a 被认成了 %q", got)
	}
}

func TestCheckMismatch(t *testing.T) {
	cases := []struct {
		name    string
		file    string
		content string
		want    bool
	}{
		{"一致，不告警", "app-arm64-v8a.apk", naming.ABIARM64, false},
		{"文件名没标 ABI，不告警", "app-release.apk", naming.ABIARM64, false},
		{"内容是 universal，不告警", "app-arm64-v8a.apk", naming.ABIUniversal, false},
		{"文件名标 arm64 内容是 v7a —— 要告警", "app-arm64-v8a.apk", naming.ABIARMv7, true},
		{"文件名标 x86 内容是 arm64 —— 要告警", "app-x86.apk", naming.ABIARM64, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, got := upstream.CheckMismatch(tc.file, tc.content)
			if got != tc.want {
				t.Fatalf("CheckMismatch = %v，期望 %v", got, tc.want)
			}
			if got && m.String() == "" {
				t.Error("告警内容为空")
			}
		})
	}
}

// ---- 挑最新 -----------------------------------------------------------------

func rel(id int64, tag, published string, draft, pre bool) gh.Release {
	return gh.Release{ID: id, TagName: tag, PublishedAt: published, Draft: draft, Prerelease: pre}
}

func TestLatestReleasableSkipsDraftAndPrerelease(t *testing.T) {
	got, ok := upstream.LatestReleasable([]gh.Release{
		rel(1, "v1.0.0", "2025-01-01T00:00:00Z", false, false),
		rel(2, "v2.0.0-beta", "2025-06-01T00:00:00Z", false, true), // 更新的预发布，必须跳过
		rel(3, "v1.5.0", "2025-08-01T00:00:00Z", true, false),      // 更新的 draft，必须跳过
	}, false)
	if !ok {
		t.Fatal("应当挑得出一个")
	}
	if got.TagName != "v1.0.0" {
		t.Errorf("挑中 %q，期望 v1.0.0", got.TagName)
	}
}

// TestLatestReleasableIncludesPrerelease 钉住 `upstream.includePrerelease: true` 的语义：
// prerelease 与正式版**一视同仁**（按时间排，不看标记），而 **draft 依然不收**。
//
// 后半句是这条测试真正的价值：draft 没有开关，一旦有人把这里的条件写成
// `if r.Prerelease && !include` 之外的样子（比如顺手把 Draft 也挂上开关），
// 表现是"收录的时候镜像了一个上游作者自己都还没发布的东西"，而那是静默的。
func TestLatestReleasableIncludesPrerelease(t *testing.T) {
	rels := []gh.Release{
		rel(1, "v1.0.0", "2025-01-01T00:00:00Z", false, false),
		rel(2, "v2.0.0-beta", "2025-06-01T00:00:00Z", false, true),  // 更新的预发布：这次要收
		rel(3, "v3.0.0-draft", "2025-08-01T00:00:00Z", true, false), // 更新的 draft：永远不收
	}
	got, ok := upstream.LatestReleasable(rels, true)
	if !ok {
		t.Fatal("应当挑得出一个")
	}
	if got.TagName != "v2.0.0-beta" {
		t.Errorf("挑中 %q，期望 v2.0.0-beta（开了开关后 prerelease 与正式版按时间排）", got.TagName)
	}
	// 候选集里应当正好是那两个非 draft 的，顺序按发布时间倒序。
	cands := upstream.Releasable(rels, true)
	if len(cands) != 2 || cands[0].TagName != "v2.0.0-beta" || cands[1].TagName != "v1.0.0" {
		t.Errorf("候选集 = %v，期望 [v2.0.0-beta v1.0.0]", tags(cands))
	}
	if n := len(upstream.Releasable(rels, false)); n != 1 {
		t.Errorf("不开开关时候选集有 %d 个，期望 1 个", n)
	}
}

func tags(rels []gh.Release) []string {
	out := make([]string, 0, len(rels))
	for _, r := range rels {
		out = append(out, r.TagName)
	}
	return out
}

// TestExcludedNote 钉住"没有可镜像的发布"那句告警的措辞随开关变 ——
// 开过 includePrerelease 之后还说"全是 draft/prerelease"会让人去上游找一个不存在的东西。
func TestExcludedNote(t *testing.T) {
	if got := upstream.ExcludedNote(false); !strings.Contains(got, "prerelease") {
		t.Errorf("未开开关时应当提到 prerelease，得到 %q", got)
	}
	if got := upstream.ExcludedNote(true); strings.Contains(got, "prerelease") {
		t.Errorf("开了开关后不该再提 prerelease（它已经不在排除项里了），得到 %q", got)
	}
}

func TestLatestReleasablePicksNewest(t *testing.T) {
	got, _ := upstream.LatestReleasable([]gh.Release{
		rel(1, "v1.0.0", "2025-01-01T00:00:00Z", false, false),
		rel(2, "v2.0.0", "2025-06-01T00:00:00Z", false, false),
		rel(3, "v1.5.0", "2025-03-01T00:00:00Z", false, false),
	}, false)
	if got.TagName != "v2.0.0" {
		t.Errorf("挑中 %q，期望 v2.0.0（按发布时间而不是列表顺序）", got.TagName)
	}
}

// TestLatestReleasableTieBreaksOnID 钉住"同一秒发布"时的确定性。
//
// 不确定的挑选意味着同一次对账跑两遍可能镜像不同的版本 —— 那会让账本
// 的 diff 抖动，也会让"输在 tie-break 上"的那个版本永远进不来。
func TestLatestReleasableTieBreaksOnID(t *testing.T) {
	same := "2025-06-01T00:00:00Z"
	got, _ := upstream.LatestReleasable([]gh.Release{
		rel(10, "v1.0.0", same, false, false),
		rel(20, "v2.0.0", same, false, false),
	}, false)
	if got.TagName != "v2.0.0" {
		t.Errorf("挑中 %q，期望 ID 更大的 v2.0.0", got.TagName)
	}
}

func TestLatestReleasableNoneAvailable(t *testing.T) {
	// 全是 draft/prerelease，或一个都没有 —— 都必须如实报 false，
	// 让调用方去告警，而不是退而求其次镜像一个预发布。
	if _, ok := upstream.LatestReleasable(nil, false); ok {
		t.Error("空列表应当返回 false")
	}
	if _, ok := upstream.LatestReleasable([]gh.Release{
		rel(1, "v1", "2025-01-01T00:00:00Z", true, false),
		rel(2, "v2", "2025-01-01T00:00:00Z", false, true),
	}, false); ok {
		t.Error("只有 draft/prerelease 时应当返回 false")
	}
	// 开了开关：prerelease 变成候选，但**全是 draft** 时仍然要如实报 false。
	if _, ok := upstream.LatestReleasable([]gh.Release{
		rel(1, "v1", "2025-01-01T00:00:00Z", true, false),
	}, true); ok {
		t.Error("开了 includePrerelease 也不该把 draft 当候选")
	}
}
