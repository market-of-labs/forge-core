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
func addBody(appID string, extra ...[2]string) string {
	base := [][2]string{
		{issue.LabelAppID, appID},
		{issue.LabelName, "Example App"},
		{issue.LabelAuthor, "Example Org"},
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

// ---- 新增 -------------------------------------------------------------------

func TestDecideIntake_AddAccepted(t *testing.T) {
	c := ctxWith()
	d := DecideIntake(c, addBody("com.example.newapp",
		[2]string{issue.LabelAssetPat, `(?i)release\.apk$`},
		[2]string{issue.LabelCategories, "工具, 效率"},
		[2]string{issue.LabelABIs, "arm64-v8a、armeabi-v7a"},
	))
	mustAccept(t, d)

	s := d.Source
	if s.ID != "com.example.newapp" || s.Name != "Example App" || s.Author != "Example Org" {
		t.Fatalf("基本字段没落对：%+v", s)
	}
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
	// 中英文逗号/顿号都是分隔符（表单说明写的是"逗号分隔"，中文输入法下全角太常见）。
	if !reflect.DeepEqual(s.Categories, []string{"工具", "效率"}) {
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
func TestDecideIntake_AddOptionalFieldsLeftBlank(t *testing.T) {
	c := ctxWith()
	d := DecideIntake(c, addBody("com.example.newapp",
		[2]string{issue.LabelAssetPat, "_No response_"},
		[2]string{issue.LabelCategories, "_No response_"},
		[2]string{issue.LabelABIs, "_No response_"},
	))
	mustAccept(t, d)

	s := d.Source
	if s.Upstream.AssetPattern != "" {
		t.Fatalf("留空的资产正则应当是空串（走默认），得到 %q", s.Upstream.AssetPattern)
	}
	if len(s.Categories) != 0 || len(s.ABIWhitelist) != 0 {
		t.Fatalf("留空的可选列表应当是空的：categories=%v abiWhitelist=%v", s.Categories, s.ABIWhitelist)
	}
	if err := s.Validate(""); err != nil {
		t.Fatalf("留空可选字段的申请必须仍然合法：%v", err)
	}
}

// §2.5 规则 5：add 模板只用于**新** id。已存在的必须走 change 模板。
func TestDecideIntake_AddRejectsExistingID(t *testing.T) {
	c := ctxWith(githubSource("com.example.app"))
	mustReject(t, DecideIntake(c, addBody("com.example.app")), "change-source.yml")
}

func TestDecideIntake_AddRejectsMissingRequired(t *testing.T) {
	c := ctxWith()
	// 缺「显示名」。
	body := form(
		[2]string{issue.LabelAppID, "com.example.newapp"},
		[2]string{issue.LabelAuthor, "Example Org"},
		[2]string{issue.LabelRepo, "example/app"},
	)
	mustReject(t, DecideIntake(c, body), "显示名")
}

func TestDecideIntake_AddRejectsBadRepoSlug(t *testing.T) {
	c := ctxWith()
	// 不是 owner/name 形状：repo 会被拼进上游 API URL，所以要在读配置时就挡掉。
	mustReject(t, DecideIntake(c, addBody("com.example.newapp", [2]string{issue.LabelRepo, "justaname"})),
		"申请内容不合法")
}

func TestDecideIntake_AddRejectsBadRegex(t *testing.T) {
	c := ctxWith()
	// 正则非法必须在**收录时**就报出来。留到遍历上游才发现，会等到"某个上游恰好发版"
	// 那一刻才炸，排查成本高得多。
	mustReject(t, DecideIntake(c, addBody("com.example.newapp", [2]string{issue.LabelAssetPat, `[`})),
		"申请内容不合法")
}

func TestDecideIntake_AddRejectsUnknownABI(t *testing.T) {
	c := ctxWith()
	mustReject(t, DecideIntake(c, addBody("com.example.newapp", [2]string{issue.LabelABIs, "riscv64"})),
		"申请内容不合法")
}

func TestDecideIntake_AddRejectsReservedID(t *testing.T) {
	c := ctxWith()
	// `_incoming` 是手动上传的暂存 Release 的 tag（03 §3.1），不是合法 appId。
	mustReject(t, DecideIntake(c, addBody("_incoming")), "申请内容不合法")
}

// 两套模板的字段混在一张单里：拒绝，不猜。猜错方向的后果是拿「目标 appId」
// 去建一个新的来源文件 —— 那是一次静默的覆盖。
func TestDecideIntake_RejectsMixedTemplates(t *testing.T) {
	c := ctxWith(githubSource("com.example.app"))
	body := form(
		[2]string{issue.LabelAppID, "com.example.app"},
		[2]string{issue.LabelTargetAppID, "com.example.app"},
		[2]string{issue.LabelName, "Example App"},
		[2]string{issue.LabelAuthor, "Example Org"},
		[2]string{issue.LabelRepo, "example/app"},
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
		[2]string{issue.LabelAppID, "com.example.real"},
		[2]string{issue.LabelName, "Real Name"},
		[2]string{issue.LabelAuthor, "Real Org"},
		[2]string{issue.LabelRepo, "example/real"},
		// 手工追加的第二段：应当被忽略，而不是覆盖上面那一项。
		[2]string{issue.LabelRepo, "attacker/evil"},
	)
	d := DecideIntake(c, body)
	mustAccept(t, d)
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
	body := addBody("com.example.newapp",
		[2]string{issue.LabelCategories, "工具, 效率, 网络"},
		[2]string{issue.LabelABIs, "x86, arm64-v8a, universal"},
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
