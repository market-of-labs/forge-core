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
	// SchemaVersion 恒为 2 —— 沿用原版 Obtainium 的 ExportSchema v2（02 §2.1）。
	SchemaVersion = 2

	// SentinelHost 是哨兵源地址的前缀。`.invalid` 是 RFC 2606 保留 TLD，
	// **规范保证不可解析** —— 所以这个地址永不联网，纯粹当唯一键与展示用（02 规则 8）。
	//
	// 绝不要改成 `.local`：那是 mDNS 保留域，在真实网络里**可能真的被解析**，
	// 于是哨兵地址会变成一次真实请求。
	SentinelHost = "https://market.invalid/"

	// OverrideSource 恒为 "HTML"（02 §2.2）：哨兵 host 走不到任何源时 getSource 会抛
	// UnsupportedURLError，虽然被 Obtainium 吞掉，但会在设备日志里留噪声。
	OverrideSource = "HTML"

	// KindObtainium / KindCompanion 是 02 规则 9 允许的**全部** kind 取值。
	KindObtainium = "obtainium"
	KindCompanion = "companion"

	// IncomingTag 是手动上传暂存 Release 的 tag（03 §3.2）。它不是合法 appId，
	// check-manifest 对它直接判失败（03 §3.1）。
	IncomingTag = "_incoming"

	// SourceGitHub / SourceManual 是 sources/{appId}.json 的 source 取值（03 §2.2）。
	// **种类不可自动变更**（03 §2.5 规则 3）：两种来源的二进制入料路径完全不同。
	SourceGitHub = "github"
	SourceManual = "manual"

	// UpstreamGitHubRelease 是 v1 唯一支持的上游类型（03 §2.2）。
	UpstreamGitHubRelease = "github-release"

	// AuthorUnknown 是**自动收录**的手动来源在拿到真作者之前的占位值（03 §2.5.3 / §3.2）。
	//
	// 为什么必须有值、不能留空：`author` 是 02 规则 2 的必填字段，而那条规则是 **Errorf** ——
	// 一个空 author 会让**整份清单**判失败，市场里所有条目一起下架，不是只废掉这一条。
	// 而 `paused: true` 也救不了它（那个布尔只管"要不要去上游看新版本"，条目照样渲染）。
	//
	// 它只会出现在"APK 先传上来、条目由搬运流程当场建出来"这条路上：手动上传不再走新增单，
	// 所以收录时没有人可以问作者；APK 里没有作者字段，也没有上游仓库可以取 owner。
	// **看到这个值就等于"这条来源还在等一张 change-source.yml"** —— 它是唯一的提示信号。
	AuthorUnknown = "未知"

	// EntryCountWarn 是清单条目数的软告警阈值（03 §5.3）：全量推送把市场规模
	// 直接暴露为设备端单次 deep-link URI 的大小（01 §3.6），超过就该警觉。
	EntryCountWarn = 150

	// ReleaseAssetCountWarn 是单个 Release 的 asset 数软告警阈值。硬上限是 1000
	// （03 §3.3，GitHub 官方明文），提前在 800 报警是为了不撞顶。
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

// SentinelURL 渲染条目的哨兵源地址。
func SentinelURL(id string) string { return SentinelHost + id }

// NowISO 是 exportedAt 的取值格式（02 §2.1：ISO-8601 UTC）。
// 手动上传的版本也用它填 Version.PublishedAt（见 job.recordPlaced）。
func NowISO() string { return time.Now().UTC().Format(time.RFC3339) }

// ReleaseDate 把发布时间转成清单 releaseDate 字段的整数形式。
//
// ⚠️ **单位是微秒**，权威是 Obtainium 的解析器而不是本仓库里的任何一份数据：
//
//	lib/providers/source_provider.dart:301-303
//	    releaseDate: json['releaseDate'] == null
//	        ? null
//	        : DateTime.fromMicrosecondsSinceEpoch(json['releaseDate']),
//
// 旁边 292 行的 lastUpdateCheck 同样是微秒，两者是一致的。
//
// 曾经这里用的是 UnixMilli()，理由是"store 里现存的 apps.json 写的是 13 位毫秒"。
// 那个理由只比对了「文档 vs 数据」两个候选，漏掉了真正说了算的第三方 —— 结果
// 13 位被 Obtainium 当微秒读，每个 App 的发布日期都显示成 **1970-01-21**，且静默。
// 数据文件错得和代码一模一样，所以互相印证、谁都没发现。数据已一并改正。
//
// 这个字段也不是纯展示：additionalSettings.releaseDateAsVersion 会拿它当版本号
// 参与比较（Obtainium lib/providers/app_json_migration.dart:58），差 1000 倍会直接
// 影响判更新。
func ReleaseDate(t time.Time) int64 { return t.UTC().UnixMicro() }

// ---- 02 §2.1 清单信封 -------------------------------------------------------

// Manifest 是 apps.json 的顶层结构。
type Manifest struct {
	SchemaVersion int     `json:"schemaVersion"`
	ExportedAt    string  `json:"exportedAt"`
	GeneratedBy   string  `json:"generatedBy,omitempty"`
	Apps          []Entry `json:"apps"`
}

// Entry 是清单里的一个 App 条目（02 §2.2）。字段顺序即 JSON 输出顺序 ——
// 保持它在规范里的顺序，让生成的 apps.json 有稳定的 diff。
type Entry struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Author          string `json:"author"`
	URL             string `json:"url"`
	OverrideSource  string `json:"overrideSource"`
	LatestVersion   string `json:"latestVersion"`
	APKUrls         string `json:"apkUrls"`
	OtherAssetUrls  string `json:"otherAssetUrls"`
	PreferredAPKIdx int    `json:"preferredApkIndex"`
	// AdditionalSettings 是 JSON **字符串** map（02 §2.3），不是嵌套对象 ——
	// 原版 Obtainium 的 ExportSchema 就是这么存的，改不了。
	AdditionalSettings string   `json:"additionalSettings"`
	ReleaseDate        int64    `json:"releaseDate,omitempty"`
	ChangeLog          string   `json:"changeLog"`
	Categories         []string `json:"categories"`
	// Kind 是**清单侧元数据**，不是 Obtainium 字段：伴侣应用生成 deep link 前
	// 必须把带 kind 的条目整体剔除（01 §3.11）。omitempty 让普通 App 不出现该键。
	Kind string `json:"kind,omitempty"`
}

// ---- 03 §2.2 sources/{appId}.json ------------------------------------------

// Source 是一个 App 的**维护输入**。它是唯一事实源：apps.json 与 Release 现状
// 全部可以从它 + 上游推出。
//
// 上半部分是人填的（走 issue 表单），下半部分的 Versions 是 forge 写的（见那里的说明）。
type Source struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Author string `json:"author"`
	// Desc 是一句话简介，可选。它**独立存**，只在合成清单时拼到 Name 后面
	// （由 DisplayName 决定怎么拼）—— 存成拼接后的整串，将来改分隔符就得重写全部数据，
	// 而且申请人"改简介"会变成"改显示名"。
	Desc       string   `json:"desc,omitempty"`
	Source     string   `json:"source"` // SourceGitHub | SourceManual
	Paused     bool     `json:"paused"`
	Categories []string `json:"categories,omitempty"`
	Kind       string   `json:"kind,omitempty"`
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
// 20 这个数字是**量出来的**，不是拍的：Obtainium 列表行的标题是
// `maxLines: 1` + `TextOverflow.ellipsis`（`app_list_tile.dart`），手机上一行
// 连名字带简介大约放得下 17 个汉字 —— 所以 20 rune 的简介在大多数条目上不会把
// 标题挤成省略号，而再长就一定挤。它同时也是"这是简介不是描述"的强制提醒。
const MaxDescRunes = 20

// DescSeparator 是 Name 与 Desc 之间的分隔符。
//
// 用中点而不是 `-`/`()`/`|`：Obtainium 的显示名里本来就允许出现后三者
// （`app-suffix`、`Foo (Bar)`），而中点几乎不会自然出现在应用名里 ——
// 一眼就能看出"中点两边是两个不同的字段"。
const DescSeparator = " · "

// DisplayName 是**清单里的**显示名：有简介时拼成 `名字 · 简介`。
//
// 这是 desc 唯一被消费的地方（D42）—— `sources/` 里两者始终分开存，
// 于是改分隔符、改拼法都不用重写数据。
// IsManual 报告这条来源有没有上游：`true` = 二进制走 `_incoming` 上传队列（03 §3.2），
// 对账碰不到它、身份三件套也没有地方可读。
//
// 判据只有 Source 一个字段，所以它就是 `Source == SourceManual` 的一个名字 —— 名字的
// 存在只是为了在调用点读起来是"这条来源的性质"，而不是散落的字符串比较。
func (s *Source) IsManual() bool { return s.Source == SourceManual }

func (s *Source) DisplayName() string {
	if s.Desc == "" {
		return s.Name
	}
	return s.Name + DescSeparator + s.Desc
}

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

// Endpoints 是地址模板。**它是人改的配置，不是产物** —— 第一期与部署期的唯一差别
// 就是这个文件里的第三行（03 §2.3）。
type Endpoints struct {
	TagTemplate       string `json:"tagTemplate"`
	AssetNameTemplate string `json:"assetNameTemplate"`
	AssetURLTemplate  string `json:"assetUrlTemplate"`
}

// ---- 03 §2.4 版本账本（Source.Versions） -------------------------------------

// Version 是一个已镜像版本的全部 ABI 分片。apps.json 只描述最新版，历史版本无处存放
// （tag 里没有版本段），这块由它补上。
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
	// 为什么必须存这一份：03 §5.1 要求清单的 `latestVersion` 存**原始 versionName**
	// （展示与判更新用，原版语义），而文件名里只有清洗后的 token。两者在
	// versionName 含空白/斜杠时**不同**，只存 token 就永远推不回原始值。
	//
	// 这一块不是客户端契约（D23），所以多存一份不违反任何东西。缺该字段时按 Version
	// 兜底，所以缺元数据的版本照样能读。
	VersionName string `json:"versionName,omitempty"`
	// PublishedAt 是该版本的上游发布时间（RFC3339 UTC），用来回填清单的 releaseDate。
	//
	// 不存它，第一次重建清单就会把 releaseDate 丢掉，而那是个**静默的数据退化** ——
	// 清单格式合法、客户端只是不再显示日期。
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
	// 暂时不进清单（`changeLog` 仍然留空，理由见 manifest.Build 里的 URI 预算）。
	ReleaseNote string `json:"releaseNote,omitempty"`
	// Assets 是该版本的全部分片，**顺序即 apkUrls 的顺序**（02 §2.4：universal 在前）。
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

// DisplayVersion 返回用于清单 `latestVersion` 的原始版本名，见 VersionName 的说明。
func (v *Version) DisplayVersion() string {
	if v.VersionName != "" {
		return v.VersionName
	}
	return v.Version
}

// ABIs 按 assets 出现顺序返回 ABI token 列表。
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

// Latest 返回"最新"的那个版本，供清单的 latestVersion / apkUrls 使用。
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
// check-manifest 会额外检查"最新版的 versionCode 是否是该 App 里最大的"，
// 把反常情形作为告警暴露出来，而不是由这里悄悄替人做决定。
func (s *Source) Latest() *Version {
	if len(s.Versions) == 0 {
		return nil
	}
	return &s.Versions[len(s.Versions)-1]
}
