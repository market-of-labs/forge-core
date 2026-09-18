package job

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/market-of-labs/forge-core/internal/gh"
	"github.com/market-of-labs/forge-core/internal/model"
)

// 这一组钉的是对账里唯一有真正判断的那一段：**水位线**（pickTargets）。
// 它是纯函数 —— 不发请求、不碰文件系统。

// candidates 造一份"上游 Release 列表"，**按发布时间从新到旧**（与
// upstream.Releasable 的输出一致）。
func candidates(tags ...string) []gh.Release {
	out := make([]gh.Release, 0, len(tags))
	for _, t := range tags {
		out = append(out, gh.Release{TagName: t})
	}
	return out
}

// ledgerWith 造一个工作副本，其中那条来源的账本已镜像过 upstreamTags 这些上游 Release。
// 位次无关紧要 —— 水位线判据是集合成员，不是顺序。
func ledgerWith(appID string, upstreamTags ...string) *Ctx {
	src := model.Source{ID: appID}
	for i, t := range upstreamTags {
		src.Versions = append(src.Versions, model.Version{
			Version:     fmt.Sprintf("1.0.%d", i),
			UpstreamTag: t,
		})
	}
	return &Ctx{
		Sources: []model.Source{src},
		Log:     func(string, ...any) {},
	}
}

func tagsOf(rs []gh.Release) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.TagName)
	}
	return out
}

func pickAndCheck(t *testing.T, c *Ctx, appID string, cands []gh.Release, want ...string) *model.Report {
	t.Helper()
	if cands == nil {
		cands = []gh.Release{}
	}
	rep := &model.Report{}
	got := pickTargets(c, appID, cands, rep)
	if want == nil {
		want = []string{}
	}
	if !reflect.DeepEqual(tagsOf(got), want) {
		t.Fatalf("挑出的版本不对：\nwant=%v\ngot =%v", want, tagsOf(got))
	}
	return rep
}

// 绝大多数轮次的正常结果：上游最新的那个已经镜像过了，什么都不做。
func TestPickTargets_NothingToDo(t *testing.T) {
	c := ledgerWith("com.example.app", "v3")
	pickAndCheck(t, c, "com.example.app", candidates("v3", "v2", "v1"))
}

// §4.4 的漏跑自愈：跳过的那几天发的版本要补上，但**只补到水位线为止** ——
// 水位线以下的（v1 及更老）属于 D33 的"不追溯"，永远不翻。
func TestPickTargets_MissedOneDayCatchesUp(t *testing.T) {
	c := ledgerWith("com.example.app", "v2")
	pickAndCheck(t, c, "com.example.app", candidates("v3", "v2", "v1"), "v3")
}

// 补多个时必须是**从老到新**：账本的位次就是"追加顺序"，而
// Source.Latest() 取最后一个 —— 顺序反了会把老版本当成最新。
func TestPickTargets_CatchUpIsOldestFirst(t *testing.T) {
	c := ledgerWith("com.example.app", "v1")
	pickAndCheck(t, c, "com.example.app", candidates("v4", "v3", "v2", "v1"), "v2", "v3", "v4")
}

// D33：收录时只镜像**当刻的最新版本**，更老的版本不追溯。
func TestPickTargets_FirstIntakeTakesOnlyNewest(t *testing.T) {
	c := ledgerWith("com.example.app") // 一个版本都没有
	pickAndCheck(t, c, "com.example.app", candidates("v3", "v2", "v1"), "v3")
}

// 工作副本里压根没有这个 App（刚收录）—— 与"有 App 但没版本"同一条路。
func TestPickTargets_UnknownAppTakesOnlyNewest(t *testing.T) {
	c := ledgerWith("com.example.other", "v1")
	pickAndCheck(t, c, "com.example.app", candidates("v3", "v2", "v1"), "v3")
}

// 记下的 upstreamTag 在上游列表里找不到（上游把那个 Release 删了 / 改了 tag）：
// 按"一个都没镜像过"处理。代价只是少补几个老版本，而不追溯本来就是既定口径 ——
// 反过来（当成"全都镜像过"）会让这个 App 从此再也不更新。
func TestPickTargets_WatermarkGoneTakesOnlyNewest(t *testing.T) {
	c := ledgerWith("com.example.app", "v0-deleted")
	pickAndCheck(t, c, "com.example.app", candidates("v3", "v2", "v1"), "v3")
}

func TestPickTargets_SingleCandidate(t *testing.T) {
	t.Run("已镜像", func(t *testing.T) {
		c := ledgerWith("com.example.app", "v1")
		pickAndCheck(t, c, "com.example.app", candidates("v1"))
	})
	t.Run("未镜像", func(t *testing.T) {
		c := ledgerWith("com.example.app")
		pickAndCheck(t, c, "com.example.app", candidates("v1"), "v1")
	})
}

// 空候选不能崩。调用方（resolveOne）在更早的地方就挡了这种情况，但那道防线在
// 另一个函数里 —— 单独测/复用本函数时，"一个 Release 都没有"是最自然的输入之一。
func TestPickTargets_EmptyCandidatesDoesNotPanic(t *testing.T) {
	c := ledgerWith("com.example.app")
	pickAndCheck(t, c, "com.example.app", []gh.Release{})
	pickAndCheck(t, c, "com.example.app", nil)
}

// 补多个版本是要让人看见的：通常意味着前面有几天没跑成。
func TestPickTargets_WarnsWhenCatchingUp(t *testing.T) {
	c := ledgerWith("com.example.app", "v1")
	rep := pickAndCheck(t, c, "com.example.app", candidates("v3", "v2", "v1"), "v2", "v3")
	if len(rep.Warnings()) == 0 {
		t.Fatal("补齐多个版本时应当有一条告警（让人知道前面漏跑了）")
	}
	if rep.HasErrors() {
		t.Fatal("补齐是正常路径，不该产生硬错误")
	}

	// 只补一个（正常轮次）不该吵。
	c2 := ledgerWith("com.example.app", "v2")
	rep2 := pickAndCheck(t, c2, "com.example.app", candidates("v3", "v2"), "v3")
	if len(rep2.Warnings()) != 0 {
		t.Fatalf("只补一个版本不该告警：%v", rep2.Warnings())
	}
}

// ---- 分片合并 ---------------------------------------------------------------

func abiSeq(as []model.Asset) string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.ABI)
	}
	return strings.Join(out, "/")
}

// 02 §2.4：universal 在前，其后按固定集。
func TestMergeAssets_OrdersByContract(t *testing.T) {
	got := mergeAssets(
		[]model.Asset{{ABI: "x86_64", File: "a-1.0-x86_64.apk"}},
		[]model.Asset{
			{ABI: "universal", File: "a-1.0-universal.apk"},
			{ABI: "arm64-v8a", File: "a-1.0-arm64-v8a.apk"},
		},
	)
	if want := "universal/arm64-v8a/x86_64"; abiSeq(got) != want {
		t.Fatalf("ABI 顺序不对：want=%s got=%s", want, abiSeq(got))
	}
}

// 同一个 ABI 在新的一批里又出现 → **新的覆盖旧的**。这不是洁癖：改名前后
// 同一个 ABI 会有两个不同名的文件，账本里必须只留指向当前 Release 的那一个。
func TestMergeAssets_NewWinsOnSameABI(t *testing.T) {
	got := mergeAssets(
		[]model.Asset{{ABI: "arm64-v8a", File: "旧名.apk"}},
		[]model.Asset{{ABI: "arm64-v8a", File: "新名.apk"}},
	)
	if len(got) != 1 || got[0].File != "新名.apk" {
		t.Fatalf("同 ABI 应当被新的覆盖：%+v", got)
	}
}

// sortAssets 的去重规则与 mergeAssets **相反**：保留**先出现**的那份。
// 两条规则不同是有意的（合并时"新的"更可信，排序时"先来的"是已知的），
// 所以各自钉一遍，免得日后有人"顺手统一"掉。
func TestSortAssets_KeepsFirstOnDuplicateABI(t *testing.T) {
	got := sortAssets([]model.Asset{
		{ABI: "arm64-v8a", File: "先来.apk"},
		{ABI: "arm64-v8a", File: "后到.apk"},
		{ABI: "universal", File: "u.apk"},
	})
	if len(got) != 2 {
		t.Fatalf("同一 ABI 重复项应当被去掉：%+v", got)
	}
	if got[0].ABI != "universal" {
		t.Fatalf("universal 应当排在最前：%+v", got)
	}
	if got[1].File != "先来.apk" {
		t.Fatalf("去重应当保留先出现的那份：%+v", got)
	}
}

// 排序必须是确定的 —— 它是清单里 apkUrls 顺序与账本位次的直接来源。
func TestSortAssets_Deterministic(t *testing.T) {
	in := []model.Asset{
		{ABI: "x86", File: "x.apk"},
		{ABI: "arm64-v8a", File: "a.apk"},
		{ABI: "universal", File: "u.apk"},
		{ABI: "armeabi-v7a", File: "v.apk"},
		{ABI: "x86_64", File: "x64.apk"},
	}
	first := abiSeq(sortAssets(in))
	for i := 0; i < 20; i++ {
		if got := abiSeq(sortAssets(in)); got != first {
			t.Fatalf("第 %d 次排序结果不同：%s vs %s", i, first, got)
		}
	}
	if want := "universal/arm64-v8a/armeabi-v7a/x86_64/x86"; first != want {
		t.Fatalf("固定集顺序不对：want=%s got=%s", want, first)
	}
	// 输入不该被就地打乱（调用方可能还要用原顺序）。
	if in[0].ABI != "x86" {
		t.Fatalf("sortAssets 就地改了入参：%+v", in)
	}
}

// TestUpstreamAssetDownloadsUseUpstreamRepo 钉住一个真出过事的点：asset id 是
// **仓库内**的编号，上游 asset 必须去上游仓库取。这里曾经写死 StoreRepo，于是
// 每个上游 asset 都 404，而镜像侧把它容忍成一条 WARN —— 症状是"每天都是绿的、
// 什么都没镜像"，新增单则一律死在"探身份"。
func TestUpstreamAssetDownloadsUseUpstreamRepo(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		io.Copy(io.Discard, r.Body)
		w.Write(emptyZip)
	}))
	defer srv.Close()

	ghc, err := gh.New(gh.Config{Token: "t", BaseURL: srv.URL, UploadBaseURL: srv.URL})
	if err != nil {
		t.Fatalf("建客户端：%v", err)
	}
	c := &Ctx{Env: &Env{StoreRepo: "o/store"}, GH: ghc, Log: func(string, ...any) {}}

	// 内容不是 APK，两处都必然报错 —— 本测试只问"请求打到哪个仓库"，所以忽略返回值。
	c.readAssetMeta(context.Background(), "owner/up", gh.Asset{ID: 7, Name: "a.apk"})
	c.uploadAsset(context.Background(), "owner/up", &gh.Release{ID: 1}, "t.apk", gh.Asset{ID: 7, Name: "a.apk"})

	// 两次下载各一次请求，之后才是上传。
	want := []string{"/repos/owner/up/releases/assets/7", "/repos/owner/up/releases/assets/7"}
	if len(paths) < 2 || !reflect.DeepEqual(paths[:2], want) {
		t.Fatalf("上游 asset 没去上游仓库取：want=%v got=%v（StoreRepo 是 %s）", want, paths, c.Env.StoreRepo)
	}
	if len(paths) > 2 && !strings.HasPrefix(paths[2], "/repos/o/store/") {
		t.Fatalf("上传没落到 store：%s", paths[2])
	}
}

// emptyZip 是一个零条目的合法 zip：让 apkmeta.Read 干净地报"读不出元数据"，
// 而不是拿一段乱码去试探它的容错。
var emptyZip = append([]byte("PK\x05\x06"), make([]byte, 18)...)

// TestSyncReleaseBodyFromUpstreamReadme 钉住"正文 = 上游 README"，以及**相同就不写**。
//
// 后半条不是省请求的洁癖：内部 Release 是应用级的、正文只有一份，而镜像轮次里
// 绝大多数时候 README 一个字节都没变 —— 每轮都 PATCH 一次等于每天对每个应用写一次
// Release，白造一堆无意义的写入。
func TestSyncReleaseBodyFromUpstreamReadme(t *testing.T) {
	const md = "# 上游项目\n\n这个 App 是干什么的。\n"
	var patched []string
	var reads int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/readme"):
			reads++
			io.WriteString(w, fmt.Sprintf(`{"name":"README.md","encoding":"base64","content":%q}`,
				base64.StdEncoding.EncodeToString([]byte(md))))
		case r.Method == http.MethodPatch:
			var p map[string]any
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				t.Errorf("解 PATCH 体：%v", err)
			}
			b, _ := p["body"].(string)
			patched = append(patched, b)
			io.WriteString(w, `{"id":1,"tag_name":"com.x"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"Not Found"}`)
		}
	}))
	defer srv.Close()

	ghc, err := gh.New(gh.Config{Token: "t", BaseURL: srv.URL, UploadBaseURL: srv.URL})
	if err != nil {
		t.Fatalf("建客户端：%v", err)
	}
	c := &Ctx{Env: &Env{StoreRepo: "o/store"}, GH: ghc, Log: func(string, ...any) {}}
	ctx := context.Background()

	// 现有正文与 README 不同 → 写一次。
	rel := &gh.Release{ID: 1, TagName: "com.x", Body: "上一次同步的旧正文"}
	if err := c.syncReleaseBody(ctx, rel, "owner/up"); err != nil {
		t.Fatalf("syncReleaseBody：%v", err)
	}
	if len(patched) != 1 || patched[0] != md {
		t.Fatalf("正文没写成上游 README：%q", patched)
	}

	// 已经一致 → 一次 PATCH 都不该有（但仍要去读 README 才知道一不一致）。
	rel.Body = md
	if err := c.syncReleaseBody(ctx, rel, "owner/up"); err != nil {
		t.Fatalf("syncReleaseBody：%v", err)
	}
	if len(patched) != 1 {
		t.Fatalf("正文没变却 PATCH 了 %d 次", len(patched)-1)
	}
	if reads != 2 {
		t.Fatalf("README 读取次数 = %d，期望 2", reads)
	}
}

// TestAssetsOfSplitsGhosts 钉住"名字在 ⟺ 文件在"这条不变量**在读取处**就被恢复。
//
// 幽灵（state=starter：上传开始了、从未 finalize，有名字有 size 却没字节）不能在更下游
// 被各处理一遍 —— 那样每多一个消费者就多一处可能忘掉，而 2026-09-17 那次事故里正好
// 有三个消费者同时被骗（BuildIndex 记进账本、镜像按名跳过、placeAPKs 去下它）。
// 在这里分开之后，ReleaseAssets 的契约才回到 03 §3.1 那条规则的前提上：那条规则说
// "幂等按 asset 名判断"，而它默认名字意味着文件。
func TestAssetsOfSplitsGhosts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 三条：一个正常资产、一个幽灵、一个**没给 state** 的（老 fake / 手工构造的样子，
		// 必须仍然算可下载，否则这次改动会把所有既有 fixture 一起判成幽灵）。
		io.WriteString(w, `[
			{"id":1,"name":"com.x-1.0-universal.apk","size":10,"state":"uploaded"},
			{"id":2,"name":"com.x-1.1-universal.apk","size":83336512,"state":"starter"},
			{"id":3,"name":"com.x-1.0-arm64-v8a.apk","size":9}
		]`)
	}))
	defer srv.Close()

	ghc, err := gh.New(gh.Config{Token: "t", BaseURL: srv.URL, UploadBaseURL: srv.URL})
	if err != nil {
		t.Fatalf("建客户端：%v", err)
	}
	c := &Ctx{Env: &Env{StoreRepo: "o/store"}, GH: ghc, Log: func(string, ...any) {}}
	rel := &gh.Release{ID: 7}

	live, ghosts, err := c.assetsOf(context.Background(), rel)
	if err != nil {
		t.Fatalf("assetsOf：%v", err)
	}
	if len(live) != 2 || len(ghosts) != 1 {
		t.Fatalf("分组不对：live=%d ghosts=%d（live=%v ghosts=%v）", len(live), len(ghosts), live, ghosts)
	}
	if _, ok := live["com.x-1.1-universal.apk"]; ok {
		t.Error("幽灵进了 live —— 这正是 2026-09-17 那次事故的起点")
	}
	if g, ok := ghosts["com.x-1.1-universal.apk"]; !ok || g.ID != 2 {
		t.Errorf("幽灵没被单独收好（它是唯一需要被点名处置的那一个）：%+v", ghosts)
	}

	// ReleaseAssets 是给绝大多数调用方的那一面：它必须看不见幽灵。
	only, err := c.ReleaseAssets(context.Background(), rel)
	if err != nil {
		t.Fatalf("ReleaseAssets：%v", err)
	}
	if len(only) != 2 {
		t.Fatalf("ReleaseAssets 只该给可下载的：%+v", only)
	}
	if _, ok := only["com.x-1.1-universal.apk"]; ok {
		t.Error("ReleaseAssets 把幽灵给了出去 —— 它的契约是「名字在 ⟺ 文件在」")
	}
}

// TestMirrorGateSeparatesGhostFromRealAsset 钉住那次事故的**唯一开关**。
//
// 2026-09-17：`existing` 里混进了一个幽灵，"名字在"于是被当成"文件在" ⇒ 判成
// "跳过（幂等）" ⇒ 那一版永远不重传，而同一个动作还把它记进了账本 ⇒ `pickTargets`
// 把它当"已镜像"，水位线一跳跳过它，它下面更新的那些版本（p27…p35）一个都轮不到。
// 整整一晚，每一轮都是绿的，只有两条 WARN。
//
// 所以这个判定必须把"真文件占名"与"幽灵占名"分开，而后者**绝不能**是 gateSkip。
// 真文件那一条是从前唯一的行为，必须原样保住（`--clobber` 会让正在下载的客户端拿到
// 半个文件，03 §3.1）。
func TestMirrorGateSeparatesGhostFromRealAsset(t *testing.T) {
	real := gh.Asset{ID: 1, Name: "com.x-1.0-universal.apk", State: "uploaded"}
	ghost := gh.Asset{ID: 2, Name: "com.x-1.1-universal.apk", State: "starter"}
	existing := map[string]gh.Asset{real.Name: real}
	ghosts := map[string]gh.Asset{ghost.Name: ghost}

	cases := []struct {
		target string
		want   gateVerdict
	}{
		{real.Name, gateSkip},                   // 真文件占着 → 幂等跳过
		{ghost.Name, gateGhost},                 // 幽灵占着 → 硬错误，绝不能是 skip
		{"com.x-1.2-universal.apk", gateUpload}, // 空着 → 传
	}
	for _, tc := range cases {
		got, who := mirrorGate(tc.target, existing, ghosts)
		if got != tc.want {
			t.Errorf("%s：verdict 错了（want %d got %d）", tc.target, tc.want, got)
			continue
		}
		if got == gateUpload {
			if who.ID != 0 {
				t.Errorf("%s：没人占着，却回传了一个 occupant：%+v", tc.target, who)
			}
			continue
		}
		if who.Name != tc.target {
			t.Errorf("%s：回传的 occupant 是 %q（处置那条命令要用它的 id，指错了会删错东西）",
				tc.target, who.Name)
		}
	}
}

// TestGhostRefusalNamesTheAssetToDelete 钉住那条硬错误**可执行**。
//
// 整轮是绿的，这条 ERROR 是唯一痕迹；而它每一轮都会再来一次，直到有人处置。所以它必须
// 自带一条能直接粘贴的命令、且带上 asset id —— 光说"有个 asset 占着名字"等于没说，
// 人还得自己去翻 API。
func TestGhostRefusalNamesTheAssetToDelete(t *testing.T) {
	c := &Ctx{Env: &Env{StoreRepo: "market-of-labs/store"}}
	g := gh.Asset{ID: 571176931, Name: "dev.thejaustin.obtainiumplus-1.6.10-p26-universal.apk",
		State: "starter", CreatedAt: "2026-09-17T21:27:19Z"}

	err := c.ghostRefusal("dev.thejaustin.obtainiumplus", g.Name, g)
	if err == nil {
		t.Fatal("撞上幽灵必须报错（跳过就是 2026-09-17 那次事故本身）")
	}
	msg := err.Error()
	for _, want := range []string{
		"571176931",            // asset id：处置命令要用它
		"starter",              // 状态：人据此确认"上传从未完成"
		"2026-09-17T21:27:19Z", // 创建时间：用来判断是不是自己刚才传的
		"repos/market-of-labs/store/releases/assets/571176931", // 可直接粘贴的那条命令
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("硬错误里缺 %q：\n%s", want, msg)
		}
	}
}
