package migrate

import (
	"errors"

	"gorm.io/gorm"
)

// 016_add_sync_managed 为 channel_models 和 channel_grants 表添加 sync_managed 列。
// 旧行默认 false: 手动/legacy 创建的模型和授权不会被自动同步删除。
// 新列 NOT NULL DEFAULT false, AutoMigrate 自动添加, 无需额外 DDL。

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 16,
		Up:      migrateSyncManaged,
	})
}

// migrateSyncManaged 确认 sync_managed 列已由 AutoMigrate 添加。
// 旧行自动获得 false 默认值; 手动/legacy 模型和授权不会被同步删除。
func migrateSyncManaged(db *gorm.DB) error {
	if db == nil {
		return errors.New("migrate 016: nil database")
	}
	// AutoMigrate 已通过 struct tag 添加 sync_managed 列(NOT NULL DEFAULT false)。
	// 无需额外 DDL, 此处仅为版本标记。
	return nil
}
