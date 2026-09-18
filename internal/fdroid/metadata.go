// Package fdroid 负责**渲染 fdroidserver 读的那两份输入**，以及**读回它产出的产物**。
//
// 它在整条流水线里的位置：`sources/{appId}.json` 是本市场唯一的事实源（03 §2.2），
// 而 fdroidserver 只认它自己那套输入 —— 每应用一份 `fdroid/metadata/<id>.yml` 与一份
// 仓库级的 `fdroid/config.yml`。本包就是这两者之间的那一层投影（02 §2.5 的映射表），
// 外加一个**只读**的产物解析器（`index.go`），供 `check-repo` 自检用（02 §2.8）。
//
// 分工的边界是刻意的：本包**不碰** fdroidserver，也**不碰**进程与磁盘布局 ——
// 它只做「结构体 ⇄ 字节」的转换。谁把结果写到哪、`fdroid update` 在哪个目录里跑、
// 产物往哪儿拷，全是 `job.BuildRepo` 的事。这样安排的理由很实际：
// **本机没有 Debian 环境，跑不了 fdroidserver**（计划第 3 条：验证以 CI 为主），
// 所以这一整包的正确性必须能靠本机 `go test` 穷举，而 CI 只需要回答一个窄问题 ——
// "它渲染出来的东西 fdroidserver 收不收"。
package fdroid

import (
	"fmt"
	"unicode/utf8"

	"github.com/market-of-labs/forge-core/internal/model"
)

// Render 把一份 sources/{appId}.json 渲染成 fdroidserver 读的 `<id>.yml`（02 §2.5）。
//
// 它**不写文件**：文件名（`<id>.yml`）与目录由调用方决定。而"文件名必须与 id 逐字一致"
// 这条硬规则由 `Source.Validate` 的 fileName 参数把关 —— fdroidserver 是**按文件名**
// 把 APK 与元数据对上的（§2.5 表格第一行），对不上的表现是**静默地少一个应用**，
// 而不是报错。所以这里也顺手把 Validate 跑一遍：本函数可能被任何路径调用，
// 而"渲染一份没校验过的来源"是那种只在索引里才看得出来的错误。
//
// 映射表逐条对应 §2.5，下面只记几条**为什么**：
//
//   - 恒写 `AutoUpdateMode: None` + `UpdateCheckMode: None` —— 这两行是把 fdroidserver
//     从"自己去找更新"降级成**纯索引生成器**的开关。本市场的分工是执行体抓上游、
//     fdroidserver 只管把手上这批 APK 编成索引（03 §5.1）。少了它们，它会去上游翻版本，
//     而那是一条我们完全控制不了、也不该依赖的路径。
//   - `Name` **照写**（§2.5 说它可以省略、省略时 fdroidserver 会从 APK 的 label 读）。
//     写它是刻意的：`name` 是**人维护的**字段，允许与上游 label 不同（改名、加中文名），
//     省略就等于把它交给上游 —— 那正是这个字段存在的意义所在。
//   - **不写** `License` —— 没有来源（上游 Release 里读不到）。猜一个许可证出去，
//     是在替上游做法律声明。
//   - **不写** `RepoType` / `Repo` / `Builds` —— 那是"从源码构建"用的，我们是镜像现成 APK。
//   - `paused` 在这里**不产生任何字段**，它由调用方决定要不要渲染这个文件（§2.5）。
//   - `versions` 账本**不参与渲染** —— 索引里的版本信息全部由 fdroidserver 从真实 APK
//     现读，账本的用途退回到"我们的水位线与审计"（03 §2.4）。
func Render(src *model.Source) ([]byte, error) {
	if src == nil {
		return nil, fmt.Errorf("fdroid: 来源为 nil")
	}
	if err := src.Validate(""); err != nil {
		return nil, fmt.Errorf("fdroid: 来源 %q 不合法，不渲染 metadata：%w", src.ID, err)
	}

	// 非法 UTF-8 是**静默损坏**：Go 的 json 解码会把坏字节换成 U+FFFD 而不报错，
	// 于是一个本来只是"编码坏了"的名字会以"名字里有个问号方块"的形式进索引。
	// 在这里拦一道，让它在源头响起来。
	for _, f := range []struct{ name, val string }{
		{"name", src.Name}, {"desc", src.Desc}, {"author", src.Author}, {"id", src.ID},
	} {
		if !utf8.ValidString(f.val) {
			return nil, fmt.Errorf("fdroid: 来源 %q 的 %s 不是合法 UTF-8：%q", src.ID, f.name, f.val)
		}
	}

	var d doc

	// 顺序照 §2.5 表格的行序 —— 那份表格就是这份文件的规格，
	// 照抄它让"改映射"和"改规范"永远是同一个动作。
	d.Str("Name", src.Name)

	// desc 为空就**不写这个键**（而不是写一个空 Summary）：§2.8 把"summary 缺失"
	// 定为软告警，而写一个空串会让那条告警失效 —— 它再也分不出"没写"和"写了但空"。
	if src.Desc != "" {
		d.Str("Summary", src.Desc)
	}

	d.Str("AuthorName", src.Author)

	// 分类为空同样不写（§2.6「宁缺勿错」）。`Categories` 的映射细节见 categories.go。
	if cats, _ := Categories(src.Categories); len(cats) > 0 {
		d.List("Categories", cats)
	}

	// SourceCode / IssueTracker 只在有 GitHub 上游时才有意义 —— manual 来源
	// （走 _incoming 上传）没有上游仓库可指向，编一个出来就是错的。
	// host 写死 github.com 是成立的：Upstream.Type 只有 `github-release` 一种取值（§2.2）。
	if src.Source == model.SourceGitHub && src.Upstream != nil {
		base := "https://github.com/" + src.Upstream.Repo
		d.Str("SourceCode", base)
		d.Str("IssueTracker", base+"/issues")
	}

	// ⚠️ 这两行是**契约**不是配置：删掉它们 fdroidserver 就会开始自己找更新。
	d.Plain("AutoUpdateMode", "None")
	d.Plain("UpdateCheckMode", "None")

	return d.Bytes(), nil
}

// MetadataFileName 返回一份来源对应的 metadata 文件名（§2.5 第一行：**必须逐字一致**）。
//
// 单独抽成函数而不是在调用点拼 `id + ".yml"`：这条规则的重要性全在"逐字"两个字上，
// 而它被写散在两处时就一定会有一天只改了一处。
func MetadataFileName(appID string) string { return appID + ".yml" }

// UnknownCategories 返回一份来源里词表外的分类标签，供 `check-repo` 出软告警（§2.8）。
//
// 抽出来是因为 `Render` 把 unknown 丢掉了（它只关心写出去什么）——
// 而"某个标签没被映射"这件事必须有人报，否则一个新加的中文标签会静默地什么都没发生。
func UnknownCategories(src *model.Source) []string {
	_, unknown := Categories(src.Categories)
	return unknown
}
