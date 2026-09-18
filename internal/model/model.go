// Package model 是两份契约的 Go 表示：**02 的清单格式**（消费方 = 伴侣应用 / CF）
// 与 **03 §2 的 store 布局**（消费方 = forge 自己）。
//
// 这里只放类型与纯函数：不读文件、不碰网络、不知道 GitHub 存在。校验在 validate.go，
// 地址模板在 endpoints.go。这样"契约长什么样"与"怎么去拿数据"彻底分开 —— 前者可以被单测
// 钉死，后者坏了不影响前者。
package model

import (
	"strings"
	"time"
)

// ---- 契约常量 ---------------------------------------------------------------
//
// 这些值在 02/03 里被反复引用且**两期都不变**。写成常量而不是散在代码里的字面量，
// 是为了让"改契约"变成一次编译期可见的改动，而不是一次 grep。

const (
	// IncomingTag 是手动上传暂存 Release 的 tag（03 §3.2）。它不是合法 appId，
	// check-repo 对它直接判失败（03 §3.1）。
	IncomingTag = "_incoming"

	// SourceGitHub / SourceManual 是 sources/{appId}.json 的 source 取值（03 §2.2）。
	// **种类不可自动变更**（03 §2.5 规则 3）：两种来源的二进制入料路径完全不同。
	SourceGitHub = "github"
	SourceManual = "manual"

	// UpstreamGitHubRelease 是 v1 唯一支持的上游类型（03 §2.2）。
	UpstreamGitHubRelease = "github-release"

	// AuthorUnknown 是**自动收录**的手动来源在拿到真作者之前的占位值（03 §2.5.3 / §3.2）。
	//
	// 为什么必须有值、不能留空：`author` 渲染成 metadata 的 `AuthorName`，而
	// `fdroid.Render` 见空 author 直接报错 —— 于是 `build-repo` **整轮中止**，
	// 一个字节都不会回写，市场从那一刻起停止更新（不是"只少一个应用"）。
	//
	// `paused: true` 确实能绕过它（paused 的来源根本不渲染），但代价是这个应用
	// **从索引里消失** —— 已经装了的用户不会掉，但也再也收不到更新。那是停用一个
	// 来源的手段，不是给一条待补录的条目打补丁的手段。
	//
	// 它只会出现在"APK 先传上来、条目由搬运流程当场建出来"这条路上：手动上传不再走新增单，
	// 所以收录时没有人可以问作者；APK 里没有作者字段，也没有上游仓库可以取 owner。
	// **看到这个值就等于"这条来源还在等一张 change-source.yml"** —— 它是唯一的提示信号。
	AuthorUnknown = "未知"

	// ReleaseAssetCountWarn 是单个 Release 的 asset 数软告警阈值。硬上限是 1000
	// （03 §3.3，GitHub 官方明文），提前在 800 报警是为了不撞顶。
	//
	// 关于这条阈值在 F-Droid 路线下的**新**处境：索引不再由我们手工组装，而是
	// fdroidserver 扫 `repo/` 扫出来的 —— 但 APK 仍然一个一个躺在 Release 里，
	// 所以这条上限一点没变松。恰恰相反：老版本靠 CI cache 累积保留（D 轮的 cache 决定）
	// 意味着 asset 数**单调增长**，而每轮只下最新版意味着我们不会主动重下老版本来稀释它。
	ReleaseAssetCountWarn = 800
)

// ReviewCategories 是「分类标签」勾选项的固定词表，必须与两份 issue 模板里的
// options 逐字一致（`TestTemplateVocabularyMatchesGo` 会直接读 yml 钉住这件事）。
//
// 它**刻意不参与 Source.Validate**：取值在**表单侧**就已经被锁在闭集里（词表与
// 模板逐字钉死，见上），而 `sources/` 的唯一入口就是那张表单（D48 之后人不直接
// 写数据源），所以在校验里再说一遍拦不到任何东西。词表存在的理由是**清单的筛选条
// 不被随手造出来的标签塞满** —— 那条防线在入口处，不在出口处。
//
// 顺序在这里**只是给回评文案用的**，不影响任何判定 —— 与 naming.ABISet 不同，
// 那个的顺序是排序位次，动不得。
var ReviewCategories = []string{"工具", "效率", "媒体", "通讯", "开发", "游戏", "其他"}

// NowISO 是 RFC3339 UTC 时间戳的取值格式。
//
// 它曾经只服务于清单的 `exportedAt`（02 §2.1），那个字段随 F-Droid 那条路一起没了。
// 现在唯一的调用点是手动上传记版本时填 `Version.PublishedAt` —— 但格式没变：
// 规格里凡是要写一个时刻的地方都写 RFC3339 UTC，所以它作为**格式**的单一出处留着。
func NowISO() string { return time.Now().UTC().Format(time.RFC3339) }

// ---- 03 §2.2 sources/{appId}.json ------------------------------------------

// Source 是一个 App 的**维护输入**。它是唯一事实源：metadata、Release 现状
// 全部可以从它 + 上游推出。
//
// 上半部分是人填的（走 issue 表单），下半部分的 Versions 是 forge 写的（见那里的说明）。
type Source struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Author string `json:"author"`
	// Desc 是一句话简介，可选。它**独立存**，只在渲染 metadata 时落成 F-Droid 的
	// `Summary` 字段（02 §2.5）—— 与 `name` 是两个字段、不是一个拼接结果的来源。
	//
	// 它曾经是"拼到显示名后面"的那半段（`名字 · 简介`，D42）。F-Droid 的
	// `Name`/`Summary` 本来就是两字段，于是那套拼接连同它的分隔符一起作废了：
	// 客户端怎么摆这两段是**客户端**的事，不是数据的事。
	Desc       string   `json:"desc,omitempty"`
	Source     string   `json:"source"` // SourceGitHub | SourceManual
	Paused     bool     `json:"paused"`
	Categories []string `json:"categories,omitempty"`
	// Upstream 仅在 source == "github" 时必填。
	Upstream *Upstream `json:"upstream,omitempty"`
	// ABIWhitelist 缺省空 = 该版本全 ABI 分片都镜像；用于控体积（D14）。
	ABIWhitelist []string `json:"abiWhitelist,omitempty"`
	// Versions 是**机器写的**版本账本：这个 App 已镜像了哪些版本、每个版本有哪些 ABI 分片。
	//
	// 它曾经是独立文件 `store/index.json`（D23），现在并进来源文件里。理由是**归属**：
	// 账本天然属于某一个应用，而拆成两份的代价是真实的 —— 两份文件要靠 id 对齐、
	// "移除一个来源"会在 index 里留下一段无人认领的孤儿账本、每次重建都得回答
	// "index 里有而 sources 里没有的怎么办"。
	//
	// ⚠️ 上面那些字段是**人填的**（走 issue 表单），这一块是 **forge 写的** ——
	// 每次对账都会整段重写（`BuildIndex`），手改它下一轮就被盖回去。
	//
	// 放在结构体最后，是为了让 JSON 里"人填的那段"始终在前面、机器那段始终在末尾，
	// diff 里一眼能看出改的是哪一半。
	Versions []Version `json:"versions,omitempty"`
}

// 简介的长度上限，单位是 **rune**（汉字算一个）。
//
// 20 这个数字是**量出来的**，不是拍的：它当初是照着 Obtainium 列表行的标题
// （`maxLines: 1` + `TextOverflow.ellipsis`，`app_list_tile.dart`）定的，一行
// 大约放得下 17 个汉字。F-Droid 客户端那边 `Summary` 的处境一样 —— 它同样是
// 列表行里的一行小字。所以这个上限**没变**，只是现在它约束的是 `Summary` 本身，
// 而不再是"标题里跟在名字后面的那半段"。
const MaxDescRunes = 20

// TruncateDesc 把一份申请里填的简介裁到 MaxDescRunes 个 rune 并去掉首尾空白。
//
// 按 **rune** 切，不能按 byte 切 —— 按 byte 会把一个汉字劈成半个（产生非法 UTF-8，
// 客户端那边是一串替换字符）。首尾空白也在这里去掉：模板里的输入框很容易多带一个空格，
// 而它在列表里表现为"名字和简介之间隔了两个空格"。
//
// 裁而不拒是刻意的：这个字段**纯装饰**，而新增单一次往返是**一天**
// （03 §2.5.1 两拍）。为了一个简介让人重填一整天，代价和收益不成比例。
func TruncateDesc(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > MaxDescRunes {
		return strings.TrimSpace(string(r[:MaxDescRunes]))
	}
	return s
}

// Upstream 描述 GitHub 上游（03 §2.2）。它**只存在于 forge 与 sources/**，// 绝不进清单 —— 清单里所有地址一律指向本市场仓库（02 规则 3）。
type Upstream struct {
	Type         string `json:"type"` // 只支持 UpstreamGitHubRelease
	Repo         string `json:"repo"` // "owner/name"
	AssetPattern string `json:"assetPattern,omitempty"`
	// IncludePrerelease 决定上游标了 prerelease 的发布要不要一起镜像（默认否 = 只镜像正式版）。
	//
	// 为什么挂在 upstream 块里而不是 Source 顶层：manual 来源没有上游，也就没有这个概念。
	//
	// 打开之后 prerelease 与正式版**一视同仁**（同一个候选集、同一条 upstreamTag 水位线，
	// 见 upstream.Releasable）—— 也就是说设备端会拿到比当前正式版更新的预发布版。
	// 那是开启这个开关的**本意**，不是意外；而唯一需要它的场景是"上游把新版只发成 prerelease"。
	// 它只说上游那一侧，与 store 自己的 `_incoming` 队列无关（后者常驻 draft，
	// 没有 prerelease 这个状态可言）。
	IncludePrerelease bool `json:"includePrerelease,omitempty"`
}

// ---- 03 §2.3 store/endpoints.json ------------------------------------------

// Endpoints 是地址配置。**它是人改的配置，不是产物** —— 第一期与部署期的差别
// 就落在 repoUrl 这一行上（03 §2.3）。
//
// 它的形状从"三个模板"变成了"两个模板 + 一个成品地址"：
//
//	· 前两个仍然只描述**名字**（Release tag 与 asset 文件名），本机可校验；
//	· 第三个的**替代品** repoUrl 不是一个模板，而是一个可以直接用的地址 ——
//	  因为下载地址不再由我们产出（客户端拿 `address + "/" + 文件名` 自拼，D62），
//	  于是 `{fileName}` 这个占位符连同它那一整套模板机制一起没有了。
//
// repoUrl 是 `index-v2.json` 里 `repo.address` 的**唯一出处**，也就是客户端的源地址。
type Endpoints struct {
	TagTemplate       string `json:"tagTemplate"`
	AssetNameTemplate string `json:"assetNameTemplate"`
	RepoURL           string `json:"repoUrl"`
}

// ---- 03 §2.4 版本账本（Source.Versions） -------------------------------------

// Version 是一个已镜像版本的全部 ABI 分片。
//
// ⚠️ F-Droid 路线下它与索引的关系**反过来了**，这一点值得先看清楚再读下面的字段：
// 以前是账本 → 清单的 `apkUrls`，索引里有什么完全由账本决定；现在索引由 fdroidserver
// 扫 `repo/` 目录扫出来（以 APK 的 sha256 为键），所以**索引的事实源是磁盘上的文件**，
// 不是这里。账本记错了不会让索引跟着错，只会让"该去 cache 里取哪些文件"这件事失准。
//
// 那它还剩什么用？两件，都不是可有可无的：
//
//	· `UpstreamTag` 是**对账的幂等键**（见那个字段）—— 与路线无关，且不可替代；
//	· 它是一个可读的"这个应用镜像过什么"的记录。老版本靠 CI cache 累积保留，
//	  而 cache 是一个二进制目录、里面没有清单 —— 出问题时账本是唯一能对得上的东西。
//
// 整块是**机器写的**（见 Source.Versions 的说明）：每次对账都由 BuildIndex 重写。
type Version struct {
	// Version 是**清洗后**的 version token —— 它就是文件名里的那一段（02 §2.4）。
	Version string `json:"version"`
	// VersionCode 用 omitempty 落地 03 §5.2 的「拿不到时省略该字段而非写 0」——
	// 「拿不到」在 APK 解析结果里唯一的表现就是零值（见 apkmeta.HasVersionCode）。
	VersionCode int32 `json:"versionCode,omitempty"`
	// VersionName 是 APK 的**原始** versionName，与 Version（清洗后的 token）区分开。
	//
	// 两者在 versionName 含空白/斜杠时**不同**，而文件名的 charset 是硬契约（02 §2.4），
	// 于是 token 是有损的 —— 只存 token 就永远推不回原值。
	//
	// 它曾经只服务于清单的 `latestVersion`。那个字段随清单一起没了，而这一份留着
	// 是因为它**取不回来**：索引里的 versionName 由 fdroidserver 从 APK 现读，
	// 谁都不会去记"这一版当时叫什么"，除了账本。
	//
	// 缺该字段时按 Version 兜底，所以缺元数据的版本照样能读。
	VersionName string `json:"versionName,omitempty"`
	// PublishedAt 是该版本的上游发布时间（RFC3339 UTC）。
	//
	// 它曾经只服务于清单的 `releaseDate`（那个字段连同它的微秒单位一起没了）。
	// 现在它是账本里**唯一的时间坐标** —— "最新"这个判断本身就依赖"账本按时间追加"
	// 这一写入顺序（见 Source.Latest）。
	//
	// 不存它，首次重建账本就会把它丢掉，而那是**静默的**数据退化：账本仍然完全合法，
	// 只是版本之间的先后关系没了，不会有任何一层为此报错。
	PublishedAt string `json:"publishedAt,omitempty"`
	// UpstreamTag 是产出这一版本的**上游 Release tag**，用来让对账变成一次集合差。
	//
	// 为什么需要它：03 §4.4 的对账要回答"这个上游发布镜像过了吗"，而幂等必须是
	// **纯名字判断**（§4.6 规则 3：禁 --clobber，靠目标 asset 名已存在来跳过）。
	// 问题在于目标 asset 名里的 `{version}` 取自 **APK 内的 versionName**（§5.1），
	// 不下载就不知道 —— 那就成了"为了判断要不要下载，先得下载"。
	//
	// 记录上游 tag 把这件事拆开了：镜像**当时**就知道 tag（是它触发的），记下来；
	// 之后对账只要 `上游 latest.tag ∈ 该来源的 upstreamTag` 就能判定已镜像，
	// 一个字节都不用下。缺该字段时按"未镜像"处理（会多下一次，然后被 asset 名幂等挡住），
	// 所以缺元数据的版本照样能读。
	UpstreamTag string `json:"upstreamTag,omitempty"`
	// ReleaseNote 是**上游那个 Release 的正文**（发布方写的更新说明），markdown 原文。
	//
	// 它是"每版一份"的东西，所以存账本；而**我们 Release 的正文**放的是上游 README
	// （项目级、与版本无关，见 job.syncReleaseBody）。两者是不同的东西：一个是"这一版
	// 改了什么"，一个是"这个 App 是干什么的"。
	//
	// 它**不在我们自己 Release 的元数据里**（那份 body 已经被 README 占了），所以和
	// versionName/versionCode/publishedAt/upstreamTag 是同一类东西：重建账本时只能从
	// 旧账本里继承回来，否则 build-index 会**静默**丢掉它。
	//
	// ⚠️ 它目前**没有任何消费者**：F-Droid 的 `ChangeLog` 字段我们没写（02 §2.5 的映射
	// 表里没有它），而清单的 `changeLog` 随清单一起没了。
	//
	// 留着是因为它是**上游写的、丢了就找不回来的**东西 —— "暂时没地方用"和"该删"
	// 是两回事，而这条数据的来源（上游 Release 正文）在下一次对账之后就变了。
	ReleaseNote string `json:"releaseNote,omitempty"`
	// Assets 是该版本的全部分片。
	//
	// 顺序在这里仍然是**有意义的**：universal 在前（02 §2.4）。它曾经直接决定清单
	// `apkUrls` 的次序，现在次序由客户端自己折叠 ABI 时决定 —— 但账本是喂给
	// `job` 的那份输入，保留一个稳定的顺序仍然是对的，否则同一份数据会产出两种 diff。
	Assets []Asset `json:"assets"`
}

// MirroredUpstreamTag 报告上游 tag 是否已经镜像过（见 Version.UpstreamTag 的说明）。
func (s *Source) MirroredUpstreamTag(tag string) bool {
	if tag == "" {
		return false
	}
	for i := range s.Versions {
		if s.Versions[i].UpstreamTag == tag {
			return true
		}
	}
	return false
}

// ABIs 按 assets 出现顺序返回 ABI token 列表。
//
// 与 Latest 是一对：`src.Latest().ABIs()` 就是"这个应用最新版的全部 ABI 分片"，
// 而那正是每轮要去上游取的那组文件（索引覆盖的决定：只取最新版，老版本走 cache）。
func (v *Version) ABIs() []string {
	out := make([]string, 0, len(v.Assets))
	for _, a := range v.Assets {
		out = append(out, a.ABI)
	}
	return out
}

// Asset 是一个具体的 asset 文件。
type Asset struct {
	ABI  string `json:"abi"`
	File string `json:"file"`
	Size int64  `json:"size"`
}

// FindVersion 按 version token 取版本子树。
func (s *Source) FindVersion(version string) *Version {
	for i := range s.Versions {
		if s.Versions[i].Version == version {
			return &s.Versions[i]
		}
	}
	return nil
}

// Latest 返回"最新"的那个版本，供组装每轮的下载清单使用（见 ABIs）。
//
// **判据是"列表最后一个"，不是"versionCode 最大"** —— 这是刻意的：
//
//	版本排序在上游之间没有统一语义。versionCode 可能被发布者写错或回退，
//	versionName 更是自由文本（`2.0-rc1` 与 `2.0` 谁新取决于人）。
//
// 唯一可靠的"新"是**时间**，而账本的生成方式本身就是按时间追加的
// （reconcile 按上游发布时间顺序镜像，build-index 保留已知版本的位置、新版本追加到末尾）。
// 所以"最后一个 = 最新"是这套写入顺序的直接推论，不需要再猜排序规则。
//
// ⚠️ 这与**上游**判"哪个 Release 最新"是两件事：那边按 GitHub 给的时间排
// （`job/upstream` 的候选序），这边按账本的写入序。两者在正常流向下一致，
// 而一旦不一致，`Latest()` 会说的是"账本认为最新的那个"——
// 所以它只该被用在"从账本取东西"的场景，不该被用来判定要不要去上游取新版本。
func (s *Source) Latest() *Version {
	if len(s.Versions) == 0 {
		return nil
	}
	return &s.Versions[len(s.Versions)-1]
}
