package op

import (
	"fmt"

	"github.com/bestruirui/octopus/internal/model"
	"gorm.io/gorm"
)

// syncDeleteResult 记录单次同步删除的授权和模型, 供计数和后续 group 收敛。
type syncDeleteResult struct {
	removedGrants   int
	removedModels   int
	deletedGrantIDs []int // 已删除的 grant ID, 用于 clearActiveItems
	deletedModelIDs []int // 已删除的 model ID, 供 mutation 快照感知
}

// applySyncDeletions 在同步成功且非 Partial 时, 删除该 key 下不再被上游返回的 sync_managed=true 授权,
// 并清理由此产生的孤儿模型(sync_managed=true 且全渠道无授权)。仅删, 不改协议。
//
// 删除条件严格: 仅 Err==nil && !Partial && len(Models)>0 的凭据参与; 其他凭据的授权一律不动。
// 其他 key 的授权阻止删 model; manual/legacy(sync_managed=false)的模型永不被删除。
// 授权可位于 manual 模型上(sync_managed=true 的授权可删), 但模型只有 sync_managed=true 才删。
func applySyncDeletions(tx *gorm.DB, channelID int, discoveries []KeyDiscovery) (syncDeleteResult, error) {
	result := syncDeleteResult{}
	// 收集每个 key 的发现结果模型名集合, 仅成功且非 Partial 且非空的凭据参与删除。
	keyReturnedModels := make(map[int]map[string]bool)
	for _, disc := range discoveries {
		if disc.Err != nil || disc.Partial || len(disc.Models) == 0 {
			continue
		}
		models := make(map[string]bool, len(disc.Models))
		for _, m := range disc.Models {
			models[m.Name] = true
		}
		keyReturnedModels[disc.KeyID] = models
	}
	if len(keyReturnedModels) == 0 {
		return result, nil
	}
	// 查出该渠道所有 sync_managed=true 的授权及其关联的模型名、模型 ID 和 key ID。
	// 仅按 grant 的 sync_managed 过滤, 不按 model 的 sync_managed — 授权可位于 manual 模型上。
	type grantWithModel struct {
		GrantID   int
		ModelID   int
		ModelName string
		KeyID     int
	}
	var managedGrants []grantWithModel
	if err := tx.Table("channel_grants").
		Select("channel_grants.id as grant_id, channel_models.id as model_id, channel_models.name as model_name, channel_grants.channel_key_id as key_id").
		Joins("INNER JOIN channel_models ON channel_models.id = channel_grants.channel_model_id").
		Where("channel_models.channel_id = ? AND channel_grants.sync_managed = ?",
			channelID, true).
		Find(&managedGrants).Error; err != nil {
		return result, fmt.Errorf("failed to load managed grants: %w", err)
	}
	// 筛选出应删除的授权: 该 grant 的 key 参与了删除(在 keyReturnedModels 中),
	// 且该 grant 的模型名不在返回的模型集合中。
	var grantsToDelete []int
	candidateModelIDs := make(map[int]bool) // 可能成为孤儿的 model ID(仅 sync_managed=true 的模型)
	for _, g := range managedGrants {
		returned, ok := keyReturnedModels[g.KeyID]
		if !ok {
			continue // 该 key 未参与删除(错误/空/Partial), 不删其授权
		}
		if !returned[g.ModelName] {
			grantsToDelete = append(grantsToDelete, g.GrantID)
			candidateModelIDs[g.ModelID] = true
		}
	}
	if len(grantsToDelete) == 0 {
		return result, nil
	}
	// 删除授权前, 先清除引用这些授权的 group items 的 active_item_id(复用既有 clearActiveItems)。
	if err := clearActiveItems(tx, grantsToDelete); err != nil {
		return result, fmt.Errorf("failed to clear active group items: %w", err)
	}
	// 删除授权, 用 RowsAffected 获取真实删除计数。
	res := tx.Where("id IN ?", grantsToDelete).Delete(&model.ChannelGrant{})
	if res.Error != nil {
		return result, fmt.Errorf("failed to delete managed grants: %w", res.Error)
	}
	result.removedGrants = int(res.RowsAffected)
	result.deletedGrantIDs = grantsToDelete
	// 检查候选模型是否已成为孤儿: 仅 sync_managed=true 的模型才删。
	// 注意: 仅限本次删除的 grant 所涉及的 model; 不全局扫描因手动删授权或失败留下的孤儿。
	for modelID := range candidateModelIDs {
		// 先确认该模型仍为 sync_managed=true(manual 模型不删)。
		var cm model.ChannelModel
		if err := tx.Where("id = ? AND sync_managed = ?", modelID, true).First(&cm).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				continue // 模型已不存在或非 sync_managed, 跳过
			}
			return result, fmt.Errorf("failed to load model %d for orphan check: %w", modelID, err)
		}
		var count int64
		if err := tx.Model(&model.ChannelGrant{}).Where("channel_model_id = ?", modelID).Count(&count).Error; err != nil {
			return result, fmt.Errorf("failed to count grants for orphan model %d: %w", modelID, err)
		}
		if count == 0 {
			// 已成为孤儿且 sync_managed=true, 删除模型。
			dres := tx.Where("id = ?", modelID).Delete(&model.ChannelModel{})
			if dres.Error != nil {
				return result, fmt.Errorf("failed to delete orphan model %d: %w", modelID, dres.Error)
			}
			if dres.RowsAffected > 0 {
				result.removedModels += int(dres.RowsAffected)
				result.deletedModelIDs = append(result.deletedModelIDs, modelID)
			}
		}
	}
	return result, nil
}
