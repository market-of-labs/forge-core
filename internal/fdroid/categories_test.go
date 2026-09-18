package fdroid

import (
	"slices"
	"testing"

	"github.com/market-of-labs/forge-core/internal/model"
)

// TestVocabularyMatchesSpec 逐字钉住 §2.6 的固定枚举。
//
// 写死 24 个值而不是只数个数：这个列表是**客户端的筛选条**，写错一个字母不会报错，
// 只会让那个分类下的应用永远搜不到。而且它是从规格里抄来的，
// 抄错时唯一的提示就是这份 diff。
func TestVocabularyMatchesSpec(t *testing.T) {
	want := []string{
		"Ads", "Automation", "Connectivity", "Development", "Food", "Games", "Graphics",
		"Internet", "Money", "Multimedia", "Navigation", "Office", "Reading", "Religion",
		"Science", "Security", "Sports", "System", "Theming", "Time", "Writing",
		"Phone & SMS", "Calendar", "Weather",
	}
	if !slices.Equal(FdroidCategories, want) {
		t.Errorf("§2.6 的枚举对不上了。\n想：%q\n实得：%q", want, FdroidCategories)
	}
	if len(FdroidCategories) != 24 {
		t.Errorf("枚举有 %d 项，规格是 24 项", len(FdroidCategories))
	}
	// `Phone & SMS` 是唯一含 `&` 的值 —— 它正是 plainSafe 里那个 `&` 的来由，
	// 少了它 `Name`/`Categories` 的裸写规则就没有存在理由了。
	if !slices.Contains(FdroidCategories, "Phone & SMS") {
		t.Error("枚举里少了 `Phone & SMS` —— 注意 `&` 两边各一个空格，不是 `Phone&SMS`")
	}
}

// TestCategoryMapCoversTemplateVocabulary 钉住"映射表的键 == issue 模板的词表"。
//
// 这是**跨仓库契约的一半**：另一半（模板 yml 里的 options）由 `model` 包的直接读 yml
// 测试钉住。两条合起来才能保证"表单里出现过的标签，映射表都认识"。
//
// 报错时把两边都打出来，因为这条最常见的坏法就是"加了词表项忘了加映射"
// （或者反过来加了映射却没人选得到），它们要改的地方不同。
func TestCategoryMapCoversTemplateVocabulary(t *testing.T) {
	for _, c := range model.ReviewCategories {
		if _, ok := CategoryMap[c]; !ok {
			t.Errorf("issue 模板词表里有 %q，而 CategoryMap 不认识它 —— "+
				"申请人选了它之后会静默地得到一个没有分类的应用（并报一条软告警）", c)
		}
	}
	for k := range CategoryMap {
		if !slices.Contains(model.ReviewCategories, k) {
			t.Errorf("CategoryMap 里有 %q，而 issue 模板词表里没有 —— "+
				"这条映射永远走不到，是死代码（多半是词表改过而这里忘了跟）", k)
		}
	}
}

// TestCategoryMapTargetsAreInVocabulary 钉住映射表的**值**都落在 §2.6 的枚举里。
//
// 这是那张表里最容易打错字的地方（`Developer` vs `Development`），
// 而打错的后果完全静默 —— 所以让它在编译期的测试里响，而不是在客户端里。
//
// 空串是合法值：它是"刻意的无对应"（§2.6 宁缺勿错），见 CategoryMap 的注释。
func TestCategoryMapTargetsAreInVocabulary(t *testing.T) {
	for src, dst := range CategoryMap {
		if dst == "" {
			continue
		}
		if !IsFdroidCategory(dst) {
			t.Errorf("CategoryMap[%q] = %q，而它不在 §2.6 的枚举里 —— "+
				"运行期这会被判成 unknown（退化成「没有分类」），但那是兜底不是许可", src, dst)
		}
	}
}

// TestCategories 覆盖映射本身的四条路径。
func TestCategories(t *testing.T) {
	cases := []struct {
		name        string
		in          []string
		wantMapped  []string
		wantUnknown []string
	}{
		{"空输入", nil, nil, nil},
		{"单项", []string{"工具"}, []string{"System"}, nil},
		{"多项保持输入顺序", []string{"媒体", "工具"}, []string{"Multimedia", "System"}, nil},
		{"去重：同一个标签出现两次只写一次",
			[]string{"工具", "工具"}, []string{"System"}, nil},
		{"去重后仍保持首次出现的顺序（不是排序后）",
			[]string{"媒体", "工具", "媒体"}, []string{"Multimedia", "System"}, nil},
		{"空串被跳过", []string{"", "工具", ""}, []string{"System"}, nil},
		// `其他` 是**刻意**不映射的：它必须既不产分类、也不产告警。
		// 这条是最容易写错的一条 —— 见 Categories 里那三个分支的顺序。
		{"「其他」静默丢弃，不报 unknown", []string{"其他"}, nil, nil},
		{"「其他」与真分类混在一起时只丢它自己",
			[]string{"工具", "其他", "媒体"}, []string{"System", "Multimedia"}, nil},
		{"词表外的值进 unknown", []string{"工具", "读都没见过"}, []string{"System"}, []string{"读都没见过"}},
		{"unknown 去重且排序", []string{"zzz", "aaa", "zzz"}, nil, []string{"aaa", "zzz"}},
		{"空串不算 unknown（它只是没填）", []string{""}, nil, nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mapped, unknown := Categories(c.in)
			if !slices.Equal(mapped, c.wantMapped) {
				t.Errorf("mapped：想 %q，实得 %q", c.wantMapped, mapped)
			}
			if !slices.Equal(unknown, c.wantUnknown) {
				t.Errorf("unknown：想 %q，实得 %q", c.wantUnknown, unknown)
			}
		})
	}
}

// TestCategoriesTypoInMapBecomesUnknown 覆盖那个**平时走不到**的分支：
// 映射表自己指向一个枚举外的值。
//
// 为什么值得为它做一次全局变量改写：那条分支保护的是"有人改了 CategoryMap 并且打错字"
// 这个将来 —— 而它一旦生效，后果是索引里出现一个谁都不认的分类，
// **没有任何一层会报错**（fdroidserver 不校验 Categories 的取值）。
// 一个跑不到的兜底分支等于没有，所以要主动踩一次确认它真的在。
func TestCategoriesTypoInMapBecomesUnknown(t *testing.T) {
	const key = "工具" // 一个真实存在的键
	orig, had := CategoryMap[key]
	if !had {
		t.Fatalf("前置条件不成立：CategoryMap 里没有 %q", key)
	}
	t.Cleanup(func() { CategoryMap[key] = orig })

	CategoryMap[key] = "Developer" // `Development` 少写三个字母 —— 这就是要防的错法

	mapped, unknown := Categories([]string{key})
	if len(mapped) != 0 {
		t.Errorf("打错的目标值不该被写出去，实得 mapped=%q", mapped)
	}
	if !slices.Equal(unknown, []string{key}) {
		t.Errorf("打错的目标值应当报成 unknown=%q，实得 %q", []string{key}, unknown)
	}
}

// TestIsFdroidCategory 顺带钉住一个容易想当然的地方：**判定是逐字相等，不是大小写无关**。
func TestIsFdroidCategory(t *testing.T) {
	for _, yes := range []string{"System", "Phone & SMS", "Calendar"} {
		if !IsFdroidCategory(yes) {
			t.Errorf("IsFdroidCategory(%q) = false", yes)
		}
	}
	for _, no := range []string{"system", "SYSTEM", "Phone&SMS", "Phone &sms", "", "工具", "developer"} {
		if IsFdroidCategory(no) {
			t.Errorf("IsFdroidCategory(%q) = true —— 它不在枚举里（枚举是逐字比对）", no)
		}
	}
}

// TestCategoriesIsIdempotent 是个不变式：把已经映射好的值再喂进去不该产分类。
//
// 它挡的是"某天有人把 Categories 的输出又当输入用"（比如在 check-repo 里
// 拿索引里的 Categories 再映射一次）—— 那是 English 值，映射表不认识，
// 于是会静默地变成"没有分类"，而不是报错。
func TestCategoriesIsIdempotent(t *testing.T) {
	mapped, _ := Categories([]string{"工具", "媒体"})
	again, unknown := Categories(mapped)
	if len(again) != 0 {
		t.Errorf("二次映射产出了 %q —— 说明有人在把输出当输入用", again)
	}
	if len(unknown) == 0 {
		t.Error("二次映射应当把英文值报成 unknown（它是信号，不是噪声）")
	}
}
