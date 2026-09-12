package job

import (
	"testing"

	"github.com/market-of-labs/forge-core/internal/apkmeta"
	"github.com/market-of-labs/forge-core/internal/model"
	"github.com/market-of-labs/forge-core/internal/store"
)

// 这一组钉的是"没有家的 APK 当场建条目"那一步（03 §3.2）。
//
// 手动上传的入口就是把 APK 传进 `_incoming`，所以条目的来源只有这一个函数 ——
// 而它建出来的东西要**立刻**能被后面几步用上（安置、写账本、重建清单），中间任何一环
// 对不上都表现为"传了却没反应"。真跑 IntakeIncoming 要联网下载 APK，所以这里只测
// 这一段：它恰好是整条链上唯一不碰网络的那一步。

// newCtxOnTempRepo 造一个 store 工作副本在临时目录里的 Ctx。
// 不需要建目录：WriteJSON 自己会建（sources/ 不存在时 LoadSources 返回空，不报错）。
func newCtxOnTempRepo(t *testing.T) *Ctx {
	t.Helper()
	return &Ctx{Repo: store.Repo{Root: t.TempDir()}, Log: func(string, ...any) {}}
}

// 新建的条目必须是一份**完整、合法、可加载**的来源文件，而不是一个半成品：
// appId 取包名、显示名取 label、作者先记占位值（`author` 空着会让整份清单判失败，
// 02 规则 2 是 Errorf）。
func TestCreateManualSource_FromAPK(t *testing.T) {
	c := newCtxOnTempRepo(t)

	src, err := c.createManualSource(&apkmeta.Meta{
		Package:     "com.example.closed",
		Label:       "自研应用",
		VersionName: "1.0",
		ABIs:        []string{"arm64-v8a"},
	})
	if err != nil {
		t.Fatalf("建条目失败了：%v", err)
	}

	if src.ID != "com.example.closed" || src.Name != "自研应用" {
		t.Errorf("包名/显示名该按 APK 内容落下来：%+v", src)
	}
	if src.Author != model.AuthorUnknown {
		t.Errorf("作者该是占位值 %q（APK 里没有作者，之后用 change-source.yml 补）：%q",
			model.AuthorUnknown, src.Author)
	}
	if src.Source != model.SourceManual || src.Upstream != nil {
		t.Errorf("手动来源不能有上游：source=%q upstream=%+v", src.Source, src.Upstream)
	}

	// **必须并进内存里的 c.Sources**：同一条链的下一步（写账本）就是靠 c.Source(id)
	// 找这条来源的，找不到会静默跳过 —— 表现是"文件在、账本空"。
	if c.Source("com.example.closed") == nil {
		t.Fatal("新建的条目没并进内存工作副本，后面写账本会找不到它")
	}

	// 磁盘上那份必须能被 store 原样加载回来（校验、JSON 形状、文件名都对了）。
	got, err := c.Repo.LoadSources()
	if err != nil {
		t.Fatalf("读回 sources/：%v", err)
	}
	if len(got) != 1 || got[0].ID != "com.example.closed" || got[0].Author != model.AuthorUnknown {
		t.Fatalf("落盘的那份读回来不对：%+v", got)
	}
}

// 读不出 label 时显示名退到包名 —— 空显示名在客户端里是一行没有标题的条目。
// 这不是"猜"，而是把一个必然能显示的值放上去，好不好看是之后用变更单的事。
func TestCreateManualSource_FallsBackToPackageWhenNoLabel(t *testing.T) {
	c := newCtxOnTempRepo(t)

	src, err := c.createManualSource(&apkmeta.Meta{Package: "com.example.nolabel", Label: ""})
	if err != nil {
		t.Fatalf("建条目失败了：%v", err)
	}
	if src.Name != "com.example.nolabel" {
		t.Errorf("读不出 label 时显示名该退到包名，得到 %q", src.Name)
	}
}

// 包名不能当 appId 时（保留名 `_incoming` 是最要紧的一例：那是暂存 Release 的 tag）
// 必须**报错让调用方保留这个 asset**，而不是写出一份会污染 sources/ 的文件。
func TestCreateManualSource_RejectsReservedAppID(t *testing.T) {
	c := newCtxOnTempRepo(t)

	if _, err := c.createManualSource(&apkmeta.Meta{Package: model.IncomingTag, Label: "x"}); err == nil {
		t.Fatal("保留名当 appId 该报错")
	}
	if got, err := c.Repo.LoadSources(); err != nil || len(got) != 0 {
		t.Errorf("失败了却写了文件：%+v（%v）", got, err)
	}
}
