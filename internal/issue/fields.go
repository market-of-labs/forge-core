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
//
// 新增侧字段分两组：**标准源**（repo 必填，其余可选）与**手动源**（`来源类型` 选了
// `手动上传`，这时上游那组必须留空、而身份三件套必须自己填）。身份三件套之所以只对
// 手动源出现，是因为它**没有 APK 与仓库可读**：标准源的 appId/显示名/作者都是从上游
// 读出来的，让人手填只会多一个填错的地方（03 §2.6）；而手动源在申请时还没有任何
// 二进制（第一份 APK 是收录之后才传进 `_incoming` 的），除了申请人没人知道这几样。
// 唯一从头到尾都派生不出来的自由文本是 `一句话简介`：它进的是用户唯一会扫的那一行
// （Obtainium 的列表标题，见 model.DisplayName）。
const (
	LabelOrigin     = "来源类型"
	LabelRepo       = "上游 GitHub 仓库"
	LabelAppID      = "包名（仅手动上传时填）"
	LabelName       = "显示名（仅手动上传时填）"
	LabelAuthor     = "作者 / 组织（仅手动上传时填）"
	LabelAssetPat   = "资产匹配正则（可选）"
	LabelCategories = "分类标签（可选）"
	LabelABIs       = "只镜像哪些 ABI（可选）"
	LabelDesc       = "一句话简介（可选）"
	// LabelKind 是个 dropdown，选项是 `普通应用` / `obtainium` / `companion`，
	// 带 default 且 required —— 也就是**正文里一定有一个选项文本**（见 KindOptionNormal）。
	LabelKind = "应用类型"
	// LabelPrerelease 底下只有**一个**勾选项（PrereleaseOption）：布尔量在 issue 表单里
	// 只有 checkboxes 表达得了 —— dropdown 没有"未选中"这个渲染结果，要它表达 false
	// 就得凭空造一个 `否` 取值，再在 Go 里做一次"否 → false"的映射，多一处能漂移的地方。
	//
	// 代价是它会被 `TestCheckboxVocabularyMatchesGo` 看见（那个测试断言模板里每个
	// `- label:` 选项都是 Go 认识的取值），所以 PrereleaseOption 必须是个 Go 常量并
	// 加进那份期望集。这是对的：那个测试的实质是"模板里出现的每个选项名，Go 都认识"。
	LabelPrerelease = "拉取预发布版本（可选）"

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

// PrereleaseOption 是「拉取预发布版本」那组的唯一个勾选项，与 add-source.yml 的
// options 逐字一致。**不含空格** —— checkboxItem 在空格处停下（见 issue.go）。
//
// 它只是"勾了没有"的载体：值本身不进任何数据文件（`upstream.includePrerelease`
// 是个 bool），所以这里不需要像 ReviewCategories 那样是一份"合法取值表"。
const PrereleaseOption = "拉取预发布版本"

// 「来源类型」下拉的两个选项，与 add-source.yml 的 options 逐字一致。
//
// 它是正文里**唯一**能表达"这条来源有没有上游"的字段，所以两处依赖它：
// 取值（ParseAdd 决定 repo 必不必填）与认模板（Detect，见那里的说明）。
const (
	// OriginGitHub 是默认项：上游仓库自己发 Release，forge 去镜像它。
	OriginGitHub = "GitHub 上游发布"
	// OriginManual 选中时，上游那组字段必须留空，身份三件套必须填 —— 见 LabelOrigin 上面的说明。
	OriginManual = "手动上传"
)

// KindOptionNormal 是「应用类型」下拉里代表"普通应用"的那一项 —— 它映射到**空串**
// （`Source.Kind` 的零值），不是 `model.KindObtainium`/`KindCompanion` 之外的第三个取值。
//
// 为什么要造这么一个选项、而不是让它空着：dropdown **没有"未选中"这个渲染结果**。
// 留空的代价是取值要靠 GitHub 把空白渲染成 `_No response_`（可选 input/textarea 是这么
// 渲染的，但这是**外部行为**，本仓库里没有一行代码能证明它对 dropdown 也成立）。赌错的
// 后果不是少一个选项，而是**每一张新增单都被 ValidateKind 拒掉** —— 唯一的入口整个哑掉。
// 所以模板里给它一个 default，正文里就一定有一个合法选项文本，取值不再依赖任何假设。
const KindOptionNormal = "普通应用"

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
// 变更侧是 `目标 appId`、新增侧是 `上游 GitHub 仓库`，认这一对就够。
//
// **手动源要看 `来源类型`**：它的 `上游 GitHub 仓库` 是留空的（没有上游），
// 只看 repo 会把手动新增单判成"既不新增也不变更"而整个拒掉。
//
// 两边都对不上时返回 ok=false（拒绝），且**不看标题**：本函数只拿得到正文，
// 而手打的 issue 正文结构本来就不该信。
func (f *Form) Detect() (Kind, bool) {
	hasAdd := f.Get(LabelRepo) != "" || f.Get(LabelOrigin) == OriginManual
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

// AddRequest 是一次新增申请（标准源或手动源）。
//
// 必填的是哪几项取决于 Origin：标准源必填 Repo，手动源必填 AppID/Name/Author。
// 这一对关系在 ParseAdd 里判（"缺字段"是取值问题），而字段**是否合法**由 job 层判，
// 与手写来源文件那条入口共用同一批校验（见 job.validateAdd）。
type AddRequest struct {
	// Origin 是「来源类型」下拉的原样取值，`OriginGitHub` 或 `OriginManual`。
	Origin string
	Repo   string
	// AppID / Name / Author 只对**手动源**有意义：标准源的这三样是从上游 APK 与仓库里
	// 读出来的，申请人填了也不作数（job 层会在回评里点名说明）。手动源没有上游可读，
	// 而它的第一份 APK 要到收录之后才上传，所以只能由申请人填 —— 包名还是**唯一的定位键**
	// （`_incoming` 里那个 APK 靠它找回家）。
	AppID        string
	Name         string
	Author       string
	AssetPattern string
	Categories   []string
	ABIWhitelist []string
	// Desc 是**原样**取出来的简介，没有裁长度 —— 上限与截断都在 job 层
	// （model.TruncateDesc），因为那需要给申请人回一句"被裁了"，而本包只负责取值。
	Desc string
	// Kind 是模板下拉里**原样**读出来的值：`obtainium` / `companion` / 空串（= 普通应用）。
	// 合法性不在本包判 —— 复用 model.ValidateKind，判定规则与上手写来源文件时是同一批。
	Kind string
	// IncludePrerelease 是"上游的 prerelease 也一起镜像"（模板里勾了那一项）。
	IncludePrerelease bool
}

// Manual 报告这份申请是不是手动源（`source: "manual"`，二进制走 `_incoming` 上传队列）。
//
// 判据只有下拉那一个值，所以手动源与标准源在**同一张模板**里、走**同一条**收录链，
// 分叉点少到只有读得出这个 bool 的那几处。
func (r *AddRequest) Manual() bool { return r.Origin == OriginManual }

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
	r := &AddRequest{Origin: f.Get(LabelOrigin)}
	var err error
	// 上游那一组**一律读出来**（即使手动源该留空）：job 层要靠"填了没有"决定是拒绝
	// 还是照收，静默丢掉一个申请人特意填的值是最难发现的那种假回评。
	r.Repo = f.Get(LabelRepo)
	r.AssetPattern = f.Get(LabelAssetPat)
	// 身份三件套同理：标准源填了也不作数（会从上游读），但那句话得有人去说。
	r.AppID = f.Get(LabelAppID)
	r.Name = f.Get(LabelName)
	r.Author = f.Get(LabelAuthor)
	// 必填只对**该填的那些**成立：手动源没有上游仓库，标准源没有身份三件套可填
	// （模板里那三个的 label 写着"仅手动上传时填"）。所以这里只钉标准源的 Repo 一项。
	if !r.Manual() {
		if r.Repo, err = f.Required(LabelRepo); err != nil {
			return nil, err
		}
	}
	// 勾选项走 Checked（认 GitHub 渲染的 `- [X]`）—— 把整张任务列表当成一个
	// 逗号分隔串去切会切得乱七八糟。
	r.Categories = f.Checked(LabelCategories)
	r.ABIWhitelist = f.Checked(LabelABIs)
	r.Desc = f.Get(LabelDesc)
	// 下拉的那一项"普通应用"落到空串上，正是 `Source.Kind` 的零值语义（见 KindOptionNormal）。
	// 顺带兜住一种历史形态：模板换成带 default 的 dropdown **之前**提交的单子没有这一段，
	// 取值同样是空串 = 普通应用 —— 老单子不会因为这次模板改动而失效。
	r.Kind = f.Get(LabelKind)
	if r.Kind == KindOptionNormal {
		r.Kind = ""
	}
	// 只关心"有没有被勾中"，值本身丢掉 —— 但**不**要求它等于 PrereleaseOption：
	// 用户能伪造 `- [X] 随便什么`，那时按"勾了"处理仍是安全的（bool 没有别的取值），
	// 而拒绝它只会让人对着一份合法申请挠头。
	r.IncludePrerelease = len(f.Checked(LabelPrerelease)) > 0
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
		// 同前三个：**无法通过 issue 把简介清空**（空 = 不改）—— 模板表达力的限制，
		// 模板里那句话如实写着这一点（要清空就在说明里写一句，由人处理）。
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
