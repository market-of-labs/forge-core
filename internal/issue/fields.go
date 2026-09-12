package issue

import (
	"fmt"
	"strings"
)

// 两份 issue 模板的字段标题。
//
// ⚠️ 这些字符串必须与 `market-of-labs/store` 的
// `.github/ISSUE_TEMPLATE/{add,change}-source.yml` 里的 **label** 逐字一致 ——
// GitHub 渲染出来的正文标题用的就是 label，改模板而忘了改这里，表现是
// "所有申请都被判为缺字段"，而不是解析错误。`TestLabelsMatchStoreTemplates`
// 会直接读那两个 yml 来钉住这件事。
//
// 括号里的空格是模板里真实存在的（`作者 / 组织`），不要"顺手对齐"掉。
// 新增侧只有五个字段：repo 必填，其余四个可选。**appId / 显示名 / 作者都从模板里
// 删掉了** —— 它们能从 APK 与仓库里读出来，让人手填只会多一个填错的地方（03 §2.6）。
// 唯一保留的自由文本是 `一句话简介`：它是**唯一从任何地方都派生不出来**的东西，
// 而它进的是用户唯一会扫的那一行（Obtainium 的列表标题，见 model.DisplayName）。
const (
	LabelRepo       = "上游 GitHub 仓库"
	LabelAssetPat   = "资产匹配正则（可选）"
	LabelCategories = "分类标签（可选）"
	LabelABIs       = "只镜像哪些 ABI（可选）"
	LabelDesc       = "一句话简介（可选）"

	LabelTargetAppID = "目标 appId"
	LabelAction      = "动作"
	LabelNewName     = "新的显示名（仅「修改元数据」时填）"
	LabelNewAuthor   = "新的作者 / 组织（仅「修改元数据」时填）"
	LabelNewAssetPat = "新的资产匹配正则（仅「修改元数据」时填）"
	LabelNewCats     = "新的分类标签（仅「修改元数据」时填）"
	LabelNewDesc     = "新的简介（仅「修改元数据」时填）"
	// LabelReason **刻意不解析**：维护者读的本来就是 issue 本身，把它取出来也没有
	// 第二个消费者。留着常量只是让"模板里有哪几个字段"这件事在一处看得全。
	LabelReason = "说明（可选）"
)

// 动作取值，与 change-source.yml 的 dropdown options 逐字一致。
const (
	ActionEdit   = "修改元数据"
	ActionPause  = "暂停更新"
	ActionResume = "恢复更新"
	ActionRemove = "移除"
)

// Kind 是一份申请的类型。
type Kind int

const (
	// KindAdd 对应「新增 · 标准源」模板。
	KindAdd Kind = iota
	// KindChange 对应「变更 · 移除」模板。
	KindChange
)

func (k Kind) String() string {
	if k == KindAdd {
		return "新增"
	}
	return "变更"
}

// Detect 判断一份正文来自哪份模板。
//
// 判据是**结构**而不是 issue 标题：标题是用户可以随手改的（模板只给个默认值），
// 而正文里出现哪套字段由表单渲染决定。两份模板各有一个对方没有的字段 ——
// 新增侧是 `上游 GitHub 仓库`、变更侧是 `目标 appId`，认这一对就够。
//
// 两边都对不上时返回 ok=false（拒绝），且**不看标题**：本函数只拿得到正文，
// 而手打的 issue 正文结构本来就不该信。
func (f *Form) Detect() (Kind, bool) {
	hasAdd := f.Get(LabelRepo) != ""
	hasChange := f.Get(LabelTargetAppID) != ""
	switch {
	case hasChange && !hasAdd:
		return KindChange, true
	case hasAdd && !hasChange:
		return KindAdd, true
	default:
		// 两套都在（有人把两份模板拼在了一起）或一套都不在（手打 issue 而不是走模板）。
		// 都拒绝，不猜 —— 猜错方向的后果是拿"目标 appId"去建一个新的来源文件。
		return KindAdd, false
	}
}

// AddRequest 是一次「新增 · 标准源」申请。
//
// 只有 Repo 是必填。**这里没有 AppID / Name / Author** —— 申请人无从知道它们
// （写错了还得靠人发现），所以改由对账阶段从 APK 的 package 与 label 读出来。
type AddRequest struct {
	Repo         string
	AssetPattern string
	Categories   []string
	ABIWhitelist []string
	// Desc 是**原样**取出来的简介，没有裁长度 —— 上限与截断都在 job 层
	// （model.TruncateDesc），因为那需要给申请人回一句"被裁了"，而本包只负责取值。
	Desc string
}

// ChangeRequest 是一次「变更 · 移除」申请。
type ChangeRequest struct {
	AppID string
	// Action 是四个动作之一；不认识的动作会让上层拒绝。
	Action string

	// 以下仅在 Action == ActionEdit 时有值。**用指针区分"没填"与"填了空"** ——
	// 03 §2.5 规则 2 要求"只改申请涉及的字段"，所以"没填"必须是可判定的：
	// 若用空串表示没填，就无法区分"把显示名改成空"和"这次不改显示名"。
	NewName       *string
	NewAuthor     *string
	NewAssetPat   *string
	NewDesc       *string
	NewCategories []string
}

// ParseAdd 解析一份新增申请。
//
// 本函数只做**取值**，不判合法性（词表、正则能不能编译）—— 那是 job 层的事，
// 与手改文件那条入口共用同一批校验，见 03 §2.6。
func ParseAdd(f *Form) (*AddRequest, error) {
	r := &AddRequest{}
	var err error
	if r.Repo, err = f.Required(LabelRepo); err != nil {
		return nil, err
	}
	r.AssetPattern = f.Get(LabelAssetPat)
	// 勾选项走 Checked（认 GitHub 渲染的 `- [X]`）—— 把整张任务列表当成一个
	// 逗号分隔串去切会切得乱七八糟。
	r.Categories = f.Checked(LabelCategories)
	r.ABIWhitelist = f.Checked(LabelABIs)
	r.Desc = f.Get(LabelDesc)
	return r, nil
}

// ParseChange 解析一份变更申请。
func ParseChange(f *Form) (*ChangeRequest, error) {
	r := &ChangeRequest{}
	var err error
	if r.AppID, err = f.Required(LabelTargetAppID); err != nil {
		return nil, err
	}
	if r.Action, err = f.Required(LabelAction); err != nil {
		return nil, err
	}

	if r.Action == ActionEdit {
		// 三个"新值"字段都用**是否出现在正文里**判定，而不是"值非空"：
		// 模板渲染时未填的字段会变成 `_No response_`（已被 cleanValue 归成空串），
		// 但"用户填了又删空"和"用户压根没填"在这里都该算"不改"，
		// 因为 §2.5 规则 2 的语义是"只改申请里明确给出的字段"，
		// 而一个空的显示名不是合法的目标值。
		if v := strings.TrimSpace(f.Get(LabelNewName)); v != "" {
			r.NewName = &v
		}
		if v := strings.TrimSpace(f.Get(LabelNewAuthor)); v != "" {
			r.NewAuthor = &v
		}
		if v := strings.TrimSpace(f.Get(LabelNewAssetPat)); v != "" {
			r.NewAssetPat = &v
		}
		// 同前三个：**无法通过 issue 把简介清空**（空 = 不改），
		// 与分类一样是模板表达力的限制，要清空就走入口甲直接改文件。
		if v := strings.TrimSpace(f.Get(LabelNewDesc)); v != "" {
			r.NewDesc = &v
		}
		// 勾选即**全量替换**；一项都没勾 = 不改（见模板说明）。所以 nil 在这里
		// 天然表示"没涉及这个字段"，不需要像上面三个那样用指针区分"填了空"。
		r.NewCategories = f.Checked(LabelNewCats)
	}
	return r, nil
}

// KnownAction 报告 action 是不是模板里定义的四个之一。
func KnownAction(action string) bool {
	switch action {
	case ActionEdit, ActionPause, ActionResume, ActionRemove:
		return true
	}
	return false
}

// Summary 给"修改元数据"生成一句人话，用于回评里说明到底改了什么。
func (r *ChangeRequest) Summary() string {
	var parts []string
	if r.NewName != nil {
		parts = append(parts, fmt.Sprintf("显示名 → %q", *r.NewName))
	}
	if r.NewAuthor != nil {
		parts = append(parts, fmt.Sprintf("作者 → %q", *r.NewAuthor))
	}
	if r.NewAssetPat != nil {
		parts = append(parts, fmt.Sprintf("资产正则 → %q", *r.NewAssetPat))
	}
	if r.NewDesc != nil {
		parts = append(parts, fmt.Sprintf("简介 → %q", *r.NewDesc))
	}
	if len(r.NewCategories) > 0 {
		parts = append(parts, fmt.Sprintf("分类 → %s", strings.Join(r.NewCategories, "/")))
	}
	if len(parts) == 0 {
		return "没有任何字段被修改"
	}
	return strings.Join(parts, "；")
}
