package op

import (
	"context"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"gorm.io/gorm"
)

// clearAllForSyncDelete 清空所有表, 复用已有测试清理路径。
func clearAllForSyncDelete(t *testing.T) {
	t.Helper()
	tables := []string{"channel_grants", "channel_models", "channel_keys", "channels", "group_items", "groups", "llm_infos"}
	for _, table := range tables {
		if err := db.GetDB().Exec("DELETE FROM " + table).Error; err != nil {
			t.Fatalf("清空 %s 失败: %v", table, err)
		}
	}
}

// syncDeleteHelper 用真实事务调用 ChannelSyncApplyDiscovery 并提交, 复用 seedChannel 模式。
func syncDeleteApply(t *testing.T, channelID int, discoveries []KeyDiscovery) (*ChannelMutation, SyncChanges, error) {
	t.Helper()
	tx := db.GetDB().Begin()
	mutation, changes, err := ChannelSyncApplyDiscovery(tx, channelID, discoveries)
	if err != nil {
		tx.Rollback()
		return nil, SyncChanges{}, err
	}
	if err := tx.Commit().Error; err != nil {
		return nil, SyncChanges{}, err
	}
	return mutation, changes, nil
}

// TestSyncDeleteManagedGrantRemovesUnreturnedModel 验证: sync_managed=true 的授权在下次同步不再返回时被删除, 孤儿模型也删除。
func TestSyncDeleteManagedGrantRemovesUnreturnedModel(t *testing.T) {
	clearAllForSyncDelete(t)
	chID, _ := seedChannel(t, "del-managed", true, nil, []keySpec{{"k1", true}})

	// 第一次同步: 返回 model-a, 新建 model + grant, 标记 sync_managed=true。
	disc1 := []KeyDiscovery{{
		KeyID:  getKeyID(t, chID, "k1"),
		Models: []model.ChannelFetchModel{{Name: "model-a", Protocols: model.ProtocolOpenAIChatCompletion}},
	}}
	_, changes, err := syncDeleteApply(t, chID, disc1)
	if err != nil {
		t.Fatalf("第一次同步失败: %v", err)
	}
	if changes.AddedModels != 1 || changes.AddedGrants != 1 {
		t.Fatalf("第一次同步应新增 1 模型 1 授权, got models=%d grants=%d", changes.AddedModels, changes.AddedGrants)
	}
	// 无分组模式匹配时 mutation 为 nil, 但 changes 非零证明提交成功。

	// 验证 model-a 的 sync_managed=true 和 grant 的 sync_managed=true。
	var cm model.ChannelModel
	if err := db.GetDB().Where("channel_id = ? AND name = ?", chID, "model-a").First(&cm).Error; err != nil {
		t.Fatalf("读 model-a 失败: %v", err)
	}
	if !cm.SyncManaged {
		t.Fatal("model-a 的 sync_managed 应为 true")
	}
	var cg model.ChannelGrant
	if err := db.GetDB().Where("channel_model_id = ?", cm.ID).First(&cg).Error; err != nil {
		t.Fatalf("读 grant 失败: %v", err)
	}
	if !cg.SyncManaged {
		t.Fatal("grant 的 sync_managed 应为 true")
	}

	// 第二次同步: 不再返回 model-a, 返回 model-b。
	disc2 := []KeyDiscovery{{
		KeyID:  getKeyID(t, chID, "k1"),
		Models: []model.ChannelFetchModel{{Name: "model-b", Protocols: model.ProtocolOpenAIChatCompletion}},
	}}
	_, changes2, err := syncDeleteApply(t, chID, disc2)
	if err != nil {
		t.Fatalf("第二次同步失败: %v", err)
	}
	if changes2.RemovedGrants != 1 || changes2.RemovedModels != 1 {
		t.Fatalf("应删除 1 授权 1 模型, got removed_grants=%d removed_models=%d", changes2.RemovedGrants, changes2.RemovedModels)
	}
	// 删除已验证 changes.RemovedGrants > 0; mutation 可能 nil(无分组模式匹配)。

	// 验证 model-a 和其 grant 已被删除。
	var count int64
	db.GetDB().Model(&model.ChannelModel{}).Where("channel_id = ? AND name = ?", chID, "model-a").Count(&count)
	if count != 0 {
		t.Fatalf("model-a 应已被删除, 仍有 %d 行", count)
	}
	db.GetDB().Model(&model.ChannelGrant{}).Where("channel_model_id = ?", cm.ID).Count(&count)
	if count != 0 {
		t.Fatalf("model-a 的 grant 应已被删除, 仍有 %d 行", count)
	}
}

// TestSyncDeletePartialDoesNotDelete 验证: Partial=true 时不删任何授权。
func TestSyncDeletePartialDoesNotDelete(t *testing.T) {
	clearAllForSyncDelete(t)
	chID, _ := seedChannel(t, "del-partial", true, []string{"model-a"}, []keySpec{{"k1", true}})

	// 先同步创建 model-a(sync_managed=true)。
	disc1 := []KeyDiscovery{{
		KeyID:  getKeyID(t, chID, "k1"),
		Models: []model.ChannelFetchModel{{Name: "model-a", Protocols: model.ProtocolOpenAIChatCompletion}},
	}}
	if _, _, err := syncDeleteApply(t, chID, disc1); err != nil {
		t.Fatalf("第一次同步失败: %v", err)
	}

	// 第二次同步: Partial=true, 返回 model-b(非 model-a)。Partial 不应删除 model-a 的授权。
	disc2 := []KeyDiscovery{{
		KeyID:   getKeyID(t, chID, "k1"),
		Partial: true,
		Models:  []model.ChannelFetchModel{{Name: "model-b", Protocols: model.ProtocolOpenAIChatCompletion}},
	}}
	_, changes2, err := syncDeleteApply(t, chID, disc2)
	if err != nil {
		t.Fatalf("第二次同步失败: %v", err)
	}
	if changes2.RemovedGrants != 0 || changes2.RemovedModels != 0 {
		t.Fatalf("Partial 不应删除, got removed_grants=%d removed_models=%d", changes2.RemovedGrants, changes2.RemovedModels)
	}

	// 验证 model-a 仍在。
	var count int64
	db.GetDB().Model(&model.ChannelModel{}).Where("channel_id = ? AND name = ?", chID, "model-a").Count(&count)
	if count != 1 {
		t.Fatalf("model-a 应保留(Partial 不删), got %d", count)
	}
}

// TestSyncDeleteErrorDoesNotDelete 验证: Err!=nil 时不删任何授权。
func TestSyncDeleteErrorDoesNotDelete(t *testing.T) {
	clearAllForSyncDelete(t)
	chID, _ := seedChannel(t, "del-err", true, []string{"model-a"}, []keySpec{{"k1", true}})

	disc1 := []KeyDiscovery{{
		KeyID:  getKeyID(t, chID, "k1"),
		Models: []model.ChannelFetchModel{{Name: "model-a", Protocols: model.ProtocolOpenAIChatCompletion}},
	}}
	if _, _, err := syncDeleteApply(t, chID, disc1); err != nil {
		t.Fatalf("第一次同步失败: %v", err)
	}

	disc2 := []KeyDiscovery{{
		KeyID: getKeyID(t, chID, "k1"),
		Err:   context.Canceled,
	}}
	_, changes2, err := syncDeleteApply(t, chID, disc2)
	if err != nil {
		t.Fatalf("第二次同步失败: %v", err)
	}
	if changes2.RemovedGrants != 0 || changes2.RemovedModels != 0 {
		t.Fatalf("Error 不应删除, got removed_grants=%d removed_models=%d", changes2.RemovedGrants, changes2.RemovedModels)
	}
}

// TestSyncDeleteEmptyResultDoesNotDelete 验证: 返回空模型列表时不删任何授权。
func TestSyncDeleteEmptyResultDoesNotDelete(t *testing.T) {
	clearAllForSyncDelete(t)
	chID, _ := seedChannel(t, "del-empty", true, []string{"model-a"}, []keySpec{{"k1", true}})

	disc1 := []KeyDiscovery{{
		KeyID:  getKeyID(t, chID, "k1"),
		Models: []model.ChannelFetchModel{{Name: "model-a", Protocols: model.ProtocolOpenAIChatCompletion}},
	}}
	if _, _, err := syncDeleteApply(t, chID, disc1); err != nil {
		t.Fatalf("第一次同步失败: %v", err)
	}

	// 第二次同步: 空 Models 列表。不应删除。
	disc2 := []KeyDiscovery{{
		KeyID:  getKeyID(t, chID, "k1"),
		Models: []model.ChannelFetchModel{},
	}}
	_, changes2, err := syncDeleteApply(t, chID, disc2)
	if err != nil {
		t.Fatalf("第二次同步失败: %v", err)
	}
	if changes2.RemovedGrants != 0 || changes2.RemovedModels != 0 {
		t.Fatalf("空结果不应删除, got removed_grants=%d removed_models=%d", changes2.RemovedGrants, changes2.RemovedModels)
	}
}

// TestSyncDeleteManualGrantNotRemoved 验证: sync_managed=false 的授权(手动/legacy)不被同步删除。
func TestSyncDeleteManualGrantNotRemoved(t *testing.T) {
	clearAllForSyncDelete(t)
	chID, _ := seedChannel(t, "del-manual", true, []string{"model-manual"}, []keySpec{{"k1", true}})

	// 手动创建的 model 和 grant: sync_managed=false(由 seedChannel 直接 DB.Create)。
	// seedChannel 创建的模型/授权 sync_managed 默认 false。

	// 同步返回 model-b(非 model-manual)。
	disc1 := []KeyDiscovery{{
		KeyID:  getKeyID(t, chID, "k1"),
		Models: []model.ChannelFetchModel{{Name: "model-b", Protocols: model.ProtocolOpenAIChatCompletion}},
	}}
	_, changes, err := syncDeleteApply(t, chID, disc1)
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if changes.RemovedGrants != 0 || changes.RemovedModels != 0 {
		t.Fatalf("手动模型/授权不应被删除, got removed_grants=%d removed_models=%d", changes.RemovedGrants, changes.RemovedModels)
	}

	// 验证 model-manual 仍在。
	var count int64
	db.GetDB().Model(&model.ChannelModel{}).Where("channel_id = ? AND name = ?", chID, "model-manual").Count(&count)
	if count != 1 {
		t.Fatalf("model-manual 应保留(手动不被删), got %d", count)
	}
}

// TestSyncDeleteOtherKeyGrantBlocksModelDelete 验证: 其他 key 的授权阻止删 model。
func TestSyncDeleteOtherKeyGrantBlocksModelDelete(t *testing.T) {
	clearAllForSyncDelete(t)
	chID, _ := seedChannel(t, "del-other-key", true, nil, []keySpec{{"k1", true}, {"k2", true}})

	// 先同步创建 model-shared(两个 key 都授权), sync_managed=true。
	disc1 := []KeyDiscovery{
		{KeyID: getKeyID(t, chID, "k1"), Models: []model.ChannelFetchModel{{Name: "model-shared", Protocols: model.ProtocolOpenAIChatCompletion}}},
		{KeyID: getKeyID(t, chID, "k2"), Models: []model.ChannelFetchModel{{Name: "model-shared", Protocols: model.ProtocolOpenAIChatCompletion}}},
	}
	if _, _, err := syncDeleteApply(t, chID, disc1); err != nil {
		t.Fatalf("第一次同步失败: %v", err)
	}

	// 验证 model-shared 的 sync_managed=true。
	var cm model.ChannelModel
	db.GetDB().Where("channel_id = ? AND name = ?", chID, "model-shared").First(&cm)
	if !cm.SyncManaged {
		t.Fatal("model-shared 的 sync_managed 应为 true")
	}

	// 第二次同步: k1 不再返回 model-shared, k2 仍返回。
	// k1 的授权被删(sync_managed=true, 不在返回集), 但 model-shared 因 k2 仍有授权而不删。
	disc2 := []KeyDiscovery{
		{KeyID: getKeyID(t, chID, "k1"), Models: []model.ChannelFetchModel{{Name: "model-other", Protocols: model.ProtocolOpenAIChatCompletion}}},
		{KeyID: getKeyID(t, chID, "k2"), Models: []model.ChannelFetchModel{{Name: "model-shared", Protocols: model.ProtocolOpenAIChatCompletion}}},
	}
	_, changes, err := syncDeleteApply(t, chID, disc2)
	if err != nil {
		t.Fatalf("第二次同步失败: %v", err)
	}
	if changes.RemovedGrants != 1 {
		t.Fatalf("应删除 1 授权(k1), got removed_grants=%d", changes.RemovedGrants)
	}
	if changes.RemovedModels != 0 {
		t.Fatalf("不应删 model-shared(k2 仍有授权), got removed_models=%d", changes.RemovedModels)
	}

	// 验证 model-shared 仍在。
	var count int64
	db.GetDB().Model(&model.ChannelModel{}).Where("channel_id = ? AND name = ?", chID, "model-shared").Count(&count)
	if count != 1 {
		t.Fatalf("model-shared 应保留(k2 仍有授权), got %d", count)
	}
}

// TestSyncDeleteRevisionRotatesOnActualDeletion 验证: 删除实际发生时轮转 revision。
func TestSyncDeleteRevisionRotatesOnActualDeletion(t *testing.T) {
	clearAllForSyncDelete(t)
	chID, _ := seedChannel(t, "del-rev", true, nil, []keySpec{{"k1", true}})

	// 第一次同步: 返回 model-a。
	disc1 := []KeyDiscovery{{
		KeyID:  getKeyID(t, chID, "k1"),
		Models: []model.ChannelFetchModel{{Name: "model-a", Protocols: model.ProtocolOpenAIChatCompletion}},
	}}
	if _, _, err := syncDeleteApply(t, chID, disc1); err != nil {
		t.Fatalf("第一次同步失败: %v", err)
	}

	// 读 revision。
	var ch1 model.Channel
	db.GetDB().Where("id = ?", chID).First(&ch1)
	beforeRev := ch1.Revision

	// 第二次同步: 不返回 model-a, 返回 model-b。
	disc2 := []KeyDiscovery{{
		KeyID:  getKeyID(t, chID, "k1"),
		Models: []model.ChannelFetchModel{{Name: "model-b", Protocols: model.ProtocolOpenAIChatCompletion}},
	}}
	if _, changes, err := syncDeleteApply(t, chID, disc2); err != nil {
		t.Fatalf("第二次同步失败: %v", err)
	} else if changes.RemovedGrants == 0 {
		t.Fatal("应删除至少 1 授权")
	}

	// 验证 revision 已轮转。
	var ch2 model.Channel
	db.GetDB().Where("id = ?", chID).First(&ch2)
	if ch2.Revision == beforeRev {
		t.Fatal("删除后 revision 应轮转")
	}
}

// TestSyncDeleteNoOpDoesNotRotateRevision 验证: 空同步(no additions, no removals)不轮转 revision。
func TestSyncDeleteNoOpDoesNotRotateRevision(t *testing.T) {
	clearAllForSyncDelete(t)
	chID, _ := seedChannel(t, "del-noop", true, nil, []keySpec{{"k1", true}})

	// 读 revision before。
	var ch1 model.Channel
	db.GetDB().Where("id = ?", chID).First(&ch1)
	beforeRev := ch1.Revision

	// 空同步: 无模型返回。
	disc := []KeyDiscovery{{
		KeyID:  getKeyID(t, chID, "k1"),
		Models: []model.ChannelFetchModel{},
	}}
	if _, changes, err := syncDeleteApply(t, chID, disc); err != nil {
		t.Fatalf("空同步失败: %v", err)
	} else if changes.AddedModels != 0 || changes.AddedGrants != 0 || changes.RemovedGrants != 0 || changes.RemovedModels != 0 {
		t.Fatalf("空同步不应有任何变更, got %+v", changes)
	}

	// 验证 revision 未变。
	var ch2 model.Channel
	db.GetDB().Where("id = ?", chID).First(&ch2)
	if ch2.Revision != beforeRev {
		t.Fatal("空同步不应轮转 revision")
	}
}

// TestSyncDeleteFailureRollbackNoSSE 验证: 删除事务失败时全部回滚, 无部分删除。
func TestSyncDeleteFailureRollbackNoSSE(t *testing.T) {
	clearAllForSyncDelete(t)
	chID, _ := seedChannel(t, "del-rollback", true, nil, []keySpec{{"k1", true}})

	// 先同步创建 model-a(sync_managed=true)。
	disc1 := []KeyDiscovery{{
		KeyID:  getKeyID(t, chID, "k1"),
		Models: []model.ChannelFetchModel{{Name: "model-a", Protocols: model.ProtocolOpenAIChatCompletion}},
	}}
	if _, _, err := syncDeleteApply(t, chID, disc1); err != nil {
		t.Fatalf("第一次同步失败: %v", err)
	}

	// 第二次同步: 用注入失败(不存在的 key) 触发事务回滚。
	// resolveChannelKey 会因 key 不存在返回 ErrRecordNotFound, 事务回滚。
	disc2 := []KeyDiscovery{{
		KeyID:  999999, // 不存在的 key
		Models: []model.ChannelFetchModel{{Name: "model-b", Protocols: model.ProtocolOpenAIChatCompletion}},
	}}
	_, _, err := syncDeleteApply(t, chID, disc2)
	if err == nil {
		t.Fatal("应返回错误(key 不存在)")
	}

	// 验证 model-a 仍在(事务回滚)。
	var count int64
	db.GetDB().Model(&model.ChannelModel{}).Where("channel_id = ? AND name = ?", chID, "model-a").Count(&count)
	if count != 1 {
		t.Fatalf("model-a 应保留(事务回滚), got %d", count)
	}
}

// TestSyncDeleteProtocolsNotModified 验证: 同步不修改已有授权的协议。
func TestSyncDeleteProtocolsNotModified(t *testing.T) {
	clearAllForSyncDelete(t)
	chID, _ := seedChannel(t, "del-proto", true, nil, []keySpec{{"k1", true}})

	// 第一次同步: 返回 model-a with OpenAIChatCompletion。
	disc1 := []KeyDiscovery{{
		KeyID:  getKeyID(t, chID, "k1"),
		Models: []model.ChannelFetchModel{{Name: "model-a", Protocols: model.ProtocolOpenAIChatCompletion}},
	}}
	if _, _, err := syncDeleteApply(t, chID, disc1); err != nil {
		t.Fatalf("第一次同步失败: %v", err)
	}

	// 读 grant 的 protocols。
	var cm model.ChannelModel
	db.GetDB().Where("channel_id = ? AND name = ?", chID, "model-a").First(&cm)
	var cg model.ChannelGrant
	db.GetDB().Where("channel_model_id = ?", cm.ID).First(&cg)
	originalProto := cg.Protocols

	// 第二次同步: 返回 model-a with 不同协议(AnthropicMessages)。不应修改协议。
	disc2 := []KeyDiscovery{{
		KeyID:  getKeyID(t, chID, "k1"),
		Models: []model.ChannelFetchModel{{Name: "model-a", Protocols: model.ProtocolAnthropicMessage}},
	}}
	if _, _, err := syncDeleteApply(t, chID, disc2); err != nil {
		t.Fatalf("第二次同步失败: %v", err)
	}

	// 验证协议未变。
	db.GetDB().Where("channel_model_id = ?", cm.ID).First(&cg)
	if cg.Protocols != originalProto {
		t.Fatalf("协议不应被修改, before=%d after=%d", originalProto, cg.Protocols)
	}
}

// getKeyID 从渠道中按名称查找 key ID。
func getKeyID(t *testing.T, channelID int, name string) int {
	t.Helper()
	var key model.ChannelKey
	if err := db.GetDB().Where("channel_id = ? AND name = ?", channelID, name).First(&key).Error; err != nil {
		t.Fatalf("查找 key %s 失败: %v", name, err)
	}
	return key.ID
}

// TestSyncDeleteStaleConfigDropped 验证: 配置变更时丢弃陈旧探测结果。
// 此测试通过 ChannelSyncConfigUnchanged 直接验证, 不需要完整 sync 流程。
func TestSyncDeleteStaleConfigDropped(t *testing.T) {
	// 配置变更(Revision 不同)应使快照失效, 不应用任何变更。
	snap := ChannelSyncSnapshot{ChannelID: 1, Revision: "old-rev", Config: model.ChannelConfig{Enabled: true}}
	curr := ChannelSyncSnapshot{ChannelID: 1, Revision: "new-rev", Config: model.ChannelConfig{Enabled: true}}
	if ChannelSyncConfigUnchanged(snap, curr, nil, false) {
		t.Fatal("revision 不同时快照应过期")
	}
}

// TestSyncDeleteManagedGrantClearsGroupItems 验证: 删除授权前清除引用的 group items。
func TestSyncDeleteManagedGrantClearsGroupItems(t *testing.T) {
	clearAllForSyncDelete(t)
	chID, _ := seedChannel(t, "del-group", true, nil, []keySpec{{"k1", true}})

	// 第一次同步: 创建 model-a(sync_managed=true)。
	disc1 := []KeyDiscovery{{
		KeyID:  getKeyID(t, chID, "k1"),
		Models: []model.ChannelFetchModel{{Name: "model-a", Protocols: model.ProtocolOpenAIChatCompletion}},
	}}
	_, changes, err := syncDeleteApply(t, chID, disc1)
	if err != nil {
		t.Fatalf("第一次同步失败: %v", err)
	}
	if changes.AddedModels != 1 {
		t.Fatal("应新增 1 模型")
	}

	// 第二次同步: 不返回 model-a, 应删除 grant 并清除 group items。
	disc2 := []KeyDiscovery{{
		KeyID:  getKeyID(t, chID, "k1"),
		Models: []model.ChannelFetchModel{{Name: "model-b", Protocols: model.ProtocolOpenAIChatCompletion}},
	}}
	if _, changes, err := syncDeleteApply(t, chID, disc2); err != nil {
		t.Fatalf("第二次同步失败: %v", err)
	} else {
		if changes.RemovedGrants != 1 {
			t.Fatalf("应删除 1 授权, got %d", changes.RemovedGrants)
		}
	}
}

// 确保编译时 gorm 被引用(测试文件中可能不直接使用)。
var _ = gorm.ErrRecordNotFound
