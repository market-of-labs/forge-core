package fdroid

import "sort"

// FdroidCategories 是 02 §2.6 的固定枚举 —— **这就是可用值**，逐字照抄，
// 大小写与 `&` 两边的空格都算契约的一部分（`Phone & SMS` 不是 `Phone & SMS` 之外的任何写法）。
//
// 为什么值得抄成一个常量而不是散在映射表里：它同时是两个东西的事实源 ——
// ① 映射表的目标值必须落在它里面（`TestCategoryMapTargetsAreInVocabulary` 钉住），
// ② `check-repo` 判"Categories 里有词表外的值"这条软告警时要用它。
//
// 这不是本市场自己的词表 —— 客户端的筛选条是按 F-Droid 的枚举画的，写一个表外的值
// 不会报错，只会让那个应用在任何一个分类下都搜不到。
var FdroidCategories = []string{
	"Ads", "Automation", "Connectivity", "Development", "Food", "Games", "Graphics",
	"Internet", "Money", "Multimedia", "Navigation", "Office", "Reading", "Religion",
	"Science", "Security", "Sports", "System", "Theming", "Time", "Writing",
	"Phone & SMS", "Calendar", "Weather",
}

// IsFdroidCategory 报告 c 是否属于 §2.6 的固定枚举。
func IsFdroidCategory(c string) bool {
	for _, v := range FdroidCategories {
		if v == c {
			return true
		}
	}
	return false
}

// CategoryMap 把 sources/*.json 里的**中文自由词**映射到 F-Droid 的固定枚举（02 §2.6）。
//
// 键的集合就是 issue 模板那份勾选词表（`model.ReviewCategories`）—— 两者必须是同一套，
// `TestCategoryMapCoversTemplateVocabulary` 会逐项钉住这条。为什么不需要在这里"校验"它：
// 模板侧的选项由 GitHub 服务端先校验（只可能是那几个字面量），而 `sources/` 的唯一入口
// 就是那张表单（D48），所以表单到不了的值不必在此设防 —— 需要设防的是**将来加了词表项
// 却忘了加映射**，而那正是一条单测能钉死的事。
//
// ⚠️ 值写成**空串**是刻意的，含义是"这个标签不对应任何 F-Droid 分类"，
// 不是"忘了填"：§2.6 的判据是"小而保守，宁缺勿错"—— 写一个错的分类比不写更误导，
// 因为客户端会照着错的分类把应用列在错的筛选条下。
// 现有数据里的实际取值只有 `工具` / `效率` / `媒体` 三种（12 个来源里 10 个有分类），
// 其余几项是模板词表里存在但暂时没人选过的。
var CategoryMap = map[string]string{
	"工具": "System",
	"效率": "Office",
	"媒体": "Multimedia",
	"通讯": "Internet",
	"开发": "Development",
	"游戏": "Games",
	"其他": "", // 见上：无对应，不写 Categories
}

// Categories 把一份来源的 categories 映射成 F-Droid 分类。
//
// 返回两个值：
//
//	mapped  —— 要写进 metadata 的分类，**已去重、保持首次出现的顺序**
//	unknown —— 词表（CategoryMap 的键）里没有的原始值，调用方据此出软告警
//
// ⚠️ 映射表里写了、但结果**不在 FdroidCategories 里**的值会被当成 unknown 处理，
// 而不是照写出去。这是刻意的失败方向：`CategoryMap` 里打错一个字母
// （`Developer` 而不是 `Development`）后果是静默的 —— 索引里出现一个谁都不认的分类，
// 而没有任何一层会报错。宁可退化成"这个应用没有分类"，也不要写一个错的进去。
func Categories(src []string) (mapped, unknown []string) {
	seen := make(map[string]bool, len(src))
	for _, raw := range src {
		if raw == "" || seen[raw] {
			continue
		}
		seen[raw] = true

		// ⚠️ 三个分支的顺序是有讲究的，**不能**合并成一个 `!ok || !IsFdroidCategory(to)`：
		// `其他` 的映射值是空串，而空串不在枚举里 —— 合并写法会把它判成 unknown，
		// 于是每个带 `其他` 的应用每轮都报一条软告警，而那是**刻意设计的行为**不是异常。
		to, ok := CategoryMap[raw]
		if !ok {
			// 词表外：可能是数据里留了个从没见过的旧值 —— 值得报出来。
			unknown = append(unknown, raw)
			continue
		}
		if to == "" {
			// 刻意不映射（"宁缺勿错"），静默丢掉。
			continue
		}
		if !IsFdroidCategory(to) {
			// 映射表自己打错了（比如 `Developer` 而不是 `Development`）。
			// 报成 unknown 而不是照写出去 —— 见上面函数注释里的失败方向。
			unknown = append(unknown, raw)
			continue
		}
		mapped = append(mapped, to)
	}
	sort.Strings(unknown)
	return mapped, unknown
}
