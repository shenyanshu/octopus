package op

import (
	"context"
	"fmt"
	"strings"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/utils/cache"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// llmCacheEntry 缓存中每条模型价格记录携带来源信息, 供 API 动态派生与日志记账统一读取。
type llmCacheEntry struct {
	model.LLMPrice
	Source model.LLMSource
}

var llmModelCache = cache.New[string, llmCacheEntry](16) // 数据库中的模型价格与来源。

// LLMList 返回缓存中的全部模型价格。
func LLMList() []model.LLMInfo {
	models := make([]model.LLMInfo, 0, llmModelCache.Len())
	for m, entry := range llmModelCache.GetAll() {
		models = append(models, model.LLMInfo{
			Name:     m,
			Source:   entry.Source,
			LLMPrice: entry.LLMPrice,
		})
	}
	return models
}

// LLMUpdate 更新已存在的模型价格并同步缓存; 更新强制来源为 manual。
func LLMUpdate(info model.LLMInfo, ctx context.Context) error {
	_, ok := llmModelCache.Get(info.Name)
	if !ok {
		return fmt.Errorf("model not found")
	}
	info.Source = model.LLMSourceManual
	if err := db.GetDB().WithContext(ctx).Save(&info).Error; err != nil {
		return err
	}
	llmModelCache.Set(info.Name, llmCacheEntry{LLMPrice: info.LLMPrice, Source: info.Source})
	return nil
}

// LLMDelete 删除未被任何渠道引用的模型价格。
func LLMDelete(modelName string, ctx context.Context) error {
	_, ok := llmModelCache.Get(modelName)
	if !ok {
		return fmt.Errorf("model not found")
	}
	for _, channelModel := range channelModelCache.GetAll() {
		if strings.ToLower(channelModel.Name) == modelName {
			return fmt.Errorf("model is referenced by channel")
		}
	}
	if err := db.GetDB().WithContext(ctx).Delete(&model.LLMInfo{Name: modelName}).Error; err != nil {
		return err
	}
	llmModelCache.Del(modelName)
	return nil
}

// LLMRebuild 补齐渠道模型缺的价格记录(auto), 清理无引用的 auto 记录, 保留所有 manual。
// 不复制参考价到 DB, auto 价格由 API 动态派生。刷新目录后 auto 展示/新请求立即生效。
func LLMRebuild(ctx context.Context) error {
	// 刷新参考目录由调用方(cmd/start.go 初始化或定时任务)通过 price.UpdateLLMPrice 完成;
	// op 包不直接调用 price 以避免循环依赖。rebuild 时假定目录已是最新。
	// 补齐渠道模型缺的 auto 价格记录。
	channelModels := channelModelCache.GetAll()
	missingInfos := make([]model.LLMInfo, 0, len(channelModels))
	for _, cm := range channelModels {
		key := strings.ToLower(cm.Name)
		if _, ok := llmModelCache.Get(key); !ok {
			missingInfos = append(missingInfos, model.LLMInfo{
				Name: key, Source: model.LLMSourceAuto, LLMPrice: model.LLMPrice{},
			})
		}
	}
	if len(missingInfos) > 0 {
		if err := db.GetDB().WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&missingInfos).Error; err != nil {
			return err
		}
		for _, info := range missingInfos {
			llmModelCache.Set(info.Name, llmCacheEntry{LLMPrice: info.LLMPrice, Source: info.Source})
		}
	}
	// 清理无引用的 auto 记录, 保留 manual。
	if err := LLMCleanupGhosts(ctx); err != nil {
		return err
	}
	return nil
}

// LLMListCount 返回缓存中模型价格记录数量。
func LLMListCount() int {
	return llmModelCache.Len()
}

// LLMCleanupGhosts 删除已经不被任何渠道引用的 auto 模型价格; manual 记录即使用户显式设定但无渠道引用也保留。
// 以 DB 权威渠道模型名集合判定引用, 不依赖 channelModelCache(可能陈旧)。
// DELETE 本身须 source=auto, 避免误删 manual。
func LLMCleanupGhosts(ctx context.Context) error {
	// 从 DB 查询所有渠道模型名作为权威引用集合, 不使用 channelModelCache(可能陈旧)。
	var dbModels []model.ChannelModel
	if err := db.GetDB().WithContext(ctx).Find(&dbModels).Error; err != nil {
		return fmt.Errorf("查询渠道模型失败: %w", err)
	}
	referencedModelNames := make(map[string]struct{}, len(dbModels))
	for _, cm := range dbModels {
		referencedModelNames[strings.ToLower(cm.Name)] = struct{}{}
	}

	// 从 DB 查询所有价格记录, 仅 auto 且无引用的才是 ghost。
	var allInfos []model.LLMInfo
	if err := db.GetDB().WithContext(ctx).Find(&allInfos).Error; err != nil {
		return fmt.Errorf("查询价格记录失败: %w", err)
	}
	ghostModelNames := make([]string, 0)
	for _, info := range allInfos {
		// manual 保留, 不当 ghost 删除。
		if info.Source == model.LLMSourceManual {
			continue
		}
		if _, ok := referencedModelNames[info.Name]; !ok {
			ghostModelNames = append(ghostModelNames, info.Name)
		}
	}
	if len(ghostModelNames) == 0 {
		return nil
	}
	if err := db.GetDB().WithContext(ctx).Where("name IN ? AND source = ?", ghostModelNames, model.LLMSourceAuto).Delete(&model.LLMInfo{}).Error; err != nil {
		return err
	}
	for _, name := range ghostModelNames {
		llmModelCache.Del(name)
	}
	return nil
}

// LLMCreate 写入已在外部入口规范化的模型价格; 创建强制来源为 manual。
func LLMCreate(info model.LLMInfo, ctx context.Context) error {
	_, ok := llmModelCache.Get(info.Name)
	if ok {
		return fmt.Errorf("model already exists")
	}
	info.Source = model.LLMSourceManual
	if err := db.GetDB().WithContext(ctx).Create(&info).Error; err != nil {
		return err
	}
	llmModelCache.Set(info.Name, llmCacheEntry{LLMPrice: info.LLMPrice, Source: info.Source})
	return nil
}

// LLMBatchCreate 批量写入已规范化且去重的模型价格，并跳过已有模型。
func LLMBatchCreate(llmInfos []model.LLMInfo, ctx context.Context) error {
	if len(llmInfos) == 0 {
		return nil
	}
	newLLMInfos := make([]model.LLMInfo, 0, len(llmInfos))
	for _, llmInfo := range llmInfos {
		if _, ok := llmModelCache.Get(llmInfo.Name); ok {
			continue
		}
		// 批量创建来自渠道同步的模型价格, 缺省 source=auto, 四价留零占位。
		if llmInfo.Source == "" {
			llmInfo.Source = model.LLMSourceAuto
		}
		newLLMInfos = append(newLLMInfos, llmInfo)
	}
	if len(newLLMInfos) == 0 {
		return nil
	}
	if err := db.GetDB().WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&newLLMInfos).Error; err != nil {
		return err
	}
	names := make([]string, len(newLLMInfos))
	for i, llmInfo := range newLLMInfos {
		names[i] = llmInfo.Name
	}
	var savedLLMInfos []model.LLMInfo
	if err := db.GetDB().WithContext(ctx).Where("name IN ?", names).Find(&savedLLMInfos).Error; err != nil {
		return err
	}
	for _, llmInfo := range savedLLMInfos {
		llmModelCache.Set(llmInfo.Name, llmCacheEntry{LLMPrice: llmInfo.LLMPrice, Source: llmInfo.Source})
	}
	return nil
}

// LLMBatchSave 批量更新或新增模型价格，并在写入成功后同步价格缓存。
func LLMBatchSave(llmInfos []model.LLMInfo, ctx context.Context) error {
	if len(llmInfos) == 0 {
		return nil
	}
	if err := db.GetDB().WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(&llmInfos).Error; err != nil {
		return err
	}
	for _, llmInfo := range llmInfos {
		source := llmInfo.Source
		if source == "" {
			source = model.LLMSourceAuto
		}
		llmModelCache.Set(llmInfo.Name, llmCacheEntry{LLMPrice: llmInfo.LLMPrice, Source: source})
	}
	return nil
}

// LLMGet 按价格表统一使用的小写模型名读取数据库价格缓存。
func LLMGet(name string) (model.LLMPrice, error) {
	entry, ok := llmModelCache.Get(strings.ToLower(name))
	if !ok {
		return model.LLMPrice{}, fmt.Errorf("model not found")
	}
	return entry.LLMPrice, nil
}

// ensureAutoLLMInfo 在事务内为模型名补缺 auto 价格记录, ON CONFLICT DO NOTHING 不覆盖已有 manual。
// auto 记录的四价为零占位, 实际价格由参考目录动态派生。失败时返回错误使父事务回滚。
func ensureAutoLLMInfo(tx *gorm.DB, modelName string) error {
	key := strings.ToLower(strings.TrimSpace(modelName))
	if key == "" {
		return nil
	}
	// ON CONFLICT(name) DO NOTHING: 已存在(manual 或 auto)均不覆盖。
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).
		Create(&model.LLMInfo{Name: key, Source: model.LLMSourceAuto, LLMPrice: model.LLMPrice{}}).Error; err != nil {
		return fmt.Errorf("failed to ensure auto llm info for %s: %w", key, err)
	}
	return nil
}

// llmRefreshCache 从数据库刷新模型价格缓存。
func llmRefreshCache(ctx context.Context) error {
	models := []model.LLMInfo{}
	if err := db.GetDB().WithContext(ctx).Find(&models).Error; err != nil {
		return err
	}
	for _, model := range models {
		llmModelCache.Set(model.Name, llmCacheEntry{LLMPrice: model.LLMPrice, Source: model.Source})
	}
	return nil
}
