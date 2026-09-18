package fdroid

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/market-of-labs/forge-core/internal/naming"
)

// 产物契约里的常量（02 §2.1–2.3）。它们被 `check-repo` 逐条断言，所以写成常量 ——
// "改契约"应当是一次编译期可见的改动，而不是一次 grep。
const (
	// EntryJarName 是签名入口。整个仓库的信任链从这里开始：客户端先下它、验签、
	// 读出 entry.json、再按里面的 sha256 去校验 index-v2.json。所以它坏了不是
	// "少一个文件"，是**所有客户端拒绝这个源**。
	EntryJarName = "entry.jar"
	// EntryJSONName 是 entry.jar 里那个条目名。
	EntryJSONName = "entry.json"
	// EntryJarEntryCount 是 entry.jar 应有的条目数：entry.json + META-INF/MANIFEST.MF
	// + META-INF/<alias>.SF + META-INF/<alias>.RSA。实测（spike ③）与 IzzyOnDroid 的
	// 真实实例同构，两边都是 4。
	//
	// 多一个条目通常意味着有人往里面塞了东西；少一个意味着签名不完整 ——
	// 两种都是"看起来正常但客户端会拒绝"，所以按**恰好等于**断言而不是"至少"。
	EntryJarEntryCount = 4
	// IndexV2Name 是主索引。
	IndexV2Name = "index-v2.json"
	// IndexVersionV2 是 entry.json 的 `version` 字段值（02 §2.2）。
	IndexVersionV2 = 20002
)

// EntryJSON 是 entry.jar 里那份 `entry.json`（02 §2.2）。
//
// 它是**信任链的第一环**：`Index.SHA256` 必须等于磁盘上 `index-v2.json` 的实际哈希值，
// 客户端在安装任何东西之前会先验这一条。它断了的表现不是报错，而是客户端
// 认为这个源是坏的 —— 而服务端这边一切看起来都正常。
type EntryJSON struct {
	Timestamp int64 `json:"timestamp"`
	Version   int   `json:"version"`
	Index     struct {
		// Name 是索引的文件名，**带前导斜杠**（`/index-v2.json`）——
		// 这是 F-Droid 的写法，不是笔误，也不要"顺手"去掉。
		Name        string `json:"name"`
		SHA256      string `json:"sha256"`
		Size        int64  `json:"size"`
		NumPackages int    `json:"numPackages"`
	} `json:"index"`
	// Diffs 是增量索引，本市场留空 `{}`（02 §2.9）。**不解析它的内容**：
	// 字段存在就够了，真要支持增量是另一件事。
	Diffs map[string]json.RawMessage `json:"diffs"`
}

// IndexV2 是 `index-v2.json` 里**我们消费的那部分**（02 §2.3）。
//
// 刻意只声明用得上的字段：这份文件的结构由 fdroidserver 决定、将来还可能加字段，
// 而 Go 的 json 解码默认忽略未知键 —— 于是"它加了新字段"对我们永远无害。
// 反过来说，**不要把整份文件映成一个 map[string]any 去逐键取**：
// 那样每个字段名的拼写错误都会变成一次运行时 nil，而不是一次编译错误。
type IndexV2 struct {
	Repo struct {
		// Address 是仓库地址。客户端取 APK 的地址是 `Address + "/" + file.Name`，
		// 所以它必须与 CF 网关实际服务的地址一致（02 §2.3）—— 见 check-repo 的硬错误第 3 条。
		Address string `json:"address"`
	} `json:"repo"`
	Packages map[string]Package `json:"packages"`
}

// Package 是一个应用在索引里的条目。
type Package struct {
	// Metadata 是 fdroidserver 从 metadata/*.yml 读到的那些字段（name/summary/categories…）。
	// 它是**开放**的，所以按 RawMessage 收着，需要哪个键再解哪个 ——
	// 我们不靠它做任何判定，只看几个可选字段在不在（`check-repo` 的软告警）。
	Metadata map[string]json.RawMessage `json:"metadata"`
	// Versions 以 **APK 的 sha256** 为键，**不是 versionCode**。
	//
	// 这一点是整个索引结构里最反直觉的地方，也是 spike ③ 专门验的一条：
	// 同一 versionCode 的多个 ABI 变体因此可以共存（它们的字节不同 ⇒ sha256 不同），
	// 而"同一版本有几个变体"只能靠数这个 map 的长度得到。
	Versions map[string]AppVersion `json:"versions"`
}

// AppVersion 是索引里一个具体的 APK 变体。
type AppVersion struct {
	File     APKFile  `json:"file"`
	Manifest Manifest `json:"manifest"`
}

// APKFile 是索引对磁盘上那个文件的声明。
//
// `SHA256` 与 `Size` **必须与磁盘一致**（02 §2.8 硬错误第 4 条）：客户端下载后会校验，
// 不一致的表现不是"装不上"这么简单，而是**下载完才发现装不上**。
type APKFile struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Manifest 是 fdroidserver 从 APK 里**现读**出来的信息（02 §2.8 硬错误第 5、6 条）。
type Manifest struct {
	// VersionCode 必须 > 0。fdroidserver 从 APK 里读，所以它若为 0 或缺失，
	// 说明那个 APK 的 manifest 本身有问题（而不是我们没提供这个值）。
	VersionCode int64 `json:"versionCode"`
	// NativeCode 是 APK 里带原生库的 ABI 列表。**universal 包这里是空的** ——
	// 见 AppVersion.HasNativeCodeOrUniversal。
	NativeCode []string `json:"nativecode"`
	// Signer 是 APK 自己的签名者（**不是**仓库签名，两把完全不同的钥匙，02 §2.7）。
	Signer string `json:"signer"`
}

// HasNativeCodeOrUniversal 回答 02 §2.8 硬错误第 5 条的"非空的 nativecode **或**显式的 universal"。
//
// 为什么不能只判 `len(NativeCode) > 0`：universal 包（一个 APK 装所有 ABI）本来就
// 没有 nativecode —— fdroidserver 会**整个省略这个键**，而不是写一个空数组。
// 所以"空"在这里是**正常**的，前提是那个文件确实是 universal 包。
//
// 判据取文件名里那个 ABI token（`-universal.apk`），也就是 02 §2.4 的命名契约 ——
// 这是本市场自己的硬约定（D63：fdroidserver 不改名），所以它比任何猜测都可靠。
func (v AppVersion) HasNativeCodeOrUniversal() bool {
	if len(v.Manifest.NativeCode) > 0 {
		return true
	}
	stem, ok := strings.CutSuffix(v.File.Name, naming.Ext)
	if !ok {
		return false
	}
	return strings.HasSuffix(stem, "-"+naming.ABIUniversal)
}

// ReadEntryJar 打开 `entry.jar`，读出里面的 `entry.json`（02 §2.2）。
//
// ⚠️ 它**不验签** —— JAR 签名（PKCS#7）不适合手搓，验签由 `jarsigner -verify` 子进程做，
// 那是 job 层的事（`check-repo` 也因此在容器里跑）。本函数只回答"这个 jar 读得开吗、
// entry.json 在不在、能不能解析"，那三条不需要任何密码学。
func ReadEntryJar(path string) (*EntryJSON, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("fdroid: 打开 %s 失败：%w", path, err)
	}
	defer zr.Close()

	for _, f := range zr.File {
		if f.Name != EntryJSONName {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("fdroid: 打开 %s 里的 %s 失败：%w", path, EntryJSONName, err)
		}
		defer rc.Close()

		var e EntryJSON
		dec := json.NewDecoder(rc)
		if err := dec.Decode(&e); err != nil {
			return nil, fmt.Errorf("fdroid: 解析 %s 里的 %s 失败：%w", path, EntryJSONName, err)
		}
		return &e, nil
	}
	return nil, fmt.Errorf("fdroid: %s 里没有 %s", path, EntryJSONName)
}

// JarEntries 返回 jar 里的条目名，顺序即 zip 里的出现顺序。
func JarEntries(path string) ([]string, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("fdroid: 打开 %s 失败：%w", path, err)
	}
	defer zr.Close()

	out := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		out = append(out, f.Name)
	}
	return out, nil
}

// ReadIndexV2 读 `index-v2.json`（02 §2.3）。
func ReadIndexV2(path string) (*IndexV2, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("fdroid: 读取 %s 失败：%w", path, err)
	}
	var x IndexV2
	if err := json.Unmarshal(b, &x); err != nil {
		return nil, fmt.Errorf("fdroid: 解析 %s 失败：%w", path, err)
	}
	return &x, nil
}

// SHA256File 算一个文件的 sha256（十六进制小写），供与索引里记的值比对。
//
// 用流式而不是 `os.ReadFile` + `sha256.Sum256`：这里要处理的正是 APK，
// 而它们是整个仓库里最大的文件（每个 20 MB 量级，一次对账要过几十个）。
func SHA256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// SortedPackageIDs 按包名升序返回索引里的全部包。
//
// Go 的 map 迭代顺序是**随机**的，而 `check-repo` 要产出稳定的报告与稳定的退出判定 ——
// 一个"每轮告警顺序都不同"的报告会让人没法做 diff，也会让"这轮是不是变差了"没法回答。
func (x *IndexV2) SortedPackageIDs() []string {
	return slices.Sorted(maps.Keys(x.Packages))
}

// SortedVersionKeys 按 sha256 升序返回一个包的全部版本键。
func (p Package) SortedVersionKeys() []string {
	return slices.Sorted(maps.Keys(p.Versions))
}
