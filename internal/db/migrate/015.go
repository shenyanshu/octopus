package migrate

// migrateLLMSource 为 llm_infos 增加 source 列并将全部旧记录标记为 auto。
// 旧记录四价清零(auto 的零占位), 实际价格由参考目录动态派生。

import (
	"github.com/bestruirui/octopus/internal/model"
	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 15,
		Up:      migrateLLMSource,
	})
}

// migrateLLMSource 清零旧记录的四价并统一 source 为 auto。
// AutoMigrate 已添加 source 列(default 'auto'), 旧记录自动获得 'auto'。
// 旧记录的四价是参考目录复制值, 需清零以匹配 auto 零占位语义, 实际价格由参考目录动态派生。
func migrateLLMSource(tx *gorm.DB) error {
	if err := tx.Model(&model.LLMInfo{}).
		Where("source = ? OR source = ? OR source IS NULL", "", "auto", nil).
		Updates(map[string]any{"input": 0, "output": 0, "cache_read": 0, "cache_write": 0}).Error; err != nil {
		return err
	}
	return nil
}
