package model

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// 本文件用真实 SQLite 证明分数列的迁移与默认值行为, 而不是依赖对 GORM 标签的推断。

// openScoreTestDB 为用例建立独立 SQLite 库。
func openScoreTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	gormDB, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "score.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	return gormDB
}

// 历史库升级: AutoMigrate 为已有行补 score 列时必须带上默认值 99, 而不是落成 0。
func TestGroupItemScoreAutoMigrateBackfillsDefault(t *testing.T) {
	gormDB := openScoreTestDB(t)
	// 先建不带 score 列的旧表并播种历史行, 模拟升级前的库。
	if err := gormDB.Exec(`CREATE TABLE group_items (
		id integer PRIMARY KEY AUTOINCREMENT,
		group_id integer NOT NULL,
		channel_grant_id integer NOT NULL,
		priority integer NOT NULL)`).Error; err != nil {
		t.Fatalf("建旧表失败: %v", err)
	}
	if err := gormDB.Exec(`INSERT INTO group_items (group_id, channel_grant_id, priority) VALUES (1, 7, 1)`).Error; err != nil {
		t.Fatalf("播种历史行失败: %v", err)
	}

	if err := gormDB.AutoMigrate(&GroupItem{}); err != nil {
		t.Fatalf("AutoMigrate 失败: %v", err)
	}

	var item GroupItem
	if err := gormDB.First(&item, "group_id = ?", 1).Error; err != nil {
		t.Fatalf("读取历史行失败: %v", err)
	}
	if item.Score != 99 {
		t.Fatalf("迁移后历史行 score = %d, 想要默认 99", item.Score)
	}
}

// 新建成员不提交分数时必须落库为默认 99; 显式写入的 0 是合法分数, 不受列默认值吞并。
func TestGroupItemCreateOmitsZeroScoreForDefault(t *testing.T) {
	gormDB := openScoreTestDB(t)
	if err := gormDB.AutoMigrate(&GroupItem{}); err != nil {
		t.Fatalf("AutoMigrate 失败: %v", err)
	}
	created := GroupItem{GroupID: 1, ChannelGrantID: 7, Priority: 1}
	if err := gormDB.Create(&created).Error; err != nil {
		t.Fatalf("建成员失败: %v", err)
	}

	var reloaded GroupItem
	if err := gormDB.First(&reloaded, created.ID).Error; err != nil {
		t.Fatalf("重读成员失败: %v", err)
	}
	if reloaded.Score != 99 {
		t.Fatalf("新成员 score = %d, 想要默认 99", reloaded.Score)
	}

	if err := gormDB.Model(&GroupItem{}).Where("id = ?", created.ID).Update("score", 0).Error; err != nil {
		t.Fatalf("更新分数失败: %v", err)
	}
	if err := gormDB.First(&reloaded, created.ID).Error; err != nil {
		t.Fatalf("重读成员失败: %v", err)
	}
	if reloaded.Score != 0 {
		t.Fatalf("显式写入的 0 分 = %d, 想要 0", reloaded.Score)
	}
}

// 持久分数列对 JSON 两侧均不可见: 分组接口与逻辑备份不透出, 载荷注入也无法进入结构体。
func TestGroupItemScoreHiddenFromJSON(t *testing.T) {
	encoded, err := json.Marshal(GroupItem{ID: 1, GroupID: 2, ChannelGrantID: 3, Priority: 1, Score: 97})
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	if strings.Contains(string(encoded), "score") {
		t.Fatalf("JSON 透出了持久分数: %s", encoded)
	}

	var decoded GroupItem
	if err := json.Unmarshal([]byte(`{"id":5,"score":13}`), &decoded); err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if decoded.Score != 0 {
		t.Fatalf("载荷注入的 score 进入结构体: %d", decoded.Score)
	}
}
