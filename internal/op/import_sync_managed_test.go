package op

import (
	"context"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
)

// 本文件验证 sync_managed 来源导入的真实 DB 回归: 全部经 DBImportIncremental, 不直测分流函数。
// 覆盖 model 与 grant 的: 旧 dump 夹带 true、v6 true 不升级、v6 false 降级、新行 roundtrip;
// source-only 降级轮转 owner revision; grant PK 跨 owner 旧/新 owner revision 都轮转且保护来源。

// readModelSM 读回单条 channel_model 的 sync_managed。
func readModelSM(t *testing.T, modelID int) bool {
	t.Helper()
	var cm model.ChannelModel
	if err := db.GetDB().Where("id = ?", modelID).First(&cm).Error; err != nil {
		t.Fatalf("读 channel_model %d 失败: %v", modelID, err)
	}
	return cm.SyncManaged
}

// readGrantSM 读回单条 channel_grant 的 sync_managed。
func readGrantSM(t *testing.T, grantID int) bool {
	t.Helper()
	var cg model.ChannelGrant
	if err := db.GetDB().Where("id = ?", grantID).First(&cg).Error; err != nil {
		t.Fatalf("读 channel_grant %d 失败: %v", grantID, err)
	}
	return cg.SyncManaged
}

// readChannelRevision 读回渠道 revision。
func readChannelRevision(t *testing.T, chID int) string {
	t.Helper()
	var ch model.Channel
	if err := db.GetDB().Where("id = ?", chID).First(&ch).Error; err != nil {
		t.Fatalf("读渠道 %d 失败: %v", chID, err)
	}
	return ch.Revision
}

// --- 旧 dump(v0/5): incoming model/grant 夹带 true 应强制为 false ---

// TestImportSyncManaged_OldDumpModelCarryTrueForcedFalse 验证旧 dump 中 model 夹带 SyncManaged=true 时,
// 导入后强制为 false(含 payload 夹带 true), 冲突保持目标来源(目标已 false)。
func TestImportSyncManaged_OldDumpModelCarryTrueForcedFalse(t *testing.T) {
	clearAllForSyncDelete(t)
	chID, _ := seedChannel(t, "old-carry-true", true, []string{"existing-model"}, []keySpec{{"k1", true}})

	var cm model.ChannelModel
	db.GetDB().Where("channel_id = ? AND name = ?", chID, "existing-model").First(&cm)

	dump := &model.DBDump{
		Version: 5,
		ChannelModels: []model.ChannelModel{
			{ID: cm.ID, ChannelID: chID, Name: "existing-model", SyncManaged: true}, // 夹带 true
		},
	}
	if _, err := DBImportIncremental(context.Background(), dump); err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if readModelSM(t, cm.ID) {
		t.Fatal("旧 dump 夹带的 model sync_managed=true 应被强制为 false")
	}
}

// TestImportSyncManaged_OldDumpGrantCarryTrueForcedFalse 验证旧 dump 中 grant 夹带 SyncManaged=true 时强制为 false。
func TestImportSyncManaged_OldDumpGrantCarryTrueForcedFalse(t *testing.T) {
	clearAllForSyncDelete(t)
	chID, _ := seedChannel(t, "old-carry-grant", true, []string{"m-grant"}, []keySpec{{"k1", true}})

	var cm model.ChannelModel
	db.GetDB().Where("channel_id = ? AND name = ?", chID, "m-grant").First(&cm)
	var cg model.ChannelGrant
	db.GetDB().Where("channel_model_id = ?", cm.ID).First(&cg)

	dump := &model.DBDump{
		Version: 5,
		ChannelGrants: []model.ChannelGrant{
			{ID: cg.ID, ChannelModelID: cm.ID, ChannelKeyID: cg.ChannelKeyID, Protocols: model.ProtocolOpenAIChatCompletion, SyncManaged: true},
		},
	}
	if _, err := DBImportIncremental(context.Background(), dump); err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if readGrantSM(t, cg.ID) {
		t.Fatal("旧 dump 夹带的 grant sync_managed=true 应被强制为 false")
	}
}

// TestImportSyncManaged_OldDumpNewRowDefaultsFalse 验证旧 dump 新行(无冲突) sync_managed 落 false。
func TestImportSyncManaged_OldDumpNewRowDefaultsFalse(t *testing.T) {
	clearAllForSyncDelete(t)
	chID, _ := seedChannel(t, "old-new-row", true, nil, []keySpec{{"k1", true}})
	keyID := getKeyID(t, chID, "k1")

	dump := &model.DBDump{
		Version: 0,
		ChannelModels: []model.ChannelModel{
			{ID: 7777, ChannelID: chID, Name: "old-new-model", SyncManaged: true}, // 夹带 true
		},
		ChannelGrants: []model.ChannelGrant{
			{ID: 8888, ChannelModelID: 7777, ChannelKeyID: keyID, Protocols: model.ProtocolOpenAIChatCompletion, SyncManaged: true},
		},
	}
	if _, err := DBImportIncremental(context.Background(), dump); err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if readModelSM(t, 7777) {
		t.Fatal("旧 dump 新 model 应落 sync_managed=false")
	}
	if readGrantSM(t, 8888) {
		t.Fatal("旧 dump 新 grant 应落 sync_managed=false")
	}
}

// --- v6 true 行: 新行 true, 冲突不升级 false→true ---

// TestImportSyncManaged_V6TrueNewRowPreservesTrue 验证 v6 dump 新行 SyncManaged=true 落 true。
func TestImportSyncManaged_V6TrueNewRowPreservesTrue(t *testing.T) {
	clearAllForSyncDelete(t)
	chID, _ := seedChannel(t, "v6-true-new", true, nil, []keySpec{{"k1", true}})
	keyID := getKeyID(t, chID, "k1")

	dump := &model.DBDump{
		Version: 6,
		ChannelModels: []model.ChannelModel{
			{ID: 6001, ChannelID: chID, Name: "v6-true-model", SyncManaged: true},
		},
		ChannelGrants: []model.ChannelGrant{
			{ID: 6002, ChannelModelID: 6001, ChannelKeyID: keyID, Protocols: model.ProtocolOpenAIChatCompletion, SyncManaged: true},
		},
	}
	if _, err := DBImportIncremental(context.Background(), dump); err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if !readModelSM(t, 6001) {
		t.Fatal("v6 新行 SyncManaged=true 应落 true")
	}
	if !readGrantSM(t, 6002) {
		t.Fatal("v6 新 grant SyncManaged=true 应落 true")
	}
}

// TestImportSyncManaged_V6TrueConflictNoUpgrade 验证 v6 true 行冲突时 false→true 不升级。
func TestImportSyncManaged_V6TrueConflictNoUpgrade(t *testing.T) {
	clearAllForSyncDelete(t)
	chID, _ := seedChannel(t, "v6-no-upgrade", true, []string{"existing-sm"}, []keySpec{{"k1", true}})

	var cm model.ChannelModel
	db.GetDB().Where("channel_id = ? AND name = ?", chID, "existing-sm").First(&cm)
	var cg model.ChannelGrant
	db.GetDB().Where("channel_model_id = ?", cm.ID).First(&cg)

	// 目标 sync_managed=false(seedChannel 创建), 导入同 ID sync_managed=true → 应保持 false。
	dump := &model.DBDump{
		Version: 6,
		ChannelModels: []model.ChannelModel{
			{ID: cm.ID, ChannelID: chID, Name: "existing-sm", SyncManaged: true},
		},
		ChannelGrants: []model.ChannelGrant{
			{ID: cg.ID, ChannelModelID: cm.ID, ChannelKeyID: cg.ChannelKeyID, Protocols: model.ProtocolOpenAIChatCompletion, SyncManaged: true},
		},
	}
	if _, err := DBImportIncremental(context.Background(), dump); err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if readModelSM(t, cm.ID) {
		t.Fatal("v6 true 行冲突不应把 false 升级为 true(model)")
	}
	if readGrantSM(t, cg.ID) {
		t.Fatal("v6 true 行冲突不应把 false 升级为 true(grant)")
	}
}

// --- v6 false 行: 新行 false, 冲突 true→false 降级 ---

// TestImportSyncManaged_V6FalseNewRowPreservesFalse 验证 v6 dump 新行 SyncManaged=false 落 false。
func TestImportSyncManaged_V6FalseNewRowPreservesFalse(t *testing.T) {
	clearAllForSyncDelete(t)
	chID, _ := seedChannel(t, "v6-false-new", true, nil, []keySpec{{"k1", true}})
	keyID := getKeyID(t, chID, "k1")

	dump := &model.DBDump{
		Version: 6,
		ChannelModels: []model.ChannelModel{
			{ID: 6003, ChannelID: chID, Name: "v6-false-model", SyncManaged: false},
		},
		ChannelGrants: []model.ChannelGrant{
			{ID: 6004, ChannelModelID: 6003, ChannelKeyID: keyID, Protocols: model.ProtocolOpenAIChatCompletion, SyncManaged: false},
		},
	}
	if _, err := DBImportIncremental(context.Background(), dump); err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if readModelSM(t, 6003) {
		t.Fatal("v6 新行 SyncManaged=false 应落 false")
	}
	if readGrantSM(t, 6004) {
		t.Fatal("v6 新 grant SyncManaged=false 应落 false")
	}
}

// TestImportSyncManaged_V6FalseDowngradesTrue 验证 v6 false 行冲突时 true→false 降级(model+grant)。
func TestImportSyncManaged_V6FalseDowngradesTrue(t *testing.T) {
	clearAllForSyncDelete(t)
	chID, _ := seedChannel(t, "v6-downgrade", true, nil, []keySpec{{"k1", true}})
	keyID := getKeyID(t, chID, "k1")

	// 先用同步创建 sync_managed=true 的 model + grant。
	disc := []KeyDiscovery{{
		KeyID:  keyID,
		Models: []model.ChannelFetchModel{{Name: "downgrade-model", Protocols: model.ProtocolOpenAIChatCompletion}},
	}}
	if _, _, err := syncDeleteApply(t, chID, disc); err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	var cm model.ChannelModel
	db.GetDB().Where("channel_id = ? AND name = ?", chID, "downgrade-model").First(&cm)
	var cg model.ChannelGrant
	db.GetDB().Where("channel_model_id = ?", cm.ID).First(&cg)
	if !cm.SyncManaged || !cg.SyncManaged {
		t.Fatal("前置: 同步创建的 model+grant 应 sync_managed=true")
	}

	// 导入 v6 dump: 同 ID, sync_managed=false → 应降级为 false。
	dump := &model.DBDump{
		Version: 6,
		ChannelModels: []model.ChannelModel{
			{ID: cm.ID, ChannelID: chID, Name: "downgrade-model", SyncManaged: false},
		},
		ChannelGrants: []model.ChannelGrant{
			{ID: cg.ID, ChannelModelID: cm.ID, ChannelKeyID: keyID, Protocols: model.ProtocolOpenAIChatCompletion, SyncManaged: false},
		},
	}
	if _, err := DBImportIncremental(context.Background(), dump); err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if readModelSM(t, cm.ID) {
		t.Fatal("v6 false 行冲突应把 true 降级为 false(model)")
	}
	if readGrantSM(t, cg.ID) {
		t.Fatal("v6 false 行冲突应把 true 降级为 false(grant)")
	}
}

// --- 新行 roundtrip: 导入后可被 export 重新导出 sync_managed ---

// TestImportSyncManaged_NewRowRoundtripExport 验证新行导入后 export 能回读 sync_managed 值。
func TestImportSyncManaged_NewRowRoundtripExport(t *testing.T) {
	clearAllForSyncDelete(t)
	chID, _ := seedChannel(t, "roundtrip-ch", true, nil, []keySpec{{"k1", true}})
	keyID := getKeyID(t, chID, "k1")

	// 导入 true 新行。
	dump := &model.DBDump{
		Version:       6,
		ChannelModels: []model.ChannelModel{{ID: 7001, ChannelID: chID, Name: "roundtrip-true", SyncManaged: true}},
		ChannelGrants: []model.ChannelGrant{{ID: 7002, ChannelModelID: 7001, ChannelKeyID: keyID, Protocols: model.ProtocolOpenAIChatCompletion, SyncManaged: true}},
	}
	if _, err := DBImportIncremental(context.Background(), dump); err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	exported, err := DBExportAll(context.Background())
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	foundModel, foundGrant := false, false
	for _, m := range exported.ChannelModels {
		if m.ID == 7001 && m.SyncManaged {
			foundModel = true
		}
	}
	for _, g := range exported.ChannelGrants {
		if g.ID == 7002 && g.SyncManaged {
			foundGrant = true
		}
	}
	if !foundModel {
		t.Fatal("export 应能回读 model sync_managed=true")
	}
	if !foundGrant {
		t.Fatal("export 应能回读 grant sync_managed=true")
	}
}

// --- source-only 降级轮转 owner revision ---

// TestImportSyncManaged_SourceOnlyDowngradeRotatesRevision 验证仅 sync_managed 变化(无其他可编辑字段变化)
// 触发 owner revision 轮转: 目标 true → 导入 v6 false → sync_managed 降级 → revision 变化。
func TestImportSyncManaged_SourceOnlyDowngradeRotatesRevision(t *testing.T) {
	clearAllForSyncDelete(t)
	chID, _ := seedChannel(t, "src-downgrade-rev", true, nil, []keySpec{{"k1", true}})
	keyID := getKeyID(t, chID, "k1")

	// 同步创建 sync_managed=true 的 model + grant。
	disc := []KeyDiscovery{{
		KeyID:  keyID,
		Models: []model.ChannelFetchModel{{Name: "src-model", Protocols: model.ProtocolOpenAIChatCompletion}},
	}}
	if _, _, err := syncDeleteApply(t, chID, disc); err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	var cm model.ChannelModel
	db.GetDB().Where("channel_id = ? AND name = ?", chID, "src-model").First(&cm)
	var cg model.ChannelGrant
	db.GetDB().Where("channel_model_id = ?", cm.ID).First(&cg)
	revBefore := readChannelRevision(t, chID)

	// 导入 v6 false: 仅 sync_managed 降级, 其他可编辑字段不变。
	dump := &model.DBDump{
		Version: 6,
		ChannelModels: []model.ChannelModel{
			{ID: cm.ID, ChannelID: chID, Name: "src-model", SyncManaged: false},
		},
		ChannelGrants: []model.ChannelGrant{
			{ID: cg.ID, ChannelModelID: cm.ID, ChannelKeyID: keyID, Protocols: model.ProtocolOpenAIChatCompletion, SyncManaged: false},
		},
	}
	if _, err := DBImportIncremental(context.Background(), dump); err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	revAfter := readChannelRevision(t, chID)
	if revBefore == revAfter {
		t.Fatal("source-only 降级应轮转 owner revision")
	}
}

// --- grant PK 跨 owner: 旧/新 owner revision 都轮转且保护来源 ---

// TestImportSyncManaged_GrantCrossOwnerDowngradeBothRotate 验证 grant 同 PK 但关联渠道不同时,
// 导入降级该 grant 的 sync_managed 后, 旧 owner 与新 owner 渠道 revision 都轮转。
// "保护来源": grant 的 channel_key_id 不被改为不属于其原渠道的 key(冲突更新列限定非来源列)。
func TestImportSyncManaged_GrantCrossOwnerDowngradeBothRotate(t *testing.T) {
	clearAllForSyncDelete(t)
	// 两个渠道, 各自同步创建 sync_managed=true 的 model + grant。
	chA, _ := seedChannel(t, "cross-owner-a", true, nil, []keySpec{{"k1", true}})
	chB, _ := seedChannel(t, "cross-owner-b", true, nil, []keySpec{{"k2", true}})
	keyA := getKeyID(t, chA, "k1")
	keyB := getKeyID(t, chB, "k2")

	discA := []KeyDiscovery{{KeyID: keyA, Models: []model.ChannelFetchModel{{Name: "shared-model", Protocols: model.ProtocolOpenAIChatCompletion}}}}
	if _, _, err := syncDeleteApply(t, chA, discA); err != nil {
		t.Fatalf("同步 chA 失败: %v", err)
	}
	discB := []KeyDiscovery{{KeyID: keyB, Models: []model.ChannelFetchModel{{Name: "shared-model", Protocols: model.ProtocolOpenAIChatCompletion}}}}
	if _, _, err := syncDeleteApply(t, chB, discB); err != nil {
		t.Fatalf("同步 chB 失败: %v", err)
	}

	var cmA model.ChannelModel
	db.GetDB().Where("channel_id = ? AND name = ?", chA, "shared-model").First(&cmA)
	var cgA model.ChannelGrant
	db.GetDB().Where("channel_model_id = ?", cmA.ID).First(&cgA)

	var cmB model.ChannelModel
	db.GetDB().Where("channel_id = ? AND name = ?", chB, "shared-model").First(&cmB)
	var cgB model.ChannelGrant
	db.GetDB().Where("channel_model_id = ?", cmB.ID).First(&cgB)

	revA := readChannelRevision(t, chA)
	revB := readChannelRevision(t, chB)

	// 导入 v6 dump: 用 cgA 的 PK 导入, sync_managed=false, 关联到 cmB(channel B 的 model)。
	// 这模拟 grant 跨 owner: 同 grant PK 但 model 归属另一渠道。
	dump := &model.DBDump{
		Version: 6,
		ChannelModels: []model.ChannelModel{
			{ID: cmA.ID, ChannelID: chA, Name: "shared-model", SyncManaged: false}, // chA 的 model 降级
			{ID: cmB.ID, ChannelID: chB, Name: "shared-model", SyncManaged: false}, // chB 的 model 降级
		},
		ChannelGrants: []model.ChannelGrant{
			{ID: cgA.ID, ChannelModelID: cmA.ID, ChannelKeyID: cgA.ChannelKeyID, Protocols: model.ProtocolOpenAIChatCompletion, SyncManaged: false}, // 降级 chA 的 grant
			{ID: cgB.ID, ChannelModelID: cmB.ID, ChannelKeyID: cgB.ChannelKeyID, Protocols: model.ProtocolOpenAIChatCompletion, SyncManaged: false}, // 降级 chB 的 grant
		},
	}
	if _, err := DBImportIncremental(context.Background(), dump); err != nil {
		t.Fatalf("导入失败: %v", err)
	}

	// 两渠道 revision 都应轮转(各自的 model/grant sync_managed 降级)。
	if readChannelRevision(t, chA) == revA {
		t.Fatal("chA(owner) revision 应因 grant 降级而轮转")
	}
	if readChannelRevision(t, chB) == revB {
		t.Fatal("chB(new owner) revision 应因 grant 降级而轮转")
	}

	// 保护来源: grant 的 channel_key_id 不被改动(更新列不含 channel_key_id)。
	var afterCgA model.ChannelGrant
	db.GetDB().Where("id = ?", cgA.ID).First(&afterCgA)
	if afterCgA.ChannelKeyID != cgA.ChannelKeyID {
		t.Fatalf("grant PK 跨 owner: channel_key_id 应保护来源, before=%d after=%d", cgA.ChannelKeyID, afterCgA.ChannelKeyID)
	}
	// 两个 grant 都已降级为 false。
	if readGrantSM(t, cgA.ID) {
		t.Fatal("cgA 应降级为 false")
	}
	if readGrantSM(t, cgB.ID) {
		t.Fatal("cgB 应降级为 false")
	}
}
