package job

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/market-of-labs/forge-core/internal/apkmeta"
	"github.com/market-of-labs/forge-core/internal/gh"
	"github.com/market-of-labs/forge-core/internal/issue"
	"github.com/market-of-labs/forge-core/internal/model"
	"github.com/market-of-labs/forge-core/internal/naming"
)

// 待办队列的两个标签（03 §2.6）。**issue 本身就是队列** —— 不另建数据库，
// 标签就是状态，而"标签跳变"就是"该回评了"的信号。于是回评天然幂等：
// 同一轮重复扫到同一张单，标签已经是目标值，就不再说话。
const (
	// LabelPending 已受理，等对账去上游读出 appId。
	LabelPending = "待收录"
	// LabelNeedInfo 解析失败或申请不合法，等申请人补完。
	LabelNeedInfo = "待补充"
)

// queueLabels 是扫描器要认领的全部标签。
var queueLabels = []string{LabelPending, LabelNeedInfo}

// placedVersion 是搬运过程中为一个 version token 攒起来的分片。
// 与 versionAcc 同构，但**不带 UpstreamTag** —— 手动上传没有上游 tag 可记。
type placedVersion struct {
	versionName string
	versionCode int32
	assets      []model.IndexAsset
}

// ---- intake-issue：issue → sources/{appId}.json（03 §2.5 / §5.5） --------------
//
// 这条路径的输入是**陌生人写的字符串**，所以它被刻意切成两半：
//
//	DecideIntake  —— 纯函数，body → 决定。不碰网络、不碰文件系统。
//	IntakeIssue   —— 拿决定去落盘、回评、关单。
//
// 切开的价值不只是好测：它把"外部字符串能影响的范围"限死在 DecideIntake 的返回值里。
// 上层拿到的是一份 **已经构造好的 model.Source**，没有任何一处会把 issue 正文里的
// 内容当命令、当路径、当表达式去用（§2.5 规则 6 / §6 三条不变量的落点）。

// IntakeDecision 是一次申请的裁决结果。
type IntakeDecision struct {
	// Kind 是判定出来的申请类型，供调用方分流（只有新增单会被挂进待办队列）。
	Kind issue.Kind
	// Accept 为真时 Source/Delete 有意义；为假时 Reply 是拒绝理由。
	Accept bool
	// Pending 为真表示"已受理，但身份还没定"：**不写文件、不关单**，
	// 只打上待收录标签，等对账阶段去上游读出 appId（03 §2.6）。
	Pending bool
	// Hold 为真表示"本单留在打开状态等下一轮重评"，不关单。
	//
	// 新增单**一律**为真：它成败都在下一轮对账才见分晓，而 issue 本身就是队列
	// （不另建数据库）。变更为假 —— 它是即时的，留一张没人再看第二眼的单只是噪音。
	Hold bool
	// Reply 是回评正文，成功与拒绝都要写清原因（§2.5 规则 6）。
	Reply string
	// Source 是要写入的**完整**内容。注意它是"改完之后的整份"，不是补丁 ——
	// 补丁的写法要求每个调用点都知道"哪份文件"，而 §2.5 规则 2 要的语义
	// （只改申请涉及的字段）在 DecideIntake 里就已经兑现了：它是**从当前文件
	// 复制一份再改**，不是凭申请内容凭空造一个。
	//
	// Pending 为真时它是**半成品**：ID/Name/Author 都还空着，要等对账填。
	Source *model.Source
	// Delete 为真表示"移除"（§2.5 规则 4：移除 = 删掉该文件）。
	Delete bool
	// Summary 一行摘要，给日志与提交信息用。
	Summary string
}

// DecideIntake 解析 issue 正文并给出裁决。**纯函数**：同样的 (sources, body) 必然
// 得到同样的结论，不发一个请求、不写一个字节。
//
// 对账阶段的待办扫描也复用它（见 pending.go）—— 申请人改完正文、或在评论里补了正则
// 之后，"这份申请现在合法了吗"必须有**同一个**答案，两条路径各有各的判断就会漂移。
func DecideIntake(c *Ctx, body string) *IntakeDecision {
	f := issue.Parse(body)

	kind, ok := f.Detect()
	if !ok {
		d := reject("无法判断这份申请是「新增」还是「变更」。\n\n" +
			"请用仓库里的 issue 模板重新提交：新增来源用 `add-source.yml`，" +
			"修改或移除已有来源用 `change-source.yml`。**两份模板的字段不要混在一张单里。**")
		d.Kind = issue.KindAdd
		return d
	}
	if kind == issue.KindAdd {
		d := decideAdd(c, f)
		d.Kind = kind
		d.Hold = true
		return d
	}
	d := decideChange(c, f)
	d.Kind = kind
	return d
}

// decideAdd 裁决一份新增申请。
//
// **它不写文件。** 申请人填的只有 repo，appId 要从上游 APK 里读出来，而 DecideIntake
// 是纯函数、不许联网。所以这里只做三件不需要网络的事：取值、本地校验、把申请挂进
// 待办队列 —— 真正的解析与落盘在对账阶段（03 §2.6）。
func decideAdd(c *Ctx, f *issue.Form) *IntakeDecision {
	r, err := issue.ParseAdd(f)
	if err != nil {
		return reject(fmt.Sprintf("申请缺少必填字段：%v", err))
	}
	if err := validateAdd(r); err != nil {
		return reject(fmt.Sprintf("申请内容不合法：%v", err))
	}

	// 简介**裁而不拒**：上限 20 rune 是量出来的（见 model.MaxDescRunes），
	// 而新增单一次往返是一天，为一个纯装饰字段让人重填不划算。
	// 裁过就明说 —— 默默把人写的东西切掉、还回评说"已收录"，是最难发现的那种假回评。
	desc := model.TruncateDesc(r.Desc)
	descNote := ""
	if desc != strings.TrimSpace(r.Desc) {
		descNote = fmt.Sprintf(
			"\n> ⚠️ 你填的简介超过了 %d 个字的长度上限，已**截断**为上面这个。\n"+
				"> 它进的是 Obtainium 列表里的标题行（单行、超出即省略号），写长了显示不全。\n"+
				"> 想换一个的话，**编辑正文**重填即可。\n",
			model.MaxDescRunes)
	}

	// 按 repo 反查查重。**只告警、不拒绝**：一个仓库合法地可以发布多个不同包名的
	// 应用（那时每一条都得各自带 assetPattern 消歧），所以"同仓库已存在"不等于
	// "重复申请"。真正的把关在解析那一步 —— 多包名会被拒。
	dup := ""
	if same := c.SourcesByRepo(r.Repo); len(same) > 0 {
		dup = fmt.Sprintf(
			"\n> ⚠️ `sources/` 里已经有 %d 条记录指向 `%s`（`%s`）。如果这个仓库确实发布"+
				"多个**不同包名**的应用，请忽略这条；否则这可能是重复申请，解析出来的"+
				"appId 已存在时会被拒。\n",
			len(same), r.Repo, same[0].ID)
	}

	return &IntakeDecision{
		Accept:  true,
		Pending: true,
		Summary: fmt.Sprintf("登记 %s", r.Repo),
		// 此刻还没有 appId，所以 Source 只填得出上游那半边。ID/Name/Author
		// 由对账阶段的 probe 填（它们是从 APK 与仓库里读出来的，不是猜的）。
		Source: &model.Source{
			Source: model.SourceGitHub,
			Upstream: &model.Upstream{
				Type:         model.UpstreamGitHubRelease,
				Repo:         r.Repo,
				AssetPattern: r.AssetPattern,
			},
			Categories:   r.Categories,
			ABIWhitelist: r.ABIWhitelist,
			Desc:         desc,
		},
		Reply: fmt.Sprintf(
			"已收到，登记为**待收录**。\n\n"+
				"| 字段 | 值 |\n|---|---|\n"+
				"| 上游仓库 | `%s` |\n| 资产正则 | `%s` |\n"+
				"| 一句话简介 | %s |\n"+
				"| 分类标签 | %s |\n| 只镜像 ABI | %s |\n"+
				"%s%s\n"+
				"下一轮对账会去上游取**当前最新**的 APK，从包里读出包名与显示名，"+
				"然后写入 `sources/` 并回评给你核对。\n\n"+
				"⚠️ 仓库填错的话会**静默收错应用**，所以那条回评请务必看一眼。\n\n"+
				"本单保持打开，解析成功后自动关闭；被拒了会写明原因，"+
				"按回评说的改完**编辑正文**即可，不用重开单。",
			r.Repo,
			orDefault(r.AssetPattern, DefaultAssetPatternNote),
			orDefault(desc, "未填"),
			orDefault(strings.Join(r.Categories, " / "), "未勾选"),
			orDefault(strings.Join(r.ABIWhitelist, " / "), "全部"),
			dup, descNote),
	}
}

// validateAdd 校验新增申请里本地就能判的那一半。
//
// 规则本身复用 model 与 naming 里的那两份定义（`Upstream.Validate` 覆盖
// owner/name 形状与正则可编译，`naming.IsABI` 是固定 ABI 集），所以 issue 能写进来的
// 东西不可能比手改文件能写进来的更多 —— 这里只是换个调用方式，不是第二套规则。
func validateAdd(r *issue.AddRequest) error {
	up := &model.Upstream{
		Type:         model.UpstreamGitHubRelease,
		Repo:         r.Repo,
		AssetPattern: r.AssetPattern,
	}
	if err := up.Validate(); err != nil {
		return err
	}
	for _, c := range r.Categories {
		if !slices.Contains(model.ReviewCategories, c) {
			return fmt.Errorf("分类标签 %q 不在可选范围内（%s）—— "+
				"请用模板里的勾选项，不要手改正文",
				c, strings.Join(model.ReviewCategories, " / "))
		}
	}
	for _, a := range r.ABIWhitelist {
		if !naming.IsABI(a) {
			return fmt.Errorf("ABI %q 不在固定集 %v 内 —— 请用模板里的勾选项，不要手改正文",
				a, naming.ABISet)
		}
	}
	return nil
}

func decideChange(c *Ctx, f *issue.Form) *IntakeDecision {
	r, err := issue.ParseChange(f)
	if err != nil {
		return reject(fmt.Sprintf("申请缺少必填字段：%v", err))
	}
	if !issue.KnownAction(r.Action) {
		return reject(fmt.Sprintf(
			"动作 %q 不认识。只能是：`%s` / `%s` / `%s` / `%s`。",
			r.Action, issue.ActionEdit, issue.ActionPause, issue.ActionResume, issue.ActionRemove))
	}

	cur := c.Source(r.AppID)
	if cur == nil {
		return reject(fmt.Sprintf(
			"`sources/` 里没有 `%s`。\n\n"+
				"想新增一个来源请改用 **`add-source.yml`**（03 §2.5 规则 5）。", r.AppID))
	}

	// 从当前文件复制一份再改 —— §2.5 规则 2 的"只改申请涉及的字段"由此兑现：
	// 没被申请提到的字段连碰都没碰过。用浅拷贝即可，因为下面改的
	// 都是**替换**而不是原地修改子结构（Upstream 会整体换新指针）。
	next := *cur
	if cur.Upstream != nil {
		up := *cur.Upstream
		next.Upstream = &up
	}

	switch r.Action {
	case issue.ActionRemove:
		// §2.5 规则 4 / 02 §2.9：移除 = 删掉文件，**不是置标志**。
		// 语义上"维护侧立即生效、设备端不传播"—— 存量行留在用户设备上，
		// 这是已知且接受的限制（L5），必须在回评里讲清楚，否则报"移除"的人
		// 会以为全世界的手机下次刷新就没了。
		return &IntakeDecision{
			Accept:  true,
			Delete:  true,
			Source:  &next,
			Summary: fmt.Sprintf("移除 %s", r.AppID),
			Reply: fmt.Sprintf(
				"已移除 `%s`（删掉 `sources/%s.json`）。\n\n"+
					"⚠️ **移除只影响后续同步，不会从已安装用户的设备上删除任何东西**"+
					"（02 §2.9 / L5）：清单只能控制增量，控制不了设备端存量。\n"+
					"已收录的版本资产仍然在 Release 里（D13 全保留），只是新设备再也看不到它。\n\n"+
					"如果只是想**停更**而不是下架，下次请用「暂停更新」——它保留条目，"+
					"老用户仍能看到历史版本。",
				r.AppID, r.AppID),
		}

	case issue.ActionPause:
		next.Paused = true
		return &IntakeDecision{
			Accept:  true,
			Source:  &next,
			Summary: fmt.Sprintf("暂停 %s", r.AppID),
			Reply: fmt.Sprintf(
				"已暂停 `%s` 的更新（`paused: true`）。\n\n"+
					"**只停「追加新版本」**：已有的 asset 一个不动，条目也照常留在清单里，"+
					"`latestVersion` 冻结在最后一个已镜像的版本（03 §4.4 / §2.2）。"+
					"老用户仍然可以装到历史版本。",
				r.AppID),
		}

	case issue.ActionResume:
		next.Paused = false
		return &IntakeDecision{
			Accept:  true,
			Source:  &next,
			Summary: fmt.Sprintf("恢复 %s", r.AppID),
			Reply: fmt.Sprintf(
				"已恢复 `%s` 的更新。\n\n"+
					"下一轮对账会从上游补齐**上次暂停之后发布过的**版本（03 §4.4 的漏跑自愈），"+
					"不会回填暂停之前的历史版本 —— 那属于 D33 的「不追溯」。",
				r.AppID),
		}

	default: // issue.ActionEdit
		if r.NewAssetPat != nil && cur.Source != model.SourceGitHub {
			// manual 源没有上游，"资产匹配正则"对它是一个不存在的概念。
			// 静默忽略会让人以为改成功了，所以拒绝并说明。
			return reject(fmt.Sprintf(
				"`%s` 是 `source: \"manual\"` 的手动来源，没有上游，改「资产匹配正则」没有意义。\n\n"+
					"它的二进制走 `_incoming` 上传队列（03 §3.2）。",
				r.AppID))
		}

		if r.NewName != nil {
			next.Name = *r.NewName
		}
		if r.NewAuthor != nil {
			next.Author = *r.NewAuthor
		}
		if r.NewAssetPat != nil && next.Upstream != nil {
			next.Upstream.AssetPattern = *r.NewAssetPat
		}
		if r.NewDesc != nil {
			// 与新增侧同一个上限、同一个"裁而不拒"的理由；区别是这里把裁过的值
			// **写回 r** —— 于是下面 Summary() 里打出来的就是最终生效的那个值，
			// 回评里不会出现"说要改成 30 个字"而实际存了 20 个字这种对不上的话。
			d := model.TruncateDesc(*r.NewDesc)
			r.NewDesc = &d
			next.Desc = d
		}
		// 模板里"新分类"是自由文本列表，**空列表 = 不改**，不是"清空"。
		// 因此无法通过 issue 把分类清掉 —— 那是模板表达力的限制，
		// 不是这条路径的疏漏；真要清空就用入口甲直接改文件（§2.5）。
		if len(r.NewCategories) > 0 {
			next.Categories = r.NewCategories
		}

		if r.Summary() == "没有任何字段被修改" {
			return reject(fmt.Sprintf(
				"这次「修改元数据」没有填任何新值，无事可做。\n\n"+
					"请至少填一个新的显示名 / 作者 / 资产正则 / 简介 / 分类。\n"+
					"（要暂停或移除请改用对应的动作——`%s` 的当前值是：显示名 %q，作者 %q。）",
				r.AppID, cur.Name, cur.Author))
		}

		// **`source` 种类不可自动变更**（§2.5 规则 3）。这里不是"检查后放行"，
		// 而是结构上就不可能发生：`change-source.yml` 里没有任何字段能表达它，
		// 而 next 是从 cur 复制的，`Source` 与 `Upstream` 的**存在性**都不由申请决定。
		// 将来若给模板加了新字段，这一行注释就是提醒：别让它碰到 next.Source。
		if err := next.Validate(r.AppID + ".json"); err != nil {
			return reject(fmt.Sprintf("改完之后的内容不合法，已放弃本次修改：%v", err))
		}

		return &IntakeDecision{
			Accept:  true,
			Source:  &next,
			Summary: fmt.Sprintf("修改 %s：%s", r.AppID, r.Summary()),
			Reply: fmt.Sprintf("已修改 `%s`：\n\n- %s\n\n下一轮对账会把改动反映到清单里。",
				r.AppID, strings.ReplaceAll(r.Summary(), "；", "\n- ")),
		}
	}
}

func reject(reason string) *IntakeDecision {
	return &IntakeDecision{
		Accept:  false,
		Reply:   "**这次申请没有通过**，原因如下：\n\n" + reason + "\n\n---\n" + retryHint,
		Summary: "拒绝",
	}
}

// retryHint 是拒绝回评的收尾。
//
// 新增单被留开（`Hold`），所以这里说的是"编辑正文"，**不是**"重开一张" ——
// 让申请人重开一张会把同一件事变成两条挂着的单，而旧的这条还得有人去关。
const retryHint = "**这张单会保持打开。** 改好后直接**编辑正文**（右上角 `...` → Edit），" +
	"下一轮对账会重新评估，不用重开一张。\n" +
	"如果你认为这个判断不对，直接在这里回一句，或者找维护者。"

// DefaultAssetPatternNote 是回评里展示默认正则用的占位文本（与 upstream 的默认值一致）。
//
// 不带反引号：调用点会把整个值包进反引号（真正则需要 code 格式，而默认值是被顶替
// 上去的同一个位置），自带一层会渲染成双层。
const DefaultAssetPatternNote = "(?i)\\.apk$（默认：任何 .apk）"

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// IntakeIssue 是 `intake-issue` 动词：拉 issue → 裁决 → 落盘 → 回评 → 关单。
//
// # 顺序是刻意的：先落盘并推送，再回评
//
// 如果反过来（先回评"已收录"再推送），推送失败时 issue 上留着一句假话，
// 而这是唯一会给协作者看到的东西 —— 之后的补跑会以"已存在"为由拒绝它，
// 于是那句假话永远留在那儿。先推送、后回评，最坏情况是"改动落地了但没人回话"，
// 那是个看得见、可重跑的状态。
//
// **已经关闭的 issue 直接跳过**：dispatch 可能因为 edit 之类的原因重复送达，
// 重复回评+重复写入是没必要的噪音。
func IntakeIssue(ctx context.Context, c *Ctx, number int) (*IntakeDecision, error) {
	if err := c.Env.RequireToken("处理 issue"); err != nil {
		return nil, err
	}

	is, err := c.GH.GetIssue(ctx, c.Env.StoreRepo, number)
	if err != nil {
		return nil, fmt.Errorf("读 issue #%d：%w", number, err)
	}
	if is.PullRequest != nil {
		// PR 也会出现在 issues 端点里，而它的"正文"是代码 diff，不是申请。
		c.Log("issue #%d 其实是个 PR，跳过", number)
		return nil, nil
	}
	if is.State == "closed" {
		c.Log("issue #%d 已关闭，跳过（幂等）", number)
		return nil, nil
	}

	// issue 正文是**不可信输入**，DecideIntake 是纯函数，这里之前没有任何 IO 副作用。
	d := DecideIntake(c, is.Body)

	switch {
	case d.Pending:
		// 登记：打「待收录」标签 + 回评，**不写文件、不关单**。
		// issue 自己就是待办队列，对账阶段会来认领（见 pending.go）。
		if err := c.GH.AddLabels(ctx, c.Env.StoreRepo, number, LabelPending); err != nil {
			return d, fmt.Errorf("给 issue #%d 打标签：%w", number, err)
		}
		c.Log("#%d 登记为待收录（未写文件，等对账解析）", number)

	case d.Accept:
		if d.Delete {
			if err := c.Repo.DeleteSource(d.Source.ID); err != nil {
				return d, fmt.Errorf("删除 sources/%s.json：%w", d.Source.ID, err)
			}
			c.Log("#%d 移除 sources/%s.json", number, d.Source.ID)
		} else {
			if err := c.Repo.WriteSource(d.Source); err != nil {
				return d, fmt.Errorf("写 sources/%s.json：%w", d.Source.ID, err)
			}
			c.Log("#%d 写入 sources/%s.json：%s", number, d.Source.ID, d.Summary)
		}
		// 只提交本次改动涉及的路径（CommitBack 内部固定暂存范围），
		// 并且**提交信息里带上 issue 号**：回看 store 历史时能直接对上。
		if _, err := c.CommitBack(ctx, fmt.Sprintf("%s（#%d）", d.Summary, number)); err != nil {
			return d, err
		}

	default:
		c.Log("#%d 拒绝：%s", number, d.Summary)
	}

	// 回评是**无条件**的（§2.5 规则 6：拒绝也要写清原因）。
	// 而关单跳变才发生 —— 见上面 Hold 的说明。
	if err := c.GH.CommentIssue(ctx, c.Env.StoreRepo, number, d.Reply); err != nil {
		return d, fmt.Errorf("回评 issue #%d：%w", number, err)
	}
	if d.Hold {
		c.Log("#%d 回评并留开（等下一轮重评）", number)
		return d, nil
	}
	if err := c.GH.CloseIssue(ctx, c.Env.StoreRepo, number); err != nil {
		return d, fmt.Errorf("关闭 issue #%d：%w", number, err)
	}
	c.Log("#%d 回评并关闭", number)
	return d, nil
}

// ---- intake-incoming：_incoming → 正式 Release（03 §3.2 / §4.6） -------------

// CheckIncomingGate 是 §4.6 的闸门（规则 2）：只有 `_incoming` 的**正式**发布才发车。
//
// 放在下载之前：一次误发布（比如你 Publish 了某个 App 的正式 Release）不该
// 在花掉几十 MB 流量之后才被拒绝。
func CheckIncomingGate(env *Env) error {
	if env.ReleaseTag != model.IncomingTag {
		// 上游 store 的 forward.yml 转发的是**所有** published 事件（它故意不筛，
		// 因为筛选逻辑属于 forge 的职责），所以这里必须自己判断。
		return fmt.Errorf("release tag 是 %q，不是 %q —— 不是暂存队列的发布，忽略",
			env.ReleaseTag, model.IncomingTag)
	}
	if env.Prerelease {
		// `published` 对预发布**同样触发**。预发布是"还没准备好"的意思，
		// 不该被搬运（规则 2）。
		return fmt.Errorf("%s 这次是 prerelease —— 预发布不搬运（规则 2）", model.IncomingTag)
	}
	return nil
}

// IncomingResult 是一次 _incoming 搬运的结果。
type IncomingResult struct {
	Moved   []string // 已搬运到正式 Release 的 asset 名
	Kept    []string // 没能安置、**留在 _incoming** 等人工决定的 asset 名
	AppIDs  []string // 本次涉及的 appId（去重）
	Cleaned bool     // 是否成功清场（改回 draft + 删除已搬运的 asset）
}

// IntakeIncoming 搬 `_incoming` 的 asset 到各自的正式 Release，然后清场。
//
// # ABI 与版本号一律来自 APK 内容
//
// 与 mirror-upstream 同一条规矩（信内容，改名按内容）：文件名叫什么都不影响
// 它被安放的名字。这里连"文件名上的 ABI 线索"都不再比对告警 —— 手动上传的
// 文件名本来就是人随手打的，逐条告警只会制造噪音。内容是什么就是什么。
//
// # 安置不了的东西一律**留在原地**
//
// 删除是单向的：删掉就没了，而"这个包找不到家"正是最需要人来看一眼的情况。
// 所以本函数只删除**确认已安置**的 asset，其余的原样留在 `_incoming` 里并逐个
// 说明原因（§3.2：不静默丢弃）。
func IntakeIncoming(ctx context.Context, c *Ctx) (*IncomingResult, error) {
	if err := c.Env.RequireToken("搬运 _incoming"); err != nil {
		return nil, err
	}
	res := &IncomingResult{}

	rel, err := c.GH.GetRelease(ctx, c.Env.StoreRepo, model.IncomingTag)
	if isNotFound(err) {
		// 暂存 Release 不存在：通常是把 tag 打错了，或者还没建。
		// 不自动建 —— 一个空的 draft 建出来只会掩盖"你其实传到了别处"这个事实。
		c.Log("`%s` Release 不存在。若刚上传过 APK，请确认传进的是 tag 为 `%s` 的 **draft** Release（03 §3.2）",
			model.IncomingTag, model.IncomingTag)
		return res, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读 %s Release：%w", model.IncomingTag, err)
	}

	assets, err := c.GH.ListAssets(ctx, c.Env.StoreRepo, rel.ID)
	if err != nil {
		return nil, fmt.Errorf("列 %s 的 asset：%w", model.IncomingTag, err)
	}
	if len(assets) == 0 {
		// 规则 3：空队列直接退出。这是重复 Publish 最常见的样子。
		c.Log("`%s` 里没有 asset，无事可做（规则 3）", model.IncomingTag)
		return res, nil
	}
	c.Log("`%s` 里有 %d 个 asset 待搬运", model.IncomingTag, len(assets))

	// 每个 appId 的正式 Release 只取一次，并且跨 asset 复用同一份"已有名字"集合 ——
	// 否则两个 asset 落到同一个目标名时，第二次上传才发现名字被占。
	type target struct {
		rel   *gh.Release
		names map[string]gh.Asset
		dirty bool // 本轮是否真的往里加过东西
	}
	targets := map[string]*target{}
	var targetOrder []string

	// record 把一次成功安置写进 index。与 mirror-upstream 一样，
	// 这是唯一知道 versionName/versionCode 的时刻（Release 里没有它们）。
	placed := map[string]map[string]*placedVersion{}

	var movedIDs []int64
	for _, a := range assets {
		meta, err := c.readIncomingMeta(ctx, a)
		if err != nil {
			res.Kept = append(res.Kept, a.Name)
			c.LogBlock("  ", fmt.Sprintf("%s：读不出 APK 元数据（%v）—— **保留**在 _incoming，请人工确认",
				a.Name, err))
			continue
		}

		src := c.Source(meta.Package)
		if src == nil {
			// §3.2 的"找不着家"：告警并保留。这通常是"先传了 APK 还没来得及收录"，
			// 或者包名打错（比如把 com.example.app.debug 传上来了）。
			res.Kept = append(res.Kept, a.Name)
			c.LogBlock("  ", fmt.Sprintf(
				"%s：package 是 %q，但 sources/ 里没有这个 appId —— **保留**在 _incoming。"+
					"要收录它请先开一张「新增来源」的单（03 §2.5），或确认包名是否打错（debug 后缀？）",
				a.Name, meta.Package))
			continue
		}

		contentABI := meta.ABIToken()
		if !abiAllowed(src.ABIWhitelist, contentABI) {
			// 白名单是**镜像侧**的体积阀门（D14），手动上传同样受它约束 ——
			// 否则"手动"这条路径能绕开所有体积控制。
			res.Kept = append(res.Kept, a.Name)
			c.LogBlock("  ", fmt.Sprintf("%s：内容判为 %s，不在 %s 的 abiWhitelist %v 内 —— **保留**在 _incoming",
				a.Name, contentABI, src.ID, src.ABIWhitelist))
			continue
		}
		if !meta.HasVersionName() {
			res.Kept = append(res.Kept, a.Name)
			c.LogBlock("  ", fmt.Sprintf("%s：APK 里没有 versionName，无法生成 {version} token —— **保留**在 _incoming",
				a.Name))
			continue
		}
		version, err := naming.SanitizeVersion(meta.VersionName)
		if err != nil {
			res.Kept = append(res.Kept, a.Name)
			c.LogBlock("  ", fmt.Sprintf("%s：%v —— **保留**在 _incoming", a.Name, err))
			continue
		}

		t, ok := targets[src.ID]
		if !ok {
			r, err := c.EnsureRelease(ctx, src.ID, src.Name)
			if err != nil {
				return res, fmt.Errorf("准备 %s 的 Release：%w", src.ID, err)
			}
			names, err := c.ReleaseAssets(ctx, r)
			if err != nil {
				return res, fmt.Errorf("列 %s 的 asset：%w", src.ID, err)
			}
			t = &target{rel: r, names: names}
			targets[src.ID] = t
			targetOrder = append(targetOrder, src.ID)
		}

		target2, err := c.Endpoints.AssetName(src.ID, version, contentABI)
		if err != nil {
			return res, fmt.Errorf("渲染 %s 的目标文件名：%w", src.ID, err)
		}

		if _, dup := t.names[target2]; dup {
			// 规则 3：目标已存在 → 不重复上传。但**仍然算"已安置"**，
			// 因为清场的目的是"别让下次 Publish 重复解析"，而这份内容
			// 已经在正式 Release 里躺着了。留着才是重复解析的根源。
			c.Log("  %s → %s：目标已存在，跳过上传（规则 3）", a.Name, target2)
		} else if err := c.moveAsset(ctx, t.rel, t.rel.ID, target2, a); err != nil {
			return res, err
		} else {
			t.names[target2] = a
			t.dirty = true
			c.Log("  %s → %s（%s，内容判 %s）", a.Name, target2, src.ID, contentABI)
		}

		res.Moved = append(res.Moved, a.Name)
		movedIDs = append(movedIDs, a.ID)

		byVer := placed[src.ID]
		if byVer == nil {
			byVer = map[string]*placedVersion{}
			placed[src.ID] = byVer
		}
		pv, ok := byVer[version]
		if !ok {
			pv = &placedVersion{versionName: meta.VersionName, versionCode: meta.VersionCode}
			byVer[version] = pv
		}
		pv.assets = append(pv.assets, model.IndexAsset{ABI: contentABI, File: target2, Size: a.Size})
	}

	// 先把索引写上盘（仍然在推送之前）：搬运这一步是唯一知道元数据的时刻。
	if len(res.Moved) > 0 {
		for _, id := range targetOrder {
			byVer := placed[id]
			if len(byVer) == 0 {
				continue
			}
			c.recordPlaced(id, byVer)
		}
		if err := WriteIndex(c, c.Index); err != nil {
			return res, err
		}
	}

	for _, id := range targetOrder {
		res.AppIDs = append(res.AppIDs, id)
	}

	// 清场：改回 draft + 删掉**已搬运**的 asset（§3.2 / 规则 6）。
	//
	// 顺序是"先改 draft 再删"：删到一半失败时，`_incoming` 已经是 draft 了，
	// 剩下的 asset 不会因为一次误 Publish 而被重新解析 —— 而反过来，
	// 删干净了却忘了改 draft，下次 Publish 会把空队列搬一遍（无害，但白跑）。
	if len(movedIDs) > 0 {
		if err := c.cleanIncoming(ctx, rel, movedIDs, len(res.Kept)); err != nil {
			return res, err
		}
		res.Cleaned = true
	}

	// 没能安置的留在原地，必须点名 —— 否则它们会静静地攒在 _incoming 里，
	// 而"攒着"表现为"每次 Publish 都搬一遍同样的几个文件"。
	for _, n := range res.Kept {
		c.Log("保留在 `%s`（等待人工决定）：%s", model.IncomingTag, n)
	}
	return res, nil
}

// readIncomingMeta 下载 _incoming 里的一个 asset 并读元数据。
func (c *Ctx) readIncomingMeta(ctx context.Context, a gh.Asset) (*apkmeta.Meta, error) {
	path, cleanup, err := c.downloadToTemp(ctx, a.ID, "forge-incoming-*.apk")
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return apkmeta.Read(path)
}

// moveAsset 把一个 asset 的内容复制到目标 Release 的目标名下。
//
// 注意是"下载再上传"而不是 GitHub 的某种"移动" —— Release asset 没有移动这个操作，
// 只能复制一份再删原件。
func (c *Ctx) moveAsset(ctx context.Context, rel *gh.Release, releaseID int64, target string, a gh.Asset) error {
	path, cleanup, err := c.downloadToTemp(ctx, a.ID, "forge-move-*.apk")
	if err != nil {
		return err
	}
	defer cleanup()

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := c.GH.UploadAsset(ctx, c.Env.StoreRepo, releaseID, target, f); err != nil {
		return fmt.Errorf("搬运 %s → %s：%w", a.Name, target, err)
	}
	return nil
}

// cleanIncoming 把 _incoming 改回 draft，并删掉本次已搬运的 asset。
//
// **绝不用 delete release**（规则 6）：删 Release 会让 tag 消失，
// 而若曾开启 Immutable Releases，那个 tag 会被**永久烧毁**，再建同名会 422。
// draft 只是个状态位，永远可逆。
func (c *Ctx) cleanIncoming(ctx context.Context, rel *gh.Release, movedIDs []int64, keptCount int) error {
	draft := true
	if _, err := c.GH.UpdateRelease(ctx, c.Env.StoreRepo, rel.ID, gh.ReleasePatch{Draft: &draft}); err != nil {
		return fmt.Errorf("把 %s 改回 draft：%w", model.IncomingTag, err)
	}
	c.Log("`%s` 已改回 draft", model.IncomingTag)

	for _, id := range movedIDs {
		if err := c.GH.DeleteAsset(ctx, c.Env.StoreRepo, id); err != nil {
			// 删不掉不该让整次搬运失败：内容已经在正式 Release 里了，
			// 剩下的是"暂存区多留了一份"，下次 Publish 时会被幂等闸门挡住（规则 3）。
			c.Log("删除 _incoming 里已搬运的 asset %d 失败（不影响正式版本，下次会被幂等闸门挡住）：%v", id, err)
		}
	}
	c.Log("清场完成：删除 %d 个已搬运 asset，保留 %d 个待人工处理", len(movedIDs), keptCount)
	return nil
}

// recordPlaced 把 _incoming 搬过来的版本写进索引。
//
// upstreamTag 留空：手动来源**本来就没有上游 tag**，这不是"漏了"。
// build-index 对 manual 源因此不该告警"没有 upstreamTag" —— 那条告警只对 github 源有意义。
func (c *Ctx) recordPlaced(appID string, byVer map[string]*placedVersion) {
	idxApp := c.Index.Find(appID)
	if idxApp == nil {
		c.Index.Apps = append(c.Index.Apps, model.IndexApp{ID: appID, Tag: appID})
		idxApp = c.Index.Find(appID)
	}

	// **必须排序**：一次批量上传可以带同一个 App 的多个版本（§3.2「一次 Publish
	// 可带多个 App 的 APK」），而位次就是 IndexApp.Latest() 的判据 ——
	// 用 map 的遍历顺序会让"哪个是最新"每次跑都不一样。
	//
	// 排序键取 versionCode 升序：它是上游自己的单调计数器，是这里唯一能表达
	// "谁更新"的东西（version token 是自由文本，2.0 之后发 1.9 是真实存在的）。
	// versionCode 拿不到（0）时退到 token 字典序 —— 不确定比错更糟，
	// 而字典序至少是确定的。
	tokens := make([]string, 0, len(byVer))
	for v := range byVer {
		tokens = append(tokens, v)
	}
	sort.SliceStable(tokens, func(i, j int) bool {
		ci, cj := byVer[tokens[i]].versionCode, byVer[tokens[j]].versionCode
		if ci != cj {
			return ci < cj
		}
		return tokens[i] < tokens[j]
	})

	for _, version := range tokens {
		pv := byVer[version]
		assets := sortAssets(pv.assets)
		if v := idxApp.FindVersion(version); v != nil {
			v.Assets = mergeAssets(v.Assets, assets)
			if v.VersionName == "" {
				v.VersionName = pv.versionName
			}
			if v.VersionCode == 0 {
				v.VersionCode = pv.versionCode
			}
			continue
		}
		idxApp.Versions = append(idxApp.Versions, model.IndexVersion{
			Version:     version,
			VersionName: pv.versionName,
			VersionCode: pv.versionCode,
			// 手动上传没有"上游发布时刻"可用。取**现在**：相对顺序正确
			// （它确实是刚进来的），而 releaseDate 在清单里是可选的展示字段。
			PublishedAt: model.NowISO(),
			Assets:      assets,
		})
	}
}
