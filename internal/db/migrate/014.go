package migrate

import (
	"fmt"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 14,
		Up:      migrateChannelRevision,
	})
}

// migrateChannelRevision 为已存在的渠道补填乐观锁版本令牌。
// AutoMigrate 会为 channels 表新增 revision 列(NOT NULL DEFAULT ”),
// 历史行的 revision 为空串, 在此为每条空串行分配独立 UUID。
// 非空 revision 保持不变(迁移幂等)。不依赖数据库方言的 UUID 生成函数, 用 Go 端 uuid.NewString 保证可移植。
func migrateChannelRevision(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	if !db.Migrator().HasTable("channels") {
		return nil
	}
	type row struct {
		ID       int    `gorm:"column:id"`
		Revision string `gorm:"column:revision"`
	}
	var rows []row
	if err := db.Table("channels").Select("id, revision").Find(&rows).Error; err != nil {
		return fmt.Errorf("failed to read channels for revision backfill: %w", err)
	}
	for _, r := range rows {
		if r.Revision != "" {
			continue
		}
		if err := db.Table("channels").Where("id = ?", r.ID).Update("revision", uuid.NewString()).Error; err != nil {
			return fmt.Errorf("failed to backfill revision for channel %d: %w", r.ID, err)
		}
	}
	return nil
}
