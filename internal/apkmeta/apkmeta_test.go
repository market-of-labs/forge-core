package apkmeta

import (
	"archive/zip"
	"bytes"
	"os"
	"reflect"
	"strings"
	"testing"
)

// makeZip 造一个只有目录项的最小 zip，用来喂 libABIs。
//
// 这里**不能**走 ReadZip —— 那条路要过 apk.OpenZipReader，它需要一个真的
// AndroidManifest.xml，而造一份合法的二进制 AXML 正是本包要验的东西，不该在测试里
// 手搓一份假的（假 AXML 只能证明解析器能读假货）。所以 ABI 提取单测直接打 libABIs，
// 端到端那一层交给下面的 TestReadZipRealAPK。
func makeZip(t *testing.T, names ...string) *zip.Reader {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatalf("造 zip 条目 %q：%v", n, err)
		}
		// 以 `/` 结尾的是目录项，zip 不允许往里写内容 —— 而目录项正是
		// 「畸形 lib 条目」那条用例要喂进去的东西。
		if strings.HasSuffix(n, "/") {
			continue
		}
		if _, err := w.Write([]byte("x")); err != nil {
			t.Fatalf("写 zip 条目 %q：%v", n, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("关 zip：%v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("重开 zip：%v", err)
	}
	return zr
}

func TestLibABIs(t *testing.T) {
	tests := []struct {
		name  string
		files []string
		want  []string
	}{
		{
			name: "单一 ABI",
			files: []string{
				"AndroidManifest.xml", "classes.dex",
				"lib/arm64-v8a/libfoo.so",
			},
			want: []string{"arm64-v8a"},
		},
		{
			// 本机 debug APK 的真实形状：4 个 ABI 目录 + 一堆干扰项
			name: "fat APK",
			files: []string{
				"AndroidManifest.xml", "classes.dex", "resources.arsc",
				"META-INF/CERT.RSA", "res/drawable/icon.png",
				"lib/arm64-v8a/libfoo.so", "lib/arm64-v8a/libbar.so",
				"lib/armeabi-v7a/libfoo.so",
				"lib/x86/libfoo.so", "lib/x86_64/libfoo.so",
			},
			want: []string{"arm64-v8a", "armeabi-v7a", "x86", "x86_64"}, // 按字典序，与契约顺序无关
		},
		{
			name:  "没有 native 库", // androidTest 那类纯 Java 包
			files: []string{"AndroidManifest.xml", "classes.dex"},
			want:  []string{},
		},
		{
			// `lib/` 本身不是目录项、`lib/x` 少一层，都不该产出 ABI
			name:  "畸形 lib 条目被忽略",
			files: []string{"lib/", "lib/README", "lib//x.so", "LIBS/arm64-v8a/libfoo.so"},
			want:  []string{},
		},
		{
			// 名字里含 lib/ 但不是顶层目录 → 不算（CutPrefix 只在开头匹配）
			name:  "嵌套 lib 不算",
			files: []string{"assets/lib/arm64-v8a/libfoo.so"},
			want:  []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := libABIs(makeZip(t, tt.files...))
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("libABIs(%v) = %v，期望 %v", tt.files, got, tt.want)
			}
		})
	}
}

func TestABIToken(t *testing.T) {
	tests := []struct {
		name string
		abis []string
		want string
	}{
		{name: "恰好一个已知 ABI", abis: []string{"arm64-v8a"}, want: "arm64-v8a"},
		{name: "恰好一个 x86", abis: []string{"x86"}, want: "x86"},
		{name: "无 native 库 → universal", abis: nil, want: "universal"},
		{name: "两个已知 ABI → universal", abis: []string{"arm64-v8a", "armeabi-v7a"}, want: "universal"},
		{name: "四个（真实 fat APK）", abis: []string{"arm64-v8a", "armeabi-v7a", "x86", "x86_64"}, want: "universal"},
		{name: "未知 ABI → universal", abis: []string{"mips"}, want: "universal"},
		{name: "未知 + 已知 → universal", abis: []string{"mips", "arm64-v8a"}, want: "universal"},
		{name: "lib/universal 目录不是 ABI 证据", abis: []string{"universal"}, want: "universal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := Meta{ABIs: tt.abis}
			if got := m.ABIToken(); got != tt.want {
				t.Errorf("ABIs=%v 的 ABIToken = %q，期望 %q", tt.abis, got, tt.want)
			}
		})
	}
}

func TestUnknownABIs(t *testing.T) {
	m := Meta{ABIs: []string{"arm64-v8a", "mips", "x86"}}
	if got, want := m.UnknownABIs(), []string{"mips"}; !reflect.DeepEqual(got, want) {
		t.Errorf("UnknownABIs() = %v，期望 %v", got, want)
	}
	if m := (Meta{ABIs: []string{"arm64-v8a"}}); m.UnknownABIs() != nil {
		t.Errorf("全是已知 ABI 时 UnknownABIs() 应为 nil，得到 %v", m.UnknownABIs())
	}
}

// TestHasVersion 是**零值陷阱**的回归测试。
//
// 实测 androidbinary 对缺失属性返回 err == nil + 零值，所以「拿不到」这个事实在
// 返回值上唯一的表现就是值为零。这两个方法若被改成 error 判据（或调用方改成信 error），
// 03 §5.2 的「拿不到时省略该字段」立刻失效 —— 会往清单里写 versionCode=0。
func TestHasVersion(t *testing.T) {
	missing := Meta{VersionCode: 0, VersionName: ""}
	if missing.HasVersionCode() {
		t.Error("versionCode 为 0 时 HasVersionCode 必须为 false")
	}
	if missing.HasVersionName() {
		t.Error("versionName 为空时 HasVersionName 必须为 false")
	}

	present := Meta{VersionCode: 1, VersionName: "1.0"}
	if !present.HasVersionCode() {
		t.Error("versionCode=1 必须是可用")
	}
	if !present.HasVersionName() {
		t.Error(`versionName="1.0" 必须是可用`)
	}

	// versionCode 1 是最小合法值（02 规则 6 要求正整数），负数属于坏数据，同样判不可用。
	if (Meta{VersionCode: -3}).HasVersionCode() {
		t.Error("负 versionCode 必须判不可用")
	}
}

func TestReadZipRejectsNonZip(t *testing.T) {
	junk := []byte("this is definitely not an apk")
	if _, err := ReadZip(bytes.NewReader(junk), int64(len(junk))); err == nil {
		t.Fatal("对非 zip 输入 ReadZip 应当报错")
	}
}

func TestReadMissingFile(t *testing.T) {
	if _, err := Read("no-such-file-9f3a1c.apk"); err == nil {
		t.Fatal("对不存在的文件 Read 应当报错")
	}
}

// TestReadZipRealAPK 是本包唯一的端到端测试，默认跳过。
//
// 真 APK 有 8 MB 量级，不适合塞进仓库当 fixture（每次 clone 都付一次代价，而且
// 二进制在 diff 里毫无信息量）。所以走环境变量：
//
//	FORGE_TEST_APK=/path/to/app.apk go test ./internal/apkmeta/ -run RealAPK -v
//
// 也接受多个路径（os.PathListSeparator 分隔），逐个断言"至少能解析出非空包名"。
func TestReadZipRealAPK(t *testing.T) {
	paths := os.Getenv("FORGE_TEST_APK")
	if paths == "" {
		t.Skip("未设 FORGE_TEST_APK，跳过真 APK 端到端测试")
	}

	for _, path := range splitPathList(paths) {
		t.Run(path, func(t *testing.T) {
			m, err := Read(path)
			if err != nil {
				t.Fatalf("Read：%v", err)
			}
			if m.Package == "" {
				t.Error("包名为空 —— 契约要求 id 等于 APK 真实包名（02 规则 7）")
			}
			t.Logf("package=%q versionCode=%d(%v) versionName=%q(%v) ABIs=%v token=%q unknown=%v",
				m.Package, m.VersionCode, m.HasVersionCode(),
				m.VersionName, m.HasVersionName(),
				m.ABIs, m.ABIToken(), m.UnknownABIs())
		})
	}
}

func splitPathList(s string) []string {
	var out []string
	for _, p := range bytes.Split([]byte(s), []byte{os.PathListSeparator}) {
		if len(p) > 0 {
			out = append(out, string(p))
		}
	}
	return out
}
