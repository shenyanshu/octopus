package migrate

import (
	"testing"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// TestMigration016AddsSyncManagedColumns 证明迁移 016 添加 sync_managed 列且旧行默认 false。
func TestMigration016AddsSyncManagedColumns(t *testing.T) {
	dbConn, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("建库失败: %v", err)
	}
	// AutoMigrate 建表(含 sync_managed 列, NOT NULL DEFAULT false)。
	if err := dbConn.AutoMigrate(&model.Channel{}, &model.ChannelModel{}, &model.ChannelGrant{}); err != nil {
		t.Fatalf("AutoMigrate 失败: %v", err)
	}

	// 插入一条渠道 + 模型 + 授权, AutoMigrate 后 sync_managed 默认 false。
	ch := model.Channel{ChannelConfig: model.ChannelConfig{Name: "m16", Enabled: true, BaseURL: "http://m16.example"}}
	if err := dbConn.Create(&ch).Error; err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	cm := model.ChannelModel{ChannelID: ch.ID, Name: "model-m16"}
	if err := dbConn.Create(&cm).Error; err != nil {
		t.Fatalf("建模型失败: %v", err)
	}
	cg := model.ChannelGrant{ChannelModelID: cm.ID, ChannelKeyID: 1, Protocols: model.ProtocolOpenAIChatCompletion}
	if err := dbConn.Create(&cg).Error; err != nil {
		t.Fatalf("建授权失败: %v", err)
	}

	// 运行迁移(幂等: 列已存在, 不报错)。
	if err := migrateSyncManaged(dbConn); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	// 验证 sync_managed 为 false(旧行默认)。
	var afterCM model.ChannelModel
	if err := dbConn.Where("id = ?", cm.ID).First(&afterCM).Error; err != nil {
		t.Fatalf("读回模型失败: %v", err)
	}
	if afterCM.SyncManaged {
		t.Fatal("旧行 sync_managed 应为 false")
	}
	var afterCG model.ChannelGrant
	if err := dbConn.Where("id = ?", cg.ID).First(&afterCG).Error; err != nil {
		t.Fatalf("读回授权失败: %v", err)
	}
	if afterCG.SyncManaged {
		t.Fatal("旧行 sync_managed 应为 false")
	}

	// 幂等: 再次运行不报错。
	if err := migrateSyncManaged(dbConn); err != nil {
		t.Fatalf("二次迁移失败: %v", err)
	}
}

// TestMigration016NilDBReturnsError 验证 nil DB 返回错误而非 panic。
func TestMigration016NilDBReturnsError(t *testing.T) {
	if err := migrateSyncManaged(nil); err == nil {
		t.Fatal("nil DB 应返回错误")
	}
}

var _ gorm.DB
