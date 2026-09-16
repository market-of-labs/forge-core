package job

import "testing"

const testRepo = "SomeOwner/SomeApp"

func TestRelinkReadme(t *testing.T) {
	raw := "https://raw.githubusercontent.com/" + testRepo + "/HEAD/"
	blob := "https://github.com/" + testRepo + "/blob/HEAD/"

	cases := []struct {
		name string
		in   string
		want string
	}{
		// ---- 该改的 ----
		{"markdown 图片 ./ 前缀", `![图](./img/x.png)`, `![图](` + raw + `img/x.png)`},
		{"markdown 图片裸路径", `![](assets/graphics/icon.svg)`, `![](` + raw + `assets/graphics/icon.svg)`},
		{"markdown 链接", `[看这里](docs/CONTRIBUTING.md)`, `[看这里](` + blob + `docs/CONTRIBUTING.md)`},
		{"markdown 链接保留 title", `[x](./a.md "标题")`, `[x](` + blob + `a.md "标题")`},
		{"根相对路径当作仓库根", `![](/assets/icons/Discord.png)`, `![](` + raw + `assets/icons/Discord.png)`},
		{"归一化 ./ 与 ../", `![](./img/../img/x.png)`, `![](` + raw + `img/x.png)`},
		{"HTML src 是图片", `<img src="./screenshots/1.png">`, `<img src="` + raw + `screenshots/1.png">`},
		{"HTML href 是链接", `<a href="docs/HOWITWORKS.md">说明</a>`, `<a href="` + blob + `docs/HOWITWORKS.md">说明</a>`},
		{"HTML 单引号", `<img src='./img/x.png'>`, `<img src='` + raw + `img/x.png'>`},
		{"同一行两种目标各走各的", `[a](docs/a.md) <img src="b.png">`,
			`[a](` + blob + `docs/a.md) <img src="` + raw + `b.png">`},

		// ---- 一个字节都不该动的 ----
		{"已是绝对地址", `![x](https://example.com/a.png)`, `![x](https://example.com/a.png)`},
		{"协议相对", `![x](//cdn.example.com/a.png)`, `![x](//cdn.example.com/a.png)`},
		{"纯锚点", `[跳到下面](#section)`, `[跳到下面](#section)`},
		{"mailto", `[写信](mailto:a@b.c)`, `[写信](mailto:a@b.c)`},
		{"HTML 绝对地址", `<img src="https://img.shields.io/badge/x.svg">`, `<img src="https://img.shields.io/badge/x.svg">`},
		{"空目标", `[空]()`, `[空]()`},
		{"没有链接的正文", "纯文字\n\n- 列表\n", "纯文字\n\n- 列表\n"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := relinkReadme(c.in, testRepo)
			if got != c.want {
				t.Errorf("relinkReadme(%q)\n 得到 %q\n 想要 %q", c.in, got, c.want)
			}
			// 幂等是硬要求：正文渲染结果已写进 Release，下一轮对账拿改写过的正文
			// 跟上游原文比 —— 不幂等就是每轮一次无谓 PATCH（或更糟：正文每轮在变，
			// 于是"相同就不 PATCH"永远不命中）。
			if again := relinkReadme(got, testRepo); again != got {
				t.Errorf("不幂等：\n 第一次 %q\n 第二次 %q", got, again)
			}
		})
	}
}

// TestRelinkReadmeNestedBadge 钉住"先图片后链接"的顺序。
//
// `[![build](badge.svg)](https://ci)` 这种 badge 包裹写法里，链接那条正则会把
// `![build](badge.svg` 整段当成链接文字（`[^\]]*` 允许其中出现 `[`）—— 顺序反了
// 里面的相对图片就漏改，而外面那条本来就该不动。
func TestRelinkReadmeNestedBadge(t *testing.T) {
	in := `[![build](./badge.svg)](https://ci.example.com/run)`
	want := `[![build](https://raw.githubusercontent.com/` + testRepo + `/HEAD/badge.svg)](https://ci.example.com/run)`
	if got := relinkReadme(in, testRepo); got != want {
		t.Errorf("嵌套 badge\n 得到 %q\n 想要 %q", got, want)
	}
}

// TestRelinkReadmeEmpty 钉住"没有 README 就原样返回"：gh.Readme 对没有 README 的
// 仓库返回空串（那是正常状态），空串必须一路静默传下去，不能在拼接里变成一串 URL。
func TestRelinkReadmeEmpty(t *testing.T) {
	if got := relinkReadme("", testRepo); got != "" {
		t.Errorf("空正文应该原样返回，得到 %q", got)
	}
}
