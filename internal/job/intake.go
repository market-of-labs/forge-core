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

// placedVersion 是搬运过程中为一个 version token 攒起来的分片。
// 与 versionAcc 同构，但**不带 UpstreamTag** —— 手动上传没有上游 tag 可记。
type placedVersion struct {
	versionName string
	versionCode int32
	assets      []model.Asset
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
	// Kind 是判定出来的申请类型，供调用方分流：新增单的身份要联网才定得出来，
	// 所以它走的是一条比变更单长得多的链（见 newsource.go）。
	Kind issue.Kind
	// Accept 为真时 Source/Delete 有意义；为假时 Reply 是拒绝理由。
	Accept bool
	// Hold 为真表示"本单要留在打开状态"，不关单。
	//
	// 新增单**一律**为真：裁决只是那条链的第一段，成不成要等身份定出来才知道。
	// 链走完且落了盘才关单（announceLanded），没走完就留开 —— 而"单开着"正是重试的
	// 全部实现：申请人改完正文（store 侧监听 `issues[edited]`）整条链重新发车。
	// 变更为假 —— 它是即时的，留一张没人再看第二眼的单只是噪音。
	Hold bool
	// Reply 是回评正文，成功与拒绝都要写清原因（§2.5 规则 6）。
	//
	// **被接受的新增单这里为空**：要回评的那些字段（包名、显示名、作者）此刻还不存在，
	// 得等身份探出来 —— 那段话由 landedReply 生成。所以裁决阶段确实无话可说。
	Reply string
	// DescNote 是一句要带给申请人的提醒，由 landedReply 拼在回评末尾。
	//
	// 现在唯一的产出者是 decideAdd 的**截断告知**：简介被裁过就必须说（见那里的注释）。
	// 它不能塞进 Reply（被接受的新增单 Reply 为空），也不能等落盘后再算 ——
	// 原始正文只在这一个函数里出现过，落到 model.Source 上的已经是裁完的值。
	DescNote string
	// Source 是要写入的**完整**内容。注意它是"改完之后的整份"，不是补丁 ——
	// 补丁的写法要求每个调用点都知道"哪份文件"，而 §2.5 规则 2 要的语义
	// （只改申请涉及的字段）在 DecideIntake 里就已经兑现了：它是**从当前文件
	// 复制一份再改**，不是凭申请内容凭空造一个。
	//
	// 新增单里它是**半成品**：ID/Name/Author 都还空着（申请人无从知道它们），
	// 由 landNewSource 探身份之后填上。
	Source *model.Source
	// Delete 为真表示"移除"（§2.5 规则 4：移除 = 删掉该文件）。
	Delete bool
	// Committed 是跑完之后才填的：这一次到底有没有真的提交。
	//
	// 裁决阶段必然为假 —— 那时候还什么都没写。它由 IntakeIssue 填，因为"提交了没有"
	// 只有真正落盘的那几步知道（内容没变时 CommitBack 会安静地不提交），
	// 而调用方（`handle-dispatch` 的摘要行）只能从裁决结果上读。
	Committed bool
	// Summary 一行摘要，给日志与提交信息用。
	Summary string
}

// DecideIntake 解析 issue 正文并给出裁决。**纯函数**：同样的 (sources, body) 必然
// 得到同样的结论，不发一个请求、不写一个字节。
//
// 它是这条链上**唯一**判断"这份申请现在合法吗"的地方：申请人改完正文、整条链重新发车
// 之后重跑的还是它（见 newsource.go）。另写一套判断就会漂移，而漂移的后果是同一份正文
// 在两个时刻得到两个互相矛盾的结论。
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
	// 被拒的变更单也留开，与新增单同一个理由：回评里写着"改完编辑正文即可"，
	// 而一张已经关掉的单不会再有人去动它。成功的变更单即时生效，关掉了事。
	if !d.Accept {
		d.Hold = true
	}
	return d
}

// decideAdd 裁决一份新增申请。
//
// **它不写文件。** 申请人填的只有 repo，appId 要从上游 APK 里读出来，而 DecideIntake
// 是纯函数、不许联网。所以这里只做两件不需要网络的事：取值、本地校验 —— 真正的解析与
// 落盘紧接着在同一条链上做（见 newsource.go）。
//
// 手动上传**不经过这里**（03 §3.2）：没有「来源类型」下拉，也就没有手动源的单子。
//
// 这里也**不再回一段"已收到"**。那一段只是把申请人刚填的东西复述回去，而它与落盘后的
// 「已收录」回评几乎逐行重叠 —— 一张单要读两条一样的表，还得分辨哪条是现在的。留下的
// 那条（landedReply）是**探过身份之后**写的事实，比复述有价值。
func decideAdd(c *Ctx, f *issue.Form) *IntakeDecision {
	r, err := issue.ParseAdd(f)
	if err != nil {
		return reject(fmt.Sprintf("申请缺少必填字段：%v", err))
	}
	if err := validateAdd(r); err != nil {
		return reject(fmt.Sprintf("申请内容不合法：%v", err))
	}
	if other := companionConflict(c, r); other != nil {
		return reject(fmt.Sprintf(
			"`kind: companion` 全局至多一条（02 规则 9），现在已经是 `%s`（%s）了。\n\n"+
				"伴侣应用换成另一个是维护者改 `sources/%s.json` 的事 —— "+
				"**这一条刻意不给申请改**：让一张申请能把整个市场的伴侣应用顶掉，"+
				"代价是全部设备的自更新源被换走。",
			other.ID, originOf(other), other.ID))
	}

	// 简介**裁而不拒**：上限 20 rune 是量出来的（见 model.MaxDescRunes），为一个纯装饰
	// 字段让人重填不值得。但**裁了就必须说** —— 默默把人写的东西切掉、再回评一句
	// "已收录"，是最难发现的那种假回评。这句话跟着决定走到落盘后的那条回评里去。
	desc := model.TruncateDesc(r.Desc)
	note := ""
	if desc != strings.TrimSpace(r.Desc) {
		note = fmt.Sprintf(
			"⚠️ 你填的简介超过了 %d 个字的长度上限，上面显示的是**截断**后的。"+
				"它进的是 Obtainium 列表里的标题行（单行、超出即省略号），写长了显示不全；"+
				"想换一个的话，**编辑正文**重填即可。\n\n",
			model.MaxDescRunes)
	}

	return &IntakeDecision{
		Accept:  true,
		Summary: fmt.Sprintf("新增 %s", r.Repo),
		// 此刻还没有 appId，所以 Source 只填得出上游那半边。ID/Name/Author
		// 由 landNewSource 探身份时填（它们是从 APK 与仓库里读出来的，不是猜的）。
		Source: &model.Source{
			Source: model.SourceGitHub,
			Upstream: &model.Upstream{
				Type:         model.UpstreamGitHubRelease,
				Repo:         r.Repo,
				AssetPattern: r.AssetPattern,
				// 这一项必须在**这里**就带上：probeIdentity 要用同一个候选集去探身份，
				// 而"最新那个 release 是 prerelease"正是开这个开关的场景 ——
				// 探身份时若把它跳掉，会从一个更老的 release 里读出身份（甚至报"没有可镜像的发布"）。
				IncludePrerelease: r.IncludePrerelease,
			},
			Kind:         r.Kind,
			Categories:   r.Categories,
			ABIWhitelist: r.ABIWhitelist,
			Desc:         desc,
		},
		DescNote: note,
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
	// kind 的规则与手改文件那条入口共用同一个函数，不另写一份判断。
	return model.ValidateKind(r.Kind)
}

// originOf 用一句话说明这条来源的二进制从哪来：上游仓库，或手动上传队列。
//
// 两处拒绝回评要念它（撞包名、撞 companion），而"手动来源没有上游"是个会让消息直接
// 崩掉的 nil —— 所以这句话只在这里说一次。
func originOf(s *model.Source) string {
	if s.Upstream == nil {
		return "手动上传来源，没有上游"
	}
	return fmt.Sprintf("来自 `%s`", s.Upstream.Repo)
}

// companionConflict 检查这份申请会不会造出**第二条** kind=companion —— 是的话返回占位
// 的那一条，否则 nil。
//
// 为什么这条规则要提前到这里判：`CheckSourceSet` 已经有一条同样的规则（02 规则 9），
// 但它在**落盘之后**才跑 —— 那时第二份文件已经在 `sources/` 里了，而后续的
// check-manifest 会因此判**硬失败**，整份清单推不出去，直到有人手动删掉那个文件。
// 一张陌生人的申请能把整个 store 卡住，这是这套流程里唯一一处"外部输入能造成持续故障"
// 的地方，堵在门口最省事。
//
// **同一条目重跑不算冲突**：编辑正文重新发车是这套设计的重试路径（见 landNewSource），
// 而那时已有的 companion 就是这份申请自己写的 —— 判据见 sameCompanionRequest。
func companionConflict(c *Ctx, r *issue.AddRequest) *model.Source {
	if r.Kind != model.KindCompanion {
		return nil
	}
	// 逐条扫、**不按来源种类筛**：`kind` 只有新增单能写（D47），而手动来源没有新增单
	// （D52），所以"有没有上游"在这里不是判据 —— 判据就是 `kind` 本身。
	for i := range c.Sources {
		s := &c.Sources[i]
		if s.Kind != model.KindCompanion || sameCompanionRequest(s, r) {
			continue
		}
		return s
	}
	return nil
}

// sameCompanionRequest 判断一条已落的 companion 来源是不是**这份申请自己**写出来的。
//
// 与 sameRequest 同一套思路（比"这份申请能决定的那一半"）：此刻 appId 还没探出来，
// 所以比的是上游仓库。
func sameCompanionRequest(s *model.Source, r *issue.AddRequest) bool {
	return s.Upstream != nil && s.Upstream.Repo == r.Repo
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
				"想新增一个来源请改用 **`add-source.yml`**（03 §2.5 规则 5）；"+
				"如果这是**手动上传**的 APK，那它还没有目的地 —— 先把它传进 `_incoming`，"+
				"条目会在搬运时按 APK 内容建出来（03 §3.2），之后再来改。", r.AppID))
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
		// 因此无法通过 issue 把分类清掉 —— 那是模板表达力的限制，不是这条路径的
		// 疏漏；真要清空只能由维护者直接改 `sources/{appId}.json` 里的那一段。
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
	"整条链会重新发车，不用重开一张。\n" +
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
// 返回的第二个值是新增单那一次单项目同步的结果（变更单恒为 nil）。
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
func IntakeIssue(ctx context.Context, c *Ctx, number int) (*IntakeDecision, *ReconcileResult, error) {
	if err := c.Env.RequireToken("处理 issue"); err != nil {
		return nil, nil, err
	}

	is, err := c.GH.GetIssue(ctx, c.Env.StoreRepo, number)
	if err != nil {
		return nil, nil, fmt.Errorf("读 issue #%d：%w", number, err)
	}
	if is.PullRequest != nil {
		// PR 也会出现在 issues 端点里，而它的"正文"是代码 diff，不是申请。
		c.Log("issue #%d 其实是个 PR，跳过", number)
		return nil, nil, nil
	}
	if is.State == "closed" {
		c.Log("issue #%d 已关闭，跳过（幂等）", number)
		return nil, nil, nil
	}

	// issue 正文是**不可信输入**，DecideIntake 是纯函数，这里之前没有任何 IO 副作用。
	d := DecideIntake(c, is.Body)

	switch {
	case d.Kind == issue.KindAdd && d.Accept:
		// 新增单：整条链在这里跑完 —— 探身份 → 落盘 → 同步这一个应用 → 回评 → 关单。
		// 它自己负责收尾，所以这一支直接返回，下面那套"回评 + 按 Hold 关单"的通用
		// 收尾只服务变更单与被拒的申请（见 newsource.go）。
		r, err := c.intakeNewSource(ctx, *is, d)
		return d, r, err

	case d.Accept:
		// 变更单（改元数据 / 暂停 / 移除）：即时生效，当场落盘提交。
		if d.Delete {
			if err := c.Repo.DeleteSource(d.Source.ID); err != nil {
				return d, nil, fmt.Errorf("删除 sources/%s.json：%w", d.Source.ID, err)
			}
			c.Log("#%d 移除 sources/%s.json", number, d.Source.ID)
		} else {
			if err := c.Repo.WriteSource(d.Source); err != nil {
				return d, nil, fmt.Errorf("写 sources/%s.json：%w", d.Source.ID, err)
			}
			c.Log("#%d 写入 sources/%s.json：%s", number, d.Source.ID, d.Summary)
		}
		// 只提交本次改动涉及的路径（CommitBack 内部固定暂存范围），
		// 并且**提交信息里带上 issue 号**：回看 store 历史时能直接对上。
		committed, err := c.CommitBack(ctx, fmt.Sprintf("%s（#%d）", d.Summary, number))
		d.Committed = committed
		if err != nil {
			return d, nil, err
		}

	default:
		c.Log("#%d 拒绝：%s", number, d.Summary)
	}

	// 回评是**无条件**的（§2.5 规则 6：拒绝也要写清原因）。
	// 而关单跳变才发生 —— 见 Hold 的说明。
	if err := c.GH.CommentIssue(ctx, c.Env.StoreRepo, number, d.Reply); err != nil {
		return d, nil, fmt.Errorf("回评 issue #%d：%w", number, err)
	}
	if d.Hold {
		c.Log("#%d 回评并留开（改完正文会重新发车）", number)
		return d, nil, nil
	}
	if err := c.GH.CloseIssue(ctx, c.Env.StoreRepo, number); err != nil {
		return d, nil, fmt.Errorf("关闭 issue #%d：%w", number, err)
	}
	c.Log("#%d 回评并关闭", number)
	return d, nil, nil
}

// ---- intake-incoming：_incoming → 正式 Release（03 §3.2 / §4.6） -------------

// incomingRelease 找到 `_incoming` 队列，没有则返回 (nil, nil)。
//
// ⚠️ **不能**用 `/releases/tags/_incoming` 取。那个端点的官方描述是「Get a
// **published** release with the specified tag」—— draft 一律 404（draft 还没有真
// tag，GitHub 把它挂在 untagged-* 引用下）。而队列的常态**就是** draft（§3.2：
// 上传与搬运都不经过"发布"这个动作），所以 by-tag 取到的永远是 404。
//
// 改用列表筛 tag_name：有 push 权限的身份调这个列表会**同时拿到 draft 与已发布**的
// Release，于是队列在哪个状态都找得到 —— 这正是要的：状态不该决定能不能搬运。
// 分页不能省（ListReleases 里已按 per_page=100 翻页）：Release 数随收录数增长，
// 越过一页之后队列会被挤出去，症状是静默的「store 里没有 _incoming」。
//
// ⚠️ **认不出队列时的修法是把队列的 tag 名改回去，不是把这里的比对放宽。**
// 别加"draft 且 tag 是 `untagged-*` 就当队列"这类看似容错的条件：下游的 cleanIncoming
// 会**删掉**它匹配到的那个 Release 里的 asset —— 认错对象就是删错文件，而 store 里的
// draft 并不只有队列（人可以用 draft 起草任何东西）。所以这里只能按 tag 名严格认；
// 认不出就报错，由 IntakeIncoming 把运行染红，让人去修队列。
func (c *Ctx) incomingRelease(ctx context.Context) (*gh.Release, error) {
	rels, err := c.GH.ListReleases(ctx, c.Env.StoreRepo)
	if err != nil {
		return nil, fmt.Errorf("列 %s 的 Release：%w", c.Env.StoreRepo, err)
	}
	for i := range rels {
		if rels[i].TagName == model.IncomingTag {
			return &rels[i], nil
		}
	}
	return nil, nil
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
// # 谁来叫它
//
// **显式触发，没有"自动"这回事**（§3.2）：人传完文件点一次手动按钮（`store` 的
// `forward-to-forge` 或 `forge` 的 `on-dispatch`，两者等效 —— 前者转发一手），或让 CI
// 发一个 repository_dispatch。队列**不经过"发布"这个动作** —— 它是常驻 draft，而 draft
// 不会产生任何 `release` 事件，往它上传/改名/删 asset 也不产生（§3.3）。所以
// "上传之后什么都没发生"是**设计**，不是故障；这一节存在的意义就是把这句话钉在
// 读代码的人眼前，免得下一个人又去接一个永远不会响的钩子。
//
// # ABI 与版本号一律来自 APK 内容
//
// 与 mirror-upstream 同一条规矩（信内容，改名按内容）：文件名叫什么都不影响
// 它被安放的名字。这里连"文件名上的 ABI 线索"都不再比对告警 —— 手动上传的
// 文件名本来就是人随手打的，逐条告警只会制造噪音。内容是什么就是什么。
//
// # 没有家的 APK 当场建条目
//
// `sources/` 里查不到这个包名，是**手动上传新来源**的正常样子（03 §3.2），不是错误：
// 条目在这里按 APK 内容建出来（appId = package，显示名 = label，作者先记「未知」），
// 于是"传一个 APK"就是收录一个新来源的全部动作。代价是这一轮会顺带写一个来源文件，
// 与它的账本一起提交。
//
// # 安置不了的东西一律**留在原地**
//
// 删除是单向的：删掉就没了，而"这个 asset 有别的问题"正是最需要人来看一眼的情况。
// 所以本函数只删除**确认已安置**的 asset，其余的原样留在 `_incoming` 里并逐个
// 说明原因（§3.2：不静默丢弃）。
func IntakeIncoming(ctx context.Context, c *Ctx) (res *IncomingResult, err error) {
	if err := c.Env.RequireToken("搬运 _incoming"); err != nil {
		return nil, err
	}
	res = &IncomingResult{}

	rel, err := c.incomingRelease(ctx)
	if err != nil {
		return nil, err
	}
	if rel == nil {
		// 队列不存在：通常是把 tag 打错了，或者还没建。**不自动建** —— 一个空的 draft
		// 建出来只会掩盖"你其实传到了别处"这个事实。
		//
		// 但这条路**必须染红**，不能只是记一行日志就 `return nil`：人点这个按钮的前提
		// 是"我刚往队列传了文件"，所以这里认不出队列 ≈ 那次上传白传了。从前它是一行
		// 日志加一次绿色运行 —— 而"绿色且什么都没发生"正是这类故障的完整外观，人不会
		// 去翻日志（2026-09-16 实际撞上过：队列被发布过一次，tag_name 被降级成
		// `untagged-*`，incomingRelease 从此认不出它，点按钮毫无反馈）。
		//
		// 错误里要给出那一条手工出路：tag_name 被降级是**已知**的成因，而用户此刻
		// 需要的正是"那我去把什么改回什么"。
		return res, fmt.Errorf("store 里没有 tag 为 `%s` 的 Release（03 §3.2：它是常驻队列，"+
			"不自动建）。若队列其实在、只是 tag 名被降级成了 `untagged-*`（发布过又被改回 "+
			"draft 会这样，见 cleanIncoming 的告警），把它的 tag 名改回 `%s` 再重试",
			model.IncomingTag, model.IncomingTag)
	}

	// 清场是**退出时的无条件动作**，不是"搬成功之后再做的一件事"（§3.2 / 规则 6）。
	// 无论从哪条路出去 —— 空队列、白名单拒绝、全被保留、中途某个 API 报错 ——
	// 队列都要被留在"干净且仍是 draft"的样子。逐个 return 去补一定会漏一个出口，
	// 所以由 defer 统一兜住。
	//
	// 顺序上"先确保 draft 再删 asset"：删到一半失败时，队列已经是干净状态了。
	var movedIDs []int64
	defer func() {
		// 收尾要给足机会：ctx 可能已经超时/被取消，而这一步恰恰是那时最要紧的
		// 一件事 —— 用一个不受取消影响的 ctx 去发它。
		cctx := context.WithoutCancel(ctx)
		cerr := c.cleanIncoming(cctx, rel, movedIDs, len(res.Kept))
		if cerr == nil {
			res.Cleaned = true
			return
		}
		// 必须吼出来，并把手工出路写在脸上。
		c.Log("⚠️ `%s` 清场失败：%v —— 请手动把队列改回 draft 并删掉已搬走的 asset（03 §3.2）",
			model.IncomingTag, cerr)

		// 清场失败只在**这一轮本来就没出别的事**时才把运行染红。有主错时让主错说话：
		// 它比"收尾没做完"更值钱，盖掉它就等于把真正的现场藏起来（那个 err 上面
		// 逐条 return 时已经带出来了）。
		if err == nil {
			err = fmt.Errorf("清场失败：%w", cerr)
		}
	}()

	assets, err := c.GH.ListAssets(ctx, c.Env.StoreRepo, rel.ID)
	if err != nil {
		// ⚠️ 出了这个函数体，返回的 res **必须**是非 nil 的：上面那个 defer 要读
		// res.Kept 去写清场日志，`return nil, ...` 会让它空指针。
		// 调用方都是拿到 err 就立刻返回，不会去碰这个 res。
		return res, fmt.Errorf("列 %s 的 asset：%w", model.IncomingTag, err)
	}
	if len(assets) == 0 {
		// 规则 3：空队列没有可搬的东西。点错了按钮、或上一轮刚搬完又点一次，
		// 都会落到这里。清场仍然由下面那个 defer 做（它不区分出口）。
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
			// 找不到家 = 这是一次**手动上传的新来源**（03 §3.2）：条目由这条流程当场建。
			// 手动上传不再走新增单，所以这里没有"先去开张单"那一档。
			var err error
			if src, err = c.createManualSource(meta); err != nil {
				res.Kept = append(res.Kept, a.Name)
				c.LogBlock("  ", fmt.Sprintf("%s：%v —— **保留**在 _incoming，请人工确认", a.Name, err))
				continue
			}
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
		pv.assets = append(pv.assets, model.Asset{ABI: contentABI, File: target2, Size: a.Size})
	}

	// 先把账本写上盘（仍然在推送之前）：搬运这一步是唯一知道元数据的时刻。
	if len(res.Moved) > 0 {
		var touched []string
		for _, id := range targetOrder {
			byVer := placed[id]
			if len(byVer) == 0 {
				continue
			}
			c.recordPlaced(id, byVer)
			touched = append(touched, id)
		}
		if err := writeLedgers(c, touched); err != nil {
			return res, err
		}
	}

	for _, id := range targetOrder {
		res.AppIDs = append(res.AppIDs, id)
	}

	// 清场（改回 draft + 删掉已搬运的 asset）不在这里做 —— 开头那个 defer 负责。
	// 放在这里只能覆盖走得最顺的那条路，而漏掉的每一条出口都是一次静默死锁。

	// 没能安置的留在原地，必须点名 —— 否则它们会静静地攒在 _incoming 里，
	// 而"攒着"表现为"每次 Publish 都搬一遍同样的几个文件"。
	for _, n := range res.Kept {
		c.Log("保留在 `%s`（等待人工决定）：%s", model.IncomingTag, n)
	}
	return res, nil
}

// createManualSource 给一个**还没有家**的 APK 当场建一条手动来源（03 §3.2）。
//
// 手动上传的入口就是"把 APK 传进 `_incoming`"，没有配套的新增单 —— 所以条目在这里
// 由流程自己建。能自动读出来的（包名 = appId、label = 显示名）就不让人再抄一遍，
// 抄一遍只会多一个抄错的地方。
//
// 作者只能先记 model.AuthorUnknown：APK 里没有这个字段，也没有上游仓库可以取 owner，
// 而 `author` 空着会让**整份清单**判失败（见那个常量的说明）。改它走 `change-source.yml`。
//
// 落盘但**不提交** —— 提交由调用方在账本也写完（或者自检过）之后统一做，
// 好让"新建的来源"与它的账本进同一个提交。
func (c *Ctx) createManualSource(meta *apkmeta.Meta) (*model.Source, error) {
	name := meta.Label
	if !meta.HasLabel() {
		// 空 label 是真实存在的（属性缺失，或指向 `@string/app_name` 而解不出来）。
		// 退到包名 —— 不好看，但**有**：空显示名在客户端里就是一行空白，而这一行
		// 是用户唯一会扫的东西。是哪一种情况只在这句日志里说得出，所以必须说。
		name = meta.Package
		c.Log("  ⚠️ APK 里读不出 label，显示名先记成包名 %q，之后用 change-source.yml 改掉", name)
	}
	src := &model.Source{
		ID:     meta.Package,
		Name:   name,
		Author: model.AuthorUnknown,
		Source: model.SourceManual,
	}
	// 校验在 WriteSource 里（与手改文件那条入口同一批规则）；这里只把失败的上下文
	// 补成"是这个 APK 的包名不能用"，因为调用点拿到的是一个要写进日志的错误。
	if err := c.Repo.WriteSource(src); err != nil {
		return nil, fmt.Errorf("把这个 APK 的包名 %q 当 appId 建条目失败了：%w", meta.Package, err)
	}
	c.Log("  新建 sources/%s.json：显示名 %q，作者先记 %q（待用 change-source.yml 补）",
		src.ID, src.Name, model.AuthorUnknown)
	c.Sources = append(c.Sources, *src)
	// 重新取一次指针：append 可能换了底层数组，上面那个 `src` 已经不指向 c.Sources 了。
	return c.Source(meta.Package), nil
}

// readIncomingMeta 下载 _incoming 里的一个 asset 并读元数据。
func (c *Ctx) readIncomingMeta(ctx context.Context, a gh.Asset) (*apkmeta.Meta, error) {
	path, cleanup, err := c.downloadToTemp(ctx, c.Env.StoreRepo, a.ID, "forge-incoming-*.apk")
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
	path, cleanup, err := c.downloadToTemp(ctx, c.Env.StoreRepo, a.ID, "forge-move-*.apk")
	if err != nil {
		return err
	}
	defer cleanup()

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	// 长度取自文件本身（UploadAsset 要如实报 Content-Length，见那里的 ⚠️）。
	fi, err := f.Stat()
	if err != nil {
		return err
	}

	if _, err := c.GH.UploadAsset(ctx, c.Env.StoreRepo, releaseID, target, f, fi.Size()); err != nil {
		return fmt.Errorf("搬运 %s → %s：%w", a.Name, target, err)
	}
	return nil
}

// cleanIncoming 把 _incoming 保持在 draft，并删掉本次已搬运的 asset。
//
// 那句 PATCH 现在**本不该有任何效果** —— 队列常驻 draft（§3.2）。留着它是为了把
// 不变量自己拧住：draft Release 的编辑页上那个「Publish release」按钮一直都在，
// 万一有人按习惯点了，下一次搬运就把它掰回来，而不是让队列悄没声地变成已发布。
// 幂等，多余的一次 PATCH 不值一提。
//
// ⚠️ **但它是这里唯一会失败、且失败很要紧的一步**：PATCH 没能把队列掰回 draft，
// 说明队列卡在"已发布"上 —— published → draft 这条回头路已在真环境验证过走得通
// （2026-09-16），真失败时唯一的出路是人去网页上看一眼（03 §3.2）。
// 所以它失败**不再短路掉下面的删除**：要删的是"已经安置好的那份的存根"，留着它
// 一点用都没有，只会让队列越积越脏、下一轮再被幂等闸门挨个挡一遍。两件事都做完，
// 再把 draft 那个错报上去。
//
// **绝不用 delete release**（规则 6）：删 Release 会让 tag 消失，
// 而若曾开启 Immutable Releases，那个 tag 会被**永久烧毁**，再建同名会 422。
func (c *Ctx) cleanIncoming(ctx context.Context, rel *gh.Release, movedIDs []int64, keptCount int) error {
	// 走 Unpublish 而不是手写一遍 UpdateRelease：make_latest=false 那条规矩（规则 4）
	// 在那里拧着，而这里手写时漏了它。tag 名也一样 —— 见下。
	//
	// 返回值不能丢：下面拿它核对 tag 名有没有留住。
	after, draftErr := c.GH.Unpublish(ctx, c.Env.StoreRepo, rel.ID, model.IncomingTag)
	if draftErr == nil {
		c.Log("`%s` 保持在 draft", model.IncomingTag)

		// ⚠️ 这一步从前只发 {draft, make_latest}，而**光发那两个字段就等于把队列报废**：
		// 不带 tag_name 时 GitHub 会把 tag 名换成一个 `untagged-<sha>` 占位名 —— 与它原本
		// 叫什么无关，哪怕它本来就已是 draft（2026-09-16 在真队列上实测两次 PATCH 各配一次
		// 独立 GET：`_incoming` → `untagged-9d3d2b2b…`；带上 tag_name 则稳住）。
		//
		// 于是每次搬运成功都会把队列打废一次：incomingRelease 按 tag_name 认队列，名字一掉
		// 就再也认不出来，人传上去的 APK 停在队列里不动、运行还是绿的 —— 一天里连撞两次的
		// 「传了却没反应」。现在 tag 名跟着一起发回去了，这条不变量就由这一步自己维持。
		//
		// 留这个核对是**当哨兵**，不是补丁：发是发了，但"GitHub 老实按它说的做"这件事我们
		// 只能从外面观察（它就是刚刚才教会我们别信默认行为）。真响起来说明修法失效了，那时
		// 现场只剩这个返回值。
		if after != nil && after.TagName != model.IncomingTag {
			c.Log("⚠️ tag 名没留住，成了 %q（发的是 `%s`）—— 下一次搬运会认不出队列，"+
				"要先把 tag 名改回 `%s`（03 §3.2 的手工出路）",
				after.TagName, model.IncomingTag, model.IncomingTag)
		}

		// 连同 tag 引用一起清掉。**必须删，而且要在这里删**：
		//
		// `release` 事件跑的是 tag 所指提交上的 workflow，而引用一旦建立就不再移动
		// （详见 gh.DeleteTagRef 的说明）。留着它，下一次 Publish 就会拿一份旧的
		// forward.yml 去解析触发器 —— 默认分支上怎么改都看不见。删掉它，Publish 会在
		// 当时的默认分支 HEAD 上重建，队列这条路才跟得上默认分支。
		//
		// 位置的两条约束：在 Unpublish **之后**（之前 Release 还挂在这个引用上，删引用
		// 等于把现场拆了），且只在 Unpublish 成功时删（失败就说明队列还是 published 的，
		// 那时引用是它的标签，删了就成了悬空的 published Release）。
		//
		// 删不掉不让整次搬运失败 —— 与下面删 asset 同一条理由：内容已经安顿好了，
		// 剩下的是"下次 Publish 可能仍旧跑不动"，而那是下一次的事。
		if err := c.GH.DeleteTagRef(ctx, c.Env.StoreRepo, model.IncomingTag); err != nil {
			c.Log("删除 `%s` 的 tag 引用失败：下一次 Publish 可能仍旧解析到旧的 forward.yml"+
				"（症状是「Publish 了却没反应」）：%v", model.IncomingTag, err)
		}
	}

	for _, id := range movedIDs {
		if err := c.GH.DeleteAsset(ctx, c.Env.StoreRepo, id); err != nil {
			// 删不掉不该让整次搬运失败：内容已经在正式 Release 里了，
			// 剩下的是"暂存区多留了一份"，下次搬运时会被幂等闸门挡住（规则 3）。
			c.Log("删除 _incoming 里已搬运的 asset %d 失败（不影响正式版本，下次会被幂等闸门挡住）：%v", id, err)
		}
	}
	c.Log("清场完成：删除 %d 个已搬运 asset，保留 %d 个待人工处理", len(movedIDs), keptCount)

	if draftErr != nil {
		return fmt.Errorf("把 %s 保持在 draft：%w", model.IncomingTag, draftErr)
	}
	return nil
}

// recordPlaced 把 _incoming 搬过来的版本写进账本。
//
// upstreamTag 留空：手动来源**本来就没有上游 tag**，这不是"漏了"。
// build-index 对 manual 源因此不该告警"没有 upstreamTag" —— 那条告警只对 github 源有意义。
func (c *Ctx) recordPlaced(appID string, byVer map[string]*placedVersion) {
	// 一定有：applyIntake 的调用方已经按 c.Source 校验过每个 id（搬运本来就要先知道
	// 往哪个 Release 放）。所以这里不需要"凭空造一个"的分支。
	src := c.Source(appID)

	// **必须排序**：一次批量上传可以带同一个 App 的多个版本（§3.2「一次 Publish
	// 可带多个 App 的 APK」），而位次就是 Source.Latest() 的判据 ——
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
		if v := src.FindVersion(version); v != nil {
			v.Assets = mergeAssets(v.Assets, assets)
			if v.VersionName == "" {
				v.VersionName = pv.versionName
			}
			if v.VersionCode == 0 {
				v.VersionCode = pv.versionCode
			}
			continue
		}
		src.Versions = append(src.Versions, model.Version{
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
