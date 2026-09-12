package store_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/market-of-labs/forge-core/internal/manifest"
	"github.com/market-of-labs/forge-core/internal/model"
	"github.com/market-of-labs/forge-core/internal/store"
)

// storeRoot 定位同级的 store 仓库（`market-of-labs/store`）。
//
// 测试的工作目录是**包目录**（`forge/internal/store`），所以往上三级到 `market-of-labs`。
// 找不到就跳过而不是失败：forge 是可以单独 clone 的公有仓库，应当能在没有 store 的
// 情况下跑通自己的单测。
func storeRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join("..", "..", "..", "store")
	if _, err := os.Stat(filepath.Join(root, "apps.json")); err != nil {
		t.Skipf("同级没有 store 仓库（%s）——跳过黄金测试", root)
	}
	return root
}

// TestGoldenRealRepo 拿**仓库里真实存在的那几个文件**跑一遍完整链路。
//
// 这是本模块最重要的一条测试：它把实现钉在**已经落地的产物**上，而不是钉在
// 我对着规范写出来的理解上。规范里任何一处措辞被我读错，这条测试都会红。
func TestGoldenRealRepo(t *testing.T) {
	repo := store.Repo{Root: storeRoot(t)}

	ep, err := repo.LoadEndpoints()
	if err != nil {
		t.Fatalf("读 endpoints.json：%v", err)
	}
	srcs, err := repo.LoadSources()
	if err != nil {
		t.Fatalf("读 sources/：%v", err)
	}
	if len(srcs) == 0 {
		t.Fatal("sources/ 里一个条目都没有 —— 那这份仓库就没有任何输入了")
	}

	want, err := repo.LoadManifest()
	if err != nil {
		t.Fatalf("读 apps.json：%v", err)
	}

	// ① 现存的 apps.json 必须通过 02 §2.8 自检。这是"文件是对的"的基线。
	if rep := want.Validate(ep); rep.HasErrors() {
		for _, p := range rep.Problems {
			t.Errorf("现存 apps.json 校验：%s", p)
		}
		t.Fatal("现存的 apps.json 没通过自检 —— 在改代码之前先看是不是数据本身有问题")
	}

	// ② sources 集合必须自洽（id 唯一、companion 至多一条）。
	if rep := model.CheckSourceSet(srcs); rep.HasErrors() {
		for _, p := range rep.Problems {
			t.Errorf("sources 集合校验：%s", p)
		}
	}

	// ③ 从两个输入**重建**清单，逐条与现存文件比对 —— sources 自带的版本账本
	// 就是第三个输入，合并之后（D48）不再有独立文件。
	//
	// 这条断言的含义是："规范 §5.1 描述的合成规则，能复原出仓库里那份清单"。
	// 能复原 = 我对 rules 的理解与产出它的那次人工操作一致。
	got, rep, err := manifest.Build(manifest.Input{
		Sources:   srcs,
		Endpoints: ep,
	})
	if err != nil {
		t.Fatalf("Build：%v", err)
	}
	for _, p := range rep.Problems {
		t.Logf("Build 告警：%s", p)
	}

	if len(got.Apps) != len(want.Apps) {
		t.Fatalf("重建出 %d 条，现存 %d 条", len(got.Apps), len(want.Apps))
	}

	wantByID := make(map[string]model.Entry, len(want.Apps))
	for _, e := range want.Apps {
		wantByID[e.ID] = e
	}
	for i := range got.Apps {
		g := got.Apps[i]
		w, ok := wantByID[g.ID]
		if !ok {
			t.Errorf("重建出了现存清单里没有的条目 %q", g.ID)
			continue
		}
		compareEntries(t, g, w)
	}

	// ④ 重建的清单本身也要过自检 —— 否则 ③ 只是在"复原一份坏数据"。
	if rep := got.Validate(ep); rep.HasErrors() {
		for _, p := range rep.Problems {
			t.Errorf("重建清单自检：%s", p)
		}
	}

	// ⑤ 信封级字段。
	if got.SchemaVersion != model.SchemaVersion {
		t.Errorf("schemaVersion = %d，期望 %d", got.SchemaVersion, model.SchemaVersion)
	}
	if got.ExportedAt == "" {
		t.Error("exportedAt 为空")
	}
}

// compareEntries 逐字段比对（按 id 配对后的）两条条目。
//
// **不比 ReleaseDate**：现存 apps.json 里的 releaseDate 是手写种子值，而账本里
// 没有 publishedAt（那份种子账本也是手写的），所以重建不出来。这不是 bug 而是
// "账本缺一个可选字段"，跑一次 `build-index`（必要时带 `-fetch-missing`）
// 从 Release 的 published_at 回填后就能对上。这条差别单独在 TestGoldenReleaseDateGap 里说明。
func compareEntries(t *testing.T, got, want model.Entry) {
	t.Helper()
	type field struct {
		name      string
		got, want string
	}
	fields := []field{
		{"name", got.Name, want.Name},
		{"author", got.Author, want.Author},
		{"url", got.URL, want.URL},
		{"overrideSource", got.OverrideSource, want.OverrideSource},
		{"latestVersion", got.LatestVersion, want.LatestVersion},
		{"apkUrls", got.APKUrls, want.APKUrls},
		{"otherAssetUrls", got.OtherAssetUrls, want.OtherAssetUrls},
		{"additionalSettings", got.AdditionalSettings, want.AdditionalSettings},
		{"kind", got.Kind, want.Kind},
	}
	for _, f := range fields {
		if f.got != f.want {
			t.Errorf("[%s] %s：重建 = %q，现存 = %q", got.ID, f.name, f.got, f.want)
		}
	}
	if got.PreferredAPKIdx != want.PreferredAPKIdx {
		t.Errorf("[%s] preferredApkIndex：重建 = %d，现存 = %d", got.ID, got.PreferredAPKIdx, want.PreferredAPKIdx)
	}
	if len(got.Categories) != len(want.Categories) {
		t.Errorf("[%s] categories：重建 = %v，现存 = %v", got.ID, got.Categories, want.Categories)
	}
}

// TestGoldenReleaseDateGap 把上面那条"不比 ReleaseDate"显式记录下来。
//
// 存在的意义是**不让一个已知的差异变成沉默的差异** —— 若哪天账本里补上了
// publishedAt，这条测试会失败，提示可以把 releaseDate 也纳入比对。
func TestGoldenReleaseDateGap(t *testing.T) {
	repo := store.Repo{Root: storeRoot(t)}
	srcs, err := repo.LoadSources()
	if err != nil {
		t.Fatalf("读 sources/：%v", err)
	}

	total, missing := 0, 0
	for i := range srcs {
		for _, v := range srcs[i].Versions {
			total++
			if v.PublishedAt == "" {
				missing++
			}
		}
	}
	if total == 0 {
		t.Fatal("sources/ 里一个版本都没有 —— 账本空着，这条测试就什么都证明不了")
	}
	if missing == 0 {
		t.Errorf("账本里每个版本都有 publishedAt 了 —— "+
			"可以把 releaseDate 加回 compareEntries 的比对字段（共 %d 个版本）", total)
	} else {
		t.Logf("账本里有 %d/%d 个版本缺 publishedAt，releaseDate 因此无法重建；"+
			"跑 build-index 可从 Release 的 published_at 回填", missing, total)
	}
}
