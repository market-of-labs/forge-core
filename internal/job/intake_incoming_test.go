package job

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

// fakeStore 是一台只实现搬运链所需端点的假 store：列 Release、列 asset、PATCH
// （复位）、DELETE（删 asset 或删 tag 引用），其余一律当作 asset 下载返回一段非 APK 内容。
//
// 它留的计数器全是"某个写动作发生了没有"：PATCH 了什么、删了几个 **asset**、删了哪些
// **tag 引用**，因为这一组要钉的恰好是**这几件事各自发生了没有、以及一个失败了另一个
// 还做不做**。
//
// ⚠️ 两种 DELETE 必须分开记。它们从前是同一种（那时只有删 asset），于是"清场顺手删掉
// `refs/tags/_incoming`"会以"多删了一个 asset"的样子出现在 `deletes` 里，把三条断言
// 一起带偏 —— 2026-09-16 就是这么红的。端点前缀不同，照实分开。
type fakeStore struct {
	*httptest.Server
	assets    []map[string]any
	patchErr  bool   // PATCH 一律 500：模拟"队列掰不回 draft"
	assetsErr bool   // 列 asset 一律 500：用来制造一个"主路径自己就带错"的出口
	noQueue   bool   // 列表里没有 `_incoming`：模拟队列被降级成 untagged-* / 压根没建
	patchTag  string // 强制 PATCH 回应里的 tag_name：模拟"GitHub 没按我们发的 tag 名办"

	patched    []map[string]any
	deletes    int      // 删掉的 asset 个数（/releases/assets/{id}）
	refDeletes []string // 删掉的 tag 引用，记的是请求路径（/git/refs/tags/{tag}）
	logs       []string
}

// newFakeStore 起一台假 store。assets 是队列里的内容；patchErr 见上。
func newFakeStore(t *testing.T, assets []map[string]any, patchErr bool) *fakeStore {
	t.Helper()
	f := &fakeStore{assets: assets, patchErr: patchErr}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/store/releases":
			// 队列在列表里以 **draft** 出现 —— 这是常态（§3.2：队列不经过发布）。
			// 旁边放一个别的 Release，钉住筛选是"按 tag 挑"而不是"拿第一个"。
			rels := []map[string]any{
				{"id": 1, "tag_name": "com.example.other", "draft": false},
				{"id": 700, "tag_name": model.IncomingTag, "draft": true},
			}
			if f.noQueue {
				// 降级之后的真实样子：队列还在、还是 draft，但 tag_name 已经是 untagged-*，
				// 于是"按 tag 挑"这件事必然挑不到它。这时**只留那个不相干的 Release**，
				// 才逼得出"认不出来"那条出口。
				rels = rels[:1]
			}
			json.NewEncoder(w).Encode(rels)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/store/releases/700/assets":
			if f.assetsErr {
				w.WriteHeader(http.StatusInternalServerError)
				json.NewEncoder(w).Encode(map[string]any{"message": "boom"})
				return
			}
			json.NewEncoder(w).Encode(f.assets)
		case r.Method == http.MethodPatch:
			var p map[string]any
			json.NewDecoder(r.Body).Decode(&p)
			f.patched = append(f.patched, p)
			if f.patchErr {
				w.WriteHeader(http.StatusInternalServerError)
				json.NewEncoder(w).Encode(map[string]any{"message": "boom"})
				return
			}
			// **照实模拟**：PATCH 一个 draft 时若不带 tag_name，GitHub 会把 tag 名换成
			// `untagged-<sha>` 占位名 —— 与它原本叫什么无关，**哪怕它本来就已是 draft**
			// （2026-09-16 在真队列上实测，PATCH 前后各做一次独立 GET 复核）。
			//
			// 这个 fake 从前一律回 `_incoming`，等于替真实 API 打了保票，于是"每次复位都会
			// 把队列打废"这件事被藏住了整整一天 —— 用例全绿，线上每次都坏。所以这里必须
			// 按实测行为来，而不是按我们希望的来。
			tag, _ := p["tag_name"].(string)
			if f.patchTag != "" {
				tag = f.patchTag // 显式覆盖：模拟 GitHub 收了 tag_name 却不照办
			} else if tag == "" {
				tag = "untagged-9d3d2b2b06d800c8cc0f"
			}
			json.NewEncoder(w).Encode(map[string]any{"id": 700, "tag_name": tag})
		case r.Method == http.MethodDelete:
			// 删 asset 还是删 tag 引用，按端点分（见 fakeStore 的说明）。
			if strings.Contains(r.URL.Path, "/releases/assets/") {
				f.deletes++
			} else {
				f.refDeletes = append(f.refDeletes, r.URL.Path)
			}
			w.WriteHeader(http.StatusNoContent)
		default: // asset 下载：给一段不是 APK 的内容，读元数据必失败
			w.Write(emptyZip)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

// deletedIncomingRef 报出"队列的 tag 引用被删了几次"。
//
// 连端点一起钉，不只数个数：`repoURL` 会对每一段做 PathEscape，把 "tags/_incoming" 整段
// 塞进去会把那个斜杠转义成 `%2F`，那时这个 DELETE 打在一个 GitHub 不认识的端点上 ——
// 而这里的计数会照记，测试照绿。所以按 "/git/refs/tags/_incoming" 认。
func (f *fakeStore) deletedIncomingRef() int {
	n := 0
	for _, p := range f.refDeletes {
		if strings.HasSuffix(p, "/git/refs/tags/"+model.IncomingTag) {
			n++
		}
	}
	return n
}

// ctx 造一个打在这台假 store 上的 Ctx。
func (f *fakeStore) ctx(t *testing.T) *Ctx {
	t.Helper()
	ghc, err := gh.New(gh.Config{Token: "t", BaseURL: f.URL, UploadBaseURL: f.URL})
	if err != nil {
		t.Fatalf("建客户端：%v", err)
	}
	// 日志收进 f.logs：这一组里有两条断言要读它 —— 降级告警该在、正常路径不该在。
	return &Ctx{Env: &Env{Token: "t", StoreRepo: "o/store"}, GH: ghc, Log: func(format string, args ...any) {
		f.logs = append(f.logs, fmt.Sprintf(format, args...))
	}}
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
			f := newFakeStore(t, tc.assets, false)
			c := f.ctx(t)

			res, err := IntakeIncoming(context.Background(), c)
			if err != nil {
				t.Fatalf("IntakeIncoming：%v", err)
			}
			if len(res.Moved) != 0 {
				t.Fatalf("这个用例不该搬成任何东西：%v", res.Moved)
			}

			// 一处 PATCH，且是 draft:true —— 队列被重新武装起来。
			if len(f.patched) != 1 || f.patched[0]["draft"] != true {
				t.Fatalf("队列必须被改回 draft，实际 PATCH 了 %d 次：%v", len(f.patched), f.patched)
			}
			// make_latest=false（规则 4）。它只有走 gh.Unpublish 才带得上 —— 这里手写过
			// 一遍 UpdateRelease 时漏的就是它，所以这条断言同时钉住"别再手写"。
			if f.patched[0]["make_latest"] != "false" {
				t.Errorf("复位必须带 make_latest=false（规则 4），实际：%v", f.patched[0])
			}
			if !res.Cleaned {
				t.Error("复位成功了，res.Cleaned 该是 true")
			}
			// 什么都没搬成 ⇒ 一个 asset 都不许删（§3.2：不安置就不丢）。
			if f.deletes != 0 {
				t.Errorf("没搬成的 asset 被删了 %d 个", f.deletes)
			}
			// 复位成功 ⇒ 顺手删掉队列的 tag 引用（D57）。与搬没搬成无关：这个引用是
			// `release` 事件的解析依据，而它一旦建立就不再移动 —— 留着它，"Publish 即
			// 发车"那条路会一直解析到一份旧的 forward.yml（症状：Publish 了却没反应，
			// 而 Actions 页面干干净净）。删掉它，下一次 Publish 在当时的默认分支 HEAD 上重建。
			if n := f.deletedIncomingRef(); n != 1 {
				t.Errorf("复位成功后该删掉队列的 tag 引用（恰好一次），实际 %d 次，请求：%v",
					n, f.refDeletes)
			}
			// 复位必须**把队列的 tag 名一起发回去**：不带就等于把队列报废（见 cleanIncoming
			// 与 gh.Unpublish 的说明）。这条是本次修复的要害 —— 少了它，fake 会照实测行为
			// 回一个 `untagged-*`，下面的告警断言随之变红。
			if f.patched[0]["tag_name"] != model.IncomingTag {
				t.Errorf("复位必须把队列的 tag 名发回去（否则队列当场报废），实际 PATCH：%v", f.patched[0])
			}
			// 队列本来就在 draft ⇒ 这次 PATCH 什么都不改，tag 名该原样留住、不该报异常。
			if logs := strings.Join(f.logs, "\n"); strings.Contains(logs, "没留住") {
				t.Errorf("正常路径不该报 tag 名没留住：\n%s", logs)
			}
		})
	}
}

// 复位失败和"删掉已搬走的 asset"是**两件事**，前者失败不许短路掉后者。
//
// 从前那条路径是 `PATCH 失败 → return`，于是掰不回 draft 时那些存根一个都不删。
// 它们的正文已经在正式 Release 里了，留着毫无用处，只会让队列越积越脏、下一轮再被
// 幂等闸门挨个挡一遍 —— 2026-09-12 那个已发布的队列真撞上时就是这个后果。
func TestCleanIncoming_StillDeletesWhenRearmFails(t *testing.T) {
	f := newFakeStore(t, nil, true)
	c := f.ctx(t)

	err := c.cleanIncoming(context.Background(),
		&gh.Release{ID: 700, TagName: model.IncomingTag}, []int64{11, 12}, 3)
	if err == nil {
		t.Fatal("掰不回 draft 必须报出去：调用方要据此把这次运行染色（03 §3.2）")
	}
	if len(f.patched) != 1 {
		t.Fatalf("该只 PATCH 一次，实际 %d 次", len(f.patched))
	}
	if f.deletes != 2 {
		t.Errorf("已搬运的 asset 该照删不误（2 个），实际删了 %d 个", f.deletes)
	}
	// ⚠️ 删引用**只在复位成功时**做。复位失败说明队列还是 published 的，那个引用正是
	// 它的标签 —— 这时删掉就成了一个悬空的 published Release（网页上按不动 Publish，
	// 也再也认不出它）。所以这条不是"顺带一提"，是这个位置的约束之一。
	if n := f.deletedIncomingRef(); n != 0 {
		t.Errorf("复位失败时不该动那个引用（队列还是 published，引用就是它的标签），实际删了 %d 次", n)
	}
}

// 清场失败必须把运行**染红**，不能只是一行 ⚠️。
//
// 这条路径的返回值从前是写死的 nil，而 res.Cleaned 只有测试在读 —— 于是"队列没掰回
// draft"在 Actions 里表现为一次绿色运行加一行容易被略过的告警。而它恰恰是唯一
// 需要人立刻接手的失败：published → draft 那条回头路见 gh.Unpublish 的说明，
// 真走不通时规则 6 又禁止删 Release，只剩人工介入。
//
// 用空队列当场景：主路径本来就是"无事可做"，没有任何别的错可以替它背锅。
func TestIntakeIncoming_RedWhenQueueCannotBeRearmed(t *testing.T) {
	f := newFakeStore(t, nil, true)
	c := f.ctx(t)

	res, err := IntakeIncoming(context.Background(), c)
	if err == nil {
		t.Fatal("清场失败必须报出来，否则这次运行是绿的")
	}
	if !strings.Contains(err.Error(), "draft") {
		t.Errorf("错误该点明是哪一步，得到：%v", err)
	}
	if res == nil {
		t.Fatal("res 不该是 nil（清场的日志要用它数保留了几个）")
	}
	if res.Cleaned {
		t.Error("清场失败了，Cleaned 不该是 true")
	}
}

// 有主错时**主错说话**，清场失败不许把它盖掉 —— 真正的原因比"收尾没做完"值钱。
// 这条和上面那条是同一个 defer 的两半，缺哪一半都是隐形的。
//
// 场景要两错并存才测得到那一半：列 asset 失败（主路径带错出去）+ PATCH 失败
// （defer 里的清场也带错）。
func TestIntakeIncoming_PrimaryErrorWinsOverCleanupError(t *testing.T) {
	f := newFakeStore(t, nil, true)
	f.assetsErr = true
	c := f.ctx(t)

	_, err := IntakeIncoming(context.Background(), c)
	if err == nil {
		t.Fatal("列 asset 失败该报错")
	}
	if !strings.Contains(err.Error(), "asset") {
		t.Errorf("主错（列 asset）该原样透出来，实际：%v", err)
	}
	if strings.Contains(err.Error(), "清场") {
		t.Errorf("主错被清场的错盖掉了：%v", err)
	}
	if len(f.patched) != 1 {
		t.Errorf("清场该照常试着复位（PATCH 一次），实际 %d 次", len(f.patched))
	}
}

// 队列认不出来时**必须染红**，而且错误里要给出那条手工出路。
//
// 这是「传了文件却没反应」这个故障的完整外观：人点按钮的前提是"我刚往队列传了文件"，
// 所以认不出队列 ≈ 那次上传白传了。从前这条出口只记一行日志、返回 `nil` 错误 ——
// 于是一次绿色运行加一句没人翻的日志（2026-09-16 实际撞上：队列被发布过一次，
// tag_name 被降级成 `untagged-*`，从此认不出它，点按钮毫无反馈）。
func TestIntakeIncoming_RedWhenQueueMissing(t *testing.T) {
	f := newFakeStore(t, nil, false)
	f.noQueue = true
	c := f.ctx(t)

	res, err := IntakeIncoming(context.Background(), c)
	if err == nil {
		t.Fatal("认不出队列必须报错，否则这次运行的结论是绿色")
	}
	// 错误要能照着做：点明队列的 tag 名，以及"改回 tag 名"这条出路。
	if !strings.Contains(err.Error(), model.IncomingTag) {
		t.Errorf("错误里该点明队列的 tag 名，得到：%v", err)
	}
	if !strings.Contains(err.Error(), "tag 名改回") {
		t.Errorf("错误里该给出那条手工出路，得到：%v", err)
	}
	if res == nil {
		t.Fatal("res 不该是 nil")
	}
	// 队列都没认出来，就不该有任何写动作：既不 PATCH，也不删东西。
	if len(f.patched) != 0 || f.deletes != 0 || len(f.refDeletes) != 0 {
		t.Errorf("认不出队列时一个写动作都不该发：PATCH %d 次、删 asset %d 个、删引用 %d 次（%v）",
			len(f.patched), f.deletes, len(f.refDeletes), f.refDeletes)
	}
}

// tag 名没留住必须**当场**喊出来，而且照常把已搬走的 asset 删干净、不染红。
//
// 复位现在会把 tag 名一起发回去（那就是修法），所以这条告警不再是"我们知道会降级"的
// 提示，而是**哨兵**：GitHub 收了 tag_name 却不照办时，队列下一次就认不出来，而等到
// 那时，能指认原因的现场只剩这个返回值 —— 所以那一下不喊，就再没机会了。
//
// 用 patchTag 模拟"不照办"（fake 的默认行为是照办）。
func TestCleanIncoming_WarnsWhenTagNameDropped(t *testing.T) {
	f := newFakeStore(t, nil, false)
	f.patchTag = "untagged-9d3d2b2b06d800c8cc0f"
	c := f.ctx(t)

	err := c.cleanIncoming(context.Background(),
		&gh.Release{ID: 700, TagName: model.IncomingTag}, []int64{11, 12}, 0)
	if err != nil {
		t.Fatalf("没留住 tag 名不是失败（draft 这个状态是对的），不该报错：%v", err)
	}
	if f.deletes != 2 {
		t.Errorf("已搬运的 asset 该照删不误（2 个），实际删了 %d 个", f.deletes)
	}
	// tag 名没留住也照样删引用 —— 别看混：**引用对不对**和**该不该删**是两件事。
	// 队列已经掰回 draft 了，此刻那个引用只会把下一次 Publish 拖回一份旧文件。
	if n := f.deletedIncomingRef(); n != 1 {
		t.Errorf("tag 名没留住不影响删引用（该删一次），实际 %d 次，请求：%v", n, f.refDeletes)
	}
	logs := strings.Join(f.logs, "\n")
	// 两样都要在：成了什么（好让人认出现场）、以及该改回什么（好让人直接动手）。
	if !strings.Contains(logs, f.patchTag) || !strings.Contains(logs, "改回") {
		t.Errorf("tag 名没留住该被当场喊出来（点明成了什么 + 该改回什么），实际日志：\n%s", logs)
	}
}
