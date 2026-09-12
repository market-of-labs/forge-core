// Package upstream 负责"从上游 Release 里挑出该镜像哪些文件"。
//
// 边界很清楚：本包**只做选择**（哪些 asset 是候选安装包、哪个 Release 是"最新"），
// 不下载、不改名、不碰 ABI 的定论。ABI 的最终判据是 **APK 内容**（`lib/<abi>/`），
// 由 internal/apkmeta 给出 —— 本包提供的 FilenameABI 只是**文件名上的线索**，
// 用于在"文件名说是 arm64、内容里却是四 ABI"时发一条告警，绝不参与决策。
//
// 这个分工就是"信内容，改名按内容"的落地：文件名是上游随手写的，内容是 APK 自己
// 带的；两者冲突时以内容为准，但要把冲突记下来。
package upstream

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/market-of-labs/forge-core/internal/gh"
	"github.com/market-of-labs/forge-core/internal/naming"
)

// DefaultAssetPattern 是 sources 里没写 assetPattern 时的兜底：任何 .apk。
const DefaultAssetPattern = `(?i)\.apk$`

// 明确不能独立安装的扩展名（03 §5.4：跳过 `.apks/.xapk/.aab`、obb、分片）。
//
// 判据是"这个文件能单独 adb install 吗"。装不上的东西镜像过来只会占 asset 名额
// （单 Release 硬上限 1000，§3.3）并让清单里出现一个点不动的下载项。
var notInstallerExt = []string{
	".apks",   // bundletool 的 split 集合包，靠 bundletool/安装器展开
	".xapk",   // 同上的另一种封装
	".apkm",   // APKMirror 的封装
	".aab",    // App Bundle，本身不可安装
	".obb",    // 游戏资源包，单独下下来没用
	".zip",    // 上游常见的"打包发布"
	".tar.gz", // 同上
}

// configSegment 匹配 split APK 的配置分片名，如
// `app-1.0-arm64-v8a-config.zh.apk`、`base-config.xxhdpi.apk`。
//
// 为什么单独识别：这类分片**扩展名也是 .apk**、也过得了 assetPattern，
// 但它必须和 base APK 一起装（少了任一片都装不上）。放进来会让清单里出现一个
// "看起来能下载、点下去装不上"的条目 —— 而 §5.4 明确要求这种版本整体跳过并标"需手动"。
var configSegment = regexp.MustCompile(`(?i)(^|[-_.])config[-_.]`)

// Match 按 assetPattern 从一组 asset 里挑出候选安装包。
//
// 返回值第二个是被跳过的，附带原因 —— 调用方要把它打进日志（§5.4：跳过要告警，
// 不能默默少镜像一个版本让人事后猜）。
func Match(assets []gh.Asset, pattern string) (picked []gh.Asset, skipped []Skip) {
	re, err := CompilePattern(pattern)
	if err != nil {
		// 模式不合法是**配置错误**，而配置错误在 reconcile 里已经被
		// model.Source.Validate 拦过一次了。走到这里说明是调用方直接塞了个坏模式，
		// 那就退化成默认模式而不是静默返回空 —— 空返回的表现是"所有版本都跳过"，
		// 那是个比报错难查得多的症状。
		re = regexp.MustCompile(DefaultAssetPattern)
	}

	for _, a := range assets {
		name := a.Name
		if !re.MatchString(name) {
			skipped = append(skipped, Skip{Name: name, Reason: "不匹配 assetPattern"})
			continue
		}
		if r, ok := NotInstaller(name); ok {
			skipped = append(skipped, Skip{Name: name, Reason: r})
			continue
		}
		picked = append(picked, a)
	}
	return picked, skipped
}

// Skip 记录一个被跳过的 asset 及原因。
type Skip struct {
	Name   string
	Reason string
}

// NotInstaller 报告一个文件名是不是"不可独立安装"的东西。
//
// 只看扩展名与名字形状，不看内容 —— 这也是它只能用来**排除**而不能用来判定的原因。
func NotInstaller(name string) (string, bool) {
	lower := strings.ToLower(name)
	for _, ext := range notInstallerExt {
		if strings.HasSuffix(lower, ext) {
			return fmt.Sprintf("%s 不是可独立安装的包", ext), true
		}
	}
	if !strings.HasSuffix(lower, naming.Ext) {
		return "不是 .apk", true
	}
	if configSegment.MatchString(name) {
		return "是 split 配置分片（必须与 base 一起装）", true
	}
	return "", false
}

// CompilePattern 编译 assetPattern，空串走默认。
func CompilePattern(pattern string) (*regexp.Regexp, error) {
	if strings.TrimSpace(pattern) == "" {
		pattern = DefaultAssetPattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("assetPattern %q 不是合法正则：%w", pattern, err)
	}
	return re, nil
}

// ---- 文件名上的 ABI 线索（只用于告警） ---------------------------------------

// abiHints 按**从长到短**排列，保证 `arm64-v8a` 不会被 `arm` 抢先匹配。
//
// 每项是"文件名里可能出现的写法 → 规范 ABI"。这些写法是实测常见的那几种，
// 不是为了穷尽 —— 认不出来就返回 false，而认不出来的后果仅仅是少一条告警。
var abiHints = []struct {
	needle string
	abi    string
}{
	{"arm64-v8a", naming.ABIARM64},
	{"armeabi-v7a", naming.ABIARMv7},
	{"aarch64", naming.ABIARM64},
	{"x86_64", naming.ABIX86_64},
	{"x86-64", naming.ABIX86_64},
	{"arm64", naming.ABIARM64},
	{"armeabi", naming.ABIARMv7},
	{"amd64", naming.ABIX86_64},
	{"x86", naming.ABIX86},
	{"i686", naming.ABIX86},
	{"universal", naming.ABIUniversal},
	{"noarch", naming.ABIUniversal},
	{"fat", naming.ABIUniversal},
	{"all", naming.ABIUniversal},
	{"arm", naming.ABIARMv7},
}

// FilenameABI 从文件名里**猜**一个 ABI。
//
// ⚠️ 只用于比对告警。真正的 ABI 判据是 APK 内容（internal/apkmeta）。这里的返回值
// 绝不参与改名决策 —— "信内容，改名按内容"就是这条边界。
func FilenameABI(name string) (string, bool) {
	// 去掉扩展名再匹配，免得 `.apk` 里的 `a`、`k` 之类搅局（当前不出问题，
	// 但把输入限定在"名字主体"上是对的）。
	base := strings.ToLower(strings.TrimSuffix(name, naming.Ext))
	for _, h := range abiHints {
		if strings.Contains(base, h.needle) {
			return h.abi, true
		}
	}
	return "", false
}

// Mismatch 描述"文件名说的 ABI"与"内容说的 ABI"对不上。
type Mismatch struct {
	File     string
	Filename string // 文件名线索（可能为空 = 文件名里没写 ABI）
	Content  string // APK 内容判出的 ABI
}

func (m Mismatch) String() string {
	if m.Filename == "" {
		return fmt.Sprintf("%s：文件名未标 ABI，内容判为 %s", m.File, m.Content)
	}
	return fmt.Sprintf("%s：文件名标为 %s，内容判为 %s", m.File, m.Filename, m.Content)
}

// CheckMismatch 比对文件名线索与内容判定。
//
// 返回 false 表示**无需告警**。三种情况不告警：
//   - 文件名里没写 ABI（上游就是叫 `app-release.apk`，没什么可冲突的）；
//   - 内容判为 universal：一个装了多个 ABI 的包，文件名写其中某一个是很常见的
//     宣传口径，不构成错误；
//   - 两者一致。
//
// 会告警的是"文件名说是给 arm64 的，内容里却只有 armeabi-v7a 的原生库" ——
// 这正是 §5.4/§5.5 要求按内容判定的那类上游打包错误。
func CheckMismatch(file, contentABI string) (Mismatch, bool) {
	hint, ok := FilenameABI(file)
	if !ok || contentABI == naming.ABIUniversal || hint == contentABI {
		return Mismatch{}, false
	}
	return Mismatch{File: file, Filename: hint, Content: contentABI}, true
}

// ---- 挑"最新" --------------------------------------------------------------

// Releasable 挑出可镜像的 Release（非 draft；prerelease 由 includePrerelease 决定），
// 按发布时间**从新到旧**排列。
//
// 为什么需要整份有序列表而不只是"最新的那个"：§4.4 承诺"某天 runner 挂了、cron 被跳过，
// 第二天自然补齐"。要兑现这句，对账就必须知道"比上次镜像的那个更新的有哪些" ——
// 那是一次有序列表上的区间查询，只看最新的一个做不到（中间的版本会被静默丢掉）。
//
// 排序键是 PublishedAt 倒序；同一秒发布时退到 ID 倒序。ID 单调递增，所以 tie-break
// 也是确定的 —— 不确定的排序会让同一次对账跑两遍镜像不同的版本。
//
// includePrerelease（sources 里的 `upstream.includePrerelease`）是**唯一的**例外入口：
// 它为真时 prerelease 与正式版一视同仁地进候选集，顺序也一视同仁地由时间决定。
// 需要它的场景只有一个 —— 上游把新版只发成了 prerelease，于是"不收 prerelease"
// 意味着这个应用永远停在旧版本上。默认为假：绝大多数上游的 prerelease 是测试包。
//
// **draft 永远不收**，没有开关：draft 在上游是"还没发布"的意思（GitHub 的 UI 里
// 只有作者看得见），镜像一个作者自己都还没发布的东西没有正当场景。
func Releasable(rels []gh.Release, includePrerelease bool) []gh.Release {
	cands := make([]gh.Release, 0, len(rels))
	for _, r := range rels {
		if r.Draft || (r.Prerelease && !includePrerelease) {
			continue
		}
		cands = append(cands, r)
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].PublishedAt != cands[j].PublishedAt {
			return cands[i].PublishedAt > cands[j].PublishedAt
		}
		return cands[i].ID > cands[j].ID
	})
	return cands
}

// LatestReleasable 从一组 Release 里挑出该镜像的那个：非 draft（prerelease 见
// includePrerelease），按发布时间取最新。
//
// 为什么不能用 `/releases/latest` 就完事：那个端点只看"最新非 draft 非 prerelease"，
// 语义上是对的，但它**不告诉我们列表里还有没有别的**。而 §4.4 的对账需要知道
// "上游是不是发布了新版" —— 那正是这一个判断。多拉一页列表换来的是：404（上游
// 一个 Release 都没有）能被明确区分出来，而不是当作网络错误。
//
// 返回值 ok=false 表示没有可镜像的 Release（见 ExcludedNote，或一个都没有）。
func LatestReleasable(rels []gh.Release, includePrerelease bool) (gh.Release, bool) {
	c := Releasable(rels, includePrerelease)
	if len(c) == 0 {
		return gh.Release{}, false
	}
	return c[0], true
}

// ExcludedNote 描述当前候选集把哪些发布排除在外，只给"没有可镜像的发布"那句告警当措辞用。
//
// 单独一个函数是因为它有两个消费者（对账与收录），而**说错原因**是这套设计里明确要
// 消灭的东西：开过 includePrerelease 之后还说"全是 draft/prerelease"，
// 会让人去上游找一个根本不存在的 draft。
func ExcludedNote(includePrerelease bool) string {
	if includePrerelease {
		return "全是 draft"
	}
	return "全是 draft/prerelease"
}
