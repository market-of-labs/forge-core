package model_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/market-of-labs/forge-core/internal/model"
)

// phase1Endpoints 是第一期真实使用的模板（03 §2.3）。
//
// 与它前身的差别只有第三行：`assetUrlTemplate` 整条没了，换成一个成品地址 `repoUrl`。
// 前者是"我们怎么拼每个 APK 的地址"，后者是"客户端去哪找这个仓库" ——
// F-Droid 那条路上地址不再由我们逐条产出（D62）。
func phase1Endpoints() model.Endpoints {
	return model.Endpoints{
		TagTemplate:       "{appId}",
		AssetNameTemplate: "{appId}-{version}-{abi}.apk",
		RepoURL:           "https://apps.example.com/fdroid/repo",
	}
}

func TestEndpointsValidateAcceptsRealTemplates(t *testing.T) {
	real := []model.Endpoints{
		phase1Endpoints(),
		{
			// 部署期的真实形态：同一个仓库地址，但换成最终的 CF 域。
			TagTemplate:       "{appId}",
			AssetNameTemplate: "{appId}-{version}-{abi}.apk",
			RepoURL:           "https://cf.example.com/fdroid/repo",
		},
		{
			// 本机验证那种：`python -m http.server` 不做 TLS，所以回环 http 开了一格
			// （见 isLoopbackHost）。它连的必然是一个回环地址。
			TagTemplate:       "{appId}",
			AssetNameTemplate: "{appId}-{version}-{abi}.apk",
			RepoURL:           "http://localhost:8000/repo",
		},
		{
			// 带端口 + 127.0.0.1：Hostname() 剥端口那条逻辑的回归位。
			TagTemplate:       "{appId}",
			AssetNameTemplate: "{appId}-{version}-{abi}.apk",
			RepoURL:           "http://127.0.0.1:8080",
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
				RepoURL:           "https://example.com/repo",
			},
			want: "命名契约",
		},
		{
			// 单个探针挡不住"模板忽略变量、返回常量"，所以用两个探针。
			name: "tagTemplate 返回常量而非 appId",
			ep: model.Endpoints{
				TagTemplate:       "release",
				AssetNameTemplate: "{appId}-{version}-{abi}.apk",
				RepoURL:           "https://example.com/repo",
			},
			want: "tag 必须恒等于 appId",
		},
		{
			// 少了 version 段：文件名不再承载版本，索引与客户端折叠同时失效。
			name: "assetNameTemplate 少了 version",
			ep: model.Endpoints{
				TagTemplate:       "{appId}",
				AssetNameTemplate: "{appId}-{abi}.apk",
				RepoURL:           "https://example.com/repo",
			},
			want: "命名契约",
		},
		{
			name: "占位符拼错",
			ep: model.Endpoints{
				TagTemplate:       "{appId}",
				AssetNameTemplate: "{appId}-{version}-{ABI}.apk", // 大小写错
				RepoURL:           "https://example.com/repo",
			},
			want: "不是已知变量",
		},
		{
			name: "空模板",
			ep: model.Endpoints{
				TagTemplate:       "",
				AssetNameTemplate: "{appId}-{version}-{abi}.apk",
				RepoURL:           "https://example.com/repo",
			},
			want: "tagTemplate 为空",
		},
		{
			name: "repoUrl 为空",
			ep: model.Endpoints{
				TagTemplate:       "{appId}",
				AssetNameTemplate: "{appId}-{version}-{abi}.apk",
				RepoURL:           "  ",
			},
			want: "repoUrl 为空",
		},
		{
			// 明文 http 会被客户端拒绝；回环那一格豁免管不到这里。
			name: "repoUrl 是 http（非回环）",
			ep: model.Endpoints{
				TagTemplate:       "{appId}",
				AssetNameTemplate: "{appId}-{version}-{abi}.apk",
				RepoURL:           "http://example.com/repo",
			},
			want: "必须是 https",
		},
		{
			// 这条是"看起来能用"的典型：多数服务端会把双斜杠折掉，于是它跑得通 ——
			// 而地址是用户添加源时存下来的，发出去就改不回来了。
			name: "repoUrl 带尾斜杠",
			ep: model.Endpoints{
				TagTemplate:       "{appId}",
				AssetNameTemplate: "{appId}-{version}-{abi}.apk",
				RepoURL:           "https://example.com/repo/",
			},
			want: "尾斜杠",
		},
		{
			name: "repoUrl 没有 host",
			ep: model.Endpoints{
				TagTemplate:       "{appId}",
				AssetNameTemplate: "{appId}-{version}-{abi}.apk",
				RepoURL:           "https:///repo",
			},
			want: "没有 host",
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

// TestLoopbackExemptionIsPrecise 钉住那条 http 豁免**只**落在回环上。
//
// 它是本机验证（03 §7 #11）唯一的入口，而一条写松了的豁免会直接变成
// "线上跑了一份明文 http 的源地址"—— 那是客户端拒绝、而我们会一直不知道的形态。
func TestLoopbackExemptionIsPrecise(t *testing.T) {
	base := phase1Endpoints()

	for _, host := range []string{
		"http://localhost.evil.com/repo",
		"http://127.0.0.1.evil.com/repo",
		"http://[::1].evil.com/repo",
		"http://192.168.1.10/repo", // 内网但不是回环
	} {
		ep := base
		ep.RepoURL = host
		if err := ep.Validate(); err == nil {
			t.Errorf("%s 不该被豁免", host)
		}
	}
}

func TestRenderRejectsEmptyValueAndUnclosedBrace(t *testing.T) {
	ep := phase1Endpoints()

	// 空 appId 渲染出的文件名以 `-` 开头、且切不出版本 —— 这种"能构造出来但必然错"的
	// 名字不该被静默经手。
	if _, err := ep.AssetName("", "1.0", "universal"); err == nil {
		t.Error("空 appId 应当报错")
	} else if !strings.Contains(err.Error(), "空值") {
		t.Errorf("错误信息不对：%v", err)
	}

	broken := model.Endpoints{
		TagTemplate:       "{appId",
		AssetNameTemplate: "{appId}-{version}-{abi}.apk",
	}
	if _, err := broken.Tag("com.foo"); err == nil {
		t.Error("未闭合的 { 应当报错")
	} else if !strings.Contains(err.Error(), "未闭合") {
		t.Errorf("错误信息不对：%v", err)
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

// TestCheckSourceSetRejectsDuplicateID 钉住那**唯一**剩下的跨条目规则。
//
// 从前这里还有第二条：`kind: companion` 至多一条 —— 那是给自研伴侣应用留的全局闸门，
// 随 D58 一起作废（companion 这条路整条没了，`Source` 上已经没有 kind 字段）。
// 而 id 唯一仍然必须是硬错误：两条同 id 的来源会让 `sources/{appId}.json`
// 这个文件名约定本身失效 —— 先写的赢，后写的不报错，表现是"改了没生效"。
func TestCheckSourceSetRejectsDuplicateID(t *testing.T) {
	mk := func(id string) model.Source {
		return model.Source{ID: id, Name: "n", Author: "a", Source: model.SourceManual}
	}

	rep := model.CheckSourceSet([]model.Source{mk("com.a"), mk("com.a")})
	if !rep.HasErrors() {
		t.Error("两个同 id 的条目应当判失败")
	}

	rep = model.CheckSourceSet([]model.Source{mk("com.a"), mk("com.b")})
	if rep.HasErrors() {
		t.Errorf("id 各不相同应当通过：%v", rep.Errors())
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

// ---- desc：长度上限 ---------------------------------------------------------

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
		t.Error("含换行的 desc 该被拒绝（它渲染成 metadata 的单行 Summary）")
	}

	ok := base
	ok.Desc = strings.Repeat("长", model.MaxDescRunes)
	if err := ok.Validate("com.example.app.json"); err != nil {
		t.Errorf("正好等于上限的 desc 该通过：%v", err)
	}
}
