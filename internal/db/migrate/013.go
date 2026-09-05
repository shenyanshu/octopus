package migrate

import (
	"fmt"

	"gorm.io/gorm"
)

func init() {
	RegisterBeforeAutoMigration(Migration{
		Version: 13,
		Up:      migrateGroupItemEnabled,
	})
}

// migrateGroupItemEnabled 为 group_items 添加成员级启用开关列。
// 在 AutoMigrate 之前以 SQLite 原生 ALTER TABLE ADD COLUMN 建列并带 DEFAULT true:
// 历史行由此得到 true, 不被误禁用; 迁移记录防止重复执行。
// AutoMigrate 发现列已存在则跳过, 不再建列也不再覆盖默认值。
func migrateGroupItemEnabled(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	if !db.Migrator().HasTable("group_items") {
		return nil
	}
	if hasPhysicalColumn(db, "group_items", "enabled") {
		// 列已存在(迁移已跑过或 AutoMigrate 先建了), 不重复建列也不覆盖任何数据。
		return nil
	}
	// SQLite: ALTER TABLE ADD COLUMN ... NOT NULL DEFAULT true;
	// DEFAULT true 使已有行补 true, 新建行若未显式写也落 true。
	if err := db.Exec(`ALTER TABLE group_items ADD COLUMN enabled numeric NOT NULL DEFAULT 1`).Error; err != nil {
		return fmt.Errorf("failed to add group_items.enabled: %w", err)
	}
	return nil
}
