// Package store 知道 `market-of-labs/store`（数据仓库）的磁盘布局，并负责它的读写。
//
// 布局（03 §2.1）：
//
//	{root}/sources/{appId}.json     输入，人维护
//	{root}/apps.json                产物，唯一直接伺服给客户端的文件
//	{root}/store/index.json         产物，版本账本
//	{root}/store/endpoints.json     配置，地址模板
//
// 本包只做 IO：不判断业务规则、不碰网络。这样"文件在哪、怎么写"与"内容对不对"分开，
// 后者由 model.Validate 负责。
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/market-of-labs/forge-core/internal/model"
)

// 相对路径常量。apps.json 在**根目录**而不是 store/ 子目录 —— 它的路径就是设备内置的
// 默认清单地址，放进子目录会让地址出现 `store/store` 这种仓库名与目录名重复的段（03 §2.1）。
const (
	// SourcesDirName 是输入目录名。
	SourcesDirName = "sources"
	// ManifestName 是清单文件名（在根目录）。
	ManifestName = "apps.json"
	// SubDirName 是存放其余产物与契约常量的子目录名。
	SubDirName = "store"
	// IndexName 是索引文件名。
	IndexName = "index.json"
	// EndpointsName 是地址模板文件名。
	EndpointsName = "endpoints.json"
)

// Repo 是一个 store 仓库的工作副本。
type Repo struct {
	// Root 是仓库根目录的绝对路径。
	Root string
}

// SourcesDir 返回 sources/ 目录。
func (r Repo) SourcesDir() string { return filepath.Join(r.Root, SourcesDirName) }

// ManifestPath 返回 apps.json 的路径。
func (r Repo) ManifestPath() string { return filepath.Join(r.Root, ManifestName) }

// IndexPath 返回 store/index.json 的路径。
func (r Repo) IndexPath() string { return filepath.Join(r.Root, SubDirName, IndexName) }

// EndpointsPath 返回 store/endpoints.json 的路径。
func (r Repo) EndpointsPath() string { return filepath.Join(r.Root, SubDirName, EndpointsName) }

// SourcePath 返回某个 App 的 sources 文件路径。
func (r Repo) SourcePath(id string) string {
	return filepath.Join(r.SourcesDir(), id+".json")
}

// ---- 读 ---------------------------------------------------------------------

// LoadEndpoints 读地址模板。它是**人改的配置**，所以读完立刻验一遍：
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

// LoadIndex 读版本账本。文件不存在时返回一份空索引而不是错误 ——
// 索引是**派生数据**（03 §2.4：可从 Release 完全重建），首次跑或刚清空时它本就不存在。
func (r Repo) LoadIndex() (*model.Index, error) {
	ix := &model.Index{}
	err := readJSON(r.IndexPath(), ix)
	if os.IsNotExist(err) {
		return &model.Index{}, nil
	}
	if err != nil {
		return nil, err
	}
	return ix, nil
}

// LoadManifest 读清单。
func (r Repo) LoadManifest() (*model.Manifest, error) {
	m := &model.Manifest{}
	if err := readJSON(r.ManifestPath(), m); err != nil {
		return nil, err
	}
	return m, nil
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

// WriteManifest 写清单。
func (r Repo) WriteManifest(m *model.Manifest) error { return WriteJSON(r.ManifestPath(), m) }

// WriteIndex 写索引。
func (r Repo) WriteIndex(ix *model.Index) error { return WriteJSON(r.IndexPath(), ix) }

// WriteSource 写一个 sources 条目。只用于 issue 流程（03 §2.5 规则 2：只改申请涉及的字段）——
// 日常路径下 sources/ 完全由人维护，forge 不碰。
func (r Repo) WriteSource(s *model.Source) error {
	if err := s.Validate(s.ID + ".json"); err != nil {
		return err
	}
	return WriteJSON(r.SourcePath(s.ID), s)
}

// DeleteSource 移除一个 App（03 §2.5 规则 4：移除 = 删文件，不是置标志）。
//
// 注意语义（02 §2.9）：这只影响维护侧，**设备端不会传播** —— deep-link 的 import()
// 没有删除动作，已经同步过的设备上那一行原样留着。想停更就用 paused，别调这个。
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
// 而且**中断的代价不对称** —— 一份写了一半的 apps.json 会被客户端当成有效清单拉走，
// 而 rename 在 POSIX 与 Windows 上都是"要么旧内容、要么新内容"，没有中间态。
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
