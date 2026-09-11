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
const (
	LabelAppID      = "应用包名（appId）"
	LabelName       = "显示名"
	LabelAuthor     = "作者 / 组织"
	LabelRepo       = "上游 GitHub 仓库"
	LabelAssetPat   = "资产匹配正则（可选）"
	LabelCategories = "分类标签（可选）"
	LabelABIs       = "只镜像哪些 ABI（可选）"

	LabelTargetAppID = "目标 appId"
	LabelAction      = "动作"
	LabelNewName     = "新的显示名（仅「修改元数据」时填）"
	LabelNewAuthor   = "新的作者 / 组织（仅「修改元数据」时填）"
	LabelNewAssetPat = "新的资产匹配正则（仅「修改元数据」时填）"
	LabelNewCats     = "新的分类标签（仅「修改元数据」时填，逗号分隔）"
	LabelReason      = "说明（可选）"
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
// 而正文里出现哪个 appId 字段是表单渲染决定的。先看字段，字段两边都没有时才退回
// 标题前缀 —— 这样"用户把标题改花了"不会导致申请被误判成另一种。
func (f *Form) Detect() (Kind, bool) {
	hasAdd := f.Get(LabelAppID) != ""
	hasChange := f.Get(LabelTargetAppID) != ""
	switch {
	case hasChange && !hasAdd:
		return KindChange, true
	case hasAdd && !hasChange:
		return KindAdd, true
	case hasAdd && hasChange:
		// 两套字段都在：只可能是有人把两份模板的内容拼在了一起。拒绝，
		// 不猜 —— 猜错方向的后果是拿"目标 appId"去建一个新的来源文件。
		return KindAdd, false
	default:
		// 一个都没匹配上：可能是标题里写了 [新增]/[变更] 但正文没渲染出来
		// （手打 issue 而不是走模板）。这时按标题判，并且把剩下的校验交给上层。
		return KindAdd, false
	}
}

// AddRequest 是一次「新增 · 标准源」申请。
type AddRequest struct {
	AppID        string
	Name         string
	Author       string
	Repo         string
	AssetPattern string
	Categories   []string
	ABIWhitelist []string
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
	NewCategories []string

	Reason string
}

// ParseAdd 解析一份新增申请。
func ParseAdd(f *Form) (*AddRequest, error) {
	r := &AddRequest{}
	var err error
	if r.AppID, err = f.Required("应用包名", LabelAppID); err != nil {
		return nil, err
	}
	if r.Name, err = f.Required("显示名", LabelName); err != nil {
		return nil, err
	}
	if r.Author, err = f.Required("作者 / 组织", LabelAuthor); err != nil {
		return nil, err
	}
	if r.Repo, err = f.Required("上游 GitHub 仓库", LabelRepo); err != nil {
		return nil, err
	}
	r.AssetPattern = f.Get(LabelAssetPat)
	r.Categories = f.List(LabelCategories)
	r.ABIWhitelist = f.List(LabelABIs)
	return r, nil
}

// ParseChange 解析一份变更申请。
func ParseChange(f *Form) (*ChangeRequest, error) {
	r := &ChangeRequest{}
	var err error
	if r.AppID, err = f.Required("目标 appId", LabelTargetAppID); err != nil {
		return nil, err
	}
	if r.Action, err = f.Required("动作", LabelAction); err != nil {
		return nil, err
	}
	r.Reason = f.Get(LabelReason)

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
		r.NewCategories = f.List(LabelNewCats)
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
	if len(r.NewCategories) > 0 {
		parts = append(parts, fmt.Sprintf("分类 → %s", strings.Join(r.NewCategories, "/")))
	}
	if len(parts) == 0 {
		return "没有任何字段被修改"
	}
	return strings.Join(parts, "；")
}
