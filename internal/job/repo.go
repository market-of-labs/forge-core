package job

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/market-of-labs/forge-core/internal/fdroid"
	"github.com/market-of-labs/forge-core/internal/gh"
	"github.com/market-of-labs/forge-core/internal/model"
	"github.com/market-of-labs/forge-core/internal/naming"
)

// ---- build-repo（03 §5.1） ---------------------------------------------------

// BuildRepo 把 sources（自带账本）渲染成 fdroidserver 的输入，跑一次 `fdroid update`，
// 再把产物搬进对外伺服的 `repo/`。
//
// 它与被它取代的 `BuildManifest` 有一个根本差别，值得先看清楚：清单那条路上，
// 产物**完全由数据决定**（我们拼出 apps.json、写下去、收工）；F-Droid 这条路上，
// 索引是 fdroidserver **扫目录**扫出来的，所以这一步的实质是"**把磁盘布置成我们想要的
// 样子**"——摆哪些 APK、写哪些 yml，然后让工具去读。于是它的每一步失败都是
// "磁盘上少了个东西"，而不是"某个字段拼错了"，而**幂等**也从"算出一样的字节"变成了
// "布置出一样的目录"。
//
// 六段对应 03 §5.1 的六步，顺序有一处**刻意调换**：先把 APK 凑齐，再写 metadata。
// 原因是第 1 步里那条"一个 APK 都没有的源不写 metadata"——不先知道谁凑齐了文件，
// 那条规则就无处落地。cache 写回（第 6 步）则没有独立的一段：它紧跟着每一次成功的下载
// （见 fetchAsset）。
func BuildRepo(ctx context.Context, c *Ctx) (*model.Report, error) {
	rep := &model.Report{}

	// 目录先建齐：fdroidserver 是在遍历到那一步才发现目录不存在的，那时它已经在写索引了，
	// 报出来的错跟"目录没建"毫无关系。
	for _, dir := range []string{c.Repo.FdroidRepoDir(), c.Repo.FdroidMetadataDir(), c.Repo.RepoDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("建目录 %s：%w", dir, err)
		}
	}

	plans := planRepo(c)

	// ---- 第 2 步（提前）：把这一轮该有的 APK 凑进工作区 -----------------------------
	files := make(map[string]int, len(plans))
	for i := range plans {
		n, err := placeAPKs(ctx, c, &plans[i], rep)
		if err != nil {
			return nil, err
		}
		files[plans[i].src.ID] = n
	}

	// ---- 第 1 步：metadata（只有凑齐文件的来源才写） -------------------------------
	if err := renderMetadata(c, plans, files, rep); err != nil {
		return nil, err
	}

	// ---- 第 3 步：config.yml + keystore ------------------------------------------
	// cleanup 必须在 `fdroid update` **之后**才调用：签名发生在那一刻。
	cleanup, err := writeFdroidConfig(c)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	// ---- 第 4 步：fdroid update --------------------------------------------------
	if err := runFdroidUpdate(ctx, c); err != nil {
		return nil, err
	}

	// ---- 第 5 步：产物 → 根 `repo/` -----------------------------------------------
	if err := copyProducts(c); err != nil {
		return nil, err
	}
	return rep, nil
}

// repoFile 是一个分片在这台机器上的落地形态：文件名 + 账本记的字节数。
//
// size 存在的唯一用途是**校验 cache**（见 ensureLocal）。它不写进索引 —— 索引里的
// size 由 fdroidserver 自己从文件读出来，比我们记得准。
type repoFile struct {
	name string
	size int64
}

// repoPlan 是一个来源这一轮该有的文件集。
//
// 两类的差别是这一轮的核心决定（"索引覆盖：每轮只取最新版本，老版本靠 cache 累积"）：
// **最新版本的缺了去下载，老版本的缺了就算了**。
type repoPlan struct {
	src *model.Source
	// newest 是**最新版本**的分片：缺了就下载。
	newest []repoFile
	// older 是更老版本的分片：缺了**不下载**，只从 cache 取。
	older []repoFile
}

// planRepo 按每个来源的账本算出这一轮的文件集。
//
// 事实源是**账本**（`Source.Versions[].Assets[].File`）而不是现场再列一遍 Release：
// 账本刚刚由 BuildIndex 从 Release 现状重建过（RebuildAndCheck 的顺序保证了这一点），
// 两者在这一刻是同一件事，而账本多带了"哪个是最新"这个判断（Source.Latest）——
// 一个从 Release 现列的地方得自己再实现一遍。于是这里**零网络**，下载是下一段的事。
//
// paused 与"一个版本都没镜像过"的来源直接跳过：它们不该在索引里出现，
// 而**不写 metadata** 正是让它们消失的手段（02 §2.5 —— `paused` 不产生任何字段，
// 它由调用方决定要不要渲染这个文件）。
func planRepo(c *Ctx) []repoPlan {
	out := make([]repoPlan, 0, len(c.Sources))
	for i := range c.Sources {
		src := &c.Sources[i]
		if src.Paused || len(src.Versions) == 0 {
			continue
		}
		latest := src.Latest()
		p := repoPlan{src: src}
		for vi := range src.Versions {
			v := &src.Versions[vi]
			for _, a := range v.Assets {
				if a.File == "" {
					// 空文件名只可能来自手改。挡在这儿，比让它拼出一个等于目录名的
					// 路径、然后在更远的地方炸要好。
					continue
				}
				f := repoFile{name: a.File, size: a.Size}
				if v == latest {
					p.newest = append(p.newest, f)
				} else {
					p.older = append(p.older, f)
				}
			}
		}
		out = append(out, p)
	}
	return out
}

// placeAPKs 把一个来源这一轮的分片凑进 fdroid 工作区，返回落进去的文件数。
//
// 四种情形，按这个顺序问：
//
//	① 工作区里已经有了          → 用它（本地重跑时就是这种情况）
//	② cache 里有（且大小对得上） → 拷过来，**不下载**
//	③ 是最新版本               → 下载
//	④ 是更老的版本             → 放弃，报一条软告警
//
// ①②对最新版本同样适用：上一轮下过的"最新"这一轮往往还是最新，重下一次纯属浪费
// （这些文件每个 20 MB 量级）。
//
// ③④的差别就是"每轮只取最新版本"那条决定本身。它的收益是每轮不必把**全部历史版本**
// 重下一遍（对一个每天跑一次的 cron 来说那是纯浪费），代价则写在 ④ 上：
// **cache 丢了，老版本就从索引里消失**。这是设计如此、不是故障 ——
// 但它是**静默**的（索引照样合法、客户端照样能用，只是少了几版），所以必须吵一句。
func placeAPKs(ctx context.Context, c *Ctx, p *repoPlan, rep *model.Report) (int, error) {
	dir := c.Repo.FdroidRepoDir()
	n := 0

	var missingNewest []repoFile
	for _, f := range p.newest {
		ok, err := ensureLocal(c, dir, f)
		if err != nil {
			return 0, err
		}
		if ok {
			n++
			continue
		}
		missingNewest = append(missingNewest, f)
	}
	for _, f := range p.older {
		ok, err := ensureLocal(c, dir, f)
		if err != nil {
			return 0, err
		}
		if ok {
			n++
			continue
		}
		rep.Warnf(p.src.ID, "老版本的分片 %s 既不在 cache 里、工作区里也没有，本轮索引里不会有它"+
			"（索引覆盖的决定：每轮只下载最新版本，老版本只从 cache 取）。"+
			"第一次跑、或 cache 刚被清掉时属正常；否则它在告诉你 cache 没能跨轮留下来（%s）",
			f.name, cacheHint(c))
	}

	if len(missingNewest) == 0 {
		return n, nil
	}
	got, err := downloadNewest(ctx, c, p.src, missingNewest)
	n += got
	if err != nil {
		// 下载失败**不中止整轮**，理由与镜像那一步的容忍（D44）同源：一个应用的网络抖动
		// 不该让**别的应用**的更新也发不出去。而它会自愈：账本里那条版本还在，
		// 下一轮照样会试着下它（对账是幂等的，见 Reconcile）。
		//
		// 代价要说清楚：它是**静默**的 —— 这一轮的索引里那个版本缺席，装了旧版的用户
		// 看不到这次更新，而市场看起来一切正常。所以这条告警的措辞必须让人能把它
		// 对上"某某应用怎么不更新了"。
		rep.Warnf(p.src.ID, "最新版本的 %d 个分片没能取到（%v）—— 本轮索引里不会有它们，"+
			"装了旧版的用户看不到这次更新。下一轮会重试", len(missingNewest), err)
	}
	return n, nil
}

// ensureLocal 确保一个分片出现在工作区里，来源是"本来就在"或 cache。
//
// 返回 false 表示两处都够不着 —— **不是错误**：调用方按"它是不是最新版本"决定
// 接下来是下载还是放弃。
//
// cache 命中要**核对字节数**（账本里有 size），这一步是刻意的：cache 是一个没人看管的
// 二进制目录，而一个被截断的文件在里面长得和完整的文件一模一样。判据若只是"存在且非空"，
// 放它进去之后的症状会出现在很远的地方（fdroidserver 解析不动它 → `fdroid update` 失败 →
// 整轮白跑，而错误信息指向那个 APK，不指向那次中断）。大小对不上就当成**没有**，
// 让它走下载那条路 —— 那是唯一能自愈的方向。
func ensureLocal(c *Ctx, dir string, f repoFile) (bool, error) {
	dst := filepath.Join(dir, f.name)
	if fi, err := os.Stat(dst); err == nil && fi.Size() > 0 {
		return true, nil
	}
	if c.Env.APKCacheDir == "" {
		return false, nil
	}

	src := filepath.Join(c.Env.APKCacheDir, f.name)
	fi, err := os.Stat(src)
	if err != nil || fi.Size() == 0 {
		return false, nil
	}
	if f.size > 0 && fi.Size() != f.size {
		c.Log("cache 里的 %s 是 %d 字节、账本记的是 %d 字节 —— 当作没有，走下载", f.name, fi.Size(), f.size)
		return false, nil
	}
	if err := copyFile(src, dst); err != nil {
		return false, fmt.Errorf("从 cache 取 %s：%w", f.name, err)
	}
	c.Log("cache 命中 %s（%d 字节）", f.name, fi.Size())
	return true, nil
}

// cacheHint 把"cache 现在配着什么"变成告警里的一句话。
//
// 单独抽出来是因为上面两条告警的**可操作性全在这一句上**：`APK_CACHE_DIR` 为空时
// 老版本**必然**一个都留不下（这不是"cache 坏了"，是"根本没配"），而配了却没留下
// 就是另一回事（工作流的 actions/cache 没恢复成、或者 key 变了）。
// 两者的修法完全不同，所以不能让它们共用一句"cache 里没有"。
func cacheHint(c *Ctx) string {
	if c.Env.APKCacheDir == "" {
		return "本轮 **没有配 cache**（$" + EnvAPKCacheDir + " 为空）—— 这种情况下老版本一个都留不下是必然的"
	}
	return "cache 目录是 " + c.Env.APKCacheDir
}

// downloadNewest 从**本市场自己**的 Release 里取缺失的分片。
//
// 取我们自己的 Release 而不是上游：那些 APK 是镜像那一步（MirrorUpstream）传上去的，
// 而上游此刻可能已经把那个版本撤了（Release 被删、asset 被换）——
// 镜像的语义正是"上游撤了也照样供得上"（D13 全保留）。
func downloadNewest(ctx context.Context, c *Ctx, src *model.Source, missing []repoFile) (int, error) {
	if err := c.Env.RequireToken("下载自己 Release 里的 APK"); err != nil {
		return 0, err
	}
	rel, err := c.EnsureRelease(ctx, src.ID, src.Name)
	if err != nil {
		return 0, err
	}
	assets, err := c.ReleaseAssets(ctx, rel)
	if err != nil {
		return 0, err
	}

	dir := c.Repo.FdroidRepoDir()
	n := 0
	for _, f := range missing {
		a, ok := assets[f.name]
		if !ok {
			// 账本说该有、Release 里没有。**不猜**（不按前缀找近似名）：这种情况只可能
			// 来自"账本比 Release 新"，而那是某次回写没落地。猜错的后果是把一个
			// 不属于这个版本的字节放进索引。
			c.Log("Release %s 里没有 asset %q（账本里有）—— 跳过", rel.TagName, f.name)
			continue
		}
		if err := fetchAsset(ctx, c, a, filepath.Join(dir, f.name)); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// fetchAsset 把一个 asset 下到 dst，并**顺手写回 cache**。
//
// 为什么下载成功就立刻写 cache，而不是等这一轮跑完再统一写：cache 的唯一作用是省掉
// 下一次下载，而"这一轮跑完"里包含 `fdroid update` —— 它可能因为与我们无关的原因失败
// （某条 metadata 不合它的意）。那种时候本轮下载是**好的**，没有任何理由把它一起丢掉。
//
// 原子写在这里也不是洁癖：`ensureLocal` 判"这个文件在不在"看的是**存在且非空**，
// 所以一个被中断的半截文件会被当成完整的。
func fetchAsset(ctx context.Context, c *Ctx, a gh.Asset, dst string) error {
	// 第二个返回值是 Content-Length，这里**不用**它（见下面的字节数核对）。
	rc, _, err := c.GH.DownloadAsset(ctx, c.Env.StoreRepo, a.ID)
	if err != nil {
		return fmt.Errorf("下载 asset %q：%w", a.Name, err)
	}
	defer rc.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".part-*")
	if err != nil {
		return fmt.Errorf("在 %s 下建临时文件：%w", filepath.Dir(dst), err)
	}
	tmpName := tmp.Name()
	// 成功路径下 rename 之后这次删除会失败，忽略即可。
	defer os.Remove(tmpName)

	written, err := io.Copy(tmp, rc)
	if err != nil {
		tmp.Close()
		return fmt.Errorf("把 asset %q 下到 %s：%w", a.Name, tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关 %s：%w", tmpName, err)
	}
	// 字节数按 **Release 元数据里的 size** 核，不按 Content-Length（那个未知时是
	// -1，拿它比对会在某些代理后面变成假失败）。
	//
	// 0 字节与"少了几个字节"在这里都是**必然的错误**，不是洁癖：一个空文件会被
	// ensureLocal 的"存在且非空"判成没有，于是下一轮再下一遍、再空一遍 ——
	// 一个永远不报错、也永远修不好的循环；而一个短一截的文件会被当成完整的收下，
	// 然后以 fdroidserver 解析失败的面目出现在很远的地方。
	if written == 0 || (a.Size > 0 && written != a.Size) {
		return fmt.Errorf("asset %q 下下来是 %d 字节，Release 里记的是 %d 字节 —— 下载被截断了",
			a.Name, written, a.Size)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("把 %s 改名为 %s：%w", tmpName, dst, err)
	}
	c.Log("下载 asset %s（%d 字节）", a.Name, written)

	writeToCache(c, dst, a.Name)
	return nil
}

// writeToCache 把刚落地的 APK 复制一份进 cache 目录。
//
// 失败**只记一行日志**，不返回错误：cache 是优化，没有它这一轮照样是对的
// （只是下一轮要重新下）。把"cache 没写进去"升级成"这一轮失败"，等于让一个纯优化项
// 有权把整个市场停掉。
func writeToCache(c *Ctx, src, name string) {
	if c.Env.APKCacheDir == "" {
		return
	}
	if err := os.MkdirAll(c.Env.APKCacheDir, 0o755); err != nil {
		c.Log("建 cache 目录 %s 失败：%v —— 只是下一轮要重下一遍", c.Env.APKCacheDir, err)
		return
	}
	if err := copyFile(src, filepath.Join(c.Env.APKCacheDir, name)); err != nil {
		c.Log("写 cache 失败：%v —— 只是下一轮要重下一遍", err)
		return
	}
	c.Log("写入 cache %s（%s）", name, c.Env.APKCacheDir)
}

// renderMetadata 为**有文件的**来源渲染 `<id>.yml`，并清掉不该存在的那些。
//
// "有文件才写"（§5.1 第 1 步）不是洁癖：一份没有对应 APK 的 metadata 在 fdroidserver
// 眼里是一个**没有版本的包** —— 它不进索引（索引是扫 APK 扫出来的），但那个 yml 会被
// 提交、会被人看到，于是下一个人会以为"这个应用在架子上，只是没版本"，而事实是
// 它一个分片都没凑齐。让文件名与 yml 一一对应，这一整类误解就不存在了。
func renderMetadata(c *Ctx, plans []repoPlan, files map[string]int, rep *model.Report) error {
	dir := c.Repo.FdroidMetadataDir()
	want := make(map[string]bool, len(plans))

	for i := range plans {
		src := plans[i].src
		if files[src.ID] == 0 {
			// 账本里有版本（否则 planRepo 就跳过它了），却一个分片都没凑齐 ——
			// 这一轮它会**从索引里消失**。这是必须吵的一句：症状在客户端是
			// "这个应用怎么没了"，而在服务端日志里只有这一行能把它接上。
			rep.Warnf(src.ID, "账本里有 %d 个版本，但一个分片都没凑齐（cache 里没有、也没下成）—— "+
				"本轮索引里不会有它，客户端那边表现为「这个应用消失了」（已经装了的不受影响）",
				len(src.Versions))
			continue
		}

		b, err := fdroid.Render(src)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, fdroid.MetadataFileName(src.ID)), b, 0o644); err != nil {
			return fmt.Errorf("写 metadata：%w", err)
		}
		want[src.ID] = true

		// 词表外的分类是软告警（02 §2.6）：不阻断，但必须有人看见 —— 否则新加的一个
		// 中文标签会静默地什么都不发生（映射表里没有它，于是 Categories 里也没有它）。
		if unk := fdroid.UnknownCategories(src); len(unk) > 0 {
			rep.Warnf(src.ID, "分类 %s 不在映射表里，本轮不写 Categories（02 §2.6「宁缺勿错」）—— "+
				"要它们进索引就得同时改 fdroid.CategoryMap 与 issue 模板的词表",
				strings.Join(unk, " / "))
		}
	}

	return sweepMetadata(c, dir, want)
}

// sweepMetadata 删掉 metadata/ 里不再该存在的 yml。
//
// 判据就是"不在这一轮的渲染集里"，因为那正是**过时**的定义 —— 三种成因都在这里合流：
// 来源被移除（文件删了）、被 `paused`、或者一个分片都没凑齐。而它们**必须**被删：
// 那个 yml 进 git、并且会被 fdroidserver 读，留着等于让一个已经不存在的应用永远
// 留在索引里，且没有任何一层会报错（yml 合法、APK 还在、索引照建）。
//
// 只动 `*.yml`：这个目录里的别的东西（别的扩展名、点开头的、子目录）不是我们的产物，
// "清理"应当是精确的。
func sweepMetadata(c *Ctx, dir string, want map[string]bool) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读 %s：%w", dir, err)
	}

	removed := 0
	for _, de := range entries {
		name := de.Name()
		if de.IsDir() || !strings.HasSuffix(name, ".yml") || strings.HasPrefix(name, ".") {
			continue
		}
		id := strings.TrimSuffix(name, ".yml")
		if want[id] {
			continue
		}
		path := filepath.Join(dir, name)
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("删除 %s：%w", path, err)
		}
		c.Log("清掉 %s（%s 已 paused / 已移除 / 一个分片都没有）", path, id)
		removed++
	}
	if removed > 0 {
		c.Log("metadata 清理：删掉 %d 个不该存在的 yml", removed)
	}
	return nil
}

// writeFdroidConfig 把 keystore 从 secret 解到一个**仓库之外**的临时文件，
// 再写 config.yml（0600）。返回的清理函数负责删掉那个临时文件。
//
// 两处"必须"值得写下来，它们都是安全边界而不是风格：
//
//   - keystore 落在**仓库之外**（`os.CreateTemp` 的默认目录就是系统临时目录）。
//     放在仓库里再靠 `.gitignore` 挡住，等于把"私钥不进 git"这件事交给一份**数据仓库里**
//     的文件来保证；放到仓库外之后，它连"被 git 看见"的机会都没有。
//   - config.yml 必须 **0600**，因为里面明文躺着 keystore 与私钥的口令。fdroidserver
//     自己也会检查这个权限并打一句 `unsafe permissions` 的警告（实测于 spike ①）——
//     它警告的不是格式，是"这个文件现在是同机其他用户可读的"。
func writeFdroidConfig(c *Ctx) (func(), error) {
	if err := c.Env.RequireKeystore(); err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(c.Env.KeystoreB64)
	if err != nil {
		return nil, fmt.Errorf("%s 不是合法 base64：%w —— 它应当是 `base64 -w0 <keystore>` 的输出",
			EnvKeystoreB64, err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("%s 解出来是 0 字节", EnvKeystoreB64)
	}

	ks, err := os.CreateTemp("", "repo-signing-*.jks")
	if err != nil {
		return nil, fmt.Errorf("建临时 keystore 文件：%w", err)
	}
	// CreateTemp 建出来的文件本来就是 0600。显式再设一次，是因为这条**必须**成立，
	// 而它成立与否只由一行代码决定 —— 将来有人把它换成 os.Create 时这一行会跟着报错，
	// 不换的话就只剩一句注释在拦着。
	if err := ks.Chmod(0o600); err != nil {
		ks.Close()
		os.Remove(ks.Name())
		return nil, fmt.Errorf("设置 %s 的权限：%w", ks.Name(), err)
	}
	if _, err := ks.Write(raw); err != nil {
		ks.Close()
		os.Remove(ks.Name())
		return nil, fmt.Errorf("写 %s：%w", ks.Name(), err)
	}
	if err := ks.Close(); err != nil {
		os.Remove(ks.Name())
		return nil, fmt.Errorf("关 %s：%w", ks.Name(), err)
	}
	// 清理函数在**临时文件建成之后**就装好，而不是等 config.yml 写完 ——
	// 从这个文件存在的第一刻起，它每多留一秒都是在赌。
	cleanup := func() { os.Remove(ks.Name()) }

	cfg, err := fdroid.RenderConfig(c.Endpoints.RepoURL, ks.Name(), c.Env.KeystorePass, c.Env.KeyPass)
	if err != nil {
		cleanup()
		return nil, err
	}
	path := c.Repo.FdroidConfigPath()
	if err := os.WriteFile(path, cfg, 0o600); err != nil {
		cleanup()
		return nil, fmt.Errorf("写 %s：%w", path, err)
	}
	// WriteFile 在文件**已存在**时不改权限（perm 只在创建那一刻用得上），
	// 而本地重跑时那个文件正是上一轮留下的 —— 于是它会带着上一轮的权限活下来。
	if err := os.Chmod(path, 0o600); err != nil {
		cleanup()
		return nil, fmt.Errorf("设置 %s 的权限：%w", path, err)
	}
	c.Log("写入 %s（0600，含签名口令，不进 git）", path)
	return cleanup, nil
}

// runFdroidUpdate 跑 `fdroid update --pretty`，**cwd 必须是 fdroid 工作区**。
//
// cwd 不是可选的（D64）：fdroidserver 只认相对 cwd 的 `metadata/`、`repo/`、`config.yml`，
// 没有任何参数能让它换目录 —— 所以那行 `cmd.Dir` 是契约的一部分，不是方便。
func runFdroidUpdate(ctx context.Context, c *Ctx) error {
	if _, err := exec.LookPath("fdroid"); err != nil {
		// 这条流水线只在 debian:trixie 里跑得起来（ubuntu-latest 自带的 fdroidserver
		// 2.2.1 解析现代 APK 会直接崩，实测于 spike ②）。而开发者在 Windows/macOS 上
		// 直接跑 `forge build-repo` 时看到的是 `executable file not found` ——
		// 那句话不告诉他"你该进容器"。
		return fmt.Errorf("找不到 `fdroid` 可执行文件 —— build-repo 必须在 debian:trixie 容器里跑"+
			"（fdroidserver 只有 Debian 包，且版本要够新）：%w", err)
	}

	cmd := exec.CommandContext(ctx, "fdroid", "update", "--pretty")
	cmd.Dir = c.Repo.FdroidDir()
	out, err := cmd.CombinedOutput()
	// 输出照打：fdroidserver 的 INFO 行里有"这一轮扫到了哪些 APK""用哪把钥匙签的"
	// 这些只能从它嘴里知道的事。口令不在里面 —— 它只回显指纹。
	c.LogBlock("[fdroid] ", string(out))
	if err != nil {
		// 错误里不重复贴输出（上面刚打过一遍）：Actions 日志里一次失败贴两遍同样的几百行，
		// 只会让人以为发生了两件事。
		return fmt.Errorf("`fdroid update` 失败（cwd=%s，退出码见上）：%w", cmd.Dir, err)
	}
	return nil
}

// copyProducts 把工作区里的产物拷进对外伺服的 `repo/`（§5.1 第 5 步）。
//
// 规则是"**除了 APK 全拷**"，而不是"拷那五六个已知的文件名"。理由是这份清单不由我们
// 定义：fdroidserver 每加一个版本就可能多出一类文件（实测已经并存着 `index-v1.jar` 与
// `index.jar`，还有 `index.html`/`index.xml`/`index.png`/`icons/`……），
// 而漏拷一个的症状是**客户端某个功能静默失效**（比如它按 `index-v1.jar` 走的兼容路径
// 拿到 404）。D59 明说不裁剪，所以这里用排除法而不是列举法。
//
// 排除的只有两样：
//
//	.apk     体积量级与 git 不合，而且它们本来就有家（各自 appId 的 Release）——
//	         见 store 的包注释。客户端拿到的 APK 地址由 CF 网关映射过去（02 §2.3）。
//	status/  fdroidserver 自己的运行状态（`running.json` **每轮都被重写**）。它不是
//	         仓库的一部分，拷出去只会让每一轮都产生一个无意义的 diff（spike ④）。
func copyProducts(c *Ctx) error {
	root := c.Repo.FdroidRepoDir()
	dst := c.Repo.RepoDir()

	var copied []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == "status" {
				return fs.SkipDir
			}
			return nil
		}
		if strings.EqualFold(filepath.Ext(p), naming.Ext) {
			return nil
		}
		out := filepath.Join(dst, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		if err := copyFile(p, out); err != nil {
			return err
		}
		copied = append(copied, rel)
		return nil
	})
	if err != nil {
		return fmt.Errorf("把产物拷进 %s：%w", dst, err)
	}

	// 排序只为了日志稳定：一份每轮顺序都不同的清单一来没法 diff，
	// 二来会让"这两轮产物一样吗"这个问题变得没法用眼睛回答。
	sort.Strings(copied)
	c.Log("产物 %d 个文件 → %s：\n  %s", len(copied), dst, strings.Join(copied, "\n  "))
	return nil
}

// copyFile 按**覆盖**语义复制一个文件，且保证落地的一定是完整内容。
//
// 先写同目录的临时文件再改名，与 store.WriteJSON 是同一个理由 ——
// 这里要复制的正是 APK 与索引，两者都有"读到一个半截文件却不报错"的下游。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".part-*")
	if err != nil {
		return fmt.Errorf("在 %s 下建临时文件：%w", filepath.Dir(dst), err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return fmt.Errorf("复制 %s → %s：%w", src, tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关 %s：%w", tmpName, err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("把 %s 改名为 %s：%w", tmpName, dst, err)
	}
	return nil
}

// ---- check-repo（02 §2.8 + 03 §5.3） -----------------------------------------

// CheckRepo 对**磁盘上的**产物跑 02 §2.8 的自检，返回一份报告。
// 硬错误（`HasErrors()`）就是"这一轮不回写"的判据（03 §5.3）。
//
// ⚠️ 它读的是**两份目录**，这一点容易看错：
//
//	根 `repo/`（c.Repo.RepoDir）          索引那一半：entry.jar / index-v2.json
//	fdroid 工作区（c.Repo.FdroidRepoDir） APK 那一半
//
// 之所以要跨两份，是因为**产物里按设计不带 APK**（见 copyProducts 与 store 的包注释）：
// 客户端的 APK 地址由 CF 网关映射到 Release asset。所以"索引里声明的东西在不在磁盘上"
// 这条检查只能对着工作区做 —— 对着根 `repo/` 做的话，它会对**每一个**包报错，
// 而那是设计如此，不是故障。
//
// 需要子进程的只有 `jarsigner -verify` 一处（PKCS#7 不适合手搓），所以这个动词与
// build-repo 一样依赖容器；其余全部是纯 Go（zip / JSON / sha256 / 磁盘比对）。
func CheckRepo(ctx context.Context, c *Ctx) (*model.Report, error) {
	rep := &model.Report{}
	repoDir := c.Repo.RepoDir()
	apkDir := c.Repo.FdroidRepoDir()

	// ---- ① entry.jar：它在不在、几个条目、验不验得过 ------------------------------
	//
	// 它是整条信任链的第一环：客户端先下它、验签、从中读出 entry.json、
	// 再按里面的 sha256 去校验 index-v2.json。所以它坏了不是"少一个文件"，
	// 是**所有客户端拒绝这个源**。
	entryJar := filepath.Join(repoDir, fdroid.EntryJarName)
	names, err := fdroid.JarEntries(entryJar)
	if err != nil {
		rep.Errorf("", "%v —— 它缺失或损坏时客户端会**拒绝整个源**，而不只是少一个文件", err)
		return rep, nil
	}
	// 按**恰好等于**断言而不是"至少"：多一个条目通常意味着有人往里面塞了东西，
	// 少一个意味着签名不完整。两种都是"看起来正常但客户端会拒绝"。
	if len(names) != fdroid.EntryJarEntryCount {
		rep.Errorf("", "%s 有 %d 个条目（%s），契约要求恰好 %d 个（entry.json + MANIFEST.MF + .SF + .RSA）",
			fdroid.EntryJarName, len(names), strings.Join(names, " / "), fdroid.EntryJarEntryCount)
	}
	if err := verifyEntryJar(ctx, entryJar); err != nil {
		rep.Errorf("", "%s 验签失败：%v", fdroid.EntryJarName, err)
	}

	// ---- ② entry.json 与它声明的索引 -------------------------------------------------
	entry, err := fdroid.ReadEntryJar(entryJar)
	if err != nil {
		rep.Errorf("", "%v", err)
	} else {
		if entry.Version != fdroid.IndexVersionV2 {
			rep.Errorf("", "entry.json 的 version 是 %d，契约要求 %d（02 §2.2）",
				entry.Version, fdroid.IndexVersionV2)
		}
		indexPath := filepath.Join(repoDir, fdroid.IndexV2Name)
		if sum, err := fdroid.SHA256File(indexPath); err != nil {
			rep.Errorf("", "算 %s 的 sha256 失败：%v", fdroid.IndexV2Name, err)
		} else if !strings.EqualFold(sum, entry.Index.SHA256) {
			// 这一条断了的表现**不是报错**，而是客户端认为这个源是坏的 ——
			// 而服务端这边一切看起来都正常。所以它必须是硬错误。
			rep.Errorf("", "entry.json 声明的 index.sha256 是 %s，而磁盘上 %s 的实际哈希是 %s —— "+
				"客户端会拒绝这个源", entry.Index.SHA256, fdroid.IndexV2Name, sum)
		}
		// `index.name` 本该是 `/index-v2.json`（带前导斜杠是 F-Droid 的写法）。
		// **只告警不阻断**：这个字段的取值没有本地实测背书（spike 只验到条目数与验签），
		// 而把一条没验过的常量当成硬错误，风险是它一旦与此处的假设不同，
		// **每一轮的回写都会被拦下**。
		if entry.Index.Name != "" && entry.Index.Name != "/"+fdroid.IndexV2Name {
			rep.Warnf("", "entry.json 的 index.name 是 %q，预期 %q —— 若客户端真的拿它去取索引，"+
				"那就是客户端取不到索引；这一条没在 CI 里实测过，先只告警",
				entry.Index.Name, "/"+fdroid.IndexV2Name)
		}
	}

	// ---- ③ index-v2.json 自身 -------------------------------------------------------
	idx, err := fdroid.ReadIndexV2(filepath.Join(repoDir, fdroid.IndexV2Name))
	if err != nil {
		rep.Errorf("", "%v", err)
		return rep, nil
	}
	// repo.address 是**客户端要用的那个地址的前半段**（它拿 `address + "/" + 文件名` 取
	// 每一个 APK）。它的唯一出处是 endpoints.json 的 repoUrl，fdroidserver 只是把它
	// 抄进这里 —— 于是"两处不一致"只可能来自一次失败的渲染或一次手改配置，
	// 而症状是所有 APK 404，且没有任何一层会为此报错。
	if idx.Repo.Address != c.Endpoints.RepoURL {
		rep.Errorf("", "索引里的 repo.address 是 %q，而 endpoints.json 的 repoUrl 是 %q —— "+
			"客户端是拿前者拼每个 APK 的地址的，不一致的表现是所有 APK 取不到",
			idx.Repo.Address, c.Endpoints.RepoURL)
	}
	checkPackages(c, rep, idx, apkDir)

	// ---- ④ 索引与 sources 的对照 -----------------------------------------------------
	checkSourcesCoverage(c, rep, idx)

	// ---- ⑤ 阈值告警（03 §3.3，只告警不阻断） ------------------------------------------
	// 事实源是**账本**而不是现场列一遍 Release：这一轮不下载只统计，不需要网络，
	// 而账本里的文件集正是 BuildIndex 这一轮从 Release 读到的那些。代价是"改名改坏了的
	// stray asset"不计入 —— 但那一种 BuildIndex 已经单独报过一条告警了。
	counts := make(map[string]int, len(c.Sources))
	for i := range c.Sources {
		n := 0
		for vi := range c.Sources[i].Versions {
			n += len(c.Sources[i].Versions[vi].Assets)
		}
		counts[c.Sources[i].ID] = n
	}
	rep.Addf("", model.CheckReleaseAssetCounts(counts))

	return rep, nil
}

// checkPackages 逐包逐版本验索引里的声明与磁盘是否一致。
func checkPackages(c *Ctx, rep *model.Report, idx *fdroid.IndexV2, apkDir string) {
	// 图标缺失是**聚合**成一条告警的，不是每包一条：按当前的决定（不做 fastlane 图标，
	// 靠 fdroidserver 自己生成的占位图，见计划）**每个包都没有图标**，逐包报就是
	// 12 条永远存在的告警 —— 而永远存在的告警等于没有告警（它们会把真的那条淹掉）。
	missingIcons := 0

	for _, pkg := range idx.SortedPackageIDs() {
		p := idx.Packages[pkg]

		// `_incoming` 是手动上传的**暂存队列**（03 §3.2），不是应用。它出现在索引里
		// 意味着那个 Release 的 asset 被当成了版本资产，而它随时会被清场搬走 ——
		// 客户端把它列出来之后会指向一个明天就不存在的版本。
		if pkg == model.IncomingTag {
			rep.Errorf(pkg, "%q 出现在索引里。它是手动上传的暂存 Release，不是应用（03 §3.1）—— "+
				"客户端会把它列成一个应用，而它的 asset 随时会被搬走", model.IncomingTag)
		}

		// metadata 的两个软告警（02 §2.8）。键名大小写按**不敏感**匹配：
		// 索引那一半是 fdroidserver 的产物，它的键形状我们只有规格那一份描述，
		// 而猜错的代价是每轮一条假告警 —— 那会让真告警失效。
		if s, ok := metadataString(p.Metadata, "summary"); ok && strings.TrimSpace(s) == "" {
			rep.Warnf(pkg, "索引里的 summary 是空的 —— 客户端列表里那一行小字会是空白")
		} else if !ok {
			rep.Warnf(pkg, "索引里没有 summary（= sources 里没填 desc）—— 客户端列表里那一行小字会是空白")
		}
		for _, cat := range metadataStrings(p.Metadata, "categories") {
			if !fdroid.IsFdroidCategory(cat) {
				rep.Warnf(pkg, "分类 %q 不在 F-Droid 的固定枚举里（02 §2.6）—— "+
					"客户端按枚举画筛选条，表外的值不会报错，只会让这个应用在任何分类下都找不到", cat)
			}
		}

		if len(p.Versions) == 0 {
			// 一个没有任何版本的包只可能来自"有 metadata、没有 APK"—— BuildRepo 已经
			// 想办法堵住了（没文件就不写 yml），所以走到这里说明磁盘上有个漏网的 yml。
			rep.Warnf(pkg, "索引里有这个包，但它一个版本都没有 —— 检查 store/fdroid/metadata/ 下"+
				"是不是有一份没有对应 APK 的 %s.yml", pkg)
		}

		abis := make(map[string]bool, len(p.Versions))
		for _, key := range p.SortedVersionKeys() {
			v := p.Versions[key]
			f := v.File

			// 键就是那个 APK 的 sha256（02 §2.3），与它自己声明的 sha256 必须是同一个值。
			// 这一条不查也不会有症状（客户端只认 file.sha256），但它是索引**内部**不一致
			// 的唯一征兆，而内部不一致通常意味着有人手改过索引。
			if !strings.EqualFold(key, f.SHA256) {
				rep.Errorf(pkg, "版本的键（%s）与该版本自己声明的 sha256（%s）不一致", key, f.SHA256)
			}

			path := filepath.Join(apkDir, filepath.FromSlash(f.Name))
			fi, err := os.Stat(path)
			if err != nil {
				rep.Errorf(pkg, "索引里的 %s 在磁盘上不存在（找的是 %s）—— 客户端下载它会 404",
					f.Name, path)
				continue
			}
			if fi.Size() != f.Size {
				rep.Errorf(pkg, "%s 的大小是 %d 字节，索引里记的是 %d", f.Name, fi.Size(), f.Size)
			}
			sum, err := fdroid.SHA256File(path)
			if err != nil {
				rep.Errorf(pkg, "算 %s 的 sha256 失败：%v", f.Name, err)
			} else if !strings.EqualFold(sum, f.SHA256) {
				// 客户端是**下载完之后**才校验这个值的，所以不一致的表现是
				// "下完了却装不上"，而不是取不到文件。
				rep.Errorf(pkg, "%s 的实际 sha256 是 %s，索引里记的是 %s —— "+
					"客户端会在下载完之后才发现对不上", f.Name, sum, f.SHA256)
			}

			// 三条件都来自 fdroidserver 从 APK 里**现读**的 manifest，所以它们为假
			// 说明那个 APK 自己有问题，而不是我们没提供这个值。
			if v.Manifest.VersionCode <= 0 {
				rep.Errorf(pkg, "%s 的 versionCode 是 %d（必须 > 0）—— "+
					"这个值是 fdroidserver 从 APK 里读的，所以是那个 APK 的 manifest 有问题",
					f.Name, v.Manifest.VersionCode)
			}
			if !v.HasNativeCodeOrUniversal() {
				rep.Errorf(pkg, "%s 既没有 nativecode、文件名也不以 -%s.apk 结尾 —— "+
					"前者说明它不是 universal 包，后者说明它不符合命名契约（02 §2.4）",
					f.Name, naming.ABIUniversal)
			}
			// 取**第一个**而不是要求"恰好一个"：一份 APK 理论上可以有多个签名者
			// （联合签名），fdroidserver 会把它们都记下来，而客户端比对签名时
			// 也是拿第一个。缺失或空串才说明"这个包没有上游签名"。
			if len(v.Manifest.Signer.SHA256) == 0 || strings.TrimSpace(v.Manifest.Signer.SHA256[0]) == "" {
				rep.Errorf(pkg, "%s 没有 signer —— 客户端要靠它与已装版本的签名比对来决定能不能升级",
					f.Name)
			}

			if _, abi, err := naming.Split(pkg, f.Name); err == nil {
				abis[abi] = true
			}
		}

		// 只有一个 ABI 变体：那台设备的用户可能根本装不上（比如上游只发了 arm64，
		// 而用老机器的人拿到的包与它不兼容）。**universal 不算** —— 一个包罗所有架构的
		// APK 是"只有一个变体"里最正常的一种，对它告警会让这条告警变成噪音。
		if len(abis) == 1 && !abis[naming.ABIUniversal] {
			for abi := range abis {
				rep.Warnf(pkg, "这个包只有 %s 一个 ABI 变体（没有 universal 包）—— "+
					"其它架构的设备装不上；若上游本来就没发别的变体，那是正常的", abi)
			}
		}

		if !hasIcon(c.Repo.RepoDir(), pkg) {
			missingIcons++
		}
	}

	if missingIcons > 0 {
		rep.Warnf("", "%d 个包没有图标（客户端列表里会是默认图标）。当前的决定是不做 fastlane 图标，"+
			"所以这是预期内的；要改就得让上游带上 fastlane/metadata 或另想办法喂图标", missingIcons)
	}
}

// checkSourcesCoverage 双向对照索引与 sources。
func checkSourcesCoverage(c *Ctx, rep *model.Report, idx *fdroid.IndexV2) {
	// 索引里有、sources 里没有：只可能来自"来源被删了但 metadata 还在"
	// （BuildRepo 的清理没跑到）或一条没有来源的 yml。前者的表现是
	// **一个已经被移除的应用继续存在于客户端列表里**。
	for _, pkg := range idx.SortedPackageIDs() {
		if pkg == model.IncomingTag || c.Source(pkg) != nil {
			continue
		}
		rep.Warnf(pkg, "索引里有它，但 sources/ 里没有这个条目 —— 检查 store/fdroid/metadata/%s.yml "+
			"是不是残留（移除一个来源要连它的 metadata 一起清掉，BuildRepo 的清理负责这件事）", pkg)
	}

	// 非 paused 的源不在索引里。这是**最难看的一种失败**：客户端那边少了一个应用，
	// 而服务端什么都不报。所以这里按"我们能不能说出原因"分岔，让日志里那句话
	// 直接指向一个可查的事实，而不是一句"可能有问题"。
	for i := range c.Sources {
		src := &c.Sources[i]
		if src.Paused {
			continue
		}
		if _, ok := idx.Packages[src.ID]; ok {
			continue
		}
		switch {
		case len(src.Versions) == 0:
			rep.Warnf(src.ID, "非 paused 的来源没进索引：账本是空的（还没镜像过任何版本）—— "+
				"跑一次全量对账看上游有没有可镜像的发布")
		default:
			rep.Warnf(src.ID, "非 paused 的来源没进索引，而账本里有 %d 个版本 —— "+
				"通常意味着这些分片一个都不在这台机器上（老版本只从 cache 取，cache 丢了就没了；"+
				"最新版本下载失败也一样）。BuildRepo 那一步应当已经为它单独报过一条",
				len(src.Versions))
		}
	}
}

// verifyEntryJar 跑 `jarsigner -verify`。
//
// 断言的是**正面的那一句**（`jar verified.`）而不只是退出码：jarsigner 在若干种
// "其实没验成"的情形下也会返回 0，而那一行是它真的验过之后才会打的。
//
// **不加 `-strict`**：那会把一堆与"签名对不对"无关的警告（自签证书链没被信任、
// 缺时间戳……）升级成非零退出码，而我们的 jar 正是自签的 —— 加了它，每一轮都会
// 失败在一个跟签名无关的理由上。
func verifyEntryJar(ctx context.Context, path string) error {
	if _, err := exec.LookPath("jarsigner"); err != nil {
		return fmt.Errorf("找不到 `jarsigner` —— check-repo 与 build-repo 一样要在容器里跑（需要 JDK）：%w", err)
	}
	out, err := exec.CommandContext(ctx, "jarsigner", "-verify", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("jarsigner 退出码非零：%w\n%s", err, out)
	}
	if !strings.Contains(string(out), "jar verified.") {
		return fmt.Errorf("jarsigner 没有报 `jar verified.`，完整输出是：\n%s", out)
	}
	return nil
}

// hasIcon 报告这个包在产物里有没有图标。
//
// 判据是 `icons/` 下有没有以此为前缀的文件：fdroidserver 给每个应用的图标命名成
// `<包名>.<版本或哈希>.png`，所以前缀匹配就够了，不需要知道它到底是什么后缀。
// 找不到目录时按"没有"处理 —— 那正是"一个图标都没有"这一轮的实际情况。
func hasIcon(repoDir, pkg string) bool {
	entries, err := os.ReadDir(filepath.Join(repoDir, "icons"))
	if err != nil {
		return false
	}
	for _, de := range entries {
		if de.IsDir() {
			continue
		}
		if name := de.Name(); strings.HasPrefix(name, pkg+".") {
			return true
		}
	}
	return false
}

// metadataString 在索引的 metadata 里按**大小写不敏感**的键取一个字符串。
//
// 返回值里的 ok 是"这个键在不在"，不是"值非不非空" —— 调用方要靠那个区分
// "没写 summary"和"写了但空"，两者在客户端上是同一片空白，但成因不同、修法也不同。
//
// 容忍两种形状（string 与 locale → string 的 map）：v2 索引里那几个人类可读的字段
// 是**本地化**的，出现哪种形状取决于 fdroidserver 的版本，而这里判错的代价是
// 一条假告警。取不到就不判，见 checkPackages 的说明。
func metadataString(md map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := lookupFold(md, key)
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	var byLocale map[string]string
	if err := json.Unmarshal(raw, &byLocale); err == nil {
		// 取字典序最小的那一个：本市场只有中文与默认值，稳定比"取对"更重要 ——
		// 这里判的是"有没有内容"，不是"内容对不对"。
		var keys []string
		for k := range byLocale {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if byLocale[k] != "" {
				return byLocale[k], true
			}
		}
		return "", true
	}
	return "", false
}

// metadataStrings 在索引的 metadata 里按大小写不敏感的键取一个字符串列表。
// 形状对不上时返回 nil（不判），理由同 metadataString。
func metadataStrings(md map[string]json.RawMessage, key string) []string {
	raw, ok := lookupFold(md, key)
	if !ok {
		return nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

func lookupFold(md map[string]json.RawMessage, key string) (json.RawMessage, bool) {
	if raw, ok := md[key]; ok {
		return raw, true
	}
	for k, raw := range md {
		if strings.EqualFold(k, key) {
			return raw, true
		}
	}
	return nil, false
}
