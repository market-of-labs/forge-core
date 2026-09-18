package fdroid

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestArtifactContractConstants 把那几个被 `check-repo` 逐条断言的常量钉死。
//
// 它们看着像"从规格里抄来的数字"，但每一个都对应一次实测：
// `20002` 是 entry.json 的 schema 版本，`4` 是 entry.jar 的条目数
// （entry.json + MANIFEST.MF + .SF + .RSA，spike ③ 实测与 IzzyOnDroid 的真实实例同构）。
// 数字改了而没人注意到，后果是**所有客户端拒绝这个源**。
func TestArtifactContractConstants(t *testing.T) {
	if IndexVersionV2 != 20002 {
		t.Errorf("entry.json 的 version 是 %d，规格是 20002", IndexVersionV2)
	}
	if EntryJarEntryCount != 4 {
		t.Errorf("entry.jar 的条目数是 %d，实测是 4", EntryJarEntryCount)
	}
	if EntryJarName != "entry.jar" || EntryJSONName != "entry.json" || IndexV2Name != "index-v2.json" {
		t.Errorf("产物文件名被改动了：%q / %q / %q", EntryJarName, EntryJSONName, IndexV2Name)
	}
}

// writeJar 造一个最小的 jar。entries 按给定顺序写入 —— 顺序本身要能被 JarEntries 看见。
func writeJar(t *testing.T, path string, entries map[string]string, order []string) {
	t.Helper()

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("建 %s 失败：%v", path, err)
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	for _, name := range order {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("写 %s 失败：%v", name, err)
		}
		if _, err := w.Write([]byte(entries[name])); err != nil {
			t.Fatalf("写 %s 的内容失败：%v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("关上 zip 失败：%v", err)
	}
}

// realEntryJar 是一份形状正确的 entry.jar：恰好 4 个条目，第一个是 entry.json。
func realEntryJar(t *testing.T) (string, EntryJSON) {
	t.Helper()

	entry := EntryJSON{Timestamp: 1758150000, Version: IndexVersionV2}
	entry.Index.Name = "/" + IndexV2Name
	entry.Index.SHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	entry.Index.Size = 4096
	entry.Index.NumPackages = 12

	body := `{"timestamp":1758150000,"version":20002,"index":{"name":"/index-v2.json",` +
		`"sha256":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",` +
		`"size":4096,"numPackages":12},"diffs":{}}`

	path := filepath.Join(t.TempDir(), EntryJarName)
	writeJar(t, path,
		map[string]string{
			EntryJSONName:                   body,
			"META-INF/MANIFEST.MF":          "Manifest-Version: 1.0\r\n\r\n",
			"META-INF/" + KeyAlias + ".SF":  "Signature-Version: 1.0\r\n\r\n",
			"META-INF/" + KeyAlias + ".RSA": "\x30\x82binary-pkcs7",
		},
		[]string{EntryJSONName, "META-INF/MANIFEST.MF", "META-INF/" + KeyAlias + ".SF", "META-INF/" + KeyAlias + ".RSA"},
	)
	return path, entry
}

// TestReadEntryJar 覆盖正常路径。
func TestReadEntryJar(t *testing.T) {
	path, want := realEntryJar(t)

	got, err := ReadEntryJar(path)
	if err != nil {
		t.Fatalf("ReadEntryJar 报错：%v", err)
	}

	if got.Version != want.Version {
		t.Errorf("version：想 %d，实得 %d", want.Version, got.Version)
	}
	if got.Index.Name != "/index-v2.json" {
		// ⚠️ 前导斜杠是 F-Droid 的写法，不是笔误。去掉它客户端就找不到索引了。
		t.Errorf("index.name 想 %q，实得 %q（注意 F-Droid 带前导斜杠）", "/index-v2.json", got.Index.Name)
	}
	if got.Index.SHA256 != want.Index.SHA256 {
		t.Errorf("index.sha256 走样：%q", got.Index.SHA256)
	}
	if got.Index.Size != want.Index.Size || got.Index.NumPackages != want.Index.NumPackages {
		t.Errorf("index.size/numPackages 走样：%d/%d", got.Index.Size, got.Index.NumPackages)
	}
	if got.Diffs == nil {
		t.Error("diffs 应当是一个空 map 而不是 nil —— 规格要求留空 {} 而非省略这个键")
	}
}

// TestJarEntries 钉住"恰好 4 个条目"这条断言的原料。
func TestJarEntries(t *testing.T) {
	path, _ := realEntryJar(t)

	names, err := JarEntries(path)
	if err != nil {
		t.Fatalf("JarEntries 报错：%v", err)
	}
	if len(names) != EntryJarEntryCount {
		t.Errorf("条目数是 %d，应当是 %d：%v", len(names), EntryJarEntryCount, names)
	}
	// 顺序即写入顺序 —— 这条让"entry.json 必须排第一"这种约定有据可查
	// （F-Droid 客户端按名字找，不依赖顺序，但顺序稳定让产物可比对）。
	if names[0] != EntryJSONName {
		t.Errorf("第一个条目是 %q，应当是 %q", names[0], EntryJSONName)
	}
	for _, n := range names {
		if n == "" {
			t.Error("出现了空条目名")
		}
	}
}

// TestReadEntryJarRejects 覆盖三类畸形输入。
//
// 它们都以"一个错误"收场，但**触发点不同**：文件根本打不开、打得开但不是 zip、
// 是 zip 但没有 entry.json。分开测是因为 `check-repo` 要按错误内容给出不同的提示 ——
// "entry.jar 不是 zip"和"entry.jar 里没有 entry.json"是两种完全不同的故障。
func TestReadEntryJarRejects(t *testing.T) {
	t.Run("文件不存在", func(t *testing.T) {
		if _, err := ReadEntryJar(filepath.Join(t.TempDir(), "没有这个文件.jar")); err == nil {
			t.Error("不存在的文件应当报错")
		}
	})

	t.Run("不是 zip", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), EntryJarName)
		if err := os.WriteFile(p, []byte("这不是一个 zip 文件"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadEntryJar(p); err == nil {
			t.Error("非 zip 应当报错")
		}
	})

	t.Run("是 zip 但没有 entry.json", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), EntryJarName)
		writeJar(t, p, map[string]string{"META-INF/MANIFEST.MF": "x"}, []string{"META-INF/MANIFEST.MF"})
		if _, err := ReadEntryJar(p); err == nil {
			t.Error("缺 entry.json 应当报错")
		}
	})

	t.Run("entry.json 不是合法 JSON", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), EntryJarName)
		writeJar(t, p, map[string]string{EntryJSONName: "{ 这不是 json"}, []string{EntryJSONName})
		if _, err := ReadEntryJar(p); err == nil {
			t.Error("坏 JSON 应当报错")
		}
	})
}

// realIndexJSON 是一份**裁剪过的真实形状**（照 spike ③ 产出的 index-v2.json 抄的结构）。
//
// 裁剪是刻意的：这里要验的是**我们解析的那几条路径**，而真实文件里还有
// `srcmanifest`/`added`/`antifeatures` 等一堆我们不消费的键 —— 它们留在里面反而
// 变成了一份"未知键被忽略"的测试（那也是有用的，但属于另一条）。
// 两个变体刻意用**同一个 versionCode 的不同 ABI**：索引里它们靠 sha256 区分，
// 这正是 §2.3 里那个最反直觉的地方。
const realIndexJSON = `{
  "repo": {
    "address": "https://example.invalid/fdroid/repo",
    "name": "market-of-labs",
    "description": "market-of-labs 内部应用市场",
    "timestamp": 1758150000,
    "version": 20002
  },
  "packages": {
    "dev.imranr.obtainium": {
      "metadata": {"name": "Obtainium", "summary": "从任意来源安装并更新应用"},
      "versions": {
        "aa11bb22cc33dd44ee55ff660011223344556677889900aabbccddeeff001122": {
          "file": {"name": "dev.imranr.obtainium-1.6.17-arm64-v8a.apk", "sha256": "aa11bb22cc33dd44ee55ff660011223344556677889900aabbccddeeff001122", "size": 25165824},
          "manifest": {"versionCode": 23563, "nativecode": ["arm64-v8a"], "signer": "1111222233334444555566667777888899990000aaaabbbbccccddddeeeeffff"}
        },
        "bb22cc33dd44ee55ff660011223344556677889900aabbccddeeff00112233": {
          "file": {"name": "dev.imranr.obtainium-1.6.17-armeabi-v7a.apk", "sha256": "bb22cc33dd44ee55ff660011223344556677889900aabbccddeeff00112233", "size": 20971520},
          "manifest": {"versionCode": 23562, "nativecode": ["armeabi-v7a"], "signer": "1111222233334444555566667777888899990000aaaabbbbccccddddeeeeffff"}
        }
      }
    },
    "com.example.universal": {
      "metadata": {"name": "一个通用包应用"},
      "versions": {
        "cc33dd44ee55ff660011223344556677889900aabbccddeeff00112233445": {
          "file": {"name": "com.example.universal-2.0-universal.apk", "sha256": "cc33dd44ee55ff660011223344556677889900aabbccddeeff00112233445", "size": 10485760},
          "manifest": {"versionCode": 20, "signer": "2222333344445555666677778888999900001111bbbbccccddddeeeeffffaaaa"}
        }
      }
    }
  }
}`

func writeIndex(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), IndexV2Name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestReadIndexV2 覆盖我们消费的那几条路径。
func TestReadIndexV2(t *testing.T) {
	x, err := ReadIndexV2(writeIndex(t, realIndexJSON))
	if err != nil {
		t.Fatalf("ReadIndexV2 报错：%v", err)
	}

	if x.Repo.Address != "https://example.invalid/fdroid/repo" {
		// 客户端取 APK 的地址是 address + "/" + file.name，所以这个值错了
		// 表现是"能列出应用但一个都装不上"。
		t.Errorf("repo.address 走样：%q", x.Repo.Address)
	}
	if len(x.Packages) != 2 {
		t.Fatalf("包数是 %d，应当是 2", len(x.Packages))
	}

	pkg, ok := x.Packages["dev.imranr.obtainium"]
	if !ok {
		t.Fatal("没有解析出 dev.imranr.obtainium")
	}
	if len(pkg.Versions) != 2 {
		t.Fatalf("版本数是 %d，应当是 2（两个 ABI 变体，sha256 不同）", len(pkg.Versions))
	}

	// ⚠️ 键是 **sha256** 而不是 versionCode —— 下面这个查找就是那条规则的证明：
	// 拿 versionCode 当键是查不到的。
	if _, found := pkg.Versions["23563"]; found {
		t.Error("versions 的键是 sha256，不是 versionCode")
	}
	v, ok := pkg.Versions["aa11bb22cc33dd44ee55ff660011223344556677889900aabbccddeeff001122"]
	if !ok {
		t.Fatalf("按 sha256 查不到那个变体，实有的键：%v", pkg.SortedVersionKeys())
	}
	if v.File.Name != "dev.imranr.obtainium-1.6.17-arm64-v8a.apk" {
		t.Errorf("file.name 走样：%q", v.File.Name)
	}
	if v.File.Size != 25165824 {
		t.Errorf("file.size 走样：%d", v.File.Size)
	}
	if v.Manifest.VersionCode != 23563 {
		t.Errorf("manifest.versionCode 走样：%d", v.Manifest.VersionCode)
	}
	if !slices.Equal(v.Manifest.NativeCode, []string{"arm64-v8a"}) {
		t.Errorf("manifest.nativecode 走样：%v", v.Manifest.NativeCode)
	}
	if v.Manifest.Signer == "" {
		t.Error("manifest.signer 为空 —— 它是硬错误第 6 条")
	}
}

// TestIndexSortedKeysAreDeterministic 钉住"报告顺序稳定"。
//
// Go 的 map 迭代顺序是随机的，而 `check-repo` 的产出要么进日志、要么进
// issue 评论 —— 一个每轮顺序都不同的报告没法 diff，也就没法回答
// "这轮是不是比上轮差了"。
func TestIndexSortedKeysAreDeterministic(t *testing.T) {
	x, err := ReadIndexV2(writeIndex(t, realIndexJSON))
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"com.example.universal", "dev.imranr.obtainium"} // 字典序
	if got := x.SortedPackageIDs(); !slices.Equal(got, want) {
		t.Errorf("SortedPackageIDs：想 %q，实得 %q", want, got)
	}

	wantKeys := []string{
		"aa11bb22cc33dd44ee55ff660011223344556677889900aabbccddeeff001122",
		"bb22cc33dd44ee55ff660011223344556677889900aabbccddeeff00112233",
	}
	if got := x.Packages["dev.imranr.obtainium"].SortedVersionKeys(); !slices.Equal(got, wantKeys) {
		t.Errorf("SortedVersionKeys：想 %q，实得 %q", wantKeys, got)
	}

	// 连跑几次都必须是同一个顺序（第一次对不代表没有随机性 —— 之前那次可能只是碰巧）。
	first := x.SortedPackageIDs()
	for i := 0; i < 50; i++ {
		if got := x.SortedPackageIDs(); !slices.Equal(got, first) {
			t.Fatalf("第 %d 次的顺序与第一次不同：%q vs %q", i, got, first)
		}
	}
}

// TestReadIndexV2Rejects 覆盖两类畸形输入。
func TestReadIndexV2Rejects(t *testing.T) {
	t.Run("文件不存在", func(t *testing.T) {
		if _, err := ReadIndexV2(filepath.Join(t.TempDir(), "没有这个文件.json")); err == nil {
			t.Error("不存在的文件应当报错")
		}
	})
	t.Run("不是合法 JSON", func(t *testing.T) {
		if _, err := ReadIndexV2(writeIndex(t, "{")); err == nil {
			t.Error("坏 JSON 应当报错")
		}
	})
}

// TestReadIndexV2IgnoresUnknownKeys 钉住"fdroidserver 加了新字段对我们无害"。
//
// fdroidserver 的版本会变，索引会长出新键（`srcmanifest`、`antifeatures`……）。
// 我们刻意用**窄结构体**去解它而不是 `map[string]any` —— 那条路的代价是
// 每个字段名的拼写错误都变成一次运行时 nil，而不是一次编译错误；
// 收益就是这条测试：未知键进不来，也炸不了。
func TestReadIndexV2IgnoresUnknownKeys(t *testing.T) {
	body := `{
	  "repo": {"address": "https://x.invalid/r", "some_future_key": {"nested": [1,2,3]}},
	  "packages": {},
	  "a_whole_new_top_level_block": "fdroidserver 将来可能加的东西"
	}`
	x, err := ReadIndexV2(writeIndex(t, body))
	if err != nil {
		t.Fatalf("未知键不该让解析失败：%v", err)
	}
	if x.Repo.Address != "https://x.invalid/r" {
		t.Errorf("已知键没解出来：%q", x.Repo.Address)
	}
}

// TestHasNativeCodeOrUniversal 覆盖 §2.8 硬错误第 5 条的那个判定。
//
// 它看着像个平凡的长度检查，其实不是：**universal 包的 nativecode 是空的**，
// 而"空"在这里是正常的 —— fdroidserver 会整个省略这个键，而不是写一个空数组。
// 所以只判 `len > 0` 会把每一个 universal 包都误报成硬错误。
func TestHasNativeCodeOrUniversal(t *testing.T) {
	cases := []struct {
		name     string
		fileName string
		native   []string
		want     bool
	}{
		{"有 nativecode 的普通分片", "a-1.0-arm64-v8a.apk", []string{"arm64-v8a"}, true},
		{"有 nativecode 的多 ABI 分片", "a-1.0-universal.apk", []string{"arm64-v8a", "armeabi-v7a"}, true},
		{"无 nativecode 但文件名说是 universal", "a-1.0-universal.apk", nil, true},
		{"无 nativecode 且文件名不是 universal", "a-1.0-arm64-v8a.apk", nil, false},
		{"无 nativecode 且空数组（fdroidserver 的另一种写法）", "a-1.0-arm64-v8a.apk", []string{}, false},
		{"无 nativecode、文件名没有 ABI 段", "a-1.0.apk", nil, false},
		{"无 nativecode、文件名根本没后缀", "a-1.0-universal", nil, false},
		{"扩展名对但 ABI 段只是恰好以它结尾", "a-1.0-notuniversal.apk", nil, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := AppVersion{
				File:     APKFile{Name: c.fileName},
				Manifest: Manifest{NativeCode: c.native},
			}
			if got := v.HasNativeCodeOrUniversal(); got != c.want {
				t.Errorf("HasNativeCodeOrUniversal(%q, %v) = %v，想 %v",
					c.fileName, c.native, got, c.want)
			}
		})
	}
}

// TestSHA256File 与标准库对答案。
func TestSHA256File(t *testing.T) {
	// 空文件：哈希是那个有名的 e3b0c442…（它在上面当占位符用）。
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := SHA256File(empty)
	if err != nil {
		t.Fatalf("SHA256File 报错：%v", err)
	}
	const wantEmpty = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got != wantEmpty {
		t.Errorf("空文件的 sha256 是 %q，想 %q", got, wantEmpty)
	}

	// 随便一点内容，与 sha256.Sum256 对答案 —— 确认走的是**文件内容**而不是别的
	// （比如路径、或者流式读的时候漏了最后一块）。
	body := []byte("这不是一个真的 APK，但哈希不知道这件事。\n")
	p := filepath.Join(t.TempDir(), "fake.apk")
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = SHA256File(p)
	if err != nil {
		t.Fatalf("SHA256File 报错：%v", err)
	}
	sum := sha256.Sum256(body)
	if want := hex.EncodeToString(sum[:]); got != want {
		t.Errorf("sha256 是 %q，想 %q", got, want)
	}

	// 十六进制必须是小写、定长 —— 索引里的 sha256 就是这么写的，
	// 一个大写的摘要与索引里的值永远比不相等，而"看起来只差大小写"很难被注意到。
	if got != strings.ToLower(got) {
		t.Errorf("sha256 用了大写：%q", got)
	}
	if len(got) != 64 {
		t.Errorf("sha256 长度是 %d，应当是 64", len(got))
	}

	t.Run("文件不存在", func(t *testing.T) {
		if _, err := SHA256File(filepath.Join(t.TempDir(), "没有这个文件")); err == nil {
			t.Error("不存在的文件应当报错，而不是返回空哈希")
		}
	})
}

// TestSHA256FileIsStreaming 用一个大于一个读块的文件确认流式读取不丢尾巴。
//
// 这条针对的坏法很具体：`io.Copy` 换成手写的循环时漏掉最后一次短读，
// 而它只在**大文件**上显形 —— 小文件一次就读完了，永远测不出来。
// 用 8 MiB 是因为它远超任何默认缓冲区。
func TestSHA256FileIsStreaming(t *testing.T) {
	body := make([]byte, 8<<20)
	for i := range body {
		body[i] = byte(i * 7) // 别用全零：那样"漏掉一整个读块"也可能碰巧哈希对上
	}
	p := filepath.Join(t.TempDir(), "big.apk")
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := SHA256File(p)
	if err != nil {
		t.Fatalf("SHA256File 报错：%v", err)
	}
	sum := sha256.Sum256(body)
	if want := hex.EncodeToString(sum[:]); got != want {
		t.Errorf("8 MiB 文件的 sha256 对不上 —— 流式读丢了尾巴：%q vs %q", got, want)
	}
}
