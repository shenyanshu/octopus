package migrate

import (
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// legacyGroupItemV12 是迁移 013 之前的 group_items 表结构(无 enabled 列)。
type legacyGroupItemV12 struct {
	ID             int `gorm:"primaryKey"`
	GroupID        int
	ChannelGrantID int
	Priority       int
	Score          int
}

func (legacyGroupItemV12) TableName() string { return "group_items" }

// TestMigration013BackfillsTrueAndSkipsOnReRun 证明迁移 013:
// 1. 历史行(无 enabled 列)迁移后得 true;
// 2. 显式设 false 后再次迁移不会覆盖(hasPhysicalColumn 跳过)。
func TestMigration013BackfillsTrueAndSkipsOnReRun(t *testing.T) {
	dbConn, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("建库失败: %v", err)
	}
	// 建旧表(无 enabled 列)并插入历史行。
	if err := dbConn.AutoMigrate(&legacyGroupItemV12{}); err != nil {
		t.Fatalf("建旧表失败: %v", err)
	}
	if err := dbConn.Create(&legacyGroupItemV12{ID: 1, GroupID: 1, ChannelGrantID: 1, Priority: 1}).Error; err != nil {
		t.Fatalf("插入历史行失败: %v", err)
	}

	// 第一次迁移: 建列并回填 true。
	if err := migrateGroupItemEnabled(dbConn); err != nil {
		t.Fatalf("第一次迁移失败: %v", err)
	}
	var row1 struct {
		ID      int
		Enabled bool
	}
	if err := dbConn.Table("group_items").First(&row1, 1).Error; err != nil {
		t.Fatalf("读迁移后行失败: %v", err)
	}
	if !row1.Enabled {
		t.Fatalf("历史行迁移后 enabled = false, 想要 true")
	}

	// 显式设 false。
	if err := dbConn.Table("group_items").Where("id = ?", 1).Update("enabled", false).Error; err != nil {
		t.Fatalf("设 false 失败: %v", err)
	}

	// 第二次迁移: hasPhysicalColumn 返回 true, 跳过, false 保留。
	if err := migrateGroupItemEnabled(dbConn); err != nil {
		t.Fatalf("第二次迁移失败: %v", err)
	}
	var row2 struct {
		ID      int
		Enabled bool
	}
	if err := dbConn.Table("group_items").First(&row2, 1).Error; err != nil {
		t.Fatalf("读二次迁移后行失败: %v", err)
	}
	if row2.Enabled {
		t.Fatalf("显式 false 在二次迁移后被覆盖为 true")
	}
}
