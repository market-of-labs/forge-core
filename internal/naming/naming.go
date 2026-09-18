// Package naming 实现 asset 文件名的契约：{appId}-{version}-{abi}.apk。
//
// 为什么值得单独成包：这个形状是**硬依赖**而不是约定（02 §2.4）。Release 的 tag 就是
// {appId}、不含版本段（D21），所以**版本与 ABI 的唯一载体就是 asset 文件名**：
// build-index 靠它建索引、客户端靠它折叠 ABI。而两边解析失败都是**静默降级**
// （索引缺条目 / 客户端折叠不中），不会有任何报错。所以规则必须写死在一处、
// 并且被单测钉住，而不是散在九个动词里各写一遍。
//
// 本包只做纯粹的名字运算：不读文件、不碰网络。
package naming

import (
	"fmt"
	"sort"
	"strings"
)

// Ext 是所有 asset 的固定扩展名。
const Ext = ".apk"

// 单个 ABI token 的常量。它们的**值**就是 02 §2.4 的字符串，全仓库不再出现字面量。
const (
	ABIUniversal = "universal"
	ABIARM64     = "arm64-v8a"
	ABIARMv7     = "armeabi-v7a"
	ABIX86_64    = "x86_64"
	ABIX86       = "x86"
)

// ABISet 是 02 §2.4 定下的固定 ABI token 集。
//
// **顺序即契约**：它就是清单里 `apkUrls` 的列表顺序约定 —— universal 在前，
// 其后 arm64-v8a / armeabi-v7a / x86_64 / x86。客户端按设备 ABI 精确匹配后，
// 匹配不中时退到 universal，所以 universal 排在前面也是给人看的可读性。
var ABISet = []string{ABIUniversal, ABIARM64, ABIARMv7, ABIX86_64, ABIX86}

// IsABI 报告 token 是否属于固定集。
func IsABI(abi string) bool {
	for _, a := range ABISet {
		if a == abi {
			return true
		}
	}
	return false
}

// AssetName 渲染规范文件名。
//
// 调用方必须已确认 version 是**清洗过**的 token（见 SanitizeVersion）——
// 本函数不做清洗，因为它同时被"解析"一侧的对称用例引用，隐式改写会让两边不一致。
func AssetName(appID, version, abi string) string {
	return appID + "-" + version + "-" + abi + Ext
}

// Split 从文件名里解析出 version 与 abi。
//
// **为什么要传 tag（= appId）进来**：裸文件名是 {appId}-{version}-{abi}.apk，而 appId 是包名
// （含 `.`）、version 是 versionName（可能含 `-`），从纯字符串上无法唯一切分。
// 但调用方**总是**知道 appId —— Release 的 tag 就是它（D21）—— 于是这里把歧义从"猜"
// 变成"验"：先按已知 tag 剥前缀、按固定 ABI 集从尾部剥后缀，中间剩下的就是 version。
//
// 剥后缀必须**先于**剥前缀，否则 version 里以 ABI 名结尾（如 `1.0-x86`）会切错。
func Split(tag, fileName string) (version, abi string, err error) {
	rest, ok := strings.CutSuffix(fileName, Ext)
	if !ok {
		return "", "", fmt.Errorf("asset 名缺少 %s 后缀：%q", Ext, fileName)
	}

	for _, a := range ABISet {
		if v, ok := strings.CutSuffix(rest, "-"+a); ok {
			rest, abi = v, a
			break
		}
	}
	if abi == "" {
		return "", "", fmt.Errorf("asset 名的 ABI 尾缀不在固定集 %v 内：%q", ABISet, fileName)
	}

	version, ok = strings.CutPrefix(rest, tag+"-")
	if !ok {
		return "", "", fmt.Errorf("asset 名不以 %q 为前缀：%q", tag+"-", fileName)
	}
	if version == "" {
		return "", "", fmt.Errorf("asset 名的 version 段为空：%q", fileName)
	}
	return version, abi, nil
}

// SanitizeVersion 把 versionName 清洗成可用作文件名与 URL 路径段的 token（02 §2.4）。
//
// 只做**最小**清洗：规范说 versionName 就是 {version} 的取值、不做自定义改写，
// 仅当含 `/`、空白等 URL/git 不安全字符时才替换。
//
// 保留集刻意小而明确：字母、数字、`.`、`_`、`+`、`~`、`-`。其余每个**连续段**替换为单个 `-`，
// 并去掉首尾的 `-`（避免产生 `a--b`、`-1.0` 这类难看的名字）。
// 结果为空则报错 —— 那是调用方应当拒绝而不是猜的输入。
func SanitizeVersion(v string) (string, error) {
	var b strings.Builder
	b.Grow(len(v))
	prevReplaced := false

	for _, r := range v {
		if isSafeVersionRune(r) {
			b.WriteRune(r)
			prevReplaced = false
			continue
		}
		if !prevReplaced {
			b.WriteByte('-')
			prevReplaced = true
		}
	}

	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "", fmt.Errorf("versionName 清洗后为空：%q", v)
	}
	return out, nil
}

func isSafeVersionRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	switch r {
	case '.', '_', '+', '~', '-':
		return true
	}
	return false
}

// SortABIs 按契约定下的顺序排列 ABI 列表，并去掉重复项。
//
// 不在固定集内的 token 会被排到末尾（按字典序）而不是被丢弃 —— 丢弃会让"清单里少了一个
// 变体"变成一个看不见的事实，而排序只是为了可读性与 diff 稳定，不该承担过滤职责。
func SortABIs(abis []string) []string {
	rank := make(map[string]int, len(ABISet))
	for i, a := range ABISet {
		rank[a] = i
	}

	out := make([]string, 0, len(abis))
	seen := make(map[string]bool, len(abis))
	for _, a := range abis {
		if seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}

	sort.SliceStable(out, func(i, j int) bool {
		ri, iOK := rank[out[i]]
		rj, jOK := rank[out[j]]
		switch {
		case iOK && jOK:
			return ri < rj
		case iOK != jOK:
			return iOK // 已知的排在未知的前面
		default:
			return out[i] < out[j]
		}
	})
	return out
}
