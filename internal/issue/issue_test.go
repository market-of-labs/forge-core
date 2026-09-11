package issue_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/market-of-labs/forge-core/internal/issue"
)

// addBody 是 GitHub 把 add-source.yml 渲染出来的真实形状（照抄一份填好的 issue）。
const addBody = `### 应用包名（appId）

dev.imranr.obtainium

### 显示名

Obtainium

### 作者 / 组织

ImranR98

### 上游 GitHub 仓库

ImranR98/Obtainium

### 资产匹配正则（可选）

app.*\.apk$

### 分类标签（可选）

工具,效率

### 只镜像哪些 ABI（可选）

_No response_
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

### 新的分类标签（仅「修改元数据」时填，逗号分隔）

_No response_

### 说明（可选）

_No response_
`

func TestParseBasicFields(t *testing.T) {
	f := issue.Parse(addBody)

	if got := f.Get(issue.LabelAppID); got != "dev.imranr.obtainium" {
		t.Errorf("appId = %q", got)
	}
	if got := f.Get(issue.LabelName); got != "Obtainium" {
		t.Errorf("name = %q", got)
	}
	if got := f.Get(issue.LabelAuthor); got != "ImranR98" {
		t.Errorf("author = %q", got)
	}
	if got := f.Get(issue.LabelRepo); got != "ImranR98/Obtainium" {
		t.Errorf("repo = %q", got)
	}
	// 正则里的反斜杠必须原样保留 —— 它是正则的一部分，被吃掉就匹配不上了。
	if got := f.Get(issue.LabelAssetPat); got != `app.*\.apk$` {
		t.Errorf("assetPattern = %q（反斜杠被吃掉了？）", got)
	}
}

func TestNoResponseIsEmpty(t *testing.T) {
	f := issue.Parse(addBody)
	if got := f.Get(issue.LabelABIs); got != "" {
		t.Errorf("未填的 ABI 字段应当是空串，得到 %q", got)
	}
	// 但"整段的确是 _No response_"与"值里含 _No response_"是两回事。
	f2 := issue.Parse("### 说明（可选）\n\n见 _No response_ 的说明\n")
	if got := f2.Get(issue.LabelReason); got == "" {
		t.Error("值里含 _No response_ 时不该被判成空")
	}
}

func TestListSplitsBothCommaStyles(t *testing.T) {
	f := issue.Parse(addBody)
	got := f.List(issue.LabelCategories)
	if len(got) != 2 || got[0] != "工具" || got[1] != "效率" {
		t.Errorf("categories = %v", got)
	}

	// 全角逗号、顿号、分号都要能分隔：中文输入法下这几个人人都会打出来。
	f2 := issue.Parse("### 分类标签（可选）\n\n工具，效率、网络;测试\n")
	got2 := f2.List(issue.LabelCategories)
	if len(got2) != 4 {
		t.Errorf("混用分隔符时 categories = %v，期望 4 项", got2)
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
	if !strings.Contains(err.Error(), issue.LabelAppID) {
		t.Errorf("错误信息里没有点出期待的字段标题：%v", err)
	}
}

func TestParseIsTolerantOfUserProse(t *testing.T) {
	// 用户很可能在正文最前面写几行话，甚至写个 `# 标题`。
	// 只有 `##`～`####` 才算字段标题，markdown 的 `#` 不算。
	body := `# 我想申请一个应用

顺便说一下这个应用很好用。

### 应用包名（appId）

dev.imranr.obtainium

### 显示名

Obtainium

### 作者 / 组织

ImranR98

### 上游 GitHub 仓库

ImranR98/Obtainium
`
	r, err := issue.ParseAdd(issue.Parse(body))
	if err != nil {
		t.Fatalf("ParseAdd：%v", err)
	}
	if r.AppID != "dev.imranr.obtainium" {
		t.Errorf("appId = %q", r.AppID)
	}
}

func TestDuplicateLabelIsReported(t *testing.T) {
	// 有人在正文里手工插了一段重复字段。取第一个（模板渲染的那个），
	// 但要把这件事报到 Duplicates 上，由上层决定是否拒绝。
	body := `### 应用包名（appId）

dev.real.app

### 应用包名（appId）

dev.evil.app
`
	f := issue.Parse(body)
	if got := f.Get(issue.LabelAppID); got != "dev.real.app" {
		t.Errorf("重复字段应当取第一个，得到 %q", got)
	}
	if len(f.Duplicates) != 1 || f.Duplicates[0] != issue.LabelAppID {
		t.Errorf("Duplicates = %v", f.Duplicates)
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
	// 两套 appId 字段都在 = 两份模板被拼在了一起。拒绝，不猜：
	// 猜错方向的后果是拿"目标 appId"去新建一个来源文件。
	body := `### 应用包名（appId）

a

### 目标 appId

b
`
	if _, ok := issue.Parse(body).Detect(); ok {
		t.Error("两套字段同时出现时应当判定为不可识别")
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
			issue.LabelAppID, issue.LabelName, issue.LabelAuthor, issue.LabelRepo,
			issue.LabelAssetPat, issue.LabelCategories, issue.LabelABIs,
		}},
		{"change-source.yml", []string{
			issue.LabelTargetAppID, issue.LabelAction, issue.LabelNewName,
			issue.LabelNewAuthor, issue.LabelNewAssetPat, issue.LabelNewCats, issue.LabelReason,
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
