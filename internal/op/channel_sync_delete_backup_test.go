package op

import (
	"context"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
)

// TestBackupVersion6ExportIncludesSyncManaged 验证导出 dump 包含 sync_managed 字段。
func TestBackupVersion6ExportIncludesSyncManaged(t *testing.T) {
	clearAllForSyncDelete(t)
	chID, _ := seedChannel(t, "bk-sm", true, []string{"model-sm"}, []keySpec{{"k1", true}})

	// 手动设置 model + grant 的 sync_managed=true。
	var cm model.ChannelModel
	db.GetDB().Where("channel_id = ? AND name = ?", chID, "model-sm").First(&cm)
	db.GetDB().Model(&cm).Update("sync_managed", true)
	db.GetDB().Model(&model.ChannelGrant{}).Where("channel_model_id = ?", cm.ID).Update("sync_managed", true)

	// 导出。
	dump, err := DBExportAll(context.Background())
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	if dump.Version != 6 {
		t.Fatalf("导出版本应为 6, got %d", dump.Version)
	}

	// 验证 dump 中有 sync_managed=true。
	found := false
	for _, m := range dump.ChannelModels {
		if m.Name == "model-sm" && m.SyncManaged {
			found = true
		}
	}
	if !found {
		t.Fatal("导出 dump 中应包含 sync_managed=true 的 model-sm")
	}
}

// TestBackupVersion6ImportOldDumpDefaultsFalse 验证旧 dump(无 sync_managed 字段)导入后默认 false。
func TestBackupVersion6ImportOldDumpDefaultsFalse(t *testing.T) {
	clearAllForSyncDelete(t)
	chID, _ := seedChannel(t, "old-dump-ch", true, nil, []keySpec{{"k1", true}})

	// 构造旧 dump: version=5, channel_models 引用已存在渠道。
	cm := model.ChannelModel{ID: 100, ChannelID: chID, Name: "old-model"}
	cg := model.ChannelGrant{ID: 200, ChannelModelID: 100, ChannelKeyID: getKeyID(t, chID, "k1"), Protocols: model.ProtocolOpenAIChatCompletion}

	dump := &model.DBDump{
		Version:       5,
		Channels:      []model.Channel{}, // 渠道已存在
		ChannelModels: []model.ChannelModel{cm},
		ChannelGrants: []model.ChannelGrant{cg},
	}
	if _, err := DBImportIncremental(context.Background(), dump); err != nil {
		t.Fatalf("导入旧 dump 失败: %v", err)
	}

	// 验证导入后 sync_managed=false。
	var afterCM model.ChannelModel
	db.GetDB().Where("name = ?", "old-model").First(&afterCM)
	if afterCM.SyncManaged {
		t.Fatal("旧 dump 导入后 sync_managed 应为 false(默认)")
	}
	var afterCG model.ChannelGrant
	db.GetDB().Where("channel_model_id = ?", afterCM.ID).First(&afterCG)
	if afterCG.SyncManaged {
		t.Fatal("旧 dump 导入后 grant sync_managed 应为 false(默认)")
	}
}

// TestBackupVersion6ImportDoesNotUpgradeSyncManaged 验证导入不把目标 false 升级为 true。
func TestBackupVersion6ImportDoesNotUpgradeSyncManaged(t *testing.T) {
	clearAllForSyncDelete(t)
	chID, _ := seedChannel(t, "bk-no-upgrade", true, []string{"model-existing"}, []keySpec{{"k1", true}})

	// 目标已存在 model + grant, sync_managed=false(由 seedChannel 创建)。
	var cm model.ChannelModel
	db.GetDB().Where("channel_id = ? AND name = ?", chID, "model-existing").First(&cm)

	// 构造 dump: 同 ID, sync_managed=true。
	dump := &model.DBDump{
		Version:  6,
		Channels: []model.Channel{},
		ChannelModels: []model.ChannelModel{
			{ID: cm.ID, ChannelID: chID, Name: "model-existing", SyncManaged: true},
		},
		ChannelGrants: []model.ChannelGrant{},
	}
	if _, err := DBImportIncremental(context.Background(), dump); err != nil {
		t.Fatalf("导入失败: %v", err)
	}

	// 验证 sync_managed 仍为 false(不升级)。
	var afterCM model.ChannelModel
	db.GetDB().Where("id = ?", cm.ID).First(&afterCM)
	if afterCM.SyncManaged {
		t.Fatal("导入不应把目标 false 升级为 true")
	}
}

// TestBackupVersion6ImportNewRowPreservesSyncManaged 验证新行(无冲突)导入时保留显式值。
func TestBackupVersion6ImportNewRowPreservesSyncManaged(t *testing.T) {
	clearAllForSyncDelete(t)
	chID, _ := seedChannel(t, "bk-new-row", true, nil, []keySpec{{"k1", true}})

	// 构造 dump: 新 model(不存在), sync_managed=true。
	// channel_id 设为已存在渠道。
	dump := &model.DBDump{
		Version:  6,
		Channels: []model.Channel{},
		ChannelModels: []model.ChannelModel{
			{ID: 999, ChannelID: chID, Name: "new-managed-model", SyncManaged: true},
		},
		ChannelGrants: []model.ChannelGrant{},
	}
	if _, err := DBImportIncremental(context.Background(), dump); err != nil {
		t.Fatalf("导入失败: %v", err)
	}

	// 验证 sync_managed=true(新行保留显式值)。
	var cm model.ChannelModel
	db.GetDB().Where("name = ?", "new-managed-model").First(&cm)
	if !cm.SyncManaged {
		t.Fatal("新行导入应保留 sync_managed=true")
	}
}
