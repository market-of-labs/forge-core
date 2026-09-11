// Package apkmeta 从 APK 里取出 forge 需要知道的全部事实：包名、版本号、版本名、ABI 集合。
//
// **纯 Go 实现**（archive/zip + 一个二进制 AXML 解析库），不调 aapt、不需要 JDK、
// 不需要 runner 上存在任何 Android 工具。理由：03 §8 把"能不能拿到正确字段"标成整条链的
// 唯一硬依赖，而纯 Go 让这一层**可以被单测** —— 真 APK 的字节直接塞进 ReadZip 就能跑，
// 不必先假设 runner 镜像里有什么。
//
// 三个字段的语义都是契约：`package` 用于校验 id（02 规则 7）、`versionCode` 必填进
// additionalSettings、`versionName` 是 {version} token 的来源（02 §2.4）。
package apkmeta

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/shogo82148/androidbinary/apk"

	"github.com/market-of-labs/forge-core/internal/naming"
)

// Meta 是一个 APK 的全部元数据。
type Meta struct {
	// Package 是 APK 包内真实的 package，即安装身份。契约要求它 == 条目 id（02 规则 7）。
	Package string
	// VersionCode 与 VersionName 取自 manifest。
	//
	// ⚠️ **零值不代表"拿不到"以外的东西，而 error 也不代表"拿不到"** —— 见 HasVersionCode。
	VersionCode int32
	VersionName string
	// ABIs 是 APK 内 lib/<abi>/ 出现过的架构目录名（已排序、去重）。
	// 它不是"支持的 ABI"的权威声明（manifest 里没有这个东西），而是**包里实际带了哪些 native 库**。
	ABIs []string
	// Label 是 application/@android:label 解析出来的值，也就是**用户看到的那个应用名**。
	//
	// 它是 03 §2.6 "显示名由对账从 APK 派生"那条的落地处 —— 申请人不填显示名，
	// 就靠这个读出来。取值可能为空（见 HasLabel），调用方要能接住。
	Label string
}

// HasLabel 报告 Label 可用。空 label 是真实存在的：属性缺失，或属性是
// `@string/app_name` 而这个 APK 里没有 resources.arsc 可供解引用
// （androidbinary 那条路会返回 error，我们按 error 归成空）。
func (m Meta) HasLabel() bool { return m.Label != "" }

// HasVersionCode 报告 versionCode 可用。
//
// 判据是**值 > 0**，不是"解析没报错"：实测 `github.com/shogo82148/androidbinary` 对
// **缺失**的属性同样返回 `err == nil` 和零值（拿 androidTest 的 APK 验过：versionCode=0、
// versionName=""，两者都不报错），所以缺失与"真的就是 0"在返回值上不可区分，只能按值判。
// Android 侧 versionCode 0 也不是合法发布值。
//
// 这一条直接决定 03 §5.2 那句「拿不到时省略该字段而非写 0」能不能兑现 —— 照字面靠 error 判会失效。
func (m Meta) HasVersionCode() bool { return m.VersionCode > 0 }

// HasVersionName 报告 versionName 可用。判据同样是值非空，理由见 HasVersionCode。
func (m Meta) HasVersionName() bool { return m.VersionName != "" }

// UnknownABIs 返回 lib/ 里出现过、但不在 02 §2.4 固定 ABI 集内的目录名。
//
// 调用方应当把它当告警而不是错误：一个只带 mips 库的 APK 确实能解析，只是我们**无法给它命名**
// （token 集里没有 mips），于是 ABIToken 会保守地判 universal。
func (m Meta) UnknownABIs() []string {
	var out []string
	for _, a := range m.ABIs {
		if !naming.IsABI(a) {
			out = append(out, a)
		}
	}
	return out
}

// ABIToken 按 APK **内容**判定该用哪个 ABI token 命名（02 §2.4 的固定集）。
//
// 规则的四种情形，后两种是规格没写、由实现补齐的：
//
//	恰好 1 个已知 ABI        → 该 ABI
//	0 个（包里没有 native 库）→ universal   ← 规格明文：「无 ABI 标记且是唯一 APK → 判 universal」
//	≥2 个                    → universal   ← fat APK，它确实能在所有架构上装，只是体积大
//	恰好 1 个但不在固定集内   → universal   ← 无法命名，保守取 universal（调用方看 UnknownABIs 告警）
//
// 「≥2 个 → universal」是刻意的：客户端折叠时只做**精确匹配**，一个含 4 个 ABI 的 fat APK
// 若被标成其中任意一个，另外 3 种架构的设备就都拿不到它了。
func (m Meta) ABIToken() string {
	known := make([]string, 0, len(m.ABIs))
	for _, a := range m.ABIs {
		if naming.IsABI(a) && a != "universal" {
			known = append(known, a)
		}
	}
	if len(known) == 1 && len(m.ABIs) == 1 {
		return known[0]
	}
	return "universal"
}

// Read 打开磁盘上的 APK 文件。
func Read(path string) (*Meta, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	m, err := ReadZip(f, st.Size())
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// ReadZip 从任意 io.ReaderAt 读 APK。传字节切片即可测试，不必落盘
// （`bytes.NewReader(b)` + `int64(len(b))`）—— 这正是这一层能被单测的原因。
func ReadZip(r io.ReaderAt, size int64) (*Meta, error) {
	// 先取 zip 目录（ABI），再解 manifest。
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("读 zip 目录：%w", err)
	}
	m := &Meta{ABIs: libABIs(zr)}

	p, err := apk.OpenZipReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("解析 AndroidManifest.xml：%w", err)
	}
	defer p.Close()

	mf := p.Manifest()
	m.Package = p.PackageName()

	// 这两个 error 只在属性是**资源引用**且无法就地解析时非 nil
	// （如 android:versionName="@string/app_version" 且没有解 resources.arsc）。
	// 属性**缺失**不报错 —— 那条路走的是零值，由 HasVersionCode/HasVersionName 兜住。
	if vc, err := mf.VersionCode.Int32(); err == nil {
		m.VersionCode = vc
	}
	if vn, err := mf.VersionName.String(); err == nil {
		m.VersionName = vn
	}
	// label 与上面两个不同：**它的失败是常态而非异常**。`@string/app_name` 是最常见的
	// 写法，能解出来是因为 OpenZipReader 顺手读了 resources.arsc；包小到没带 arsc 的
	// 少数情况解不出来，那就留空 —— 由 HasLabel 兜住，调用方退到仓库名。
	//
	// 不去 attempt 任何"猜名字"的兜底：一个猜出来的显示名会被写进 sources/ 当成事实，
	// 而它后面还挂着 tag、Release、设备上那一行的标题。空着退回仓库名至少是诚实的。
	if lb, err := p.Label(nil); err == nil {
		m.Label = strings.TrimSpace(lb)
	}
	return m, nil
}

// libABIs 取出 zip 里 lib/<abi>/ 的第一段目录名。
//
// 用 lib/ 而不是 manifest 的声明：manifest 里没有"支持哪些 ABI"这个字段，
// 真正决定能否在某架构上装起来的就是包里带了哪些 .so。这也是 03 §3.2 要求的
// 「ABI 按 APK 内容判定」的落地方式。
func libABIs(zr *zip.Reader) []string {
	set := make(map[string]bool)
	for _, f := range zr.File {
		rest, ok := strings.CutPrefix(f.Name, "lib/")
		if !ok {
			continue
		}
		i := strings.IndexByte(rest, '/')
		if i <= 0 {
			continue
		}
		set[rest[:i]] = true
	}

	out := make([]string, 0, len(set))
	for a := range set {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}
