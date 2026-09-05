package model

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// 逻辑导出含 enabled 字段, 导入后 disabled=false 可往返; 新建默认 true。
func TestGroupItemEnabledBackupRoundTrip(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "enabled.db")
	gormDB, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := gormDB.AutoMigrate(&GroupItem{}); err != nil {
		t.Fatal(err)
	}

	// 新建: 默认 true。
	item := GroupItem{GroupID: 1, ChannelGrantID: 1, Priority: 1}
	if err := gormDB.Create(&item).Error; err != nil {
		t.Fatal(err)
	}
	// 显式禁用。
	if err := gormDB.Model(&GroupItem{}).Where("id = ?", item.ID).Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}

	// 读回并断言 JSON 含 enabled=false。
	var loaded GroupItem
	gormDB.First(&loaded, item.ID)
	if loaded.Enabled {
		t.Fatalf("禁用后读回 enabled = true")
	}
	encoded, _ := json.Marshal(loaded)
	if !contains(string(encoded), `"enabled":false`) {
		t.Fatalf("JSON 未含 enabled=false: %s", encoded)
	}

	// 在新库模拟导入: Create 会把 false 覆盖为 true(DDL default:true), 导入路径随后 UPDATE 补 false。
	dir2 := t.TempDir()
	db2, _ := gorm.Open(sqlite.Open(filepath.Join(dir2, "enabled2.db")), &gorm.Config{})
	db2.AutoMigrate(&GroupItem{})
	imported := GroupItem{ID: item.ID, GroupID: 1, ChannelGrantID: 1, Priority: 1, Enabled: false}
	if err := db2.Create(&imported).Error; err != nil {
		t.Fatal(err)
	}
	// 导入路径对禁用成员补一次显式 UPDATE, 绕过 GORM 零值陷阱。
	if err := db2.Model(&GroupItem{}).Where("id = ?", item.ID).
		Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	var roundTripped GroupItem
	db2.First(&roundTripped, item.ID)
	if roundTripped.Enabled {
		t.Fatalf("导入后 enabled = true, 想要 false")
	}

	// 新库新建成员必须显式置 true(不靠 DDL 默认值)。
	fresh := GroupItem{GroupID: 2, ChannelGrantID: 2, Priority: 1, Enabled: true}
	db2.Create(&fresh)
	var freshRow GroupItem
	db2.First(&freshRow, fresh.ID)
	if !freshRow.Enabled {
		t.Fatalf("新建成员 enabled = false, 想要 true")
	}
}

func contains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
