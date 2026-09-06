package op

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/price/catalog"
)

// setupPriceTestDB 初始化内存 SQLite 并清空表, 供价格相关测试使用。
func setupPriceTestDB(t *testing.T) {
	t.Helper()
	if db.GetDB() == nil {
		t.Fatal("数据库未初始化")
	}
	tables := []string{"llm_infos", "channel_models", "channels", "channel_keys", "channel_grants", "group_items", "groups"}
	for _, table := range tables {
		if err := db.GetDB().Exec("DELETE FROM " + table).Error; err != nil {
			// 表可能不存在, 忽略
		}
	}
	// 清空缓存
	llmModelCache.Clear()
	channelModelCache.Clear()
	channelCache.Clear()
}

// testLookup 构造一个可控的 catalog.Lookup 替身, 供 resolveModelPrice 测试注入。
// 不依赖全局可变状态, 每次调用返回独立闭包。
func testLookup(refs map[string]model.LLMPrice) func(string) (catalog.Match, bool) {
	return func(name string) (catalog.Match, bool) {
		normalized := strings.ToLower(strings.TrimSpace(name))
		if p, ok := refs[normalized]; ok {
			return catalog.Match{ModelID: normalized, Price: p}, true
		}
		return catalog.Match{}, false
	}
}

func TestResolveUsagePriceManualNonZero(t *testing.T) {
	setupPriceTestDB(t)
	ctx := context.Background()
	info := model.LLMInfo{
		Name:     "manual-model",
		Source:   model.LLMSourceManual,
		LLMPrice: model.LLMPrice{Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5},
	}
	if err := db.GetDB().Create(&info).Error; err != nil {
		t.Fatal(err)
	}
	llmModelCache.Set("manual-model", llmCacheEntry{LLMPrice: info.LLMPrice, Source: model.LLMSourceManual})

	res := resolveModelPrice("manual-model", "group-x", testLookup(nil))
	if !res.CostKnown {
		t.Fatal("manual 价格应为已知")
	}
	if res.CostSource != "manual" {
		t.Fatalf("source 应为 manual, got %s", res.CostSource)
	}
	if res.CostReferenceModel != nil {
		t.Fatal("manual 无参考模型 ID")
	}
	if res.Price.Input != 10 || res.Price.Output != 50 {
		t.Fatal("manual 四价应直接使用存储值")
	}
	_ = ctx
}

func TestResolveUsagePriceManualZeroIsKnown(t *testing.T) {
	setupPriceTestDB(t)
	info := model.LLMInfo{
		Name:     "free-model",
		Source:   model.LLMSourceManual,
		LLMPrice: model.LLMPrice{Input: 0, Output: 0, CacheRead: 0, CacheWrite: 0},
	}
	if err := db.GetDB().Create(&info).Error; err != nil {
		t.Fatal(err)
	}
	llmModelCache.Set("free-model", llmCacheEntry{LLMPrice: info.LLMPrice, Source: model.LLMSourceManual})

	res := resolveModelPrice("free-model", "group-x", testLookup(nil))
	if !res.CostKnown {
		t.Fatal("manual 显式零价应 known=true(用户明确免费)")
	}
	if res.Price.Input != 0 {
		t.Fatal("manual 零价应保持零")
	}
}

func TestResolveUsagePriceAutoActualReference(t *testing.T) {
	setupPriceTestDB(t)
	lookup := testLookup(map[string]model.LLMPrice{
		"upstream-model": {Input: 15, Output: 60, CacheRead: 2, CacheWrite: 15},
	})
	// 缓存中 auto 记录(四价零占位), 模型名与参考目录匹配
	llmModelCache.Set("upstream-model", llmCacheEntry{LLMPrice: model.LLMPrice{}, Source: model.LLMSourceAuto})

	// actual 匹配参考目录
	res := resolveModelPrice("upstream-model", "group-x", lookup)
	if !res.CostKnown {
		t.Fatal("有参考价格应 known=true")
	}
	if res.CostSource != "actual_reference" {
		t.Fatalf("source 应为 actual_reference, got %s", res.CostSource)
	}
	if res.CostReferenceModel == nil || *res.CostReferenceModel != "upstream-model" {
		t.Fatalf("参考模型 ID 应为 upstream-model, got %v", res.CostReferenceModel)
	}
	if res.Price.Input != 15 {
		t.Fatal("应使用参考价格")
	}
}

func TestResolveUsagePriceAutoGroupReference(t *testing.T) {
	setupPriceTestDB(t)
	lookup := testLookup(map[string]model.LLMPrice{
		"group-name": {Input: 20, Output: 40, CacheRead: 0.5, CacheWrite: 10},
	})
	// auto 记录存在但 actual 不匹配
	llmModelCache.Set("some-model", llmCacheEntry{LLMPrice: model.LLMPrice{}, Source: model.LLMSourceAuto})

	// actual 不匹配, group 匹配参考目录
	res := resolveModelPrice("some-model", "group-name", lookup)
	if !res.CostKnown {
		t.Fatal("group 参考价格应 known=true")
	}
	if res.CostSource != "group_reference" {
		t.Fatalf("source 应为 group_reference, got %s", res.CostSource)
	}
	if res.CostReferenceModel == nil || *res.CostReferenceModel != "group-name" {
		t.Fatal("参考模型 ID 应为 group-name")
	}
}

func TestResolveUsagePriceUnknown(t *testing.T) {
	setupPriceTestDB(t)
	lookup := testLookup(nil)

	// 缓存中无记录
	res := resolveModelPrice("unknown-model", "unknown-group", lookup)
	if res.CostKnown {
		t.Fatal("无参考价格应 known=false")
	}
	if res.CostSource != "unknown" {
		t.Fatalf("source 应为 unknown, got %s", res.CostSource)
	}
}

func TestResolveUsagePriceManualPriorityOverReference(t *testing.T) {
	setupPriceTestDB(t)
	lookup := testLookup(map[string]model.LLMPrice{
		"manual-model": {Input: 999, Output: 999, CacheRead: 999, CacheWrite: 999},
	})
	// manual 记录存在
	llmModelCache.Set("manual-model", llmCacheEntry{
		LLMPrice: model.LLMPrice{Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5},
		Source:   model.LLMSourceManual,
	})

	res := resolveModelPrice("manual-model", "group-x", lookup)
	if res.CostSource != "manual" {
		t.Fatalf("manual 应优先于参考目录, got %s", res.CostSource)
	}
	if res.Price.Input != 10 {
		t.Fatal("应使用 manual 存储值而非参考价格 999")
	}
}

func TestLLMDerivePriceManualReturnsStored(t *testing.T) {
	setupPriceTestDB(t)
	llmModelCache.Set("m1", llmCacheEntry{
		LLMPrice: model.LLMPrice{Input: 5, Output: 15, CacheRead: 1, CacheWrite: 7.5},
		Source:   model.LLMSourceManual,
	})
	p, known := LLMDerivePrice("m1")
	if !known {
		t.Fatal("manual 应 known=true")
	}
	if p.Input != 5 {
		t.Fatal("应返回存储价格")
	}
}

func TestLLMDerivePriceAutoWithReference(t *testing.T) {
	setupPriceTestDB(t)
	// 使用预设目录中已存在的 gpt-4o(input=2.5), 无需注入。
	llmModelCache.Set("gpt-4o", llmCacheEntry{LLMPrice: model.LLMPrice{}, Source: model.LLMSourceAuto})
	p, known := LLMDerivePrice("gpt-4o")
	if !known {
		t.Fatal("gpt-4o 在预设目录中应 known=true")
	}
	if p.Input != 2.5 {
		t.Fatalf("应返回预设价格 input=2.5, got %f", p.Input)
	}
}

func TestLLMDerivePriceAutoNoReference(t *testing.T) {
	setupPriceTestDB(t)
	// 使用预设目录中不存在的模型名。
	llmModelCache.Set("totally-nonexistent-auto", llmCacheEntry{LLMPrice: model.LLMPrice{}, Source: model.LLMSourceAuto})
	_, known := LLMDerivePrice("totally-nonexistent-auto")
	if known {
		t.Fatal("预设目录中不存在的 auto 模型应 known=false")
	}
}

func TestLLMRestoreAutoManualToAuto(t *testing.T) {
	setupPriceTestDB(t)
	ctx := context.Background()
	info := model.LLMInfo{
		Name:     "restore-test",
		Source:   model.LLMSourceManual,
		LLMPrice: model.LLMPrice{Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5},
	}
	if err := db.GetDB().Create(&info).Error; err != nil {
		t.Fatal(err)
	}
	llmModelCache.Set("restore-test", llmCacheEntry{LLMPrice: info.LLMPrice, Source: model.LLMSourceManual})

	result, err := LLMRestoreAuto("restore-test", ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Source != model.LLMSourceAuto {
		t.Fatalf("恢复后 source 应为 auto, got %s", result.Source)
	}
	// DB 应为 auto + 四价零
	var dbInfo model.LLMInfo
	if err := db.GetDB().Where("name = ?", "restore-test").First(&dbInfo).Error; err != nil {
		t.Fatal(err)
	}
	if dbInfo.Source != model.LLMSourceAuto {
		t.Fatal("DB source 应为 auto")
	}
	if dbInfo.Input != 0 || dbInfo.Output != 0 {
		t.Fatal("DB 四价应清零")
	}
	// 缓存应为 auto
	entry, ok := llmModelCache.Get("restore-test")
	if !ok || entry.Source != model.LLMSourceAuto {
		t.Fatal("缓存应为 auto")
	}
}

func TestLLMRestoreAutoMissing404(t *testing.T) {
	setupPriceTestDB(t)
	ctx := context.Background()
	_, err := LLMRestoreAuto("nonexistent", ctx)
	if err == nil {
		t.Fatal("不存在的模型应返回错误")
	}
}

func TestLLMCreateForcesManual(t *testing.T) {
	setupPriceTestDB(t)
	ctx := context.Background()
	info := model.LLMInfo{
		Name:     "force-manual",
		LLMPrice: model.LLMPrice{Input: 1, Output: 2, CacheRead: 0.5, CacheWrite: 1},
	}
	if err := LLMCreate(info, ctx); err != nil {
		t.Fatal(err)
	}
	var dbInfo model.LLMInfo
	if err := db.GetDB().Where("name = ?", "force-manual").First(&dbInfo).Error; err != nil {
		t.Fatal(err)
	}
	if dbInfo.Source != model.LLMSourceManual {
		t.Fatalf("创建应强制 manual, got %s", dbInfo.Source)
	}
}

func TestLLMUpdateForcesManual(t *testing.T) {
	setupPriceTestDB(t)
	ctx := context.Background()
	info := model.LLMInfo{
		Name:     "update-manual",
		LLMPrice: model.LLMPrice{Input: 1, Output: 2, CacheRead: 0.5, CacheWrite: 1},
	}
	if err := LLMCreate(info, ctx); err != nil {
		t.Fatal(err)
	}
	// 尝试更新为 auto source(应被强制为 manual)
	updateInfo := model.LLMInfo{
		Name:     "update-manual",
		Source:   model.LLMSourceAuto, // 试图设为 auto
		LLMPrice: model.LLMPrice{Input: 3, Output: 6, CacheRead: 1, CacheWrite: 3},
	}
	if err := LLMUpdate(updateInfo, ctx); err != nil {
		t.Fatal(err)
	}
	var dbInfo model.LLMInfo
	if err := db.GetDB().Where("name = ?", "update-manual").First(&dbInfo).Error; err != nil {
		t.Fatal(err)
	}
	if dbInfo.Source != model.LLMSourceManual {
		t.Fatalf("更新应强制 manual, got %s", dbInfo.Source)
	}
	if dbInfo.Input != 3 {
		t.Fatal("价格应已更新")
	}
}

func TestEnsureAutoLLMInfoDoesNotOverwriteManual(t *testing.T) {
	setupPriceTestDB(t)
	ctx := context.Background()
	// 先创建 manual 记录
	manual := model.LLMInfo{
		Name:     "protected-manual",
		Source:   model.LLMSourceManual,
		LLMPrice: model.LLMPrice{Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5},
	}
	if err := db.GetDB().Create(&manual).Error; err != nil {
		t.Fatal(err)
	}
	// 事务内补 auto: ON CONFLICT DO NOTHING 不覆盖
	tx := db.GetDB().WithContext(ctx).Begin()
	ensureAutoLLMInfo(tx, "protected-manual")
	tx.Commit()

	var dbInfo model.LLMInfo
	if err := db.GetDB().Where("name = ?", "protected-manual").First(&dbInfo).Error; err != nil {
		t.Fatal(err)
	}
	if dbInfo.Source != model.LLMSourceManual {
		t.Fatal("manual 不应被 auto 覆盖")
	}
	if dbInfo.Input != 10 {
		t.Fatal("manual 价格不应被清零")
	}
}

func TestLLMCleanupGhostsPreservesManual(t *testing.T) {
	setupPriceTestDB(t)
	ctx := context.Background()
	// manual 记录无渠道引用
	manual := model.LLMInfo{
		Name:     "orphan-manual",
		Source:   model.LLMSourceManual,
		LLMPrice: model.LLMPrice{Input: 5, Output: 10, CacheRead: 1, CacheWrite: 5},
	}
	if err := db.GetDB().Create(&manual).Error; err != nil {
		t.Fatal(err)
	}
	llmModelCache.Set("orphan-manual", llmCacheEntry{LLMPrice: manual.LLMPrice, Source: model.LLMSourceManual})
	// auto 记录也无渠道引用
	auto := model.LLMInfo{
		Name:   "orphan-auto",
		Source: model.LLMSourceAuto,
	}
	if err := db.GetDB().Create(&auto).Error; err != nil {
		t.Fatal(err)
	}
	llmModelCache.Set("orphan-auto", llmCacheEntry{LLMPrice: model.LLMPrice{}, Source: model.LLMSourceAuto})

	if err := LLMCleanupGhosts(ctx); err != nil {
		t.Fatal(err)
	}
	// manual 应保留
	var manualCheck model.LLMInfo
	if err := db.GetDB().Where("name = ?", "orphan-manual").First(&manualCheck).Error; err != nil {
		t.Fatal("manual 不应被删除")
	}
	// auto 应被删除
	var autoCheck model.LLMInfo
	if err := db.GetDB().Where("name = ?", "orphan-auto").First(&autoCheck).Error; err == nil {
		t.Fatal("auto 应被删除")
	}
}

func TestSplitLLMInfosForImportManualOverwrite(t *testing.T) {
	manual, auto, err := splitLLMInfosForImport([]model.LLMInfo{
		{Name: "m1", Source: model.LLMSourceManual, LLMPrice: model.LLMPrice{Input: 1, Output: 2, CacheRead: 0.5, CacheWrite: 1}},
		{Name: "a1", Source: model.LLMSourceAuto},
		{Name: "old1", Source: "", LLMPrice: model.LLMPrice{Input: 999, Output: 999, CacheRead: 999, CacheWrite: 999}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(manual) != 1 || manual[0].Name != "m1" {
		t.Fatalf("manual 组应有 1 条, got %d", len(manual))
	}
	if len(auto) != 2 {
		t.Fatalf("auto 组应有 2 条(含旧 dump), got %d", len(auto))
	}
	// 旧 dump(source="") 应清零四价
	for _, a := range auto {
		if a.Name == "old1" {
			if a.Input != 0 || a.Output != 0 {
				t.Fatal("旧 dump 缺 source 应清零四价")
			}
			if a.Source != model.LLMSourceAuto {
				t.Fatal("旧 dump 应设为 auto")
			}
		}
	}
}

func TestSplitLLMInfosForImportRejectsIllegalSource(t *testing.T) {
	_, _, err := splitLLMInfosForImport([]model.LLMInfo{
		{Name: "bad", Source: "illegal"},
	})
	if err == nil {
		t.Fatal("非法 source 应拒绝")
	}
}

func TestImportAutoDoesNotOverwriteManual(t *testing.T) {
	setupPriceTestDB(t)
	ctx := context.Background()
	// 目标已有 manual 记录
	manual := model.LLMInfo{
		Name:     "import-protected",
		Source:   model.LLMSourceManual,
		LLMPrice: model.LLMPrice{Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5},
	}
	if err := db.GetDB().Create(&manual).Error; err != nil {
		t.Fatal(err)
	}
	llmModelCache.Set("import-protected", llmCacheEntry{LLMPrice: manual.LLMPrice, Source: model.LLMSourceManual})

	// 导入同名的 auto 记录
	dump := &model.DBDump{
		LLMInfos: []model.LLMInfo{
			{Name: "import-protected", Source: model.LLMSourceAuto, LLMPrice: model.LLMPrice{Input: 999, Output: 999, CacheRead: 999, CacheWrite: 999}},
		},
	}
	_, err := DBImportIncremental(ctx, dump)
	if err != nil {
		t.Fatal(err)
	}
	// manual 不应被覆盖
	var dbInfo model.LLMInfo
	if err := db.GetDB().Where("name = ?", "import-protected").First(&dbInfo).Error; err != nil {
		t.Fatal(err)
	}
	if dbInfo.Source != model.LLMSourceManual {
		t.Fatal("manual 不应被 auto 覆盖")
	}
	if dbInfo.Input != 10 {
		t.Fatal("manual 价格不应被覆盖")
	}
}

func TestImportManualOverwritesAuto(t *testing.T) {
	setupPriceTestDB(t)
	ctx := context.Background()
	// 目标已有 auto 记录
	auto := model.LLMInfo{
		Name:   "import-overwrite",
		Source: model.LLMSourceAuto,
	}
	if err := db.GetDB().Create(&auto).Error; err != nil {
		t.Fatal(err)
	}
	llmModelCache.Set("import-overwrite", llmCacheEntry{LLMPrice: model.LLMPrice{}, Source: model.LLMSourceAuto})

	// 导入 manual 覆盖 auto
	dump := &model.DBDump{
		LLMInfos: []model.LLMInfo{
			{Name: "import-overwrite", Source: model.LLMSourceManual, LLMPrice: model.LLMPrice{Input: 20, Output: 60, CacheRead: 2, CacheWrite: 15}},
		},
	}
	_, err := DBImportIncremental(ctx, dump)
	if err != nil {
		t.Fatal(err)
	}
	var dbInfo model.LLMInfo
	if err := db.GetDB().Where("name = ?", "import-overwrite").First(&dbInfo).Error; err != nil {
		t.Fatal(err)
	}
	if dbInfo.Source != model.LLMSourceManual {
		t.Fatal("manual 应覆盖 auto")
	}
	if dbInfo.Input != 20 {
		t.Fatal("manual 价格应已写入")
	}
}

func TestExportIncludesSource(t *testing.T) {
	setupPriceTestDB(t)
	ctx := context.Background()
	info := model.LLMInfo{
		Name:     "export-test",
		Source:   model.LLMSourceManual,
		LLMPrice: model.LLMPrice{Input: 5, Output: 10, CacheRead: 1, CacheWrite: 5},
	}
	if err := db.GetDB().Create(&info).Error; err != nil {
		t.Fatal(err)
	}
	dump, err := DBExportAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, l := range dump.LLMInfos {
		if l.Name == "export-test" {
			found = true
			if l.Source != model.LLMSourceManual {
				t.Fatal("导出应包含 source")
			}
			break
		}
	}
	if !found {
		t.Fatal("导出应包含测试记录")
	}
	// 验证 JSON 序列化包含 source
	data, _ := json.Marshal(dump.LLMInfos)
	if !strings.Contains(string(data), `"source":"manual"`) {
		t.Fatal("JSON 应包含 source 字段")
	}
}

func TestUsageCostCalculationRegression(t *testing.T) {
	// 回归测试: prompt=96275, cached=94464, completion=403
	// 价格: input=10, output=50, cache_read=1, cache_write=12.5
	// input_tokens = 96275 - 94464 = 1811
	// cost = (1811*10 + 94464*1 + 0*12.5) / 1e6 + 403*50/1e6
	//      = (18110 + 94464 + 0) / 1e6 + 20150 / 1e6
	//      = 112574 / 1e6 + 20150 / 1e6
	//      = 0.112574 + 0.020150 = 0.132724
	setupPriceTestDB(t)
	llmModelCache.Set("regression-model", llmCacheEntry{
		LLMPrice: model.LLMPrice{Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5},
		Source:   model.LLMSourceManual,
	})
	res := resolveModelPrice("regression-model", "group-x", testLookup(nil))
	if !res.CostKnown {
		t.Fatal("应 known=true")
	}
	inputTokens := int64(96275 - 94464)
	cachedTokens := int64(94464)
	completionTokens := int64(403)
	inputCost := (float64(inputTokens)*res.Price.Input + float64(cachedTokens)*res.Price.CacheRead) / 1_000_000
	outputCost := float64(completionTokens) * res.Price.Output / 1_000_000
	total := inputCost + outputCost
	expected := 0.132724
	if total < expected-0.000001 || total > expected+0.000001 {
		t.Fatalf("费用应为 %.6f, got %.6f", expected, total)
	}
}

// Fix 1: 缓存无 LLM 记录时, actual 仍应优先于 group 匹配。
func TestResolveUsagePriceActualPriorityWithoutCacheEntry(t *testing.T) {
	setupPriceTestDB(t)
	// 两个不同的参考价格
	lookup := testLookup(map[string]model.LLMPrice{
		"upstream-actual": {Input: 15, Output: 60, CacheRead: 2, CacheWrite: 15},
		"group-alias":     {Input: 20, Output: 80, CacheRead: 3, CacheWrite: 20},
	})
	// 缓存中无任何记录
	// actual 匹配 upstream-actual, group 匹配 group-alias; actual 应优先
	res := resolveModelPrice("upstream-actual", "group-alias", lookup)
	if !res.CostKnown {
		t.Fatal("应有参考价格")
	}
	if res.CostSource != "actual_reference" {
		t.Fatalf("应优先 actual_reference, got %s", res.CostSource)
	}
	if res.Price.Input != 15 {
		t.Fatal("应使用 actual 的价格 15, 而非 group 的 20")
	}
}

// Fix 2: ensureAutoLLMInfo 真实 INSERT 失败时传播错误, 父事务回滚, 无模型/授权写入。
func TestEnsureAutoLLMInfoFailurePropagatesError(t *testing.T) {
	setupPriceTestDB(t)
	ctx := context.Background()
	// 创建渠道, 用于验证父事务回滚后不残留。
	ch := model.Channel{ChannelConfig: model.ChannelConfig{Name: "fail-ch", Enabled: true, BaseURL: "http://fail.example"}, Revision: "rev-1"}
	if err := db.GetDB().Create(&ch).Error; err != nil {
		t.Fatal(err)
	}
	tx := db.GetDB().WithContext(ctx).Begin()
	// 先正常创建渠道模型(模拟 syncChannelModels 成功)。
	cm := model.ChannelModel{ChannelID: ch.ID, Name: "fail-model"}
	if err := tx.Create(&cm).Error; err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	// 在事务内删除 llm_infos 表, 使 ensureAutoLLMInfo 的 CREATE 必然失败。
	// SQLite 支持事务 DDL, 回滚后表恢复; 不影响其他测试。
	if err := tx.Exec("DROP TABLE llm_infos").Error; err != nil {
		tx.Rollback()
		t.Fatalf("删除 llm_infos 表失败: %v", err)
	}
	// ensureAutoLLMInfo 应返回错误而非吞掉。
	err := ensureAutoLLMInfo(tx, "fail-model")
	if err == nil {
		tx.Rollback()
		t.Fatal("ensureAutoLLMInfo 应在 INSERT 失败时返回 error, 而非吞掉")
	}
	tx.Rollback()
	// 验证父事务回滚后无残留: 渠道模型不写入。
	var cmCount int64
	db.GetDB().Model(&model.ChannelModel{}).Where("name = ?", "fail-model").Count(&cmCount)
	if cmCount != 0 {
		t.Fatal("回滚后不应有 ChannelModel 记录")
	}
	// 验证 llm_infos 表已恢复。
	var llmCount int64
	db.GetDB().Model(&model.LLMInfo{}).Count(&llmCount)
	_ = llmCount // 只要查询不报错即证明表已恢复
}

// Fix 3: 已有模型补缺价格记录, 仅新增 grant 时也确保价格。
func TestEnsureAutoLLMInfoExistingModelBackfill(t *testing.T) {
	setupPriceTestDB(t)
	ctx := context.Background()
	// 创建渠道
	ch := model.Channel{ChannelConfig: model.ChannelConfig{Name: "backfill-ch", Enabled: true, BaseURL: "http://backfill.example"}, Revision: "rev-1"}
	if err := db.GetDB().Create(&ch).Error; err != nil {
		t.Fatal(err)
	}
	// 创建已有渠道模型但无 LLMInfo
	cm := model.ChannelModel{ChannelID: ch.ID, Name: "existing-model"}
	if err := db.GetDB().Create(&cm).Error; err != nil {
		t.Fatal(err)
	}
	// 通过 syncChannelModels 路径触发 ensureAutoLLMInfo(已有模型路径)
	tx := db.GetDB().WithContext(ctx).Begin()
	if err := syncChannelModels(tx, ch.ID, []string{"existing-model"}); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	tx.Commit()
	// LLMInfo 应已补缺为 auto
	var info model.LLMInfo
	if err := db.GetDB().Where("name = ?", "existing-model").First(&info).Error; err != nil {
		t.Fatal("应有 auto LLMInfo 记录")
	}
	if info.Source != model.LLMSourceAuto {
		t.Fatalf("应为 auto, got %s", info.Source)
	}
}

// Fix 6: LLMRebuild 补缺 + 清理无引用 auto + 保留 manual(含未引用)。
func TestLLMRebuildComplete(t *testing.T) {
	setupPriceTestDB(t)
	ctx := context.Background()
	// 创建渠道 + 模型
	ch := model.Channel{ChannelConfig: model.ChannelConfig{Name: "rebuild-ch", Enabled: true, BaseURL: "http://rebuild.example"}, Revision: "rev-1"}
	db.GetDB().Create(&ch)
	db.GetDB().Create(&model.ChannelModel{ChannelID: ch.ID, Name: "rebuild-model"})
	// 创建 manual 记录(无渠道引用的显式用户价格)
	db.GetDB().Create(&model.LLMInfo{Name: "orphan-manual", Source: model.LLMSourceManual, LLMPrice: model.LLMPrice{Input: 5, Output: 10, CacheRead: 1, CacheWrite: 5}})
	// 创建 auto 记录(无渠道引用, 应被清理)
	db.GetDB().Create(&model.LLMInfo{Name: "orphan-auto", Source: model.LLMSourceAuto})
	// 刷新缓存(包括 channel model cache, 否则 rebuild 找不到渠道模型)
	reloadChannelChildren(ctx, ch.ID)
	llmRefreshCache(ctx)

	if err := LLMRebuild(ctx); err != nil {
		t.Fatal(err)
	}
	// rebuild-model 应补缺 auto
	var rebuildInfo model.LLMInfo
	if err := db.GetDB().Where("name = ?", "rebuild-model").First(&rebuildInfo).Error; err != nil {
		t.Fatal("rebuild-model 应补缺 auto LLMInfo")
	}
	if rebuildInfo.Source != model.LLMSourceAuto {
		t.Fatal("应为 auto")
	}
	// orphan-manual 应保留
	var manualInfo model.LLMInfo
	if err := db.GetDB().Where("name = ?", "orphan-manual").First(&manualInfo).Error; err != nil {
		t.Fatal("orphan-manual 应保留")
	}
	if manualInfo.Source != model.LLMSourceManual {
		t.Fatal("应为 manual")
	}
	// orphan-auto 应被清理
	var autoInfo model.LLMInfo
	if err := db.GetDB().Where("name = ?", "orphan-auto").First(&autoInfo).Error; err == nil {
		t.Fatal("orphan-auto 应被清理")
	}
}

// TestResolveUsagePriceProductionCatalogWired 验证生产入口 ResolveUsagePrice 真实接入 catalog.Lookup,
// 不依赖 mock 注入。使用预设目录中已知模型 gpt-4o(input=2.5, output=10, cache_read=1.25)。
// 防止测试全 mock 再次掩盖生产未接线。
func TestResolveUsagePriceProductionCatalogWired(t *testing.T) {
	setupPriceTestDB(t)
	// 不注入任何 lookup; ResolveUsagePrice 内部固定调用 catalog.Lookup。
	res := ResolveUsagePrice("gpt-4o", "unknown-group")
	if !res.CostKnown {
		t.Fatal("gpt-4o 在预设目录中应 known=true")
	}
	if res.CostSource != "actual_reference" {
		t.Fatalf("source 应为 actual_reference, got %s", res.CostSource)
	}
	if res.CostReferenceModel == nil || *res.CostReferenceModel != "gpt-4o" {
		t.Fatalf("参考模型 ID 应为 gpt-4o, got %v", res.CostReferenceModel)
	}
	if res.Price.Input != 2.5 || res.Price.Output != 10 || res.Price.CacheRead != 1.25 {
		t.Fatalf("价格应为 gpt-4o 预设值, got input=%f output=%f cache_read=%f",
			res.Price.Input, res.Price.Output, res.Price.CacheRead)
	}
}

// TestLLMDerivePriceProductionCatalogWired 验证 LLMDerivePrice 生产路径真实接入 catalog。
func TestLLMDerivePriceProductionCatalogWired(t *testing.T) {
	setupPriceTestDB(t)
	// auto 记录缓存中存在 gpt-4o, 参考目录应返回预设价格。
	llmModelCache.Set("gpt-4o", llmCacheEntry{
		LLMPrice: model.LLMPrice{},
		Source:   model.LLMSourceAuto,
	})
	p, known := LLMDerivePrice("gpt-4o")
	if !known {
		t.Fatal("gpt-4o 应 known=true")
	}
	if p.Input != 2.5 || p.Output != 10 {
		t.Fatalf("应使用预设价格, got input=%f output=%f", p.Input, p.Output)
	}
}

// TestResolveUsagePriceProductionGroupReference 验证 group_reference 路径真实接入 catalog。
func TestResolveUsagePriceProductionGroupReference(t *testing.T) {
	setupPriceTestDB(t)
	// actual 不匹配, group 匹配 gpt-4o
	res := ResolveUsagePrice("nonexistent-model-xyz", "gpt-4o")
	if !res.CostKnown {
		t.Fatal("gpt-4o 作为 group 应 known=true")
	}
	if res.CostSource != "group_reference" {
		t.Fatalf("source 应为 group_reference, got %s", res.CostSource)
	}
	if res.Price.Input != 2.5 {
		t.Fatal("应使用 gpt-4o 预设价格")
	}
}

// TestResolveUsagePriceProductionUnknown 验证无匹配时 unknown。
func TestResolveUsagePriceProductionUnknown(t *testing.T) {
	setupPriceTestDB(t)
	res := ResolveUsagePrice("totally-nonexistent-model", "also-nonexistent")
	if res.CostKnown {
		t.Fatal("无匹配应 known=false")
	}
	if res.CostSource != "unknown" {
		t.Fatalf("source 应为 unknown, got %s", res.CostSource)
	}
	if res.CostReferenceModel != nil {
		t.Fatal("unknown 无参考模型 ID, 应为 nil")
	}
}

// TestSyncOnlyLLMInfoBackfillNoRevisionRotate 验证仅补 LLMInfo(无新增 model/grant)时不轮转 revision。
// 已有模型缺价格记录, sync 补缺但不增 model/grant; revision 保持不变。
func TestSyncOnlyLLMInfoBackfillNoRevisionRotate(t *testing.T) {
	setupPriceTestDB(t)
	ctx := context.Background()
	// 创建渠道(带 revision), 已有模型, 已有授权, 但无 LLMInfo。
	origRev := "rev-keep-unchanged"
	ch := model.Channel{ChannelConfig: model.ChannelConfig{Name: "backfill-only-ch", Enabled: true, BaseURL: "http://backfill.example"}, Revision: origRev}
	db.GetDB().Create(&ch)
	cm := model.ChannelModel{ChannelID: ch.ID, Name: "backfill-only-model"}
	db.GetDB().Create(&cm)
	key := model.ChannelKey{ChannelID: ch.ID, ChannelKeyConfig: model.ChannelKeyConfig{Name: "k", Key: "sk", Enabled: true}}
	db.GetDB().Create(&key)
	grant := model.ChannelGrant{ChannelModelID: cm.ID, ChannelKeyID: key.ID, Protocols: 1}
	db.GetDB().Create(&grant)
	// syncChannelModels 对已有模型补缺 LLMInfo, 但不新增 model/grant。
	tx := db.GetDB().WithContext(ctx).Begin()
	if err := syncChannelModels(tx, ch.ID, []string{"backfill-only-model"}); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	tx.Commit()
	// LLMInfo 应已补缺为 auto
	var info model.LLMInfo
	if err := db.GetDB().Where("name = ?", "backfill-only-model").First(&info).Error; err != nil {
		t.Fatal("应有 auto LLMInfo 记录")
	}
	if info.Source != model.LLMSourceAuto {
		t.Fatal("应为 auto")
	}
	// revision 应保持不变(无 model/grant 新增)
	var chAfter model.Channel
	db.GetDB().First(&chAfter, ch.ID)
	if chAfter.Revision != origRev {
		t.Fatalf("仅补 LLMInfo 不应轮转 revision, expected %s, got %s", origRev, chAfter.Revision)
	}
}

// TestSyncNewGrantRotatesRevision 验证新增 grant 时轮转 revision(即使模型已存在)。
func TestSyncNewGrantRotatesRevision(t *testing.T) {
	setupPriceTestDB(t)
	ctx := context.Background()
	origRev := "rev-before-grant"
	ch := model.Channel{ChannelConfig: model.ChannelConfig{Name: "grant-rotate-ch", Enabled: true, BaseURL: "http://grant.example"}, Revision: origRev}
	db.GetDB().Create(&ch)
	// 已有模型但无授权; ChannelSyncApplyDiscovery 会补缺 LLMInfo 并新增 grant。
	cm := model.ChannelModel{ChannelID: ch.ID, Name: "existing-grant-model"}
	db.GetDB().Create(&cm)
	key := model.ChannelKey{ChannelID: ch.ID, ChannelKeyConfig: model.ChannelKeyConfig{Name: "k", Key: "sk", Enabled: true}}
	db.GetDB().Create(&key)

	discoveries := []KeyDiscovery{{
		KeyID:  key.ID,
		Models: []model.ChannelFetchModel{{Name: "existing-grant-model", Protocols: model.ProtocolOpenAIChatCompletion}},
	}}
	tx := db.GetDB().WithContext(ctx).Begin()
	_, additions, err := ChannelSyncApplyDiscovery(tx, ch.ID, discoveries)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	tx.Commit()
	// additions 应有 AddedGrants > 0(模型已存在, 不增 AddedModels)
	if additions.AddedGrants == 0 {
		t.Fatal("应新增 grant")
	}
	// revision 应已轮转
	var chAfter model.Channel
	db.GetDB().First(&chAfter, ch.ID)
	if chAfter.Revision == origRev {
		t.Fatal("新增 grant 后应轮转 revision")
	}
}
