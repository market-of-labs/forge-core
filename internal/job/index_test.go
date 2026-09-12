package job

import (
	"context"
	"testing"

	"github.com/market-of-labs/forge-core/internal/gh"
	"github.com/market-of-labs/forge-core/internal/model"
)

// TestBuildLedgerKeepsReleaseNote 钉住"重建账本不许把更新说明弄丢"。
//
// 账本是**每次对账整段重写**的，而 releaseNote 只在上游的 Release 里（我们自己的
// Release 正文放的是 README）—— 忘了从旧账本继承，症状是"每轮对账之后更新说明就没了"，
// 而且是静默的：清单照样合法、check-manifest 照样过。同一个坑 publishedAt 踩过一次。
func TestBuildLedgerKeepsReleaseNote(t *testing.T) {
	const note = "修了三个 bug，另加 arm64 分片"
	src := &model.Source{
		ID:     "com.x",
		Source: model.SourceGitHub,
		Versions: []model.Version{{
			Version:     "1.0",
			VersionName: "1.0",
			VersionCode: 7,
			PublishedAt: "2026-01-01T00:00:00Z",
			UpstreamTag: "v1.0",
			ReleaseNote: note,
		}},
	}
	groups := []assetGroup{{
		version: "1.0",
		abi:     "arm64-v8a",
		asset:   gh.Asset{Name: "com.x-1.0-arm64-v8a.apk", Size: 10, CreatedAt: "2026-01-01T00:00:00Z"},
	}}

	got, _, err := buildLedger(context.Background(), &Ctx{Log: func(string, ...any) {}}, src, groups, BuildIndexOptions{})
	if err != nil {
		t.Fatalf("buildLedger：%v", err)
	}
	if len(got) != 1 {
		t.Fatalf("版本数 = %d，期望 1：%+v", len(got), got)
	}
	if got[0].ReleaseNote != note {
		t.Fatalf("重建账本把 releaseNote 丢了：%q（期望 %q）", got[0].ReleaseNote, note)
	}
}
