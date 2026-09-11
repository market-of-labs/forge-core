package model_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/market-of-labs/forge-core/internal/model"
)

// phase1Endpoints 是第一期真实使用的模板（03 §2.3）。
func phase1Endpoints() model.Endpoints {
	return model.Endpoints{
		TagTemplate:       "{appId}",
		AssetNameTemplate: "{appId}-{version}-{abi}.apk",
		AssetURLTemplate:  "https://github.com/market-of-labs/store/releases/download/{appId}/{fileName}",
	}
}

func TestEndpointsValidateAcceptsRealTemplates(t *testing.T) {
	real := []model.Endpoints{
		phase1Endpoints(),
		{
			// 部署期：只有第三行不同（03 §2.3）
			TagTemplate:       "{appId}",
			AssetNameTemplate: "{appId}-{version}-{abi}.apk",
			AssetURLTemplate:  "https://cf.example.com/asset/{appId}/{version}/{fileName}",
		},
	}
	for i, ep := range real {
		if err := ep.Validate(); err != nil {
			t.Errorf("真实模板 #%d 不该被判非法：%v", i, err)
		}
	}
}

func TestEndpointsValidateRejectsDivergence(t *testing.T) {
	tests := []struct {
		name string
		ep   model.Endpoints
		want string
	}{
		{
			// 这条是整份 Validate 存在的理由：模板改了、naming 没改 = 静默故障
			// （02 §2.4 明说解析失败会静默降级）。
			name: "assetNameTemplate 与命名契约分叉",
			ep: model.Endpoints{
				TagTemplate:       "{appId}",
				AssetNameTemplate: "{appId}_{version}_{abi}.apk", // 下划线而不是连字符
				AssetURLTemplate:  "https://example.com/{appId}/{fileName}",
			},
			want: "命名契约",
		},
		{
			// 单个探针挡不住"模板忽略变量、返回常量"，所以用两个探针。
			name: "tagTemplate 返回常量而非 appId",
			ep: model.Endpoints{
				TagTemplate:       "release",
				AssetNameTemplate: "{appId}-{version}-{abi}.apk",
				AssetURLTemplate:  "https://example.com/{appId}/{fileName}",
			},
			want: "tag 必须恒等于 appId",
		},
		{
			// 少了 version 段：文件名不再承载版本，索引与客户端折叠同时失效。
			name: "assetNameTemplate 少了 version",
			ep: model.Endpoints{
				TagTemplate:       "{appId}",
				AssetNameTemplate: "{appId}-{abi}.apk",
				AssetURLTemplate:  "https://example.com/{appId}/{fileName}",
			},
			want: "命名契约",
		},
		{
			name: "assetUrlTemplate 缺 fileName",
			ep: model.Endpoints{
				TagTemplate:       "{appId}",
				AssetNameTemplate: "{appId}-{version}-{abi}.apk",
				AssetURLTemplate:  "https://example.com/{appId}/",
			},
			want: "没有 {fileName}",
		},
		{
			// 明文 http 会被客户端拒绝；而 `.invalid` 那条约束只管哨兵地址，管不到这里。
			name: "assetUrlTemplate 是 http",
			ep: model.Endpoints{
				TagTemplate:       "{appId}",
				AssetNameTemplate: "{appId}-{version}-{abi}.apk",
				AssetURLTemplate:  "http://example.com/{appId}/{fileName}",
			},
			want: "必须是 https",
		},
		{
			name: "占位符拼错",
			ep: model.Endpoints{
				TagTemplate:       "{appId}",
				AssetNameTemplate: "{appId}-{version}-{abi}.apk",
				AssetURLTemplate:  "https://example.com/{appid}/{fileName}", // 大小写错
			},
			want: "不是已知变量",
		},
		{
			name: "空模板",
			ep: model.Endpoints{
				TagTemplate:       "",
				AssetNameTemplate: "{appId}-{version}-{abi}.apk",
				AssetURLTemplate:  "https://example.com/{fileName}",
			},
			want: "tagTemplate 为空",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.ep.Validate()
			if err == nil {
				t.Fatalf("该报错却通过了")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("错误信息里没有 %q：%v", tt.want, err)
			}
		})
	}
}

func TestRenderRejectsEmptyValueAndUnclosedBrace(t *testing.T) {
	ep := phase1Endpoints()

	// 空 appId 渲染出的地址指向仓库根而不是某个 Release —— 必然 404 的地址不该被静默写进清单。
	if _, err := ep.AssetURL("", "1.0", "a-1.0-universal.apk"); err == nil {
		t.Error("空 appId 应当报错")
	} else if !strings.Contains(err.Error(), "空值") {
		t.Errorf("错误信息不对：%v", err)
	}

	broken := model.Endpoints{
		TagTemplate:       "{appId",
		AssetNameTemplate: "{appId}-{version}-{abi}.apk",
		AssetURLTemplate:  "https://example.com/{fileName}",
	}
	if _, err := broken.Tag("com.foo"); err == nil {
		t.Error("未闭合的 { 应当报错")
	} else if !strings.Contains(err.Error(), "未闭合") {
		t.Errorf("错误信息不对：%v", err)
	}
}

func TestAPKRefsRoundTrip(t *testing.T) {
	refs := []model.APKRef{
		{Name: "com.foo-1.0-universal.apk", URL: "https://example.com/a"},
		{Name: "com.foo-1.0-arm64-v8a.apk", URL: "https://example.com/b"},
	}
	s, err := model.MarshalAPKRefs(refs)
	if err != nil {
		t.Fatalf("MarshalAPKRefs：%v", err)
	}

	got, err := model.UnmarshalAPKRefs(s)
	if err != nil {
		t.Fatalf("UnmarshalAPKRefs：%v", err)
	}
	if len(got) != len(refs) {
		t.Fatalf("往返后 %d 项，期望 %d 项", len(got), len(refs))
	}
	for i := range refs {
		if got[i] != refs[i] {
			t.Errorf("第 %d 项：得到 %+v，期望 %+v", i, got[i], refs[i])
		}
	}

	// 空列表必须渲染成 `[]` 而不是 `null`：客户端侧 decode 出 null 再 map 会炸。
	empty, err := model.MarshalAPKRefs(nil)
	if err != nil {
		t.Fatalf("MarshalAPKRefs(nil)：%v", err)
	}
	if empty != "[]" {
		t.Errorf("空列表渲染成 %q，期望 %q", empty, "[]")
	}
}

func TestUnmarshalAPKRefsRejectsMalformed(t *testing.T) {
	tests := []struct{ name, in string }{
		{"不是 JSON", "not json"},
		{"不是数组", `{"a":"b"}`},
		{"元素不是数组", `[["a"],"b"]`},
		{"元素少于两个", `[["only-name"]]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := model.UnmarshalAPKRefs(tt.in); err == nil {
				t.Errorf("UnmarshalAPKRefs(%q) 该报错", tt.in)
			}
		})
	}

	// 多出来的元素被忽略而不是报错 —— 将来若扩成 [name,url,size]，旧解析器不该炸（02 §2.9）。
	refs, err := model.UnmarshalAPKRefs(`[["n","u","extra"]]`)
	if err != nil {
		t.Fatalf("多余元素应当被忽略：%v", err)
	}
	if len(refs) != 1 || refs[0].Name != "n" || refs[0].URL != "u" {
		t.Errorf("得到 %+v", refs)
	}

	// 空串按"没有"处理，不是错误。
	if refs, err := model.UnmarshalAPKRefs(""); err != nil || refs != nil {
		t.Errorf("空串：得到 (%v, %v)", refs, err)
	}
}

func TestSetVersionCodePreservesOtherKeys(t *testing.T) {
	e := &model.Entry{AdditionalSettings: `{"sha256":"deadbeef","useVersionCodeAsOSVersion":true}`}

	if err := e.SetVersionCode(1615); err != nil {
		t.Fatalf("SetVersionCode：%v", err)
	}
	// 02 §2.3：「清单显式给出的值全部保留」—— 只覆盖一个键，不整份替换。
	m, err := e.Settings()
	if err != nil {
		t.Fatalf("Settings：%v", err)
	}
	if m["sha256"] != "deadbeef" || m["useVersionCodeAsOSVersion"] != true {
		t.Errorf("别的键被弄丢了：%v", m)
	}

	got, ok := e.VersionCode()
	if !ok || got != 1615 {
		t.Errorf("VersionCode = (%d,%v)，期望 (1615,true)", got, ok)
	}

	// 稳定渲染：同一个 map 永远产出同一个字符串，否则每次重建清单都会产生无意义的 diff。
	before := e.AdditionalSettings
	if err := e.SetVersionCode(1615); err != nil {
		t.Fatal(err)
	}
	if e.AdditionalSettings != before {
		t.Errorf("同一个值写了两次却产生不同字符串：\n  %q\n  %q", before, e.AdditionalSettings)
	}
}

func TestVersionCodeMissingVsZero(t *testing.T) {
	// "缺字段"与"值是 0"要能区分开，否则排查时看不出区别。
	missing := &model.Entry{AdditionalSettings: `{"sha256":"x"}`}
	if _, ok := missing.VersionCode(); ok {
		t.Error("缺 versionCode 时 ok 必须是 false")
	}

	zero := &model.Entry{AdditionalSettings: `{"versionCode":0}`}
	v, ok := zero.VersionCode()
	if !ok || v != 0 {
		t.Errorf("versionCode=0 应当解析成 (0,true)，得到 (%d,%v)", v, ok)
	}
}

// validManifest 造一份**应当通过**全部规则的清单，供下面的变异测试逐条破坏。
func validManifest(t *testing.T, ep model.Endpoints) *model.Manifest {
	t.Helper()

	refs := make([]model.APKRef, 0, 2)
	for _, abi := range []string{"universal", "arm64-v8a"} {
		ref, err := ep.AssetURLForABI("com.foo", "1.2.3", abi)
		if err != nil {
			t.Fatalf("造 refs：%v", err)
		}
		refs = append(refs, ref)
	}

	e := model.Entry{
		ID:             "com.foo",
		Name:           "Foo",
		Author:         "you",
		URL:            model.SentinelURL("com.foo"),
		OverrideSource: model.OverrideSource,
		LatestVersion:  "1.2.3",
		OtherAssetUrls: "[]",
		Categories:     []string{},
	}
	if err := e.SetAPKRefs(refs); err != nil {
		t.Fatalf("SetAPKRefs：%v", err)
	}
	if err := e.SetVersionCode(10203); err != nil {
		t.Fatalf("SetVersionCode：%v", err)
	}

	return &model.Manifest{
		SchemaVersion: model.SchemaVersion,
		ExportedAt:    model.NowISO(),
		Apps:          []model.Entry{e},
	}
}

// TestValidateCatchesEachRule 逐条破坏 02 §2.8 的规则，确认**每一条都真的会响**。
//
// 只测"合法输入通过"是不够的 —— 那样的测试在规则被整段删掉时依然会绿。
func TestValidateCatchesEachRule(t *testing.T) {
	ep := phase1Endpoints()

	tests := []struct {
		name    string
		mutate  func(*model.Manifest)
		want    string
		isError bool
	}{
		{
			name:    "规则 1：schemaVersion 不是 2",
			mutate:  func(m *model.Manifest) { m.SchemaVersion = 3 },
			want:    "schemaVersion",
			isError: true,
		},
		{
			name:    "规则 1：apps 为空",
			mutate:  func(m *model.Manifest) { m.Apps = nil },
			want:    "apps 为空",
			isError: true,
		},
		{
			name: "规则 1：id 重复",
			mutate: func(m *model.Manifest) {
				m.Apps = append(m.Apps, m.Apps[0])
			},
			want:    "id 重复",
			isError: true,
		},
		{
			name:    "规则 2：name 为空",
			mutate:  func(m *model.Manifest) { m.Apps[0].Name = "" },
			want:    "name 为空",
			isError: true,
		},
		{
			name:    "规则 4：latestVersion 为空",
			mutate:  func(m *model.Manifest) { m.Apps[0].LatestVersion = "" },
			want:    "latestVersion 为空",
			isError: true,
		},
		{
			// 规则 3 的核心：清单里不得出现上游直链。
			name: "规则 3：apkUrls 里是上游直链",
			mutate: func(m *model.Manifest) {
				m.Apps[0].APKUrls = `[["com.foo-1.2.3-universal.apk","https://github.com/upstream/foo/releases/download/v1.2.3/app.apk"]]`
			},
			want:    "不得出现上游直链",
			isError: true,
		},
		{
			// 02 §2.4：文件名形状是硬依赖，解析失败是**静默降级**，所以必须拦住。
			name: "02 §2.4：文件名不合契约",
			mutate: func(m *model.Manifest) {
				m.Apps[0].APKUrls = `[["app-release.apk","https://github.com/market-of-labs/store/releases/download/com.foo/app-release.apk"]]`
			},
			want:    "不符合命名契约",
			isError: true,
		},
		{
			name: "规则 4：apkUrls 为空数组",
			mutate: func(m *model.Manifest) {
				m.Apps[0].APKUrls = "[]"
			},
			want:    "apkUrls 为空",
			isError: true,
		},
		{
			name:    "规则 6：缺 versionCode",
			mutate:  func(m *model.Manifest) { m.Apps[0].AdditionalSettings = `{"sha256":"x"}` },
			want:    "缺 versionCode",
			isError: true,
		},
		{
			name:    "规则 6：versionCode 是 0",
			mutate:  func(m *model.Manifest) { m.Apps[0].AdditionalSettings = `{"versionCode":0}` },
			want:    "必须是正整数",
			isError: true,
		},
		{
			name:    "规则 8：url 不是哨兵地址",
			mutate:  func(m *model.Manifest) { m.Apps[0].URL = "https://github.com/market-of-labs/store" },
			want:    "哨兵地址",
			isError: true,
		},
		{
			name:    "规则 8：overrideSource 不是 HTML",
			mutate:  func(m *model.Manifest) { m.Apps[0].OverrideSource = "APP" },
			want:    "overrideSource",
			isError: true,
		},
		{
			name:    "规则 9：kind 取值非法",
			mutate:  func(m *model.Manifest) { m.Apps[0].Kind = "system" },
			want:    "kind = ",
			isError: true,
		},
		{
			// 03 §3.1：`_incoming` 是保留名。
			name: "保留名：id 是 _incoming",
			mutate: func(m *model.Manifest) {
				m.Apps[0].ID = model.IncomingTag
				m.Apps[0].URL = model.SentinelURL(model.IncomingTag)
			},
			want:    "保留名",
			isError: true,
		},
		{
			name:    "规则 2：otherAssetUrls 不是合法 JSON",
			mutate:  func(m *model.Manifest) { m.Apps[0].OtherAssetUrls = "oops" },
			want:    "otherAssetUrls",
			isError: true,
		},
		{
			// 只告警不阻断：02 §2.8 没有这条规则，但 ABI 顺序有约定（02 §2.4）。
			name: "02 §2.4：ABI 顺序不符合约定",
			mutate: func(m *model.Manifest) {
				m.Apps[0].APKUrls = `[["com.foo-1.2.3-arm64-v8a.apk","https://github.com/market-of-labs/store/releases/download/com.foo/com.foo-1.2.3-arm64-v8a.apk"],` +
					`["com.foo-1.2.3-universal.apk","https://github.com/market-of-labs/store/releases/download/com.foo/com.foo-1.2.3-universal.apk"]]`
			},
			want:    "ABI 顺序",
			isError: false,
		},
		{
			name: "latestVersion 在 apkUrls 里找不到对应版本",
			mutate: func(m *model.Manifest) {
				m.Apps[0].LatestVersion = "9.9.9"
			},
			want:    "找不到对应版本",
			isError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := validManifest(t, ep)
			// 先确认基线是干净的，否则下面的断言可能命中"别的问题"。
			if rep := m.Validate(ep); rep.HasErrors() {
				t.Fatalf("基线清单本身就不合法，变异测试无意义：%v", rep.Problems)
			}

			tt.mutate(m)
			rep := m.Validate(ep)

			hit := false
			for _, p := range rep.Problems {
				if strings.Contains(p.Msg, tt.want) {
					hit = true
					if tt.isError && p.Sev != model.SeverityError {
						t.Errorf("期望阻断项，得到告警：%s", p)
					}
					if !tt.isError && p.Sev != model.SeverityWarn {
						t.Errorf("期望告警，得到阻断项：%s", p)
					}
				}
			}
			if !hit {
				t.Errorf("没有任何结论包含 %q，实际结论：%v", tt.want, rep.Problems)
			}
			if tt.isError && !rep.HasErrors() {
				t.Error("期望 HasErrors() = true")
			}
			if !tt.isError && rep.HasErrors() {
				t.Errorf("这条只该告警，却产生了阻断项：%v", rep.Errors())
			}
		})
	}
}

func TestValidateWarnsOnTooManyEntries(t *testing.T) {
	ep := phase1Endpoints()
	m := validManifest(t, ep)

	// 复制到超过阈值。id 会重复，所以硬错误本来就有 —— 这里只关心条数告警是否触发。
	base := m.Apps[0]
	for i := 0; i < model.EntryCountWarn+1; i++ {
		e := base
		e.ID = "com.foo." + strings.Repeat("x", i+1)
		e.URL = model.SentinelURL(e.ID)
		m.Apps = append(m.Apps, e)
	}

	rep := m.Validate(ep)
	found := false
	for _, p := range rep.Problems {
		if strings.Contains(p.Msg, "deep-link") {
			found = true
		}
	}
	if !found {
		t.Errorf("条目数 %d 超过阈值 %d，应当告警", len(m.Apps), model.EntryCountWarn)
	}
}

func TestValidateSource(t *testing.T) {
	good := model.Source{
		ID: "com.foo", Name: "Foo", Author: "you", Source: model.SourceGitHub,
		Upstream: &model.Upstream{Type: model.UpstreamGitHubRelease, Repo: "o/r", AssetPattern: `app.*\.apk$`},
	}
	if err := good.Validate("com.foo.json"); err != nil {
		t.Fatalf("合法条目被判非法：%v", err)
	}

	tests := []struct {
		name     string
		mutate   func(*model.Source)
		fileName string
		want     string
	}{
		{name: "文件名与 id 不符", mutate: func(s *model.Source) {}, fileName: "other.json", want: "必须是"},
		{name: "文件名与 id 相符", mutate: func(s *model.Source) {}, fileName: "com.foo.json"},
		{name: "id 是保留名", mutate: func(s *model.Source) { s.ID = model.IncomingTag }, want: "保留名"},
		{name: "id 含斜杠", mutate: func(s *model.Source) { s.ID = "a/b" }, want: "斜杠"},
		{name: "source 非法", mutate: func(s *model.Source) { s.Source = "gitlab" }, want: "只能是"},
		{name: "github 缺 upstream", mutate: func(s *model.Source) { s.Upstream = nil }, want: "upstream 必填"},
		{
			name:   "upstream.type 非法",
			mutate: func(s *model.Source) { s.Upstream.Type = "gitlab-release" },
			want:   "只支持",
		},
		{
			name:   "upstream.repo 不是 owner/name",
			mutate: func(s *model.Source) { s.Upstream.Repo = "just-a-name" },
			want:   "owner/name",
		},
		{
			// 提前编译正则：留到遍历上游才发现写错，会在"某个上游恰好发版"时才炸。
			name:   "assetPattern 正则写错",
			mutate: func(s *model.Source) { s.Upstream.AssetPattern = "app.*\\.apk$(" },
			want:   "编译失败",
		},
		{
			name:   "manual 带了 upstream",
			mutate: func(s *model.Source) { s.Source = model.SourceManual },
			want:   "不该有 upstream",
		},
		{
			name:   "abiWhitelist 里有非法 ABI",
			mutate: func(s *model.Source) { s.ABIWhitelist = []string{"mips"} },
			want:   "不在固定集",
		},
		{
			name:   "kind 非法",
			mutate: func(s *model.Source) { s.Kind = "system" },
			want:   "kind = ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := good
			up := *good.Upstream
			s.Upstream = &up

			tt.mutate(&s)
			err := s.Validate(tt.fileName)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("该通过却报错：%v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("该报错却通过")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("错误信息里没有 %q：%v", tt.want, err)
			}
		})
	}
}

func TestCheckSourceSetRejectsTwoCompanions(t *testing.T) {
	mk := func(id string, kind string) model.Source {
		return model.Source{
			ID: id, Name: "n", Author: "a", Source: model.SourceManual, Kind: kind,
		}
	}

	// 两条 companion → 硬错误（与清单侧不同：sources 是输入，此时就该拦住）。
	rep := model.CheckSourceSet([]model.Source{mk("a", model.KindCompanion), mk("b", model.KindCompanion)})
	if !rep.HasErrors() {
		t.Error("两条 companion 应当判失败")
	}

	rep = model.CheckSourceSet([]model.Source{mk("a", model.KindCompanion), mk("b", model.KindObtainium)})
	if rep.HasErrors() {
		t.Errorf("一条 companion 应当通过：%v", rep.Errors())
	}
}

func TestCheckReleaseAssetCounts(t *testing.T) {
	rep := model.CheckReleaseAssetCounts(map[string]int{
		"com.big":   model.ReleaseAssetCountWarn + 1,
		"com.small": 3,
	})
	if rep.HasErrors() {
		t.Error("asset 数超阈值只该告警，不该阻断（03 §5.3）")
	}
	warns := rep.Warnings()
	if len(warns) != 1 {
		t.Fatalf("期望 1 条告警，得到 %v", warns)
	}
	if warns[0].AppID != "com.big" {
		t.Errorf("告警挂错了 App：%s", warns[0])
	}
	if !strings.Contains(warns[0].Msg, "1000") {
		t.Errorf("告警里应当提到硬上限 1000：%s", warns[0])
	}
}

func TestSentinelURL(t *testing.T) {
	// `.invalid` 是 RFC 2606 保留 TLD，规范保证不可解析（02 规则 8）。
	// 绝不能改成 `.local` —— 那是 mDNS 保留域，在真实网络里可能真的被解析。
	got := model.SentinelURL("com.foo")
	if got != "https://market.invalid/com.foo" {
		t.Errorf("SentinelURL = %q", got)
	}
	if !strings.Contains(got, ".invalid") {
		t.Error("哨兵地址必须用 .invalid 保留 TLD")
	}
}

// ---- desc：显示名拼接与长度上限（D42）--------------------------------------

// TestDisplayName 钉住清单里的显示名怎么拼。
//
// 拼法在 **manifest 合成时**才生效，`sources/` 里两者始终分开存 ——
// 这正是"以后改分隔符不用重写数据"的前提，所以这里连"没简介时不出现光秃秃的
// 分隔符"一起钉住（`Foo · ` 这种尾巴在列表里看起来就是个 bug）。
func TestDisplayName(t *testing.T) {
	withDesc := model.Source{Name: "Obtainium", Desc: "应用更新器"}
	if got, want := withDesc.DisplayName(), "Obtainium · 应用更新器"; got != want {
		t.Errorf("DisplayName = %q，期望 %q", got, want)
	}
	bare := model.Source{Name: "Obtainium"}
	if got := bare.DisplayName(); got != "Obtainium" {
		t.Errorf("没简介时 DisplayName = %q，期望就是 Name 本身", got)
	}
}

// TestTruncateDescIsRuneSafe 是"不许按 byte 切"的落地检查。
//
// 按 byte 切会把一个汉字劈成半个，结果是**非法 UTF-8** —— 到了客户端那边
// 是一串替换字符，而 JSON 编码会把它转义成 �，肉眼在 diff 里根本看不出来。
func TestTruncateDescIsRuneSafe(t *testing.T) {
	got := model.TruncateDesc(strings.Repeat("汉", model.MaxDescRunes+10))
	if !utf8.ValidString(got) {
		t.Fatalf("裁出来的不是合法 UTF-8：%q", got)
	}
	if n := len([]rune(got)); n != model.MaxDescRunes {
		t.Fatalf("裁成 %d 个字，期望 %d", n, model.MaxDescRunes)
	}

	// 边界：正好等于上限时一个都不动。
	exact := strings.Repeat("汉", model.MaxDescRunes)
	if model.TruncateDesc(exact) != exact {
		t.Error("正好等于上限时不该被改动")
	}
	// 空白：模板的输入框很容易多带一个空格，而它在列表里表现为"两个空格"。
	if got := model.TruncateDesc("  去广告  "); got != "去广告" {
		t.Errorf("首尾空白没去掉：%q", got)
	}
}

// TestSourceValidateRejectsBadDesc 钉住**手改文件**这条路上的把关。
//
// issue 那条路是"裁而不拒"（往返一天，字段纯装饰）；手改文件这条路不是 ——
// 改的人就在本地，一条立刻报出来的错误比一个被悄悄改短的值有用得多。
func TestSourceValidateRejectsBadDesc(t *testing.T) {
	base := model.Source{
		ID: "com.example.app", Name: "App", Author: "Org", Source: model.SourceManual,
	}

	tooLong := base
	tooLong.Desc = strings.Repeat("长", model.MaxDescRunes+1)
	if err := tooLong.Validate("com.example.app.json"); err == nil {
		t.Error("超过上限的 desc 该被拒绝")
	}

	multiline := base
	multiline.Desc = "第一行\n第二行"
	if err := multiline.Validate("com.example.app.json"); err == nil {
		t.Error("含换行的 desc 该被拒绝（它在客户端是单行标题的一部分）")
	}

	ok := base
	ok.Desc = strings.Repeat("长", model.MaxDescRunes)
	if err := ok.Validate("com.example.app.json"); err != nil {
		t.Errorf("正好等于上限的 desc 该通过：%v", err)
	}
}
