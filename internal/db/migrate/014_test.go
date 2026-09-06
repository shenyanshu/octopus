package migrate

import (
	"testing"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// TestMigration014BackfillsRevision 证明迁移 014:
// 1. 历史行(revision 为空)迁移后得到非空 UUID;
// 2. 已有非空 revision 的行不被覆盖(迁移幂等)。
func TestMigration014BackfillsRevision(t *testing.T) {
	dbConn, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("建库失败: %v", err)
	}
	// AutoMigrate 建 channels 表(含 revision 列, NOT NULL DEFAULT '')。
	if err := dbConn.AutoMigrate(&model.Channel{}); err != nil {
		t.Fatalf("AutoMigrate 失败: %v", err)
	}
	// 插入两条渠道: 一条 revision 为空(历史行), 一条已带 revision。
	bare := model.Channel{ChannelConfig: model.ChannelConfig{Name: "bare", Enabled: true, BaseURL: "http://bare.example"}}
	if err := dbConn.Create(&bare).Error; err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	// 手动清空第一条的 revision(模拟 AutoMigrate 新增列后的历史行)。
	if err := dbConn.Model(&model.Channel{}).Where("id = ?", bare.ID).Update("revision", "").Error; err != nil {
		t.Fatalf("清空 revision 失败: %v", err)
	}
	preset := model.Channel{ChannelConfig: model.ChannelConfig{Name: "preset", Enabled: true, BaseURL: "http://preset.example"}, Revision: "preset-uuid"}
	if err := dbConn.Create(&preset).Error; err != nil {
		t.Fatalf("建预设渠道失败: %v", err)
	}

	// 运行迁移。
	if err := migrateChannelRevision(dbConn); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	var afterBare model.Channel
	if err := dbConn.Where("id = ?", bare.ID).First(&afterBare).Error; err != nil {
		t.Fatalf("读回 bare 渠道失败: %v", err)
	}
	if afterBare.Revision == "" {
		t.Fatal("空 revision 行迁移后应补填非空 UUID")
	}
	var afterPreset model.Channel
	if err := dbConn.Where("id = ?", preset.ID).First(&afterPreset).Error; err != nil {
		t.Fatalf("读回 preset 渠道失败: %v", err)
	}
	if afterPreset.Revision != "preset-uuid" {
		t.Fatalf("已有 revision 应保持不变, got %q", afterPreset.Revision)
	}

	// 幂等: 再次运行, bare 行的 revision 不被再次改写。
	firstBareRev := afterBare.Revision
	if err := migrateChannelRevision(dbConn); err != nil {
		t.Fatalf("二次迁移失败: %v", err)
	}
	var afterBare2 model.Channel
	if err := dbConn.Where("id = ?", bare.ID).First(&afterBare2).Error; err != nil {
		t.Fatalf("二次读回 bare 渠道失败: %v", err)
	}
	if afterBare2.Revision != firstBareRev {
		t.Fatalf("迁移不幂等: %q -> %q", firstBareRev, afterBare2.Revision)
	}
}

// TestMigration014NoChannelsTableSkipsGracefully 验证 channels 表不存在时迁移不报错。
func TestMigration014NoChannelsTableSkipsGracefully(t *testing.T) {
	dbConn, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("建库失败: %v", err)
	}
	// 不建 channels 表。
	if err := migrateChannelRevision(dbConn); err != nil {
		t.Fatalf("无 channels 表时迁移应跳过不报错: %v", err)
	}
}

// TestMigration014NilDBReturnsError 验证 nil DB 返回错误而非 panic。
func TestMigration014NilDBReturnsError(t *testing.T) {
	if err := migrateChannelRevision(nil); err == nil {
		t.Fatal("nil DB 应返回错误")
	}
}

var _ gorm.DB
