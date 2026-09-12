package job

import (
	"reflect"
	"strings"
	"testing"

	"github.com/market-of-labs/forge-core/internal/issue"
	"github.com/market-of-labs/forge-core/internal/model"
)

// 这一组测试钉的是 03 §2.5 的六条冲突/归属规则 —— 也就是"一张陌生人写的单子到底
// 能不能改到文件"这件事。它是全流程里**唯一**由不可信输入驱动的一段判断，
// 所以测试从 issue 正文形状开始构造（而不是绕开解析直接塞结构体），
// 否则就测不到"标题对不上"这一类真故障。

// form 把「标题 → 值」拼成 GitHub Issue Form 渲染出来的 markdown 形状。
// 它**照原样**输出，不去重 —— 要构造"同一标题出现两次"的输入时就直接用它。
func form(fields ...[2]string) string {
	var b strings.Builder
	for _, kv := range fields {
		b.WriteString("### " + kv[0] + "\n\n" + kv[1] + "\n\n")
	}
	return b.String()
}

// withFields 把 extra 并进 base：**同名标题覆盖**，新标题追加。
//
// 为什么需要它：正文里出现两个同名标题时，解析规则是"取第一个非空的"（issue.Parse），
// 也就是**先出现的赢**。所以"覆盖一个默认值"只能在构造阶段换掉那一项，
// 不能在后面再补一个同名标题 —— 那样补的那个会被忽略，测试会以"字段没生效"
// 的面目失败，而真正的原因是构造方式错了。
func withFields(base, extra [][2]string) [][2]string {
	out := make([][2]string, 0, len(base)+len(extra))
	for _, kv := range base {
		replaced := false
		for _, ov := range extra {
			if ov[0] == kv[0] {
				out = append(out, ov)
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, kv)
		}
	}
	for _, ov := range extra {
		known := false
		for _, kv := range base {
			if kv[0] == ov[0] {
				known = true
				break
			}
		}
		if !known {
			out = append(out, ov)
		}
	}
	return out
}

// addBody 是一份**合法**的新增申请，extra 用来覆盖已有字段或追加新字段。
//
// 只有 repo 是必填的（03 §2.6）：包名/显示名/作者都被从模板里删掉了，
// 它们由对账阶段从 APK 里读出来 —— 所以这份"合法"的最小形态就是这么短。
func addBody(extra ...[2]string) string {
	base := [][2]string{
		{issue.LabelRepo, "example/app"},
	}
	return form(withFields(base, extra)...)
}

// changeBody 是一份**合法**的变更申请（动作由调用方给）。
func changeBody(appID, action string, extra ...[2]string) string {
	base := [][2]string{
		{issue.LabelTargetAppID, appID},
		{issue.LabelAction, action},
	}
	return form(withFields(base, extra)...)
}

// ctxWith 造一个只装了 sources 的 Ctx。
//
// **不需要 GH / Git / Repo** —— DecideIntake 是纯函数，这正是那条切分的价值：
// 本文件里所有用例都不发一个请求、不建一个临时目录。
func ctxWith(srcs ...model.Source) *Ctx {
	return &Ctx{
		Sources: srcs,
		Index:   &model.Index{Apps: []model.IndexApp{}},
		Log:     func(string, ...any) {},
	}
}

func githubSource(id string) model.Source {
	return model.Source{
		ID:     id,
		Name:   "Example App",
		Author: "Example Org",
		Source: model.SourceGitHub,
		Upstream: &model.Upstream{
			Type:         model.UpstreamGitHubRelease,
			Repo:         "example/app",
			AssetPattern: `(?i)\.apk$`,
		},
		Categories:   []string{"工具"},
		ABIWhitelist: []string{"arm64-v8a"},
	}
}

func manualSource(id string) model.Source {
	return model.Source{ID: id, Name: "Manual App", Author: "Example Org", Source: model.SourceManual}
}

func mustReject(t *testing.T, d *IntakeDecision, wantSubstr string) {
	t.Helper()
	if d.Accept {
		t.Fatalf("期望拒绝，实际通过了：%s\n%s", d.Summary, d.Reply)
	}
	// §2.5 规则 6：拒绝也必须写清原因，且回评不是空壳。
	if wantSubstr != "" && !strings.Contains(d.Reply, wantSubstr) {
		t.Fatalf("拒绝理由里找不到 %q：\n%s", wantSubstr, d.Reply)
	}
	if !strings.Contains(d.Reply, retryHint) {
		t.Fatalf("拒绝回评里没有给出重来一次的办法（retryHint）：\n%s", d.Reply)
	}
}

func mustAccept(t *testing.T, d *IntakeDecision) {
	t.Helper()
	if !d.Accept {
		t.Fatalf("期望通过，实际拒绝了：\n%s", d.Reply)
	}
	if strings.TrimSpace(d.Reply) == "" || strings.TrimSpace(d.Summary) == "" {
		t.Fatalf("通过的回评/摘要不该为空：summary=%q", d.Summary)
	}
}

// mustAcceptAdd 是新增申请**在裁决阶段**唯一的通过形态（03 §2.6）：受理，且**不写文件**。
//
// 它顺带把所有"DecideIntake 会落盘"的回归都挡住：这里 ID 必然是空的，而写一个 ID 为空的
// 来源文件会覆盖掉 `sources/.json`。落盘由 landNewSource 做 —— 那要联网探出身份才谈得上
// （见 newsource.go），所以裁决结果里不可能有。
//
// Reply 为空**不是漏写**：要回评的字段（包名、显示名、作者）此刻还不存在，那段话由
// landedReply 在探完身份之后生成。所以这里断言的是"摘要非空"，不是"回评非空"。
func mustAcceptAdd(t *testing.T, d *IntakeDecision) {
	t.Helper()
	if !d.Accept {
		t.Fatalf("期望通过，实际拒绝了：\n%s", d.Reply)
	}
	if strings.TrimSpace(d.Summary) == "" {
		t.Fatal("摘要不该为空")
	}
	if d.Source == nil || d.Source.ID != "" {
		t.Fatalf("裁决阶段不该带 appId（它还没被读出来）：%+v", d.Source)
	}
	if !d.Hold {
		t.Fatal("新增单必须留在打开状态 —— 关掉它就等于把这张单丢了")
	}
	if d.Kind != issue.KindAdd {
		t.Fatalf("Kind = %v，期望 KindAdd", d.Kind)
	}
}

// ---- 新增 -------------------------------------------------------------------

func TestDecideIntake_AddAccepted(t *testing.T) {
	c := ctxWith()
	d := DecideIntake(c, addBody(
		[2]string{issue.LabelAssetPat, `(?i)release\.apk$`},
		// 勾选项的真实形状：**所有**选项都在，勾中的是大写 X。
		[2]string{issue.LabelCategories, "- [X] 工具 - [ ] 效率 - [X] 媒体 - [ ] 通讯 - [ ] 开发 - [ ] 游戏 - [ ] 其他"},
		[2]string{issue.LabelABIs, "- [X] arm64-v8a - [X] armeabi-v7a - [ ] x86_64 - [ ] x86 - [ ] universal"},
	))
	mustAcceptAdd(t, d)

	s := d.Source
	// 新增一律是 github 源（§2.2：manual 没有"申请"这个动作）。
	if s.Source != model.SourceGitHub {
		t.Fatalf("新增来源的 source 必须是 %q，得到 %q", model.SourceGitHub, s.Source)
	}
	if s.Upstream == nil || s.Upstream.Repo != "example/app" || s.Upstream.Type != model.UpstreamGitHubRelease {
		t.Fatalf("upstream 没建对：%+v", s.Upstream)
	}
	if s.Upstream.AssetPattern != `(?i)release\.apk$` {
		t.Fatalf("资产正则没带上：%q", s.Upstream.AssetPattern)
	}
	if !reflect.DeepEqual(s.Categories, []string{"工具", "媒体"}) {
		t.Fatalf("分类解析错了：%v", s.Categories)
	}
	if !reflect.DeepEqual(s.ABIWhitelist, []string{"arm64-v8a", "armeabi-v7a"}) {
		t.Fatalf("ABI 白名单解析错了：%v", s.ABIWhitelist)
	}
	// 新条目必须是未暂停的（零值即为 false，这里显式钉住语义）。
	if s.Paused {
		t.Fatal("新增的条目不该是 paused")
	}
}

// `_No response_` 是 GitHub 对"可选字段留空"的渲染结果，必须等价于空。
// TestDecideIntake_AddDescIsTruncatedNotRejected 钉住简介的**裁而不拒**（D42）。
//
// 上限是量出来的（model.MaxDescRunes）：它进的是 Obtainium 列表的标题行，
// 那行是单行 + 省略号。之所以裁而不拒 —— 这个字段纯装饰，为一个简介让人重填不值当。
//
// 但**必须说出被裁了**：默默切掉申请人写的东西、再回一句"已收录"，是这套流程里
// 最难被发现的那种假回评（要等到落地后对着 APK 里的显示名才看得见差别）。这句话现在
// 走 DescNote 到 landedReply —— 裁决阶段没有回评可写，所以这里断言的就是 DescNote。
func TestDecideIntake_AddDescIsTruncatedNotRejected(t *testing.T) {
	c := ctxWith()
	long := strings.Repeat("很", model.MaxDescRunes+8)

	d := DecideIntake(c, addBody([2]string{issue.LabelDesc, long}))
	mustAcceptAdd(t, d)
	if n := len([]rune(d.Source.Desc)); n != model.MaxDescRunes {
		t.Fatalf("简介该被裁到 %d 个字，得到 %d 个字：%q", model.MaxDescRunes, n, d.Source.Desc)
	}
	if !strings.Contains(d.DescNote, "截断") {
		t.Errorf("裁过却没在提醒里说（那条提醒要跟着回评出去）：%q", d.DescNote)
	}

	// 不超限：原样保留（只去首尾空白），且不该冒出"被裁了"的噪音。
	d2 := DecideIntake(c, addBody([2]string{issue.LabelDesc, "  去广告的第三方客户端  "}))
	mustAcceptAdd(t, d2)
	if d2.Source.Desc != "去广告的第三方客户端" {
		t.Fatalf("首尾空白该去掉：%q", d2.Source.Desc)
	}
	if strings.Contains(d2.DescNote, "截断") {
		t.Errorf("没超限却说被截断了：%q", d2.DescNote)
	}

	// 留空 = 没有简介，清单里就只有应用名，不该出现一个空的分隔符。
	d3 := DecideIntake(c, addBody())
	mustAcceptAdd(t, d3)
	if d3.Source.Desc != "" || d3.Source.DisplayName() != d3.Source.Name {
		t.Fatalf("没填简介时不该改变显示名：desc=%q name=%q", d3.Source.Desc, d3.Source.DisplayName())
	}
}

func TestDecideIntake_AddOptionalFieldsLeftBlank(t *testing.T) {
	c := ctxWith()
	d := DecideIntake(c, addBody(
		[2]string{issue.LabelAssetPat, "_No response_"},
		[2]string{issue.LabelCategories, "_No response_"},
		[2]string{issue.LabelABIs, "_No response_"},
	))
	mustAcceptAdd(t, d)

	s := d.Source
	if s.Upstream.AssetPattern != "" {
		t.Fatalf("留空的资产正则应当是空串（走默认），得到 %q", s.Upstream.AssetPattern)
	}
	if len(s.Categories) != 0 || len(s.ABIWhitelist) != 0 {
		t.Fatalf("留空的可选列表应当是空的：categories=%v abiWhitelist=%v", s.Categories, s.ABIWhitelist)
	}
	// 身份还没定，所以这里能校验的只有上游那半边 —— 这正是 (*Upstream).Validate
	// 被抽出来的原因（Source.Validate 会因 ID=="" 直接失败）。
	if err := s.Upstream.Validate(); err != nil {
		t.Fatalf("留空可选字段的申请必须仍然合法：%v", err)
	}
}

// 新增侧只剩一个必填字段（repo），而它**同时是**识别模板结构的那个字段（见
// issue.Detect）—— 所以"缺必填"与"认不出是哪份模板"落在同一个出口上，
// 回评只能请人走模板重来。这条测试钉的是这个重合不是巧合。
func TestDecideIntake_AddMissingRepoIsIndistinguishableFromGarbage(t *testing.T) {
	c := ctxWith()
	mustReject(t, DecideIntake(c, form([2]string{issue.LabelAssetPat, `x`})), "无法判断")
}

func TestDecideIntake_AddRejectsBadRepoSlug(t *testing.T) {
	c := ctxWith()
	// 不是 owner/name 形状：repo 会被拼进上游 API URL，所以要在读配置时就挡掉。
	mustReject(t, DecideIntake(c, addBody([2]string{issue.LabelRepo, "justaname"})),
		"申请内容不合法")
}

func TestDecideIntake_AddRejectsBadRegex(t *testing.T) {
	c := ctxWith()
	// 正则非法必须在**收录时**就报出来。留到遍历上游才发现，会等到"某个上游恰好发版"
	// 那一刻才炸，排查成本高得多。
	mustReject(t, DecideIntake(c, addBody([2]string{issue.LabelAssetPat, `[`})),
		"申请内容不合法")
}

func TestDecideIntake_AddRejectsUnknownVocabulary(t *testing.T) {
	c := ctxWith()
	// 勾选项是固定的：手改正文塞一个词表外的值进来要挡掉，否则它会被写进 sources/
	// 而在 Obtainium 的筛选里变成一个空档。
	mustReject(t, DecideIntake(c, addBody([2]string{issue.LabelCategories, "- [X] 摸鱼"})),
		"申请内容不合法")
	mustReject(t, DecideIntake(c, addBody([2]string{issue.LabelABIs, "- [X] riscv64"})),
		"申请内容不合法")
}

// 两套模板的字段混在一张单里：拒绝，不猜。猜错方向的后果是拿「目标 appId」
// 去建一个新的来源文件 —— 那是一次静默的覆盖。
func TestDecideIntake_RejectsMixedTemplates(t *testing.T) {
	c := ctxWith(githubSource("com.example.app"))
	body := form(
		[2]string{issue.LabelRepo, "example/app"},
		[2]string{issue.LabelTargetAppID, "com.example.app"},
		[2]string{issue.LabelAction, issue.ActionPause},
	)
	mustReject(t, DecideIntake(c, body), "无法判断")
}

// 手打 issue（正文没渲染出字段）→ 拒绝，并指向模板。
func TestDecideIntake_RejectsGarbageBody(t *testing.T) {
	c := ctxWith()
	mustReject(t, DecideIntake(c, "求收录一个 App，仓库是 example/app"), "无法判断")
	mustReject(t, DecideIntake(c, ""), "无法判断")
}

// 正文里同一个标题出现两次时**先出现的赢**。这条规则是安全相关的：如果反过来
// （最后出现的赢），任何能在正文里多插一段文本的人都能改掉申请的实际内容。
// 规则的细节在 issue 包里有测试，这里钉的是它在**裁决结果**上的表现。
func TestDecideIntake_DuplicateFieldFirstWins(t *testing.T) {
	c := ctxWith()
	body := form(
		[2]string{issue.LabelRepo, "example/real"},
		// 手工追加的第二段：应当被忽略，而不是覆盖上面那一项。
		[2]string{issue.LabelRepo, "attacker/evil"},
	)
	d := DecideIntake(c, body)
	mustAcceptAdd(t, d)
	if d.Source.Upstream.Repo != "example/real" {
		t.Fatalf("重复字段应当取第一个（用户真正填的那个）：%q", d.Source.Upstream.Repo)
	}
}

// ---- 变更 -------------------------------------------------------------------

func TestDecideIntake_ChangeRejectsUnknownApp(t *testing.T) {
	c := ctxWith()
	mustReject(t, DecideIntake(c, changeBody("com.example.ghost", issue.ActionPause)), "add-source.yml")
}

func TestDecideIntake_ChangeRejectsUnknownAction(t *testing.T) {
	c := ctxWith(githubSource("com.example.app"))
	// 动作是模板里的 dropdown 值；正文可以手改，所以必须显式校验。
	mustReject(t, DecideIntake(c, changeBody("com.example.app", "删库跑路")), "动作")
}

// §2.5 规则 4：移除 = **删掉文件**，不是置标志。
func TestDecideIntake_ChangeRemoveDeletesFile(t *testing.T) {
	c := ctxWith(githubSource("com.example.app"))
	d := DecideIntake(c, changeBody("com.example.app", issue.ActionRemove))
	mustAccept(t, d)

	if !d.Delete {
		t.Fatal("移除必须是 Delete=true（删文件），而不是写一份带标志的文件")
	}
	if d.Source == nil || d.Source.ID != "com.example.app" {
		t.Fatalf("移除也要带上被删的 id：%+v", d.Source)
	}
	// L5 是已知且接受的限制，必须在回评里说清楚 —— 否则报"移除"的人会以为
	// 全世界的手机下次刷新就没了。
	if !strings.Contains(d.Reply, "不会从已安装用户的设备上删除任何东西") {
		t.Fatalf("移除的回评必须说明 L5（不传播到设备端）：\n%s", d.Reply)
	}
}

func TestDecideIntake_ChangePauseOnlyFlipsPaused(t *testing.T) {
	cur := githubSource("com.example.app")
	c := ctxWith(cur)
	d := DecideIntake(c, changeBody("com.example.app", issue.ActionPause))
	mustAccept(t, d)

	if !d.Source.Paused {
		t.Fatal("暂停应当写 paused=true")
	}
	if d.Delete {
		t.Fatal("暂停不该删文件 —— 暂停要保留条目，老用户仍能看到历史版本")
	}
	// 其余字段一个都不许动（§2.5 规则 2）。
	assertSameExceptPaused(t, cur, *d.Source)
}

func TestDecideIntake_ChangeResumeOnlyFlipsPaused(t *testing.T) {
	cur := githubSource("com.example.app")
	cur.Paused = true
	c := ctxWith(cur)
	d := DecideIntake(c, changeBody("com.example.app", issue.ActionResume))
	mustAccept(t, d)

	if d.Source.Paused {
		t.Fatal("恢复应当写 paused=false")
	}
	assertSameExceptPaused(t, cur, *d.Source)
}

// §2.5 规则 2：只改申请涉及的字段 —— 没被提到的连碰都没碰过。
func TestDecideIntake_ChangeEditTouchesOnlyRequestedFields(t *testing.T) {
	cur := githubSource("com.example.app")
	c := ctxWith(cur)
	d := DecideIntake(c, changeBody("com.example.app", issue.ActionEdit,
		[2]string{issue.LabelNewName, "新显示名"},
	))
	mustAccept(t, d)

	got := *d.Source
	if got.Name != "新显示名" {
		t.Fatalf("显示名没改到：%q", got.Name)
	}
	if got.Author != cur.Author {
		t.Fatalf("作者被顺带改了：%q → %q", cur.Author, got.Author)
	}
	if got.Upstream.AssetPattern != cur.Upstream.AssetPattern {
		t.Fatalf("资产正则被顺带改了：%q → %q", cur.Upstream.AssetPattern, got.Upstream.AssetPattern)
	}
	if got.Desc != cur.Desc {
		t.Fatalf("简介被顺带改了：%q → %q", cur.Desc, got.Desc)
	}
	if !reflect.DeepEqual(got.Categories, cur.Categories) {
		t.Fatalf("分类被顺带改了：%v → %v", cur.Categories, got.Categories)
	}
	if !reflect.DeepEqual(got.ABIWhitelist, cur.ABIWhitelist) {
		t.Fatalf("ABI 白名单被顺带改了：%v → %v", cur.ABIWhitelist, got.ABIWhitelist)
	}
	if got.Paused != cur.Paused {
		t.Fatal("暂停状态被顺带改了")
	}
}

// TestDecideIntake_ChangeEditDesc 覆盖"新的简介"这一格，重点是两件容易写错的事：
// 一个是**裁而不拒**，另一个是回评里报出来的值必须是**最终生效的那个** ——
// 申请人填了 30 个字、回评却说"简介 → 30 个字"，而文件里存的是 20 个字，
// 这种对不上只有几个月后有人去翻 sources/ 才会发现。
func TestDecideIntake_ChangeEditDesc(t *testing.T) {
	cur := githubSource("com.example.app")
	cur.Desc = "旧简介"
	c := ctxWith(cur)

	// 前 20 个字之后挂一个**只在原文里存在**的尾巴：这样"回评报的是裁过的值还是原文"
	// 才是可判定的（两者都含那 20 个"很"，只有原文含尾巴）。
	long := strings.Repeat("很", model.MaxDescRunes) + "尾巴在这儿"
	d := DecideIntake(c, changeBody("com.example.app", issue.ActionEdit,
		[2]string{issue.LabelNewDesc, long},
	))
	mustAccept(t, d)
	if n := len([]rune(d.Source.Desc)); n != model.MaxDescRunes {
		t.Fatalf("简介该被裁到 %d 个字，得到 %d：%q", model.MaxDescRunes, n, d.Source.Desc)
	}
	if strings.Contains(d.Reply, "尾巴在这儿") {
		t.Errorf("回评里报的是**没裁过的**原文 —— 回评与落盘的值对不上：\n%s", d.Reply)
	}
	// 别动显示名：显示名是 Name，简介是 Desc，它在清单里才被拼起来。
	if d.Source.Name != cur.Name {
		t.Fatalf("改简介顺带改了显示名：%q → %q", cur.Name, d.Source.Name)
	}

	// 留空 = 不改（模板表达不了"清空"），旧简介原样留着。
	d2 := DecideIntake(c, changeBody("com.example.app", issue.ActionEdit,
		[2]string{issue.LabelNewName, "只改名字"},
	))
	mustAccept(t, d2)
	if d2.Source.Desc != "旧简介" {
		t.Fatalf("没申请改简介，它却被动了：%q", d2.Source.Desc)
	}
}

// §2.5 规则 3：source 种类**不可自动变更**。这里不是"检查后放行"，而是结构上
// 不可能发生 —— change 模板里没有任何字段能表达它。测试要钉住的就是这个"不可能"。
func TestDecideIntake_ChangeEditNeverChangesSourceKind(t *testing.T) {
	t.Run("manual 保持 manual 且不长出 upstream", func(t *testing.T) {
		c := ctxWith(manualSource("com.example.manual"))
		d := DecideIntake(c, changeBody("com.example.manual", issue.ActionEdit,
			[2]string{issue.LabelNewName, "改了名字"},
			[2]string{issue.LabelNewAuthor, "改了作者"},
		))
		mustAccept(t, d)
		if d.Source.Source != model.SourceManual {
			t.Fatalf("source 被改成了 %q", d.Source.Source)
		}
		if d.Source.Upstream != nil {
			t.Fatalf("manual 源不该凭空长出 upstream：%+v", d.Source.Upstream)
		}
	})

	t.Run("github 保持 github 且不掉 upstream", func(t *testing.T) {
		c := ctxWith(githubSource("com.example.app"))
		d := DecideIntake(c, changeBody("com.example.app", issue.ActionEdit,
			[2]string{issue.LabelNewName, "改了名字"},
		))
		mustAccept(t, d)
		if d.Source.Source != model.SourceGitHub || d.Source.Upstream == nil {
			t.Fatalf("github 源被改坏了：source=%q upstream=%+v", d.Source.Source, d.Source.Upstream)
		}
	})
}

// manual 源没有上游，"资产匹配正则"对它是个不存在的概念。静默忽略会让人以为改成功了。
func TestDecideIntake_ChangeEditManualAssetPatternRejected(t *testing.T) {
	c := ctxWith(manualSource("com.example.manual"))
	mustReject(t, DecideIntake(c, changeBody("com.example.manual", issue.ActionEdit,
		[2]string{issue.LabelNewAssetPat, `\.apk$`},
	)), "没有上游")
}

// 一个字段都没填的「修改元数据」= 无事可做，拒绝并回显当前值。
func TestDecideIntake_ChangeEditNoFieldsRejected(t *testing.T) {
	c := ctxWith(githubSource("com.example.app"))
	mustReject(t, DecideIntake(c, changeBody("com.example.app", issue.ActionEdit)),
		"没有填任何新值")
}

// 改完之后**不合法**的内容一律放弃（改一半再落盘比不改更糟）。
func TestDecideIntake_ChangeEditRejectsInvalidResult(t *testing.T) {
	c := ctxWith(githubSource("com.example.app"))
	mustReject(t, DecideIntake(c, changeBody("com.example.app", issue.ActionEdit,
		[2]string{issue.LabelNewAssetPat, `[`},
	)), "改完之后的内容不合法")
}

// 只有非"修改元数据"的动作才填的字段，在别的动作下不该生效。
func TestDecideIntake_ChangeNewFieldsIgnoredOutsideEdit(t *testing.T) {
	cur := githubSource("com.example.app")
	c := ctxWith(cur)
	d := DecideIntake(c, changeBody("com.example.app", issue.ActionPause,
		[2]string{issue.LabelNewName, "偷偷改个名字"},
	))
	mustAccept(t, d)
	if d.Source.Name != cur.Name {
		t.Fatalf("「暂停更新」动作不该顺带改名：%q → %q", cur.Name, d.Source.Name)
	}
}

// 裁决是纯函数：它不能改动已经加载进来的 sources，也不能让返回值与
// c.Sources 共享可写的子结构 —— 否则后续任何一处对 d.Source 的写入
// 都会静默地改到"内存里的那份配置"，而那份配置后面还要被复用。
func TestDecideIntake_DoesNotMutateSources(t *testing.T) {
	cur := githubSource("com.example.app")
	c := ctxWith(cur)
	before := c.Sources[0]

	d := DecideIntake(c, changeBody("com.example.app", issue.ActionEdit,
		[2]string{issue.LabelNewName, "新显示名"},
		[2]string{issue.LabelNewAssetPat, `release\.apk$`},
	))
	mustAccept(t, d)

	if !reflect.DeepEqual(c.Sources[0], before) {
		t.Fatalf("DecideIntake 改了 c.Sources：\nbefore=%+v\nafter =%+v", before, c.Sources[0])
	}
	if d.Source.Upstream == cur.Upstream {
		t.Fatal("裁决结果与已加载的 source 共享了 Upstream 指针 —— 改它就会改到内存里的配置")
	}
}

// 同样的输入必须得到同样的结论：Go map 的遍历顺序不该漏进裁决里。
func TestDecideIntake_Deterministic(t *testing.T) {
	body := addBody(
		[2]string{issue.LabelCategories, "- [X] 工具 - [X] 效率 - [ ] 媒体"},
		[2]string{issue.LabelABIs, "- [X] x86 - [X] arm64-v8a - [X] universal"},
	)
	first := DecideIntake(ctxWith(), body)
	for i := 0; i < 20; i++ {
		got := DecideIntake(ctxWith(), body)
		if got.Summary != first.Summary || !reflect.DeepEqual(got.Source, first.Source) {
			t.Fatalf("第 %d 次跑出了不同的结论：\n%+v\n%+v", i, first.Source, got.Source)
		}
	}
}

// assertSameExceptPaused 断言两份 source 除了 Paused 之外逐字段相等。
func assertSameExceptPaused(t *testing.T, want, got model.Source) {
	t.Helper()
	w, g := want, got
	w.Paused, g.Paused = false, false
	if !reflect.DeepEqual(w, g) {
		t.Fatalf("除 paused 之外还有字段被改动了：\nwant=%+v\ngot =%+v", w, g)
	}
}
