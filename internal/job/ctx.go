package job

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/market-of-labs/forge-core/internal/gh"
	"github.com/market-of-labs/forge-core/internal/gitx"
	"github.com/market-of-labs/forge-core/internal/model"
	"github.com/market-of-labs/forge-core/internal/store"
)

// SkipDispatch 是每个回写提交都必须带上的标记（03 §4.5 规则 7）。
//
// # 它防的是什么
//
// 一次**自激振荡**：forge 每次回写改的恰好是 `sources/`（收录、变更、账本重建都写那里），
// 所以只要 store 那边有一条监听 `push: paths:['sources/**']` 的触发器，回路就闭合了 ——
// 回写 → push → 转发 → 对账 → 回写 → …（**PAT 触发的 push 不会被 GitHub 抑制**，
// 这条回路不会自己停）。
//
// # 那条触发器现在并不存在 —— 这个标记是**留着的保险**
//
// 2026-09-16 store 删掉了那个 `push:` 触发面（它当时是"改一个来源文件就自动对账"，
// 而那件事已经由定时对账全覆盖）。**但这个标记没有跟着删**，理由是：删掉它省不下任何
// 东西，而一旦哪天有人觉得"push 即对账"更实时、把它加回来，一个没有标记的回路是
// **静默的** —— 没有任何一步会报错，只会看到 Actions 一直在跑、提交记录一直在长。
// 留着它，那条触发器将来无论被谁加回来，回路都是断的。
//
// 所以：**不要因为"现在没有 push 触发器"就删掉它**。上面那段就是它至今还在的全部理由。
//
// # 匹配方式
//
// 是 `contains`（子串），不是整行相等 —— 只要出现在提交信息里即可（`commitMsg + " " + SkipDispatch`）。
// 放在这里而不是让每个调用点自己拼，是因为漏一次的代价是无限循环。
const SkipDispatch = "[skip-dispatch]"

// Ctx 是一次维护动作的全部上下文：一个 store 工作副本 + 一个 GitHub 客户端 + 已加载的数据。
//
// 动词之间**不互相调用**（除了编排器调动词），每个动词都是 `func(ctx, *Ctx, …) error`，
// 于是任何一个都能在测试里用一份临时目录 + 一个 httptest 服务器直接跑。
type Ctx struct {
	Env *Env
	GH  *gh.Client
	Git *gitx.Client
	// Repo 指向本次使用的 store 工作副本（可能与 Env.StoreDir 相同）。
	Repo store.Repo
	// Log 记一行诊断。为 nil 时静默。
	Log func(format string, a ...any)

	// 以下两项由 Load 填充。
	//
	// **版本账本就在 Sources 里**（`Source.Versions`）—— 它原来是一份独立的 `Index`
	// 字段（来自 `store/index.json`），合并之后不再有第二个容器，也没有"两份要对齐"。
	Sources   []model.Source
	Endpoints model.Endpoints

	// cloned 表示工作副本是我们自己克隆的临时目录，Close 时该删掉。
	cloned bool
}

// Open 准备一次运行：建客户端、准备 store 工作副本、加载数据。
//
// 工作副本有两种来源，语义不同：
//
//	Env.StoreDir 非空 → 用调用方给的目录，**不克隆也不删**。多步骤的 workflow 里
//	                    每一步都是独立进程，所以只能由 workflow 自己 checkout 一次、
//	                    每一步复用；同时也让本地调试能对着一个真实目录跑。
//	Env.StoreDir 为空 → 自己浅克隆到临时目录，Close 时删。适合"一条命令干完所有事"。
func Open(ctx context.Context, env *Env, log func(string, ...any)) (*Ctx, error) {
	if log == nil {
		log = func(string, ...any) {}
	}

	ghc, err := gh.New(gh.Config{
		Token:         env.Token,
		BaseURL:       env.APIBase,
		UploadBaseURL: env.UploadBase,
	})
	if err != nil {
		return nil, err
	}

	c := &Ctx{Env: env, GH: ghc, Log: log}

	if env.IsOwnCheckout() {
		c.Repo = store.Repo{Root: env.StoreDir}
		c.Git = &gitx.Client{Dir: env.StoreDir, Token: env.Token, Log: log}
		log("使用已有工作副本 %s", env.StoreDir)
	} else {
		dir, err := os.MkdirTemp("", "forge-store-*")
		if err != nil {
			return nil, fmt.Errorf("建临时目录：%w", err)
		}
		// Clone 失败时 MkdirTemp 建出来的目录还在，得自己清 —— 它不在 t.TempDir 的
		// 生命周期里，没人替我们收。
		git, err := gitx.Clone(ctx, env.CloneURL(env.StoreRepo), dir, env.Token, log)
		if err != nil {
			os.RemoveAll(dir)
			return nil, err
		}
		c.Repo = store.Repo{Root: dir}
		c.Git = git
		c.cloned = true
		log("浅克隆 %s 到 %s", env.StoreRepo, dir)
	}

	if err := c.Load(); err != nil {
		c.Close()
		return nil, err
	}
	log("工作副本位于 %s（HEAD %s）", c.Repo.Root, c.Git.HeadSHA(ctx))
	return c, nil
}

// Close 清掉自己克隆的临时目录。用调用方给的副本时什么都不做 ——
// 那可能是开发者的真实工作区，删掉是灾难性的。
func (c *Ctx) Close() {
	if c.cloned {
		os.RemoveAll(c.Repo.Root)
		c.cloned = false
	}
}

// Load 从工作副本读出两份输入（sources 自带版本账本 + endpoints）。
//
// 两份都读全再返回：它们互相依赖（模板对不上会让清单整个不可信），所以"读了一个就开工"
// 只会把失败推到更晚、更难看出来的地方。
func (c *Ctx) Load() error {
	ep, err := c.Repo.LoadEndpoints()
	if err != nil {
		return err
	}
	c.Endpoints = ep

	srcs, err := c.Repo.LoadSources()
	if err != nil {
		return err
	}
	c.Sources = srcs
	return nil
}

// Source 按 id 取一条来源。
func (c *Ctx) Source(id string) *model.Source {
	for i := range c.Sources {
		if c.Sources[i].ID == id {
			return &c.Sources[i]
		}
	}
	return nil
}

// ---- 回写 -------------------------------------------------------------------

// TrackedPaths 是 forge 允许回写的**全部**路径（03 §5.5：commit-back 是唯一出口）。
//
// 刻意列出而不是 `git add .`：
//
//   - `sources/` 整个目录（含删除）—— issue 流程与对账（写 `versions` 账本）都会改它。
//   - `repo/` 对外伺服的索引（`index-v2.json` / `entry.jar` / `index-v1.jar` / …）。
//     它的 APK 是 `.gitignore` 掉的（见 store 的包注释），所以这一条实际只会暂存索引。
//   - `store/fdroid/metadata/` —— fdroidserver 读的那份输入，也是人**唯一**能审阅的那半。
//
// ⚠️ 第三项刻意写的是 `store/fdroid/metadata` 而**不是** `store/fdroid`（计划里写的是后者）。
// 差别是安全性的：`store/fdroid/` 里还躺着 `config.yml`，而那个文件里**明文放着签名私钥的口令**
// （见 fdroid.RenderConfig 的说明）。把整个 `store/fdroid` 纳入暂存范围，就变成了
// "靠 store 的 `.gitignore` 记得排除它" —— 而 `.gitignore` 是一份**数据仓库里**的文件，
// 它可以被误删、可以被一条 `git add -f` 绕过、也可以在 03 的第一次部署里还没到位。
// 收窄到 metadata 之后，口令在**结构上**进不了暂存区，与它是否被忽略无关。
//
// **不含 `store/endpoints.json`**：那是人改的部署配置，forge 永远不写它。
// 把它纳入暂存范围等于给"某次误改模板"开了一条自动提交的路。
func (c *Ctx) TrackedPaths() []string {
	return []string{
		store.SourcesDirName,
		store.RepoDirName,
		store.FdroidMetadataDirRel(),
	}
}

// CommitBack 是 forge 回写 store 的**唯一出口**（03 §5.5）。
//
// 它做三件事，缺一不可：补上 [skip-dispatch]（规则 7，防自激）、按固定路径暂存
// （不 `git add .`）、推送。没有改动时安静地什么都不做并返回 false ——
// 一次什么都没发生的对账不该在 store 历史里留下一行。
//
// 返回 committed 表示是否真的推了一次。
func (c *Ctx) CommitBack(ctx context.Context, message string) (bool, error) {
	if err := c.Env.RequireToken("回写 store"); err != nil {
		return false, err
	}
	if !strings.Contains(message, SkipDispatch) {
		message = message + " " + SkipDispatch
	}

	committed, err := c.Git.Commit(ctx, gitx.DefaultIdentity, message, c.TrackedPaths()...)
	if err != nil {
		return false, err
	}
	if !committed {
		c.Log("回写：工作区没有改动，不产生提交")
		return false, nil
	}
	if err := c.Git.Push(ctx); err != nil {
		return false, err
	}
	c.Log("回写完成：%s", message)
	return true, nil
}

// ---- Release -----------------------------------------------------------------

// EnsureRelease 取（必要时新建）某个 appId 的内部 Release。
//
// tag 就是 appId 本身（D21）—— 它不含版本段，所以一个 App 的所有版本共用同一个 Release，
// 版本与 ABI 的唯一载体是 asset 文件名（02 §2.4）。
//
// 新建时 `draft: false`：内部 Release 要**已发布**才能被客户端匿名下载，
// 而它的"最新"与否不影响任何东西 —— 我们在每次改动时都显式 make_latest=false（规则 4），
// 客户端也不按 latest 找版本（它按文件名）。
func (c *Ctx) EnsureRelease(ctx context.Context, appID, name string) (*gh.Release, error) {
	tag, err := c.Endpoints.Tag(appID)
	if err != nil {
		return nil, fmt.Errorf("渲染 %s 的 tag：%w", appID, err)
	}

	rel, err := c.GH.GetRelease(ctx, c.Env.StoreRepo, tag)
	if err == nil {
		return rel, nil
	}
	if !isNotFound(err) {
		return nil, err
	}
	if err := c.Env.RequireToken("新建 Release"); err != nil {
		return nil, err
	}
	if name == "" {
		name = appID
	}
	c.Log("Release %s 不存在，新建", tag)
	return c.GH.CreateRelease(ctx, c.Env.StoreRepo, tag, name, false)
}

// assetsOf 列全一个 Release 的 asset，并按"背后有没有字节"分成两组。
//
// ghosts 是那些**占着名字却取不到字节**的（`gh.Asset.Downloadable` 为假）。两组必须
// 分开，混在一起就是 2026-09-17 那次事故的成因：幽灵被当成"文件在" ⇒ 镜像按名幂等跳过
// （那一版永远不重传）、账本记下一个取不到的版本 ⇒ 水位线一跳跳过它 ⇒ 上游 p26 卡住，
// p27…p35 永远轮不到，而**整轮是绿的**（两条 WARN，`committed=true`）。
//
// 所以「名字在 ⟺ 文件在」这条不变量要在这里恢复 —— 它是 03 §3.1"幂等按 asset 名判断"
// 那条规则的前提，而幽灵恰好能证伪它。幽灵单独给出来只为一个用途：镜像撞上它时必须
// **喊出来**（同名不能覆盖、又不能替人删，见 mirrorPlan）。
func (c *Ctx) assetsOf(ctx context.Context, rel *gh.Release) (live, ghosts map[string]gh.Asset, err error) {
	as, err := c.GH.ListAssets(ctx, c.Env.StoreRepo, rel.ID)
	if err != nil {
		return nil, nil, err
	}
	live = make(map[string]gh.Asset, len(as))
	for _, a := range as {
		if !a.Downloadable() {
			if ghosts == nil {
				ghosts = map[string]gh.Asset{}
			}
			ghosts[a.Name] = a
			continue
		}
		live[a.Name] = a
	}
	return live, ghosts, nil
}

// ReleaseAssets 列出某个 Release 现有 asset 的**名字集合**（只含真的能下载的那些）。
// 镜像的幂等判据就是它（03 §3.1：按名字判断，禁止 --clobber）。
func (c *Ctx) ReleaseAssets(ctx context.Context, rel *gh.Release) (map[string]gh.Asset, error) {
	live, _, err := c.assetsOf(ctx, rel)
	return live, err
}

// ---- 小工具 -----------------------------------------------------------------

// isNotFound 判断错误是不是 gh.ErrNotFound。抽出来是为了让调用点读起来是
// "资源不存在"而不是一串 errors.Is。
func isNotFound(err error) bool { return err != nil && errors.Is(err, gh.ErrNotFound) }

// Reportf 把一份校验报告打到日志上，并返回是否含阻断项。
//
// 告警与错误的打印**区分开且错误在后**：日志滚动时最后看到的是真正拦住这次回写的东西，
// 而不是几十条"某条目的 ABI 顺序"。
func (c *Ctx) Reportf(rep *model.Report) bool {
	if rep == nil {
		return false
	}
	for _, p := range rep.Warnings() {
		c.Log("%s", p.String())
	}
	for _, p := range rep.Errors() {
		c.Log("%s", p.String())
	}
	return rep.HasErrors()
}

// LogBlock 打一段多行文本，每行都带上前缀，便于在 Actions 日志里折叠阅读。
func (c *Ctx) LogBlock(prefix, body string) {
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		c.Log("%s%s", prefix, line)
	}
}

// CloneURL 返回某个仓库的 https 克隆地址。
//
// 从 API 根地址推导而不是写死 github.com：GHES 的 API 在 `/api/v3`，
// 写死主机名会让整个 forge 只能跑在 github.com 上，而这是零成本的通用化。
func (e *Env) CloneURL(repo string) string {
	return "https://" + e.webHost() + "/" + repo + ".git"
}

// webHost 从 API 根地址推 web 主机名。
func (e *Env) webHost() string {
	u, err := url.Parse(e.APIBase)
	if err != nil || u.Host == "" {
		return "github.com"
	}
	// github.com：api.github.com → github.com
	if rest, ok := strings.CutPrefix(u.Host, "api."); ok && strings.Trim(u.Path, "/") == "" {
		return rest
	}
	// GHES：api.<host>/api/v3 或 <host>/api/v3 —— host 本身就是 web 主机。
	return u.Host
}
