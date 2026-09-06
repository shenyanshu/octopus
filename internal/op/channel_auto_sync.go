package op

// 本文件实现批量开启渠道自动同步。
// 仅职责: 查询全部 auto_sync_models=false 的渠道, 在单事务内逐渠道更新 auto_sync_models=true
// 并轮转独立 revision, 按实际 RowsAffected 累计 count; 原 true 的完全不写不轮转。
// Enabled/keys/models/grants/stats 不变; 缓存保留 StatsMetrics, 仅更新 AutoSyncModels 与 Revision。

import (
	"context"
	"fmt"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ChannelEnableAllAutoSync 批量开启全部渠道的自动同步。
// 包含 ALL channels regardless Enabled: 查询 auto_sync_models=false 的渠道,
// 同事务逐渠道更新 auto_sync_models=true 并生成独立 uuid revision,
// 按实际 RowsAffected 累计 count; 原 true 的完全不写不轮转。
// 缓存保留 StatsMetrics, 仅替换 AutoSyncModels 与 Revision; 缓存缺 entry 时跳过(无缓存框架新增)。
func ChannelEnableAllAutoSync(ctx context.Context) (int, error) {
	var channels []model.Channel
	// 查询全部 auto_sync_models=false 的渠道, 不按 Enabled 过滤。
	if err := db.GetDB().WithContext(ctx).Where("auto_sync_models = ?", false).Find(&channels).Error; err != nil {
		return 0, fmt.Errorf("failed to load channels for auto-sync enable: %w", err)
	}
	count := 0
	if len(channels) == 0 {
		return 0, nil
	}
	// 单事务: 逐渠道更新 auto_sync_models=true 并轮转独立 revision。
	updates := make(map[int]string, len(channels)) // channelID → newRevision, 供缓存更新。
	if err := db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for i := range channels {
			newRevision := uuid.NewString()
			result := tx.Model(&model.Channel{}).Where("id = ? AND auto_sync_models = ?", channels[i].ID, false).
				Updates(map[string]any{"auto_sync_models": true, "revision": newRevision})
			if err := result.Error; err != nil {
				return fmt.Errorf("failed to update channel %d auto_sync: %w", channels[i].ID, err)
			}
			if result.RowsAffected > 0 {
				// 按实际 RowsAffected 累计; WHERE 含 auto_sync_models=false 防并发重复写。
				count += int(result.RowsAffected)
				updates[channels[i].ID] = newRevision
			}
			// RowsAffected==0: 并发已改 true, 不计入不轮转。
		}
		return nil
	}); err != nil {
		return 0, err
	}
	// 提交成功后更新缓存: 保留 StatsMetrics, 仅替换 AutoSyncModels=true 与新 Revision。
	// 缓存缺 entry 跳过(既有模式: ChannelDel/ChannelEnabled 缺 entry 时返回 not found,
	// 此处批量场景无需逐个报错, 跳过即可)。
	channelStatsNeedUpdateLock.Lock()
	for chID, newRev := range updates {
		cached, ok := channelCache.Get(chID)
		if !ok {
			continue
		}
		cached.AutoSyncModels = true
		cached.Revision = newRev
		channelCache.Set(chID, cached)
	}
	channelStatsNeedUpdateLock.Unlock()
	return count, nil
}
