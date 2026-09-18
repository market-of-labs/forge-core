// Package store 知道 `market-of-labs/store`（数据仓库）的磁盘布局，并负责它的读写。
//
// 布局（03 §2.1，F-Droid 路线）：
//
//	{root}/sources/{appId}.json        输入（元数据人填）+ 产物（`versions` 账本，forge 写）
//	{root}/store/endpoints.json       配置，地址（tag 模板 / 文件名模板 / repoUrl）
//	{root}/store/fdroid/              fdroidserver 的工作目录（cwd 必须是它）
//	{root}/store/fdroid/config.yml    产物，含签名口令 → 0600、不进 git
//	{root}/store/fdroid/metadata/     产物，<appId>.yml，进 git（它是可审阅的那一半）
//	{root}/store/fdroid/repo/         fdroidserver 扫出来的索引 + 它下过的 APK → 不进 git
//	{root}/repo/                      产物，对外伺服的索引（不含 APK，见下）
//
// ⚠️ `store/fdroid/repo/` 与根 `repo/` 是**两份**目录，名字撞在一起只是个巧合：
// 前者是 fdroidserver 的工作区（它在那里扫 APK、也在那里写索引），后者是**发给客户端的那一份**。
// 两者之间怎么搬，规则在 `job.BuildRepo` 里 —— 而那条规则是"**除了 APK 全搬**"。
//
// 为什么产物里不放 APK：APK 的体积量级（每个 20 MB）与 git 的存储方式不合，
// 而它们本来就已经有一个家 —— 各自 appId 的 Release（D13/D21）。所以根 `repo/` 里
// 只有索引，客户端拿到的 APK 地址由 CF 网关映射到 Release asset（02 §2.3）。
// 本机验证时要把 APK 手工摆进这个目录（计划第 6 条），`.gitignore` 里的
// `/repo/**/*.apk` 是给那种场合准备的纵深防御。
//
// 本包只做 IO：不判断业务规则、不碰网络。这样"文件在哪、怎么写"与"内容对不对"分开，
// 后者由 model.Validate 与 fdroid 包负责。
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/market-of-labs/forge-core/internal/model"
)

// 相对路径常量（03 §2.1）。
//
// 它们**不是**随手起的目录名：`store/fdroid/` 与它里面的 `metadata/`、`repo/`
// 都是 fdroidserver 的硬约定 —— `fdroid update` 只在 cwd 下认这两个名字，
// 没有参数能让它换（D64）。所以这几个字符串是**契约**，不只是一个偏好。
const (
	// SourcesDirName 是输入目录名。
	SourcesDirName = "sources"
	// SubDirName 是存放契约常量与 fdroid 工作区的子目录名。
	SubDirName = "store"
	// EndpointsName 是地址配置文件名。
	EndpointsName = "endpoints.json"
	// FdroidDirName 是 fdroidserver 的工作目录名（在 store/ 下）。
	FdroidDirName = "fdroid"
	// RepoDirName 是对外伺服的产物目录名 —— fdroidserver 在 fdroid/ 下产出的
	// `repo/` 也用它，因为那正是被拷到根目录的那一份东西。
	RepoDirName = "repo"
	// MetadataDirName 是 fdroidserver 读元数据的目录名。
	MetadataDirName = "metadata"
)

// Repo 是一个 store 仓库的工作副本。
type Repo struct {
	// Root 是仓库根目录的绝对路径。
	Root string
}

// SourcesDir 返回 sources/ 目录。
func (r Repo) SourcesDir() string { return filepath.Join(r.Root, SourcesDirName) }

// EndpointsPath 返回 store/endpoints.json 的路径。
func (r Repo) EndpointsPath() string { return filepath.Join(r.Root, SubDirName, EndpointsName) }

// FdroidDir 返回 fdroidserver 的工作目录（= `fdroid update` 的 cwd）。
func (r Repo) FdroidDir() string { return filepath.Join(r.Root, SubDirName, FdroidDirName) }

// FdroidMetadataDir 返回 metadata/ 目录：里面每个 `<appId>.yml` 都是 forge 写的产物。
//
// 它是 fdroid 工作区里**唯一进 git** 的子目录 —— 索引是可重建的，而元数据里
// 有人填过的信息（名字、简介、作者、分类）只在这里。
func (r Repo) FdroidMetadataDir() string {
	return filepath.Join(r.FdroidDir(), MetadataDirName)
}

// FdroidRepoDir 返回 fdroidserver 的工作区产物目录（索引 + 本轮下的 APK）。
func (r Repo) FdroidRepoDir() string { return filepath.Join(r.FdroidDir(), RepoDirName) }

// FdroidConfigPath 返回 config.yml 的路径。
func (r Repo) FdroidConfigPath() string { return filepath.Join(r.FdroidDir(), "config.yml") }

// FdroidDirRel / FdroidMetadataDirRel 返回这两个目录**相对仓库根**的路径。
//
// 回写白名单要的是相对路径（git 的 pathspec），而"布局长什么样"这件事只该在这里有一份 ——
// 让 `job.Ctx.TrackedPaths` 自己拼 `"store" + "/" + "fdroid"` 就等于把目录形状写了两遍。
//
// ⚠️ 分隔符是**正斜杠**（`path` 而不是 `filepath`）：它是 git pathspec 的形状，
// 而 git 在任何平台上都只认正斜杠。Windows 上拼出反斜杠会让这一条静默不匹配任何文件 ——
// 也就是"回写白名单少了一格"，而少了的那一格不报错，只是那些文件永远提交不上去。
func FdroidDirRel() string { return path.Join(SubDirName, FdroidDirName) }

// FdroidMetadataDirRel 是 metadata/ 相对仓库根的路径，也是 fdroid 工作区里**唯一**
// 该进 git 的那部分（见 job.Ctx.TrackedPaths）。
func FdroidMetadataDirRel() string { return path.Join(FdroidDirRel(), MetadataDirName) }

// RepoDir 返回**对外伺服**的产物目录（= 根目录下的 `repo/`）。
func (r Repo) RepoDir() string { return filepath.Join(r.Root, RepoDirName) }

// SourcePath 返回某个 App 的 sources 文件路径。
func (r Repo) SourcePath(id string) string {
	return filepath.Join(r.SourcesDir(), id+".json")
}

// ---- 读 ---------------------------------------------------------------------

// LoadEndpoints 读地址配置。它是**人改的配置**，所以读完立刻验一遍：
// 模板与命名契约分叉是静默故障（02 §2.4），必须在最早的时机炸出来。
func (r Repo) LoadEndpoints() (model.Endpoints, error) {
	var ep model.Endpoints
	if err := readJSON(r.EndpointsPath(), &ep); err != nil {
		return ep, err
	}
	if err := ep.Validate(); err != nil {
		return ep, fmt.Errorf("%s：%w", r.EndpointsPath(), err)
	}
	return ep, nil
}

// LoadSources 读 sources/ 下的全部条目，按 id 排序。
//
// 文件名必须等于 `{id}.json`，否则**直接失败**：写错名字的后果不是报错而是静默漏掉
// （按文件名取 id，对不上就永远不被处理），所以这是一条硬规则。
func (r Repo) LoadSources() ([]model.Source, error) {
	entries, err := os.ReadDir(r.SourcesDir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读 %s：%w", r.SourcesDir(), err)
	}

	var out []model.Source
	for _, de := range entries {
		name := de.Name()
		if de.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		var s model.Source
		path := filepath.Join(r.SourcesDir(), name)
		if err := readJSON(path, &s); err != nil {
			return nil, err
		}
		if err := s.Validate(name); err != nil {
			return nil, fmt.Errorf("%s：%w", path, err)
		}
		out = append(out, s)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ---- 写 ---------------------------------------------------------------------
//
// ⚠️ 这里**没有 WriteRepo**：索引不是本包写的，是 `fdroid update` 写在
// `store/fdroid/repo/` 下、再由 `job.BuildRepo` 搬出去的。本包能写的只有
// "我们来之前就存在、且下一个来的人会读"的那两样 —— sources 与 metadata。

// WriteSource 写一个 sources 条目。
//
// 两条路都走它：issue 流程（03 §2.5 规则 2：只改申请涉及的字段）与对账写回 `versions`
// 账本。**写的是整个结构体**，所以：
//
//   - 写回去的内容 = 加载时的内存快照。一次运行期间有人在网页上改同一个文件，
//     会被这一份盖掉。这是已知且接受的 —— 数据源只有 issue 与 workflow 两条入口
//     （forge 自己就是写入方），不存在"人手改文件与对账并发"这种场景。
//   - JSON 里不认识的键会被丢掉（解析时就丢了）。同上，没有第三方往这些文件里加键。
//   - 键序 = 结构体字段序，所以人手写的文件第一次被改写时会整篇重排一次，
//     之后每次都是同一种形状（稳定的 diff 是刻意要的）。
func (r Repo) WriteSource(s *model.Source) error {
	if err := s.Validate(s.ID + ".json"); err != nil {
		return err
	}
	return WriteJSON(r.SourcePath(s.ID), s)
}

// DeleteSource 移除一个 App（03 §2.5 规则 4：移除 = 删文件，不是置标志）。
//
// ⚠️ 它的后果比 paused **重**，而且重在这一份文件本身：sources/*.json 里躺着
// `versions` 账本，而账本是记录"这个应用镜像过哪些版本"的唯一地方（见 model.Version）。
// 删掉它，那些版本号与 ABI 分片就再没有地方能告诉你了 —— 而 APK 还躺在 Release 与
// CI cache 里，于是变成一堆**没人认领的文件**。
//
// 所以：想"停更但留着这条来源与它的账本"用 `paused`；只有确实要彻底移除才用这个。
// 两条路在客户端看来是一样的（下一轮索引里都没有它 —— 客户端同步后会从列表里消失，
// 已安装的不会被卸载，只是再也收不到更新）。
func (r Repo) DeleteSource(id string) error {
	path := r.SourcePath(id)
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("删除 %s：%w", path, err)
	}
	return nil
}

// ---- 通用 JSON IO -----------------------------------------------------------

// readJSON 读并解析一个 JSON 文件。
func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s：JSON 解析失败：%w", path, err)
	}
	return nil
}

// WriteJSON 以**原子**方式写一个 JSON 文件：先写同目录下的临时文件，再改名覆盖。
//
// 为什么要原子：这些文件由 workflow 写、可能被并发触发（dispatch + 每天的对账），
// 而且**中断的代价不对称** —— 一份写了一半的 JSON 既不是旧值也不是新值，而读到它的那个
// 词法错误**指不到真正的原因**（写它的人早就不在场了）；rename 在 POSIX 与 Windows 上
// 都是"要么旧内容、要么新内容"，没有中间态。
//
// 缩进 2 空格 + 结尾换行：这两个文件是**给人 review 的**，diff 可读性优先。
// 关掉 HTML 转义：默认会把 `&` 写成 `&`，在数据文件里纯属噪声（语义完全等价）。
func WriteJSON(path string, v any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("建目录 %s：%w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("在 %s 下建临时文件：%w", dir, err)
	}
	tmpName := tmp.Name()
	// 无论成功与否都尽量不留下垃圾；成功路径下 rename 之后这个 Remove 会失败，忽略即可。
	defer os.Remove(tmpName)

	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		tmp.Close()
		return fmt.Errorf("写 %s：%w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关 %s：%w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("把 %s 改名为 %s：%w", tmpName, path, err)
	}
	return nil
}
