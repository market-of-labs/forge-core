package job

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/market-of-labs/forge-core/internal/apkmeta"
	"github.com/market-of-labs/forge-core/internal/gh"
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

// 队列**从哪条路出去都必须留在 draft** —— 一次漏掉就是一个静默死锁：
// 发布过的 Release 在网页上只有 Update / Delete、没有 Publish 按钮，而往它上传
// APK 不触发任何事件（03 §3.3），于是"传了文件却没反应"，用户也没有按钮能把它
// 重新武装起来（2026-09-12 实际撞上过一次）。
//
// 这里顺带钉住**队列是怎么被找到的**：它常态就是 draft，而 draft **取不到 tag**
// （`/releases/tags/{tag}` 的官方描述是 "Get a published release"，draft 一律
// 404，GitHub 把它挂在 untagged-* 引用下）。所以 fixture 是 draft:true 且接口是
// 列表 —— by-tag 那条路在这里会直接找不到队列，测试就红了。
//
// 这条链上"没搬成"比"搬成了"更常见（上传的是 APK，随手传错是常态），所以下面两个
// 用例都是**什么都没搬成**的那种出口 —— 它们曾经一个复位都没有。
func TestIntakeIncoming_RearmsQueueEvenWhenNothingMoved(t *testing.T) {
	cases := []struct {
		name string
		// ListAssets 的返回。空 = 重复 Publish 最常见的样子。
		assets []map[string]any
	}{
		{name: "空队列", assets: nil},
		{name: "有 asset 但读不出 APK（全被保留）", assets: []map[string]any{
			{"id": 9, "name": "随手传的.zip", "size": len(emptyZip)},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var patched []map[string]any
			var deletes int

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/store/releases":
					// 队列在列表里以 **draft** 出现 —— 这是常态（§3.2：队列不经过发布）。
					// 旁边放一个别的 Release，钉住筛选是"按 tag 挑"而不是"拿第一个"。
					json.NewEncoder(w).Encode([]map[string]any{
						{"id": 1, "tag_name": "com.example.other", "draft": false},
						{"id": 700, "tag_name": model.IncomingTag, "draft": true},
					})
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/store/releases/700/assets":
					json.NewEncoder(w).Encode(tc.assets)
				case r.Method == http.MethodPatch:
					var p map[string]any
					json.NewDecoder(r.Body).Decode(&p)
					patched = append(patched, p)
					json.NewEncoder(w).Encode(map[string]any{"id": 700, "tag_name": model.IncomingTag})
				case r.Method == http.MethodDelete:
					deletes++
					w.WriteHeader(http.StatusNoContent)
				default: // asset 下载：给一段不是 APK 的内容，读元数据必失败
					w.Write(emptyZip)
				}
			}))
			defer srv.Close()

			ghc, err := gh.New(gh.Config{Token: "t", BaseURL: srv.URL, UploadBaseURL: srv.URL})
			if err != nil {
				t.Fatalf("建客户端：%v", err)
			}
			c := &Ctx{Env: &Env{Token: "t", StoreRepo: "o/store"}, GH: ghc, Log: func(string, ...any) {}}

			res, err := IntakeIncoming(context.Background(), c)
			if err != nil {
				t.Fatalf("IntakeIncoming：%v", err)
			}
			if len(res.Moved) != 0 {
				t.Fatalf("这个用例不该搬成任何东西：%v", res.Moved)
			}

			// 一处 PATCH，且是 draft:true —— 队列被重新武装起来。
			if len(patched) != 1 || patched[0]["draft"] != true {
				t.Fatalf("队列必须被改回 draft，实际 PATCH 了 %d 次：%v", len(patched), patched)
			}
			if !res.Cleaned {
				t.Error("复位成功了，res.Cleaned 该是 true")
			}
			// 什么都没搬成 ⇒ 一个 asset 都不许删（§3.2：不安置就不丢）。
			if deletes != 0 {
				t.Errorf("没搬成的 asset 被删了 %d 个", deletes)
			}
		})
	}
}
