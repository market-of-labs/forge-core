package issue_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/market-of-labs/forge-core/internal/issue"
	"github.com/market-of-labs/forge-core/internal/model"
	"github.com/market-of-labs/forge-core/internal/naming"
)

// addBody 是 GitHub 把 add-source.yml 渲染出来的真实形状（照抄一份填好的 issue）。
//
// ⚠️ 勾选项那一行的形状是**实测**的：GitHub 把 checkboxes 渲染成任务列表，
// 而且是**所有选项都在**、勾中的标 `- [X]`（大写 X）、未勾的标 `- [ ]`；
// 随后 cleanValue 把换行折叠成空格，于是到了解析器手里是一整行。
// 照抄时不许"顺手美化"成每项一行 —— 那测的就不是真实输入了。
const addBody = `### 上游 GitHub 仓库

ImranR98/Obtainium

### 资产匹配正则（可选）

app.*\.apk$

### 一句话简介（可选）

去广告的第三方客户端

### 分类标签（可选）

- [X] 工具 - [ ] 效率 - [X] 媒体 - [ ] 通讯 - [ ] 开发 - [ ] 游戏 - [ ] 其他

### 只镜像哪些 ABI（可选）

- [X] arm64-v8a - [ ] armeabi-v7a - [ ] x86_64 - [ ] x86 - [X] universal
`

// changeBody 同上，对应 change-source.yml，且是可选项全空的形态。
const changeBody = `### 目标 appId

dev.imranr.obtainium

### 动作

暂停更新

### 新的显示名（仅「修改元数据」时填）

_No response_

### 新的作者 / 组织（仅「修改元数据」时填）

_No response_

### 新的资产匹配正则（仅「修改元数据」时填）

_No response_

### 新的简介（仅「修改元数据」时填）

_No response_

### 新的分类标签（仅「修改元数据」时填）

_No response_

### 说明（可选）

_No response_
`

func TestParseBasicFields(t *testing.T) {
	f := issue.Parse(addBody)

	if got := f.Get(issue.LabelRepo); got != "ImranR98/Obtainium" {
		t.Errorf("repo = %q", got)
	}
	// 正则里的反斜杠必须原样保留 —— 它是正则的一部分，被吃掉就匹配不上了。
	if got := f.Get(issue.LabelAssetPat); got != `app.*\.apk$` {
		t.Errorf("assetPattern = %q（反斜杠被吃掉了？）", got)
	}
}

// TestAddRequestHasNoDerivedFields 钉住"申请人不填派生字段"这条（03 §2.6）。
//
// 它测的是**编译期**的事：一旦有人把 AppID/Name/Author 加回 AddRequest，
// 这里就要跟着改，改动本身就会被看见。
func TestAddRequestHasNoDerivedFields(t *testing.T) {
	r, err := issue.ParseAdd(issue.Parse(addBody))
	if err != nil {
		t.Fatalf("ParseAdd：%v", err)
	}
	if r.Repo != "ImranR98/Obtainium" {
		t.Errorf("repo = %q", r.Repo)
	}
	if r.AssetPattern != `app.*\.apk$` {
		t.Errorf("assetPattern = %q", r.AssetPattern)
	}
	// 简介同属"申请人自己知道的东西"，所以它是保留的。
	if r.Desc != "去广告的第三方客户端" {
		t.Errorf("desc = %q", r.Desc)
	}
}

// TestParseChangeDesc 钉住"新的简介"在变更侧的**指针语义**：留空 = 不改。
//
// 这是 §2.5 规则 2（只改申请涉及的字段）在简介上的落点，也是它与
// NewCategories（空切片 = 不改）唯一不同的一点 —— 简介是标量，必须用指针区分。
func TestParseChangeDesc(t *testing.T) {
	empty, err := issue.ParseChange(issue.Parse(changeBody))
	if err != nil {
		t.Fatalf("ParseChange：%v", err)
	}
	if empty.NewDesc != nil {
		t.Errorf("模板里未填简介，NewDesc 应为 nil（= 不改），实际 %q", *empty.NewDesc)
	}

	// "新值"那批字段只在 Action == 修改元数据 时才被读，所以这里连动作一起换。
	filled := strings.Replace(changeBody, "暂停更新", "修改元数据", 1)
	filled = strings.Replace(filled,
		"### 新的简介（仅「修改元数据」时填）\n\n_No response_",
		"### 新的简介（仅「修改元数据」时填）\n\n去广告", 1)
	if filled == changeBody {
		t.Fatal("fixture 没有被改动 —— 两处替换都没命中，下面测的其实还是空形态")
	}
	r, err := issue.ParseChange(issue.Parse(filled))
	if err != nil {
		t.Fatalf("ParseChange：%v", err)
	}
	if r.NewDesc == nil || *r.NewDesc != "去广告" {
		t.Fatalf("NewDesc = %v，期望「去广告」", r.NewDesc)
	}
	if !strings.Contains(r.Summary(), "简介") {
		t.Errorf("Summary 里没提到简介：%q —— 回评会漏报这次改动", r.Summary())
	}
}

func TestCheckedReadsTaskList(t *testing.T) {
	f := issue.Parse(addBody)

	got := f.Checked(issue.LabelCategories)
	want := []string{"工具", "媒体"}
	if !slices.Equal(got, want) {
		t.Errorf("categories = %v，期望 %v", got, want)
	}

	// 小写 x 也是勾中：有人手工编辑正文时会打成小写，而 GitHub 自己渲染的是大写。
	f2 := issue.Parse("### 分类标签（可选）\n\n- [x] 工具 - [ ] 效率\n")
	if got := f2.Checked(issue.LabelCategories); !slices.Equal(got, []string{"工具"}) {
		t.Errorf("小写 x 未被认作勾选：%v", got)
	}

	// 一个都没勾（或整段空着）返回 nil，而不是一个含空串的切片 ——
	// 上层用 len()==0 判"没勾"，两种都得满足。
	for _, body := range []string{
		"### 分类标签（可选）\n\n- [ ] 工具 - [ ] 效率\n",
		"### 分类标签（可选）\n\n_No response_\n",
		"",
	} {
		if got := issue.Parse(body).Checked(issue.LabelCategories); got != nil {
			t.Errorf("没有勾选时应当返回 nil，得到 %v（%q）", got, body)
		}
	}
}

func TestNoResponseIsEmpty(t *testing.T) {
	// 整段是 _No response_ 的字段（这里借变更单的"说明"）应当被归成空串。
	if got := issue.Parse(changeBody).Get(issue.LabelReason); got != "" {
		t.Errorf("未填字段应当是空串，得到 %q", got)
	}
	// 但"整段的确是 _No response_"与"值里含 _No response_"是两回事。
	f2 := issue.Parse("### 说明（可选）\n\n见 _No response_ 的说明\n")
	if got := f2.Get(issue.LabelReason); got == "" {
		t.Error("值里含 _No response_ 时不该被判成空")
	}
}

func TestParseChangePauseLeavesFieldsNil(t *testing.T) {
	f := issue.Parse(changeBody)
	r, err := issue.ParseChange(f)
	if err != nil {
		t.Fatalf("ParseChange：%v", err)
	}
	if r.Action != issue.ActionPause {
		t.Errorf("action = %q", r.Action)
	}
	// 这条是 §2.5 规则 2 的核心：非"修改元数据"的申请，三个新值字段必须是 nil，
	// 这样写回 sources/ 时才不会把既有值覆盖成空。
	if r.NewName != nil || r.NewAuthor != nil || r.NewAssetPat != nil {
		t.Errorf("暂停更新时不该带上新值：name=%v author=%v pat=%v",
			r.NewName, r.NewAuthor, r.NewAssetPat)
	}
	if len(r.NewCategories) != 0 {
		t.Errorf("暂停更新时不该带分类：%v", r.NewCategories)
	}
}

func TestParseChangeEditSetsOnlyFilledFields(t *testing.T) {
	body := `### 目标 appId

dev.imranr.obtainium

### 动作

修改元数据

### 新的显示名（仅「修改元数据」时填）

Obtainium 增强版

### 新的作者 / 组织（仅「修改元数据」时填）

_No response_

### 新的资产匹配正则（仅「修改元数据」时填）

_No response_

### 新的分类标签（仅「修改元数据」时填，逗号分隔）

_No response_

### 说明（可选）

改名了
`
	r, err := issue.ParseChange(issue.Parse(body))
	if err != nil {
		t.Fatalf("ParseChange：%v", err)
	}
	if r.NewName == nil || *r.NewName != "Obtainium 增强版" {
		t.Errorf("name = %v", r.NewName)
	}
	// 填了的进，没填的不进 —— 这条决定了写回时会不会误伤别的字段。
	if r.NewAuthor != nil {
		t.Errorf("author 没填，不该有值：%v", *r.NewAuthor)
	}
	if r.NewAssetPat != nil {
		t.Errorf("assetPattern 没填，不该有值：%v", *r.NewAssetPat)
	}
	if len(r.NewCategories) != 0 {
		t.Errorf("categories 没填，不该有值：%v", r.NewCategories)
	}
}

func TestRequiredNamesTheExpectedHeading(t *testing.T) {
	f := issue.Parse("### 目标 appId\n\nx\n")
	_, err := issue.ParseAdd(f)
	if err == nil {
		t.Fatal("缺字段时该报错")
	}
	// 错误信息必须告诉申请者**该看到哪个标题**，否则他无从下手。
	if !strings.Contains(err.Error(), issue.LabelRepo) {
		t.Errorf("错误信息里没有点出期待的字段标题：%v", err)
	}
}

func TestParseIsTolerantOfUserProse(t *testing.T) {
	// 用户很可能在正文最前面写几行话，甚至写个 `# 标题`。
	// 只有 `##`～`####` 才算字段标题，markdown 的 `#` 不算。
	body := `# 我想申请一个应用

顺便说一下这个应用很好用。

### 上游 GitHub 仓库

ImranR98/Obtainium
`
	r, err := issue.ParseAdd(issue.Parse(body))
	if err != nil {
		t.Fatalf("ParseAdd：%v", err)
	}
	if r.Repo != "ImranR98/Obtainium" {
		t.Errorf("repo = %q", r.Repo)
	}
}

func TestDuplicateLabelTakesFirst(t *testing.T) {
	// 有人在正文里手工插了一段重复字段。取值必须是**确定**的：取模板渲染的那个
	// （也就是维护者在渲染出来的正文里最先看到的那个），而不是后面那个。
	body := `### 上游 GitHub 仓库

real/app

### 上游 GitHub 仓库

evil/app
`
	if got := issue.Parse(body).Get(issue.LabelRepo); got != "real/app" {
		t.Errorf("重复字段应当取第一个，得到 %q", got)
	}
}

func TestDetectByStructureNotTitle(t *testing.T) {
	// 标题被用户改花了也没关系：结构决定类型。
	if k, ok := issue.Parse(addBody).Detect(); !ok || k != issue.KindAdd {
		t.Errorf("addBody → (%v, %v)", k, ok)
	}
	if k, ok := issue.Parse(changeBody).Detect(); !ok || k != issue.KindChange {
		t.Errorf("changeBody → (%v, %v)", k, ok)
	}
}

func TestDetectRefusesAmbiguous(t *testing.T) {
	// 两套字段都在（两份模板被拼在了一起）或一套都不在（手打的）都要拒绝，不猜：
	// 猜错方向的后果是拿"目标 appId"去新建一个来源文件。
	for _, body := range []string{
		"### 上游 GitHub 仓库\n\na/b\n\n### 目标 appId\n\nb\n",
		"### 随便什么标题\n\n一段自由发挥的文字\n",
	} {
		if _, ok := issue.Parse(body).Detect(); ok {
			t.Errorf("应当判定为不可识别：%q", body)
		}
	}
}

func TestKnownAction(t *testing.T) {
	for _, a := range []string{issue.ActionEdit, issue.ActionPause, issue.ActionResume, issue.ActionRemove} {
		if !issue.KnownAction(a) {
			t.Errorf("%q 应当被认作合法动作", a)
		}
	}
	if issue.KnownAction("删除") {
		t.Error("模板里没有「删除」这个动作 —— 它对应的是「移除」")
	}
}

// ---- 与真实模板对齐 ---------------------------------------------------------

var yamlLabel = regexp.MustCompile(`(?m)^\s*label:\s*(.+?)\s*$`)

// TestLabelsMatchStoreTemplates 直接读 store 仓库里的两份 issue 模板 yml，把常量和
// 它们的 label 对上。
//
// 存在的理由：GitHub 渲染正文时用的是 **label**，而这些 label 是中文长串、带全角
// 括号。任何一次"顺手改个措辞"都会让所有申请静默判为缺字段 —— 表现是"没人能提交成功"，
// 而不是一个解析报错。这条测试把那种改动的反馈提前到本地。
func TestLabelsMatchStoreTemplates(t *testing.T) {
	root := filepath.Join("..", "..", "..", "store", ".github", "ISSUE_TEMPLATE")
	if _, err := os.Stat(root); err != nil {
		t.Skipf("同级没有 store 仓库（%s）——跳过模板对齐测试", root)
	}

	cases := []struct {
		file   string
		labels []string
	}{
		{"add-source.yml", []string{
			issue.LabelRepo, issue.LabelAssetPat, issue.LabelDesc,
			issue.LabelCategories, issue.LabelABIs,
		}},
		{"change-source.yml", []string{
			issue.LabelTargetAppID, issue.LabelAction, issue.LabelNewName,
			issue.LabelNewAuthor, issue.LabelNewAssetPat, issue.LabelNewDesc,
			issue.LabelNewCats, issue.LabelReason,
		}},
	}

	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join(root, tc.file))
			if err != nil {
				t.Fatalf("读 %s：%v", tc.file, err)
			}
			got := map[string]bool{}
			for _, m := range yamlLabel.FindAllStringSubmatch(string(b), -1) {
				got[unquote(m[1])] = true
			}
			if len(got) == 0 {
				t.Fatalf("%s 里一个 label 都没解析出来 —— 正则或文件形状变了", tc.file)
			}
			for _, want := range tc.labels {
				if !got[want] {
					t.Errorf("%s 里没有 label %q。\n模板里的 label 有：%v\n"+
						"（改了模板就必须同步改 internal/issue/fields.go 里的常量，否则申请会全部判为缺字段）",
						tc.file, want, keys(got))
				}
			}
		})
	}
}

// optionLine 匹配 GitHub issue form 里 `- label: xxx` 那一行（缩进随意）。
var optionLine = regexp.MustCompile(`(?m)^\s*-\s*label:\s*(.+?)\s*$`)

// TestCheckboxVocabularyMatchesGo 钉住勾选项的词表：模板里的 options 必须正好是
// Go 侧那两个固定集，一个不多一个不少。
//
// 存在的理由有两层：
//
//  1. 词表是**校验**用的 —— job.validateAdd 拿 model.ReviewCategories / naming.ABISet
//     判"勾上来的这一项合不合法"。模板多一项就是"申请人能勾、但一定被拒"，
//     少一项就是"这项其实支持，但没人勾得到"。
//  2. 空间约束 —— issue.checkboxItem 用 `[^\s\]]+` 取选项名，所以**选项里不能有空格**。
//     这不是理论问题：`arm64-v8a` 这种名字里若有人改成 `arm64 v8a`，解析会静默地
//     只取到 `arm64`，然后被当成非法 ABI 拒掉。宁可在这里先炸。
//
// 比的是**集合**而不是切片：ABISet 的顺序是清单里 apkUrls 的排序位次（见其文档），
// 而模板里 options 的顺序只影响表单的展示，两者没有理由被绑在一起。
func TestCheckboxVocabularyMatchesGo(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "store", ".github", "ISSUE_TEMPLATE")
	want := map[string]bool{}
	for _, c := range model.ReviewCategories {
		want[c] = true
	}
	for _, a := range naming.ABISet {
		want[a] = true
	}

	for _, file := range []string{"add-source.yml", "change-source.yml"} {
		b, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			t.Skipf("读不到 %s —— 跳过", file)
		}
		got := map[string]bool{}
		for _, m := range optionLine.FindAllStringSubmatch(string(b), -1) {
			v := unquote(m[1])
			got[v] = true
			if strings.ContainsAny(v, " \t") {
				t.Errorf("%s 的选项 %q 里有空格 —— issue.checkboxItem 的 `[^\\s\\]]+` 会在空格处停下，"+
					"解析出来的将是一个残缺的名字", file, v)
			}
		}
		if len(got) == 0 {
			t.Fatalf("%s 里一个 `- label:` 都没解析出来", file)
		}

		// **两边差集都要空**：模板多出的项 = 勾了必被拒，Go 多出的项 = 没人勾得到。
		// 变更单只用到分类词表，所以它的选项是 want 的子集；新增单两者都有。
		inGoNotYAML := map[string]bool{}
		for v := range want {
			if !got[v] {
				inGoNotYAML[v] = true
			}
		}
		for v := range got {
			if !want[v] {
				t.Errorf("%s 里的选项 %q 不在 Go 的固定集内 —— 勾了也会被 validateAdd 拒掉", file, v)
			}
		}
		// 新增单必须**用满**整个词表；变更单只有分类那一组，它缺的项必须都是 ABI。
		for v := range inGoNotYAML {
			if file == "add-source.yml" {
				t.Errorf("add-source.yml 里缺选项 %q —— 它在 Go 的固定集里，但申请人勾不到", v)
				continue
			}
			if !naming.IsABI(v) {
				t.Errorf("%s 里缺分类选项 %q", file, v)
			}
		}
	}
}

// TestActionLabelsMatchTemplate 钉住 dropdown 的四个取值。
func TestActionLabelsMatchTemplate(t *testing.T) {
	path := filepath.Join("..", "..", "..", "store", ".github", "ISSUE_TEMPLATE", "change-source.yml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("读不到 %s —— 跳过", path)
	}
	body := string(b)
	// 只取 dropdown 的 options 段，避免匹配到别处。
	i := strings.Index(body, "id: action")
	if i < 0 {
		t.Fatal("change-source.yml 里找不到 id: action")
	}
	seg := body[i:]
	if j := strings.Index(seg, "validations:"); j >= 0 {
		seg = seg[:j]
	}
	for _, a := range []string{issue.ActionEdit, issue.ActionPause, issue.ActionResume, issue.ActionRemove} {
		if !strings.Contains(seg, a) {
			t.Errorf("模板的 action 下拉里没有 %q —— 常量与模板分叉了", a)
		}
	}
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
