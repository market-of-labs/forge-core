package store_test

import (
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/market-of-labs/forge-core/internal/fdroid"
	"github.com/market-of-labs/forge-core/internal/model"
	"github.com/market-of-labs/forge-core/internal/store"
)

// update 是黄金文件的**重录开关**：`go test ./internal/store -update`。
//
// 它存在的理由是这个测试的产物本身就是"要被提交进 store 的东西" —— 产物在 git 里，
// 所以每次**有意**改渲染（改映射表、改 YAML 引号风格）都必须重录一次，而重录要用的
// 正是渲染器本人。让测试带一个重录模式，比另外维护一个小工具可靠：
// 那个工具会漂移，而这里的重录与比对**走的是同一条代码路径**。
var update = flag.Bool("update", false, "重录 store/fdroid/metadata/ 下的黄金文件")

// storeRoot 定位同级的 store 仓库（`market-of-labs/store`）。
//
// 测试的工作目录是**包目录**（`forge-core/internal/store`），所以往上三级到 `market-of-labs`。
// 找不到就跳过而不是失败：forge-core 是可以单独 clone 的仓库，应当能在没有 store 的
// 情况下跑通自己的单测。
func storeRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join("..", "..", "..", "store")
	if _, err := os.Stat(root); err != nil {
		t.Skipf("同级没有 store 仓库（%s）——跳过黄金测试", root)
	}
	return root
}

// TestGoldenRealRepo 拿**仓库里真实存在的那些文件**跑一遍渲染链路。
//
// 它是这个模块唯一一条"用真数据"的测试，钉住的是**产物本身**而不是我对规范的理解：
//
//	输入  sources/*.json + store/endpoints.json   （人维护的）
//	产物  store/fdroid/metadata/*.yml             （build-repo 每轮重写，也在 git 里）
//
// 比对是**逐字节**的，因为那份 metadata 的实际消费者是 fdroidserver：它按文件名把 APK 与
// 元数据对上，读 YAML 里的字段编索引。任何一处措辞漂移（引号风格、键的顺序、多一个空行）
// 在这里就是一条 diff —— 而不是等到 CI 里 `fdroid update` 报一句语焉不详的话。
//
// ⚠️ 它**不**验证"fdroidserver 收不收这份 YAML" —— 那个问题的答案只在 Debian 容器里
// （本机跑不了 fdroidserver，见计划第 3 条）。本机这一侧只能保证：渲染是确定性的、
// 而且与上一次落盘的产物一致。
func TestGoldenRealRepo(t *testing.T) {
	repo := store.Repo{Root: storeRoot(t)}

	// ① 地址配置必须自洽。它是**唯一**一处"只改数据就能改行为"的开关（03 §2.3），
	// 而它能改坏的方式全在 Validate 里（协议、尾斜杠、模板与命名契约分叉）。
	ep, err := repo.LoadEndpoints()
	if err != nil {
		t.Fatalf("读 store/endpoints.json：%v", err)
	}
	if err := ep.Validate(); err != nil {
		t.Fatalf("store/endpoints.json 不合法：%v", err)
	}
	// 真实仓库的地址绝不能落在回环上 —— 那条 http 豁免是给本机验证留的（03 §7 #11）。
	if strings.HasPrefix(ep.RepoURL, "http://") {
		t.Errorf("store/endpoints.json 的 repoUrl 是明文 http（%q）："+
			"那是本机验证才该有的形态，提交进仓库就等于把这个源发给所有人", ep.RepoURL)
	}

	// ② sources 集合必须自洽。
	srcs, err := repo.LoadSources()
	if err != nil {
		t.Fatalf("读 sources/：%v", err)
	}
	if len(srcs) == 0 {
		t.Fatal("sources/ 里一个条目都没有 —— 那这份仓库就没有任何输入了")
	}
	if rep := model.CheckSourceSet(srcs); rep.HasErrors() {
		for _, p := range rep.Problems {
			t.Errorf("sources 集合校验：%s", p)
		}
	}

	// ③ 逐条渲染并比对。paused 的来源**不参与**（§2.5：它由调用方决定要不要渲染，
	// 而 BuildRepo 的选择是不渲染 —— 于是索引里没有它，这正是"暂停更新"的语义）。
	want := make(map[string][]byte, len(srcs))
	for i := range srcs {
		s := &srcs[i]
		if s.Paused {
			continue
		}
		b, err := fdroid.Render(s)
		if err != nil {
			t.Errorf("渲染 %s：%v", s.ID, err)
			continue
		}
		want[s.ID] = b
	}
	if len(want) == 0 {
		t.Fatal("没有任何非 paused 的来源 —— 渲染这一侧就无从谈起了")
	}

	// ④ 与落盘的产物逐字节比对，并核对**文件集合**。
	//
	// 集合这一半同样重要：`BuildRepo` 每轮会清掉已消失/已暂停来源的残留 yml
	// （见 sweepMetadata）。少了那个清理，一个被删掉的来源会永远留在索引里 ——
	// 客户端还能装到它，而 `sources/` 里已经没有任何东西能解释它从哪来。
	dir := repo.FdroidMetadataDir()

	if *update {
		rewriteMetadata(t, dir, want)
		return
	}
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("store 里还没有 %s —— 用 `go test ./internal/store -update` 重录一份", dir)
	}

	onDisk := listMetadata(t, dir)
	for id, wantBytes := range want {
		got, ok := onDisk[id]
		if !ok {
			t.Errorf("%s 有来源却没有 metadata 文件（%s）—— 这个应用不会出现在索引里",
				id, fdroid.MetadataFileName(id))
			continue
		}
		if got != string(wantBytes) {
			t.Errorf("%s 的 metadata 与落盘的产物不一致：\n--- 渲染 ---\n%s\n--- 落盘 ---\n%s\n"+
				"（改渲染是有意的就用 `go test ./internal/store -update` 重录）",
				id, wantBytes, got)
		}
		delete(onDisk, id)
	}
	for id := range onDisk {
		t.Errorf("%s 有 metadata 文件却没有对应的来源（或它已被 paused）—— "+
			"它正在索引里，而没有任何输入能解释它从哪来", id)
	}
}

// rewriteMetadata 重录黄金文件。**只在 `-update` 下走这条**。
//
// 它刻意把"清掉多余的 yml"也做掉（而不是只写不删）：这两件事在 `BuildRepo` 里是
// 同一个动作（渲染 + 清理，见 sweepMetadata），重录时只做一半，就会留下一份
// **本地看着干净、CI 里对不上**的 fixture。
func rewriteMetadata(t *testing.T, dir string, want map[string][]byte) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建 %s：%v", dir, err)
	}

	// 先删：不在 want 里的每一个 .yml 都是残留（来源被删了 / 被 paused 了）。
	for id := range listMetadata(t, dir) {
		if _, ok := want[id]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(dir, fdroid.MetadataFileName(id))); err != nil {
			t.Fatalf("清掉残留的 %s.yml：%v", id, err)
		}
		t.Logf("清掉残留的 %s.yml", id)
	}
	for id, b := range want {
		if err := os.WriteFile(filepath.Join(dir, fdroid.MetadataFileName(id)), b, 0o644); err != nil {
			t.Fatalf("写 %s.yml：%v", id, err)
		}
		t.Logf("重录 %s.yml（%d 字节）", id, len(b))
	}
}

// listMetadata 读出 metadata 目录下的全部 `<id>.yml`。
//
// 只认 `.yml`：fdroidserver 自己也会往这个目录里放东西（比如它生成的临时文件），
// 而那些不是我们的产物、也不该由我们断言。
func listMetadata(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读 %s：%v", dir, err)
	}
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("读 %s：%v", e.Name(), err)
		}
		out[strings.TrimSuffix(e.Name(), ".yml")] = string(b)
	}
	return out
}

// TestGoldenMetadataIsStable 钉住"渲染是确定性的"。
//
// 上面那条测试已经比对过一遍了，但它的结论依赖落盘的那份产物是对的（而那份产物是
// 上一轮 build-repo 写的）。这一条不依赖任何落盘文件：同一份输入渲染两次必须逐字节
// 相等。理由是产物在 git 里 —— 一次不确定的渲染（比如遍历 map 拼字段）会让**每一轮
// 对账都产生一个无意义的提交**，而那种 diff 看多了就没人再看 diff 了。
func TestGoldenMetadataIsStable(t *testing.T) {
	srcs, err := store.Repo{Root: storeRoot(t)}.LoadSources()
	if err != nil {
		t.Fatalf("读 sources/：%v", err)
	}
	for i := range srcs {
		s := &srcs[i]
		if s.Paused {
			continue
		}
		a, err := fdroid.Render(s)
		if err != nil {
			t.Fatalf("渲染 %s：%v", s.ID, err)
		}
		b, err := fdroid.Render(s)
		if err != nil {
			t.Fatalf("渲染 %s（第二次）：%v", s.ID, err)
		}
		if string(a) != string(b) {
			t.Errorf("%s 两次渲染不一致：\n--- 第一次 ---\n%s\n--- 第二次 ---\n%s", s.ID, a, b)
		}
	}
}

// TestGoldenLegacyFieldsGone 钉住"旧通道的字段真的从数据里清干净了"。
//
// 这不是一条形式检查。`kind` 曾是 `Source` 上的一个字段，而 JSON 解码**忽略未知字段** ——
// 于是 sources/ 里留着 `kind` 时，解码不会报错、测试不会红，只有人去看文件才发现
// 一堆没人读的死数据。D58 之后它不该再出现（03 §2.2 的字段表里没有它）。
//
// 同理 `apps.json` 不该再存在：它是旧通道的产物，留着就是一份**会漂移的陈旧清单**。
func TestGoldenLegacyFieldsGone(t *testing.T) {
	root := storeRoot(t)

	if _, err := os.Stat(filepath.Join(root, "apps.json")); err == nil {
		t.Error("store 里还有 apps.json —— 它随 Obtainium 那条路一起废了（D58），" +
			"留着只会是一份与索引对不上的陈旧清单")
	}

	srcs, err := store.Repo{Root: root}.LoadSources()
	if err != nil {
		t.Fatalf("读 sources/：%v", err)
	}
	for i := range srcs {
		// Source 上已经没有 Kind 字段，所以只能回到原始字节里找。
		b, err := os.ReadFile(filepath.Join(root, "sources", srcs[i].ID+".json"))
		if err != nil {
			t.Fatalf("读 %s：%v", srcs[i].ID, err)
		}
		if strings.Contains(string(b), `"kind"`) {
			t.Errorf("sources/%s.json 里还留着 kind 字段 —— 它已经没有任何消费者（D58）",
				srcs[i].ID)
		}
	}
}

// TestGoldenSourcesHaveLedger 钉住"每条来源都至少有一个已镜像版本"。
//
// 账本是**派生数据**（D23），跑 build-index 可以从 Release 现状重建。但它是只读回源
// 唯一能看出"这条来源到底镜像过什么"的地方 —— 空账本的来源在 `BuildRepo` 里会被跳过
// （一个 APK 都没有 ⇒ 不写 metadata，见那里的第 2 步），也就是**它在索引里不存在**。
//
// 这条测试把那种状态显式地说出来，而不是让它表现为"某个应用莫名其妙不在客户端里"。
func TestGoldenSourcesHaveLedger(t *testing.T) {
	srcs, err := store.Repo{Root: storeRoot(t)}.LoadSources()
	if err != nil {
		t.Fatalf("读 sources/：%v", err)
	}
	var empty []string
	for i := range srcs {
		if len(srcs[i].Versions) == 0 {
			empty = append(empty, srcs[i].ID)
		}
	}
	if len(empty) > 0 {
		sort.Strings(empty)
		t.Errorf("这些来源的账本是空的（= 它们不会出现在索引里，因为一个 APK 都没有）：%v\n"+
			"若这是刚收录的来源，跑一次 reconcile 就会填上；若它已经有 Release 了，说明镜像没跑成",
			empty)
	}
}
