package job

import (
	"fmt"

	"github.com/market-of-labs/forge-core/internal/manifest"
	"github.com/market-of-labs/forge-core/internal/model"
	"github.com/market-of-labs/forge-core/internal/store"
)

// BuildManifest 合成 apps.json（03 §5.1）并落盘。
//
// **它刻意不做 02 §2.8 的条目级校验** —— 那是 check-manifest 的活，两者在 workflow 里
// 是相邻的两步，失败原因因此可区分：
//
//	build-manifest  失败 = 输入有问题（模板非法 / 地址渲染不出来）
//	check-manifest  失败 = 产出有问题（生成的东西违反了清单契约）
//
// 把它们合成一步的话，一条"渲染地址失败"与一条"versionCode 缺失"会以同样的面目出现，
// 而修法完全不同。
//
// 不写半成品条目这件事在 manifest.Build 内部就做掉了（03 §5.4：没有版本的 App 压根不出条目），
// 因为那是"怎么产出"的一部分，不是"产出对不对"。
func BuildManifest(c *Ctx) (*model.Manifest, *model.Report, error) {
	// 先看 sources 集合自身：id 重复、kind:"companion" 多于一条这类跨条目问题
	// 在这里就必须报出来 —— 它们是**输入**的错，且会让产出静默地不好用。
	setRep := model.CheckSourceSet(c.Sources)

	m, buildRep, err := manifest.Build(manifest.Input{
		Sources:   c.Sources,
		Endpoints: c.Endpoints,
	})
	if err != nil {
		return nil, setRep, err
	}
	setRep.Addf("", buildRep)

	if err := store.WriteJSON(c.Repo.ManifestPath(), m); err != nil {
		return nil, setRep, fmt.Errorf("写 %s：%w", c.Repo.ManifestPath(), err)
	}
	c.Log("写入 %s：%d 条（%d 个 sources 条目）",
		c.Repo.ManifestPath(), len(m.Apps), len(c.Sources))
	return m, setRep, nil
}

// CheckManifest 读回**磁盘上的** apps.json 并跑 02 §2.8 的全部规则（03 §5.3）。
//
// 关键在于它是"读回来再查"而不是"查内存里刚生成的那份"：这样它同时覆盖了
// 手工编辑、上一次跑残留的文件、以及写入过程本身出错这三种情况。
// 返回的 report 里 HasErrors() 为真时，调用方必须阻止 commit-back —— 这是
// 03 §5.3「除硬错误外只告警不阻断」里"硬错误"的唯一落点。
func CheckManifest(c *Ctx) (*model.Manifest, *model.Report, error) {
	m, err := c.Repo.LoadManifest()
	if err != nil {
		return nil, nil, fmt.Errorf("读 %s：%w", c.Repo.ManifestPath(), err)
	}

	rep := m.Validate(c.Endpoints)

	// 跨条目的口径（规则 9：kind:"companion" 至多一条）在清单侧也再查一遍：
	// 设备端是"取第一条并日志告警"，所以这里是告警而非阻断 —— 多出来的那条
	// 不会让设备端坏掉，只是行为不确定。
	rep.Addf("", model.CheckSourceSet(c.Sources))

	c.Log("自检 %s：%d 条，%d 个错误 / %d 个告警",
		c.Repo.ManifestPath(), len(m.Apps), len(rep.Errors()), len(rep.Warnings()))
	return m, rep, nil
}

// CheckEntries 对一个**尚未落盘**的清单跑条目级校验。
// 给 build-manifest 的 --check 预演模式与测试用。
func CheckEntries(m *model.Manifest, ep model.Endpoints) *model.Report {
	if m == nil {
		return &model.Report{}
	}
	return m.Validate(ep)
}
