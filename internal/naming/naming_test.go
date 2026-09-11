package naming

import (
	"reflect"
	"testing"
)

func TestSplit(t *testing.T) {
	tests := []struct {
		name    string
		tag     string
		file    string
		version string
		abi     string
		wantErr bool
	}{
		{
			name: "普通", tag: "com.foo", file: "com.foo-1.2.3-arm64-v8a.apk",
			version: "1.2.3", abi: "arm64-v8a",
		},
		{
			// version 自带 `-` 是最常见的一种，剥后缀必须先于剥前缀。
			name: "version 含连字符", tag: "com.foo", file: "com.foo-1.2.3-beta-universal.apk",
			version: "1.2.3-beta", abi: "universal",
		},
		{
			// 这条是剥后缀必须先于剥前缀的**唯一**理由：先剥前缀会把 `-x86` 留在 version 里吗？
			// 不会 —— 但先剥前缀会让 version 变成 `1.0`、abi 变成 `x86`（把 ABI 当成了 version 的一部分）。
			name: "version 以 ABI 名结尾", tag: "com.foo", file: "com.foo-1.0-x86-arm64-v8a.apk",
			version: "1.0-x86", abi: "arm64-v8a",
		},
		{
			name: "点分 appId 不干扰", tag: "dev.imranr.obtainium",
			file:    "dev.imranr.obtainium-1.6.15-universal.apk",
			version: "1.6.15", abi: "universal",
		},
		{
			// 固定集里 x86 与 x86_64 互不为尾缀，不会误配。
			name: "x86_64 与 x86 不歧义", tag: "a", file: "a-1-x86_64.apk",
			version: "1", abi: "x86_64",
		},
		{name: "ABI 写错", tag: "com.foo", file: "com.foo-1.0-arm64.apk", wantErr: true},
		{name: "ABI 缺失", tag: "com.foo", file: "com.foo-1.0.apk", wantErr: true},
		{name: "前缀不符", tag: "com.bar", file: "com.foo-1.0-universal.apk", wantErr: true},
		{name: "version 段为空", tag: "com.foo", file: "com.foo--universal.apk", wantErr: true},
		{name: "无 apk 后缀", tag: "com.foo", file: "com.foo-1.0-universal.zip", wantErr: true},
		{name: "空文件名", tag: "com.foo", file: "", wantErr: true},
		{
			// tag 本身含 `-`（真实包名不允许，但解析器不该因此崩）。
			name: "tag 含连字符", tag: "a-b", file: "a-b-1.0-universal.apk",
			version: "1.0", abi: "universal",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			version, abi, err := Split(tt.tag, tt.file)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("期望报错，得到 version=%q abi=%q", version, abi)
				}
				return
			}
			if err != nil {
				t.Fatalf("不该报错：%v", err)
			}
			if version != tt.version || abi != tt.abi {
				t.Errorf("得到 (%q, %q)，期望 (%q, %q)", version, abi, tt.version, tt.abi)
			}
		})
	}
}

// TestAssetNameSplitRoundTrip 钉住渲染与解析互逆 —— 这是契约的闭环。
func TestAssetNameSplitRoundTrip(t *testing.T) {
	cases := []struct{ appID, version, abi string }{
		{"com.foo", "1.2.3", "universal"},
		{"dev.imranr.obtainium", "1.6.15", "arm64-v8a"},
		{"com.obtainium.companion", "1.0.0", "x86_64"},
		{"a", "1.0-x86", "x86"}, // version 以 ABI 名结尾
		{"a", "2026.09.11-beta+build.7", "armeabi-v7a"},
	}
	for _, c := range cases {
		file := AssetName(c.appID, c.version, c.abi)
		version, abi, err := Split(c.appID, file)
		if err != nil {
			t.Fatalf("AssetName(%q,%q,%q) = %q 无法解析回来：%v", c.appID, c.version, c.abi, file, err)
		}
		if version != c.version || abi != c.abi {
			t.Errorf("%q 解析回 (%q,%q)，期望 (%q,%q)", file, version, abi, c.version, c.abi)
		}
	}
}

func TestSanitizeVersion(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "1.2.3", want: "1.2.3"},
		{in: "1.0 beta", want: "1.0-beta"},
		{in: "1.0  beta", want: "1.0-beta"}, // 连续不安全字符塌缩成一个
		{in: "v1.0/alpha", want: "v1.0-alpha"},
		{in: "1.0+build.5", want: "1.0+build.5"}, // `+` 是安全字符，保持原样
		{in: "1.0_beta~rc1", want: "1.0_beta~rc1"},
		{in: "  1.0  ", want: "1.0"}, // 首尾被裁
		{in: "1.0-", want: "1.0"},
		{in: "中文版本", wantErr: true}, // 全被替换 → 裁空
		{in: "", wantErr: true},
		{in: "///", wantErr: true},
	}
	for _, tt := range tests {
		got, err := SanitizeVersion(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("SanitizeVersion(%q) 期望报错，得到 %q", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("SanitizeVersion(%q) 不该报错：%v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("SanitizeVersion(%q) = %q，期望 %q", tt.in, got, tt.want)
		}
	}
}

// TestSanitizeVersionKeepsSplitWorking 钉住清洗的**目的**：清洗后的 token 必须仍能往返，
// 否则"清洗"本身就把契约破坏了。
func TestSanitizeVersionKeepsSplitWorking(t *testing.T) {
	for _, raw := range []string{"1.0 beta", "v1.0/alpha", "  2.0  ", "1.0-x86"} {
		token, err := SanitizeVersion(raw)
		if err != nil {
			t.Fatalf("SanitizeVersion(%q)：%v", raw, err)
		}
		file := AssetName("com.foo", token, "universal")
		version, abi, err := Split("com.foo", file)
		if err != nil {
			t.Fatalf("清洗后 %q → %q 无法解析：%v", raw, file, err)
		}
		if version != token || abi != "universal" {
			t.Errorf("清洗后 %q → %q 解析回 (%q,%q)", raw, file, version, abi)
		}
	}
}

func TestSortABIs(t *testing.T) {
	tests := []struct {
		in   []string
		want []string
	}{
		{
			in:   []string{"x86", "universal", "arm64-v8a"},
			want: []string{"universal", "arm64-v8a", "x86"},
		},
		{
			// 去重，且契约顺序压过输入顺序
			in:   []string{"x86", "arm64-v8a", "x86", "universal", "armeabi-v7a", "x86_64"},
			want: []string{"universal", "arm64-v8a", "armeabi-v7a", "x86_64", "x86"},
		},
		{
			// 未知 token 排在已知之后，而不是被丢掉
			in:   []string{"mips", "universal"},
			want: []string{"universal", "mips"},
		},
		{in: nil, want: []string{}},
	}
	for _, tt := range tests {
		got := SortABIs(tt.in)
		if got == nil {
			got = []string{}
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("SortABIs(%v) = %v，期望 %v", tt.in, got, tt.want)
		}
	}
}

func TestIsABI(t *testing.T) {
	for _, a := range ABISet {
		if !IsABI(a) {
			t.Errorf("固定集里的 %q 居然 IsABI=false", a)
		}
	}
	for _, a := range []string{"mips", "arm64", "ARM64-V8A", "", "universal "} {
		if IsABI(a) {
			t.Errorf("%q 不该被判为固定集内的 ABI", a)
		}
	}
}
