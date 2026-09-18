package job

import "testing"

// TestLegacyIndexPath 钉住"哪些产物不拷进 repo/"这条判据。
//
// 这个测试的重点**不是**被丢掉的那些（丢错了顶多是某个老客户端用不了，而它本来就
// 不在支持范围内），而是**不该丢的那些** —— 尤其 `index-v2.json` 与 `entry.jar`：
// 它们是唯一被支持的索引协议，少一个这个源就整个不可用，而且**不报错**（客户端拿到
// 404，我们这边日志全绿）。所以下面"保留"那一组是逐字列出来的，不是顺手补的。
func TestLegacyIndexPath(t *testing.T) {
	cases := []struct {
		path string
		drop bool
		why  string
	}{
		// ---- 保留：v2 那一套 + 给人看的页面 ----
		{"index-v2.json", false, "唯一支持的索引，前缀只差一个数字，最容易误伤"},
		{"entry.jar", false, "v2 的入口 jar，客户端先下它再下 index-v2.json"},
		{"entry.json", false, "entry.jar 里那四项之一"},
		{"index.html", false, "给人看的页面；D59 的『不裁剪』对它仍然成立"},
		{"index.css", false, "同上"},
		{"index.png", false, "同上"},
		{"index-v2.json.asc", false, "v2 索引的签名，与 index-v1 无关"},
		{"icons/icon.png", false, "图标是给人看的，且不在根目录"},
		{"category/工具.png", false, "分类图标，不在根目录"},
		{"index-v1.d/whatever", false,
			"根目录限制：名字以 index-v1 开头的是**目录**时不能把整棵子树砍掉"},

		// ---- 丢掉：v0 ----
		{"index.xml", true, "v0 的 XML 索引"},
		{"index.jar", true, "v0 的签名索引"},
		{"index_unsigned.jar", true, "v0 的未签名索引"},

		// ---- 丢掉：v1 ----
		{"index-v1.jar", true, "v1 的签名索引"},
		{"index-v1.json", true, "v1 的 JSON 索引"},
		{"index-v1.jar.asc", true, "v1 的签名文件，前缀认法顺带覆盖"},
		{"index-v1.json.asc", true, "同上"},
		{"index-v1-whatever", true, "前缀认法的本意：fdroidserver 往这一格里加什么都挡得住"},
	}

	for _, c := range cases {
		got := legacyIndexPath(c.path)
		if got == c.drop {
			continue
		}
		want, act := "保留", "丢掉"
		if c.drop {
			want, act = "丢掉", "保留"
		}
		t.Errorf("legacyIndexPath(%q) = %v：应该%s，却%s了 —— %s",
			c.path, got, want, act, c.why)
	}
}
