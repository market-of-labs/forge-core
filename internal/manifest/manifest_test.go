package manifest

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/market-of-labs/forge-core/internal/model"
)

// 这一组钉的是 03 §5.1 的合成规则，以及 03 §5.4 那条最容易被"顺手改掉"的原则：
// **不写半成品清单条目**。后者不是洁癖 —— 一条 apkUrls 为空的条目会让 02 规则 4
// 判整份清单失败，于是 check-manifest 阻断整次回写，连别的 App 的正常更新都推不出去。

// endpoints 是一套与命名契约一致的模板（第一期的形状）。
func endpoints() model.Endpoints {
	return model.Endpoints{
		TagTemplate:       "{appId}",
		AssetNameTemplate: "{appId}-{version}-{abi}.apk",
		AssetURLTemplate: "https://github.com/market-of-labs/store/releases/download/" +
			"{appId}/{fileName}",
	}
}

func source(id string) model.Source {
	return model.Source{
		ID:     id,
		Name:   "Example App",
		Author: "Example Org",
		Source: model.SourceGitHub,
		Upstream: &model.Upstream{
			Type: model.UpstreamGitHubRelease,
			Repo: "example/app",
		},
	}
}

func asset(abi string) model.Asset {
	return model.Asset{
		ABI:  abi,
		File: "placeholder.apk", // 只用于喂给 Build，产出侧的名字由模板重算
		Size: 1024,
	}
}

func version(token string, code int32, published string, assets ...model.Asset) model.Version {
	return model.Version{
		Version:     token,
		VersionName: token,
		VersionCode: code,
		PublishedAt: published,
		Assets:      assets,
	}
}

// ledger 给一个来源挂上版本账本。
//
// 合并之后（D48）"这个 App 有哪些版本"不再来自第二个文件，所以一个用例的输入就是
// 一组**自带账本的来源** —— 而不再是"元数据一份、账本另一份，靠 id 对齐"。
func ledger(s model.Source, versions ...model.Version) model.Source {
	s.Versions = versions
	return s
}

// build 跑一次合成并断言没有硬错误。
func build(t *testing.T, srcs ...model.Source) (*model.Manifest, *model.Report) {
	t.Helper()
	m, rep, err := Build(Input{Sources: srcs, Endpoints: endpoints()})
	if err != nil {
		t.Fatalf("Build 失败：%v", err)
	}
	if rep.HasErrors() {
		t.Fatalf("合成过程不该产生硬错误：%v", rep.Errors())
	}
	return m, rep
}

// refs 解析出条目的 apkUrls。
func refs(t *testing.T, e *model.Entry) []model.APKRef {
	t.Helper()
	rs, err := e.APKRefs()
	if err != nil {
		t.Fatalf("解析 apkUrls 失败：%v", err)
	}
	return rs
}

// ---- 输入前置条件 -----------------------------------------------------------

func TestBuild_RejectsEndpointsThatDivergeFromNaming(t *testing.T) {
	// 模板与命名契约一旦分叉就是**静默故障**：清单能生成，客户端却折叠不中 ABI。
	// 所以必须在产出**之前**失败。
	cases := map[string]model.Endpoints{
		"tag 不等于 appId": func() model.Endpoints {
			e := endpoints()
			e.TagTemplate = "v{appId}"
			return e
		}(),
		"文件名形状不对": func() model.Endpoints {
			e := endpoints()
			e.AssetNameTemplate = "{appId}_{version}_{abi}.apk"
			return e
		}(),
		"地址模板没用 fileName": func() model.Endpoints {
			e := endpoints()
			e.AssetURLTemplate = "https://example.com/{appId}"
			return e
		}(),
		"地址不是 https": func() model.Endpoints {
			e := endpoints()
			e.AssetURLTemplate = "http://example.com/{appId}/{fileName}"
			return e
		}(),
	}
	for name, ep := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := Build(Input{Endpoints: ep}); err == nil {
				t.Fatal("模板与命名契约分叉时必须拒绝产出")
			}
		})
	}
}

// ---- 03 §5.4 不写半成品 -----------------------------------------------------

func TestBuild_SkipsSourceWithoutAnyVersion(t *testing.T) {
	m, rep := build(t, source("com.example.app"))
	if len(m.Apps) != 0 {
		t.Fatalf("还没有任何版本的条目不该出现在清单里：%+v", m.Apps)
	}
	if len(rep.Warnings()) == 0 {
		t.Fatal("跳过一条应当有告警，否则维护者不知道它为什么不在清单里")
	}
}

// 跳过时的告警必须**可操作**：能定位到条目（`source=`），并说明为什么本轮不会有新版本。
//
// 合并之前这条钉的是"index 里有这个 App 但一条版本都没有"（与"index 里没这个 App"
// 是两条不同的分支）。并进来源文件之后（D48）两者是同一份输入 —— 一个 `versions` 为空的
// 来源 —— 于是两条用例并成了一条，而告警内容反而是更值得钉的那部分。
func TestBuild_SkippedSourceWarningIsActionable(t *testing.T) {
	src := source("com.example.app")
	src.Paused = true
	m, rep := build(t, src)

	if len(m.Apps) != 0 {
		t.Fatalf("没有任何版本的条目不该进清单：%+v", m.Apps)
	}
	ws := rep.Warnings()
	if len(ws) != 1 {
		t.Fatalf("应当恰好一条告警，得到 %d 条：%v", len(ws), ws)
	}
	// 告警要能定位到条目、并说明本轮为什么没有它 —— 这条 guard 的价值全在这两样上，
	// 所以钉住它们，而不是只钉"没产出"。
	if !strings.Contains(ws[0].Msg, "source=") {
		t.Fatalf("告警应当带上 source=（否则不知道该看哪条 sources 文件）：%q", ws[0].Msg)
	}
	if !strings.Contains(ws[0].Msg, "paused") {
		t.Fatalf("paused 的条目应当提示它本来就不会有新版本（02 §2.9）：%q", ws[0].Msg)
	}
}

func TestBuild_SkipsVersionWithoutAssets(t *testing.T) {
	m, rep := build(t, ledger(source("com.example.app"), version("1.0", 1, "")))
	if len(m.Apps) != 0 {
		t.Fatalf("只有版本壳、没有 asset 时不该产出条目：%+v", m.Apps)
	}
	if len(rep.Warnings()) == 0 {
		t.Fatal("跳过应当有告警")
	}
}

// 一个 App 有问题不该带累别的 App —— 这是"跳过"而不是"整份失败"的意义。
func TestBuild_SkipsOnlyTheBrokenApp(t *testing.T) {
	m, _ := build(t,
		ledger(source("com.example.broken"), version("1.0", 1, "")),
		ledger(source("com.example.good"), version("2.0", 20, "", asset("universal"))))
	if len(m.Apps) != 1 || m.Apps[0].ID != "com.example.good" {
		t.Fatalf("应当只产出正常的那一条：%+v", m.Apps)
	}
}

// ---- 条目内容 ---------------------------------------------------------------

func TestBuild_EntryFields(t *testing.T) {
	src := source("com.example.app")
	src.Categories = []string{"工具", "网络"}
	src.Kind = model.KindCompanion

	m, _ := build(t, ledger(src, version("1.0", 1, "2026-09-11T10:00:00Z", asset("universal"))))
	if len(m.Apps) != 1 {
		t.Fatalf("应当产出 1 条：%+v", m.Apps)
	}
	e := m.Apps[0]

	if e.ID != "com.example.app" || e.Name != "Example App" || e.Author != "Example Org" {
		t.Fatalf("基本字段没落对：%+v", e)
	}
	// 02 规则 8：哨兵地址永不联网，只作唯一键。**绝不能是上游地址**。
	if e.URL != model.SentinelURL("com.example.app") {
		t.Fatalf("url 必须是哨兵地址，得到 %q", e.URL)
	}
	if !strings.HasPrefix(e.URL, model.SentinelHost) {
		t.Fatalf("哨兵前缀不对：%q", e.URL)
	}
	if e.OverrideSource != model.OverrideSource {
		t.Fatalf("overrideSource 必须恒为 %q，得到 %q", model.OverrideSource, e.OverrideSource)
	}
	// changeLog 故意留空：它会原样进 deep-link 的 URI（01 §3.6），是全量单次推送的预算。
	if e.ChangeLog != "" {
		t.Fatalf("changeLog 应当留空（URI 预算），得到 %q", e.ChangeLog)
	}
	if e.OtherAssetUrls != "[]" {
		t.Fatalf("otherAssetUrls 应当是 %q，得到 %q", "[]", e.OtherAssetUrls)
	}
	if e.PreferredAPKIdx != 0 {
		t.Fatalf("preferredApkIndex 默认应当是 0，得到 %d", e.PreferredAPKIdx)
	}
	if e.Kind != model.KindCompanion {
		t.Fatalf("kind 应当原样透传，得到 %q", e.Kind)
	}
	if !reflect.DeepEqual(e.Categories, []string{"工具", "网络"}) {
		t.Fatalf("分类没落对：%v", e.Categories)
	}
	// envelope
	if m.SchemaVersion != model.SchemaVersion {
		t.Fatalf("schemaVersion 必须是 %d", model.SchemaVersion)
	}
	if m.ExportedAt == "" {
		t.Fatal("exportedAt 不能为空")
	}
	if m.GeneratedBy != GeneratedBy {
		t.Fatalf("generatedBy 默认应当是 %q，得到 %q", GeneratedBy, m.GeneratedBy)
	}
}

// categories 缺省必须渲染成 `[]` 而不是 `null`：客户端的 jsonDecode 拿到 null
// 再 .map 会炸。
func TestBuild_EmptyCategoriesIsArrayNotNull(t *testing.T) {
	m, _ := build(t, ledger(source("com.example.app"), version("1.0", 1, "", asset("universal"))))

	b, err := json.Marshal(m.Apps[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"categories":[]`) {
		t.Fatalf("空分类必须渲染成 []：%s", b)
	}
	if strings.Contains(string(b), `"categories":null`) {
		t.Fatalf("空分类绝不能渲染成 null：%s", b)
	}
	// 普通 App 不该出现 kind 键（omitempty）。
	if strings.Contains(string(b), `"kind"`) {
		t.Fatalf("kind 为空时不该出现在 JSON 里：%s", b)
	}
}

// TestBuild_DescIsAppendedToName 是 desc 的**唯一**消费点（D42）。
//
// sources/ 里 desc 始终是独立字段，只有到了清单这一层才拼进 name ——
// 所以"改了分隔符不用重写数据"这句话对不对，全看这一条。
func TestBuild_DescIsAppendedToName(t *testing.T) {
	s := source("com.example.app")
	s.Desc = "去广告的第三方客户端"
	m, _ := build(t, ledger(s, version("1.0", 1, "", asset("universal"))))

	want := "Example App · 去广告的第三方客户端"
	if got := m.Apps[0].Name; got != want {
		t.Fatalf("清单里的 name = %q，期望 %q", got, want)
	}

	// 没简介的条目不该多出一个孤零零的分隔符。
	m2, _ := build(t, ledger(source("com.example.app"), version("1.0", 1, "", asset("universal"))))
	if got := m2.Apps[0].Name; got != "Example App" {
		t.Fatalf("没填简介时 name = %q，期望就是原始显示名", got)
	}
}

// latestVersion 存的是 APK 的**原始** versionName，而 apkUrls 里的 token 是清洗后的
// —— 两者在 versionName 含空白时必须都保留下来（03 §5.1）。
func TestBuild_LatestVersionIsRawVersionName(t *testing.T) {
	v := model.Version{
		Version:     "1.0-beta",
		VersionName: "1.0 beta",
		VersionCode: 1,
		Assets:      []model.Asset{asset("universal")},
	}
	m, _ := build(t, ledger(source("com.example.app"), v))

	e := m.Apps[0]
	if e.LatestVersion != "1.0 beta" {
		t.Fatalf("latestVersion 应当是原始 versionName %q，得到 %q", "1.0 beta", e.LatestVersion)
	}
	rs := refs(t, &m.Apps[0])
	if len(rs) != 1 || rs[0].Name != "com.example.app-1.0-beta-universal.apk" {
		t.Fatalf("文件名里的 token 应当是清洗后的：%+v", rs)
	}
}

// ---- apkUrls ---------------------------------------------------------------

func TestBuild_ApkURLsOrderUniversalFirst(t *testing.T) {
	v := version("1.0", 1, "",
		asset("x86_64"), asset("x86"), asset("arm64-v8a"), asset("universal"), asset("armeabi-v7a"))
	m, _ := build(t, ledger(source("com.example.app"), v))

	rs := refs(t, &m.Apps[0])
	want := []string{"universal", "arm64-v8a", "armeabi-v7a", "x86_64", "x86"}
	if len(rs) != len(want) {
		t.Fatalf("分片数不对：%+v", rs)
	}
	for i, abi := range want {
		wantName := "com.example.app-1.0-" + abi + ".apk"
		if rs[i].Name != wantName {
			t.Fatalf("第 %d 项应当是 %q，得到 %q", i, wantName, rs[i].Name)
		}
		wantURL := "https://github.com/market-of-labs/store/releases/download/" +
			"com.example.app/" + wantName
		if rs[i].URL != wantURL {
			t.Fatalf("第 %d 项的地址不对：\nwant=%s\ngot =%s", i, wantURL, rs[i].URL)
		}
	}
}

// 同一 ABI 在索引里出现两次（改名前后各留了一份）时，清单里只能出现一次 ——
// 留两个会让客户端按 ABI 折叠的结果取决于遍历顺序。
func TestBuild_DedupesSameABI(t *testing.T) {
	v := version("1.0", 1, "",
		model.Asset{ABI: "arm64-v8a", File: "旧名.apk"},
		model.Asset{ABI: "arm64-v8a", File: "新名.apk"})
	m, _ := build(t, ledger(source("com.example.app"), v))

	rs := refs(t, &m.Apps[0])
	if len(rs) != 1 {
		t.Fatalf("同一 ABI 只该出现一次：%+v", rs)
	}
}

// apkUrls 是**字符串**形式的 JSON 数组（02 §2.6 的双层编码）。
func TestBuild_ApkURLsIsDoubleEncodedString(t *testing.T) {
	m, _ := build(t,
		ledger(source("com.example.app"), version("1.0", 1, "", asset("universal"))))

	b, err := json.Marshal(m.Apps[0])
	if err != nil {
		t.Fatal(err)
	}
	// 内层引号必须被转义 —— 这正是 02 §2.6 样例里 `"[[\"name\",\"url\"]]"` 的来源。
	if !strings.Contains(string(b), `"apkUrls":"[[\"`) {
		t.Fatalf("apkUrls 必须是字符串形式的数组：%s", b)
	}
}

// ---- additionalSettings / releaseDate --------------------------------------

func TestBuild_WritesVersionCode(t *testing.T) {
	m, _ := build(t,
		ledger(source("com.example.app"), version("1.0", 42, "", asset("universal"))))

	code, ok := m.Apps[0].VersionCode()
	if !ok {
		t.Fatalf("additionalSettings 里应当有 versionCode：%q", m.Apps[0].AdditionalSettings)
	}
	if code != 42 {
		t.Fatalf("versionCode = %d，应当是 42", code)
	}
}

// 拿不到 versionCode 时**不写 0**：一条缺字段的清单应当被 check-manifest 拦住
// （规则 6），而不是带着 0 推给设备。
func TestBuild_MissingVersionCodeWarnsAndOmitsField(t *testing.T) {
	m, rep := build(t, ledger(source("com.example.app"), version("1.0", 0, "", asset("universal"))))

	if len(m.Apps) != 1 {
		t.Fatalf("缺 versionCode 仍然要产出条目（由 check-manifest 统一裁决）：%+v", m.Apps)
	}
	if _, ok := m.Apps[0].VersionCode(); ok {
		t.Fatalf("缺 versionCode 时不该写进 additionalSettings：%q", m.Apps[0].AdditionalSettings)
	}
	if strings.Contains(m.Apps[0].AdditionalSettings, `"versionCode":0`) {
		t.Fatalf("绝不能写 0 —— 那是「当作已知的 0」而不是「未知」：%q", m.Apps[0].AdditionalSettings)
	}
	found := false
	for _, w := range rep.Warnings() {
		if strings.Contains(w.Msg, "versionCode") {
			found = true
		}
	}
	if !found {
		t.Fatalf("应当有一条关于 versionCode 的告警：%v", rep.Warnings())
	}
}

func TestBuild_ReleaseDateFromPublishedAt(t *testing.T) {
	m, _ := build(t, ledger(source("com.example.app"),
		version("1.0", 1, "2026-09-11T10:00:00Z", asset("universal"))))

	// 期望值独立算出来，不复用 model.ReleaseDate —— 否则这条测试只是在复述实现。
	//
	// **微秒**，不是毫秒：权威是 Obtainium 的
	// lib/providers/source_provider.dart:303 `DateTime.fromMicrosecondsSinceEpoch`。
	// 曾经这里连着实现一起写成毫秒（13 位），而那会被 Obtainium 读成 1970 年。
	want := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC).UnixMicro()
	if got := m.Apps[0].ReleaseDate; got != want {
		t.Fatalf("releaseDate = %d，应当是 %d（微秒）", got, want)
	}
	// 顺带钉住量级：13 位(毫秒) 与 16 位(微秒) 一眼可分，写错了立刻看得见。
	if digits := len(strconv.FormatInt(m.Apps[0].ReleaseDate, 10)); digits != 16 {
		t.Fatalf("releaseDate 应当是 16 位（微秒），得到 %d 位：%d", digits, m.Apps[0].ReleaseDate)
	}
}

// publishedAt 解析不了时**留空并告警**，绝不因为一个展示字段阻断整条链。
func TestBuild_BadPublishedAtWarnsButStillEmits(t *testing.T) {
	m, rep := build(t, ledger(source("com.example.app"), version("1.0", 1, "昨天", asset("universal"))))

	if len(m.Apps) != 1 {
		t.Fatalf("坏日期不该让条目消失：%+v", m.Apps)
	}
	if m.Apps[0].ReleaseDate != 0 {
		t.Fatalf("解析不了时 releaseDate 应当留空：%d", m.Apps[0].ReleaseDate)
	}
	if len(rep.Warnings()) == 0 {
		t.Fatal("应当有一条告警")
	}
}

// ---- 版本位次 ---------------------------------------------------------------

// "最新"是**列表最后一个**，不是 versionCode 最大 —— 账本是按时间追加的。
// 这条测试把"位次即语义"钉死，免得日后有人"顺手"改成按 versionCode 排序。
func TestBuild_LatestIsLastNotMaxVersionCode(t *testing.T) {
	t.Run("按时间追加时最后就是最新", func(t *testing.T) {
		m, _ := build(t, ledger(source("com.example.app"),
			version("1.0", 10, "", asset("universal")),
			version("2.0", 99, "", asset("universal")),
		))
		if m.Apps[0].LatestVersion != "2.0" {
			t.Fatalf("latestVersion = %q，应当是 2.0", m.Apps[0].LatestVersion)
		}
	})

	t.Run("versionCode 回退时仍然按位次取", func(t *testing.T) {
		// 上游把 versionCode 写回退了是真实存在的；位次（时间）比 versionCode 可信。
		m, _ := build(t, ledger(source("com.example.app"),
			version("2.0", 99, "", asset("universal")),
			version("1.9", 5, "", asset("universal")),
		))
		if m.Apps[0].LatestVersion != "1.9" {
			t.Fatalf("latestVersion = %q，应当是位次最后那个 1.9", m.Apps[0].LatestVersion)
		}
	})
}

// ---- 暂停 -------------------------------------------------------------------

// §2.2：paused **只停"追加新版本"**，条目照常留在清单里，latestVersion 冻在
// 最后一个已镜像的版本。
func TestBuild_PausedSourceStillEmitted(t *testing.T) {
	src := source("com.example.app")
	src.Paused = true
	m, _ := build(t, ledger(src, version("1.5", 15, "", asset("universal"))))

	if len(m.Apps) != 1 {
		t.Fatalf("paused 的条目仍然要在清单里：%+v", m.Apps)
	}
	if m.Apps[0].LatestVersion != "1.5" {
		t.Fatalf("latestVersion 应当冻在已镜像的最后一个版本：%q", m.Apps[0].LatestVersion)
	}
	// 清单里**没有** paused 这个概念 —— 暂停是维护侧的输入，不是客户端契约字段。
	b, _ := json.Marshal(m.Apps[0])
	if strings.Contains(string(b), "paused") {
		t.Fatalf("paused 不该出现在清单里：%s", b)
	}
}

// ---- 顺序 -------------------------------------------------------------------

// 按 id 排序产出：清单顺序不影响语义，但稳定顺序 = 稳定 diff，
// 否则每次重建都可能整份重排，review 就失去意义了。
func TestBuild_SortedByID(t *testing.T) {
	ids := []string{"com.example.z", "com.example.a", "com.example.m"}
	srcs := make([]model.Source, 0, len(ids))
	for _, id := range ids {
		srcs = append(srcs, ledger(source(id), version("1.0", 1, "", asset("universal"))))
	}
	m, _ := build(t, srcs...)

	got := make([]string, 0, len(m.Apps))
	for _, e := range m.Apps {
		got = append(got, e.ID)
	}
	want := []string{"com.example.a", "com.example.m", "com.example.z"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("产出顺序应当是 %v，得到 %v", want, got)
	}
	// 入参不该被就地排序（调用方还要用原顺序）。
	if srcs[0].ID != "com.example.z" {
		t.Fatalf("Build 就地改了入参：%+v", srcs)
	}
}

// ---- 与 check-manifest 的接缝 -----------------------------------------------

// Build 的产出必须**直接**能通过 02 §2.8 自检。这一条比上面任何一条都重要：
// 它是"生成"与"自检"两个包的接缝，而那正是最容易各说各话的地方。
func TestBuild_OutputPassesManifestSelfCheck(t *testing.T) {
	src := source("com.example.app")
	src.Categories = []string{"工具"}

	companion := source("com.example.companion")
	companion.Kind = model.KindCompanion

	m, _ := build(t,
		ledger(src,
			version("1.0", 1, "2026-09-11T10:00:00Z", asset("universal"), asset("arm64-v8a")),
			version("1.1", 2, "2026-09-12T10:00:00Z", asset("universal"), asset("arm64-v8a")),
		),
		ledger(companion,
			version("0.1.0", 1, "2026-09-10T10:00:00Z", asset("universal"))),
	)

	rep := m.Validate(endpoints())
	if rep.HasErrors() {
		t.Fatalf("Build 的产出必须能直接过 02 §2.8 自检，却有硬错误：%v", rep.Errors())
	}
	if len(rep.Warnings()) != 0 {
		t.Fatalf("这份输入不该产生任何告警：%v", rep.Warnings())
	}
	if len(m.Apps) != 2 {
		t.Fatalf("应当产出 2 条：%+v", m.Apps)
	}
}

// 合成的产出必须是**确定的**：同样的输入两次跑必须逐字节相同（exportedAt 除外）。
func TestBuild_DeterministicApartFromTimestamp(t *testing.T) {
	// 故意**不按 id 排序**给进去：产出顺序是 Build 自己的责任。
	srcs := []model.Source{
		ledger(source("com.example.b"), version("2.0", 2, "", asset("universal"))),
		ledger(source("com.example.a"), version("1.0", 1, "", asset("arm64-v8a"), asset("universal"))),
	}

	strip := func(m *model.Manifest) string {
		cp := *m
		cp.ExportedAt = ""
		b, err := json.Marshal(cp)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	first := strip(mustBuildRaw(t, srcs...))
	for i := 0; i < 10; i++ {
		if got := strip(mustBuildRaw(t, srcs...)); got != first {
			t.Fatalf("第 %d 次产出不同：\n%s\n%s", i, first, got)
		}
	}
}

func mustBuildRaw(t *testing.T, srcs ...model.Source) *model.Manifest {
	t.Helper()
	m, _, err := Build(Input{Sources: srcs, Endpoints: endpoints()})
	if err != nil {
		t.Fatal(err)
	}
	return m
}
