package migrate

import (
	"testing"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// legacyGroupV14 是新增 auto_add_pattern 列之前的 groups 表结构。
type legacyGroupV14 struct {
	ID           int    `gorm:"primaryKey"`
	Name         string `gorm:"unique;not null"`
	Mode         string `gorm:"not null;default:manual"`
	ActiveItemID int    `gorm:"not null;default:0"`
}

func (legacyGroupV14) TableName() string { return "groups" }

// TestAutoMigrateAddsAutoAddPatternBackfillsEmpty 证明新增 auto_add_pattern 列对旧库无副作用:
// 旧表无该列时 AutoMigrate 补列并回填空串, 旧行为不变; 显式写入的规则原样保留。
// 不新增专门迁移文件: AutoMigrate 凭列默认值即可完成补列, 与项目既有 AutoMigrate 配置一致。
func TestAutoMigrateAddsAutoAddPatternBackfillsEmpty(t *testing.T) {
	dbConn, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("建库失败: %v", err)
	}
	// 建旧表(无 auto_add_pattern 列)并插入历史行。
	if err := dbConn.AutoMigrate(&legacyGroupV14{}); err != nil {
		t.Fatalf("建旧表失败: %v", err)
	}
	if err := dbConn.Create(&legacyGroupV14{ID: 1, Name: "legacy", Mode: "scored"}).Error; err != nil {
		t.Fatalf("插入历史行失败: %v", err)
	}

	// 运行生产 AutoMigrate: 由 model.Group 补列, default:'' 回填旧行。
	if err := dbConn.AutoMigrate(&model.Group{}); err != nil {
		t.Fatalf("AutoMigrate 补列失败: %v", err)
	}

	// 旧行迁移后 auto_add_pattern 应为空串, 行为不变。
	var row1 model.Group
	if err := dbConn.First(&row1, 1).Error; err != nil {
		t.Fatalf("读迁移后行失败: %v", err)
	}
	if row1.AutoAddPattern != "" {
		t.Fatalf("历史行 auto_add_pattern = %q, 想要空串", row1.AutoAddPattern)
	}

	// 新行显式写规则并再读回: 列功能正常, 规则不被默认值吞掉。
	if err := dbConn.Create(&model.Group{ID: 2, Name: "with-rule", Mode: "scored", AutoAddPattern: "^gpt-4o$"}).Error; err != nil {
		t.Fatalf("写新行失败: %v", err)
	}
	var row2 model.Group
	dbConn.First(&row2, 2)
	if row2.AutoAddPattern != "^gpt-4o$" {
		t.Fatalf("显式规则被吞: %q", row2.AutoAddPattern)
	}

	// 显式空串也能写入(清除规则路径): 不被默认值覆盖。
	if err := dbConn.Model(&model.Group{}).Where("id = ?", 2).Update("auto_add_pattern", "").Error; err != nil {
		t.Fatalf("清空规则失败: %v", err)
	}
	dbConn.First(&row2, 2)
	if row2.AutoAddPattern != "" {
		t.Fatalf("清空后规则仍非空: %q", row2.AutoAddPattern)
	}
}
