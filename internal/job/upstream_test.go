package job

import (
	"context"
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

// 这一组钉的是对账里唯一有真正判断的两段：**水位线**（pickTargets）与 **§4.6 闸门**
// （CheckIncomingGate）。两者都是纯函数 —— 不发请求、不碰文件系统。

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

// ---- §4.6 闸门（规则 2） ----------------------------------------------------

func TestCheckIncomingGate(t *testing.T) {
	cases := []struct {
		name    string
		env     Env
		wantErr string
	}{
		{
			name: "暂存队列的正式发布 → 放行",
			env:  Env{ReleaseTag: model.IncomingTag},
		},
		{
			name: "预发布不搬运",
			// `published` 对预发布**同样触发**，所以这条必须自己判。
			env:     Env{ReleaseTag: model.IncomingTag, Prerelease: true},
			wantErr: "prerelease",
		},
		{
			name: "别的 tag 不是队列发布",
			// store 侧故意不筛事件，筛选是 forge 的职责。
			env:     Env{ReleaseTag: "com.example.app"},
			wantErr: "不是",
		},
		{
			name: "tag 为空（payload 没带过来）→ 同样拒绝",
			env:  Env{},
			// 空 tag 绝不能"因为没给所以放行"—— 那正是最危险的方向。
			wantErr: "不是",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckIncomingGate(&tc.env)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("应当放行，却报错：%v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("应当拒绝，却放行了")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误信息里应当提到 %q：%v", tc.wantErr, err)
			}
		})
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
