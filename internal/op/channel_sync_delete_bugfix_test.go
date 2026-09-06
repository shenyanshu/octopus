package op

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// TestSyncDeleteManualModelSyncGrantPreservesModel 验证: manual 模型上的 sync 授权可删, 但模型保留。
func TestSyncDeleteManualModelSyncGrantPreservesModel(t *testing.T) {
	clearAllForSyncDelete(t)
	chID, _ := seedChannel(t, "del-manual-model", true, nil, []keySpec{{"k1", true}})

	disc1 := []KeyDiscovery{{
		KeyID:  getKeyID(t, chID, "k1"),
		Models: []model.ChannelFetchModel{{Name: "model-a", Protocols: model.ProtocolOpenAIChatCompletion}},
	}}
	if _, _, err := syncDeleteApply(t, chID, disc1); err != nil {
		t.Fatalf("第一次同步失败: %v", err)
	}

	// 将 model-a 改为 manual(sync_managed=false), 保留 grant 的 sync_managed=true。
	var cm model.ChannelModel
	db.GetDB().Where("channel_id = ? AND name = ?", chID, "model-a").First(&cm)
	db.GetDB().Model(&cm).Update("sync_managed", false)

	// 第二次同步: 不返回 model-a, 返回 model-b。
	// grant(sync_managed=true) 应被删, model(sync_managed=false) 应保留。
	disc2 := []KeyDiscovery{{
		KeyID:  getKeyID(t, chID, "k1"),
		Models: []model.ChannelFetchModel{{Name: "model-b", Protocols: model.ProtocolOpenAIChatCompletion}},
	}}
	_, changes2, err := syncDeleteApply(t, chID, disc2)
	if err != nil {
		t.Fatalf("第二次同步失败: %v", err)
	}
	if changes2.RemovedGrants != 1 {
		t.Fatalf("应删除 1 授权(sync_managed=true), got %d", changes2.RemovedGrants)
	}
	if changes2.RemovedModels != 0 {
		t.Fatalf("不应删 manual 模型(sync_managed=false), got %d", changes2.RemovedModels)
	}

	// 验证 model-a 仍在。
	var count int64
	db.GetDB().Model(&model.ChannelModel{}).Where("channel_id = ? AND name = ?", chID, "model-a").Count(&count)
	if count != 1 {
		t.Fatalf("model-a(manual) 应保留, got %d", count)
	}
}

// TestSyncDeleteClearsActiveItemID 验证: 删除授权前清除 group.active_item_id。
func TestSyncDeleteClearsActiveItemID(t *testing.T) {
	clearAllForSyncDelete(t)
	chID, _ := seedChannel(t, "del-active", true, nil, []keySpec{{"k1", true}})

	disc1 := []KeyDiscovery{{
		KeyID:  getKeyID(t, chID, "k1"),
		Models: []model.ChannelFetchModel{{Name: "model-a", Protocols: model.ProtocolOpenAIChatCompletion}},
	}}
	if _, _, err := syncDeleteApply(t, chID, disc1); err != nil {
		t.Fatalf("第一次同步失败: %v", err)
	}

	var cm model.ChannelModel
	db.GetDB().Where("channel_id = ? AND name = ?", chID, "model-a").First(&cm)
	var cg model.ChannelGrant
	db.GetDB().Where("channel_model_id = ?", cm.ID).First(&cg)

	grp := model.Group{Name: "grp-active"}
	if err := db.GetDB().Create(&grp).Error; err != nil {
		t.Fatalf("建分组失败: %v", err)
	}
	item := model.GroupItem{GroupID: grp.ID, ChannelGrantID: cg.ID, Score: 100, Enabled: true}
	if err := db.GetDB().Create(&item).Error; err != nil {
		t.Fatalf("建分组项失败: %v", err)
	}
	activeID := item.ID
	db.GetDB().Model(&grp).Where("id = ?", grp.ID).Update("active_item_id", activeID)

	// 验证 active_item_id 已设置。
	var beforeGrp model.Group
	db.GetDB().Where("id = ?", grp.ID).First(&beforeGrp)
	if beforeGrp.ActiveItemID != activeID {
		t.Fatalf("active_item_id 应为 %d, got %d", activeID, beforeGrp.ActiveItemID)
	}

	// 第二次同步: 不返回 model-a, 删除 grant → 应先清除 active_item_id。
	disc2 := []KeyDiscovery{{
		KeyID:  getKeyID(t, chID, "k1"),
		Models: []model.ChannelFetchModel{{Name: "model-b", Protocols: model.ProtocolOpenAIChatCompletion}},
	}}
	_, changes2, err := syncDeleteApply(t, chID, disc2)
	if err != nil {
		t.Fatalf("第二次同步失败: %v", err)
	}
	if changes2.RemovedGrants != 1 {
		t.Fatalf("应删除 1 授权, got %d", changes2.RemovedGrants)
	}

	// 验证 active_item_id 已清零。
	var afterGrp model.Group
	db.GetDB().Where("id = ?", grp.ID).First(&afterGrp)
	if afterGrp.ActiveItemID != 0 {
		t.Fatalf("active_item_id 应已清零, got %d", afterGrp.ActiveItemID)
	}
	// 验证 group item 已被 FK 级联删除。
	var itemCount int64
	db.GetDB().Model(&model.GroupItem{}).Where("group_id = ?", grp.ID).Count(&itemCount)
	if itemCount != 0 {
		t.Fatalf("group item 应已删除(FK 级联), got %d", itemCount)
	}
}

// TestSyncDeleteGrantInsertFailureRollsBack 验证: 第二次同步新增 grant 时 INSERT 失败, 事务回滚, 无残留。
// 失败发生在 INSERT 阶段(applySyncDeletions 之前), 不是删除后失败;
// 删除后失败回滚由 channelsync 包的 GORM After(gorm:update) 回调测试覆盖。
func TestSyncDeleteGrantInsertFailureRollsBack(t *testing.T) {
	clearAllForSyncDelete(t)
	chID, _ := seedChannel(t, "del-rollback-grant", true, nil, []keySpec{{"k1", true}})

	// 第一次同步: 创建 model-a(sync_managed=true) + grant。
	disc1 := []KeyDiscovery{{
		KeyID:  getKeyID(t, chID, "k1"),
		Models: []model.ChannelFetchModel{{Name: "model-a", Protocols: model.ProtocolOpenAIChatCompletion}},
	}}
	if _, _, err := syncDeleteApply(t, chID, disc1); err != nil {
		t.Fatalf("第一次同步失败: %v", err)
	}

	// 保存 before 计数。
	var grantsBefore, modelsBefore, itemsBefore int64
	db.GetDB().Model(&model.ChannelGrant{}).Where("channel_model_id IN (SELECT id FROM channel_models WHERE channel_id = ?)", chID).Count(&grantsBefore)
	db.GetDB().Model(&model.ChannelModel{}).Where("channel_id = ?", chID).Count(&modelsBefore)
	var cm model.ChannelModel
	db.GetDB().Where("channel_id = ? AND name = ?", chID, "model-a").First(&cm)
	db.GetDB().Model(&model.GroupItem{}).Where("channel_grant_id IN (SELECT id FROM channel_grants WHERE channel_model_id = ?)", cm.ID).Count(&itemsBefore)

	// 创建 group + item 指向 model-a 的 grant。
	var cg model.ChannelGrant
	db.GetDB().Where("channel_model_id = ?", cm.ID).First(&cg)
	grp := model.Group{Name: "grp-rollback", ActiveItemID: 0}
	db.GetDB().Create(&grp)
	item := model.GroupItem{GroupID: grp.ID, ChannelGrantID: cg.ID, Score: 100, Enabled: true}
	db.GetDB().Create(&item)
	activeID := item.ID
	db.GetDB().Model(&grp).Where("id = ?", grp.ID).Update("active_item_id", activeID)

	// 注入: 在 channel_grants 的 gorm:create 回调中返回错误(第二次同步新增 model-b 的 grant)。
	var triggered atomic.Bool
	rollbackCB := func(tx *gorm.DB) {
		if tx.Statement.Table == "channel_grants" && !triggered.Load() {
			triggered.Store(true)
			tx.AddError(errors.New("injected grant insert failure"))
		}
	}
	callbackName := "bugfix_test_rollback_" + gormColumn(chID)
	if err := db.GetDB().Callback().Create().After("gorm:create").Register(callbackName, rollbackCB); err != nil {
		t.Fatalf("注册回调失败: %v", err)
	}
	defer db.GetDB().Callback().Create().Remove(callbackName)

	// 第二次同步: model-b 新增 grant 时 INSERT 失败 → 事务回滚。
	// 失败在 INSERT 阶段, 此时 applySyncDeletions 尚未执行, 整个事务回滚后恢复。
	disc2 := []KeyDiscovery{{
		KeyID:  getKeyID(t, chID, "k1"),
		Models: []model.ChannelFetchModel{{Name: "model-b", Protocols: model.ProtocolOpenAIChatCompletion}},
	}}
	_, _, err := syncDeleteApply(t, chID, disc2)
	if err == nil {
		t.Fatal("应返回错误(注入的 grant insert 失败)")
	}

	// 验证无残留: grant/model/item/active 恢复。
	var grantsAfter, modelsAfter int64
	db.GetDB().Model(&model.ChannelGrant{}).Where("channel_model_id IN (SELECT id FROM channel_models WHERE channel_id = ?)", chID).Count(&grantsAfter)
	db.GetDB().Model(&model.ChannelModel{}).Where("channel_id = ?", chID).Count(&modelsAfter)
	if grantsAfter != grantsBefore {
		t.Fatalf("grant 数量应不变(事务回滚), before=%d after=%d", grantsBefore, grantsAfter)
	}
	if modelsAfter != modelsBefore {
		t.Fatalf("model 数量应不变(事务回滚), before=%d after=%d", modelsBefore, modelsAfter)
	}
	// 验证 active_item_id 恢复。
	var restoredGrp model.Group
	db.GetDB().Where("id = ?", grp.ID).First(&restoredGrp)
	if restoredGrp.ActiveItemID != activeID {
		t.Fatalf("active_item_id 应恢复为 %d(事务回滚), got %d", activeID, restoredGrp.ActiveItemID)
	}
	// 验证 group item 恢复。
	var itemCount int64
	db.GetDB().Model(&model.GroupItem{}).Where("group_id = ?", grp.ID).Count(&itemCount)
	if itemCount != 1 {
		t.Fatalf("group item 应恢复为 1(事务回滚), got %d", itemCount)
	}
}

// TestSyncDeletePostCommitFailureDBOnly 验证: 提交后子缓存刷新失败时 DB 已提交删除, PostCommitError 返回 mutation。
// 此测试仅在 op 层检查 DB 状态; cache/route/SSE 的完整编排验证由 channelsync 包的 PostCommit 编排测试覆盖。
func TestSyncDeletePostCommitFailureDBOnly(t *testing.T) {
	clearAllForSyncDelete(t)
	chID, _ := seedChannel(t, "del-pc-fail", true, nil, []keySpec{{"k1", true}})

	// 第一次同步: 创建 model-a(sync_managed=true) + grant。
	disc1 := []KeyDiscovery{{
		KeyID:  getKeyID(t, chID, "k1"),
		Models: []model.ChannelFetchModel{{Name: "model-a", Protocols: model.ProtocolOpenAIChatCompletion}},
	}}
	if _, _, err := syncDeleteApply(t, chID, disc1); err != nil {
		t.Fatalf("第一次同步失败: %v", err)
	}

	// 创建 group + item 指向 model-a 的 grant, 设置 active_item_id。
	var cm model.ChannelModel
	db.GetDB().Where("channel_id = ? AND name = ?", chID, "model-a").First(&cm)
	var cg model.ChannelGrant
	db.GetDB().Where("channel_model_id = ?", cm.ID).First(&cg)
	grp := model.Group{Name: "grp-pc-fail"}
	db.GetDB().Create(&grp)
	item := model.GroupItem{GroupID: grp.ID, ChannelGrantID: cg.ID, Score: 100, Enabled: true}
	db.GetDB().Create(&item)
	activeID := item.ID
	db.GetDB().Model(&grp).Where("id = ?", grp.ID).Update("active_item_id", activeID)
	reloadAllCacheForSyncDelete(t)

	// 注入 reload 失败。
	origReload := reloadChannelChildren
	reloadChannelChildren = func(ctx context.Context, channelID int) error {
		return gorm.ErrInvalidDB
	}
	defer func() { reloadChannelChildren = origReload }()

	// 第二次同步: 删除 model-a 的 grant + model, 提交后子缓存刷新失败。
	disc2 := []KeyDiscovery{{
		KeyID:  getKeyID(t, chID, "k1"),
		Models: []model.ChannelFetchModel{{Name: "model-b", Protocols: model.ProtocolOpenAIChatCompletion}},
	}}
	tx := db.GetDB().Begin()
	mut, _, err := ChannelSyncApplyDiscovery(tx, chID, disc2)
	if err != nil {
		tx.Rollback()
		t.Fatalf("同步失败: %v", err)
	}
	if err := tx.Commit().Error; err != nil {
		t.Fatalf("提交失败: %v", err)
	}

	commitMut, commitErr := ChannelSyncCommitAndRefresh(context.Background(), chID, mut)
	if commitErr == nil {
		t.Fatal("应返回 PostCommitError")
	}
	var pce *PostCommitError
	if !errors.As(commitErr, &pce) {
		t.Fatalf("应返回 *PostCommitError, got %T", commitErr)
	}
	// mutation 可能为 nil(无分组模式匹配); 完整的 cache/route/SSE 编排验证见 channelsync 包。
	_ = commitMut

	// DB 中删除已落库。
	var modelCount int64
	db.GetDB().Model(&model.ChannelModel{}).Where("channel_id = ? AND name = ?", chID, "model-a").Count(&modelCount)
	if modelCount != 0 {
		t.Fatalf("model-a 应已被删除(已提交), got %d", modelCount)
	}

	// group.active_item_id 应已清零(clearActiveItems 在事务内执行)。
	var afterGrp model.Group
	db.GetDB().Where("id = ?", grp.ID).First(&afterGrp)
	if afterGrp.ActiveItemID != 0 {
		t.Fatalf("active_item_id 应已清零, got %d", afterGrp.ActiveItemID)
	}
	// group item 应已被 FK 级联删除。
	var itemCount int64
	db.GetDB().Model(&model.GroupItem{}).Where("group_id = ?", grp.ID).Count(&itemCount)
	if itemCount != 0 {
		t.Fatalf("group item 应已删除, got %d", itemCount)
	}
}

// TestUpsertWithColumnsSQLErrorPropagation 验证: helper 真实 SQL 失败时返回 error。
func TestUpsertWithColumnsSQLErrorPropagation(t *testing.T) {
	clearAllForSyncDelete(t)

	rows := []model.ChannelModel{{ID: 1, ChannelID: 1, Name: "test"}}
	_, err := upsertWithColumns(
		db.GetDB(),
		rows,
		[]clause.Column{{Name: "id"}},
		[]string{"nonexistent_column"},
	)
	if err == nil {
		t.Fatal("SQL 错误应返回 error")
	}
}

// gormColumn 将 int 转为字符串用于回调名唯一化。
func gormColumn(id int) string {
	return string(rune('A' + id%26))
}

// reloadAllCacheForSyncDelete 刷新所有缓存供测试使用。
func reloadAllCacheForSyncDelete(t *testing.T) {
	t.Helper()
	reloadAllCache(t)
}
