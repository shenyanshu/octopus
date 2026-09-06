package migrate

import (
	"testing"

	"github.com/bestruirui/octopus/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// TestMigration015BackfillsRevision 测试迁移 015: 旧记录清零四价 + 设 auto, 重复执行安全。
func TestMigration015BackfillsRevision(t *testing.T) {
	// 用独立内存数据库测试, 不影响全局状态
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := gdb.AutoMigrate(&model.LLMInfo{}); err != nil {
		t.Fatal(err)
	}
	// 插入旧记录: source 为空, 四价非零
	gdb.Create(&model.LLMInfo{Name: "old-nonzero", Source: "", LLMPrice: model.LLMPrice{Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5}})
	gdb.Create(&model.LLMInfo{Name: "old-zero", Source: "", LLMPrice: model.LLMPrice{}})

	// 执行迁移
	if err := migrateLLMSource(gdb); err != nil {
		t.Fatal(err)
	}

	// 旧非零记录应清零
	var nz model.LLMInfo
	gdb.Where("name = ?", "old-nonzero").First(&nz)
	if nz.Input != 0 || nz.Output != 0 || nz.CacheRead != 0 || nz.CacheWrite != 0 {
		t.Fatalf("旧非零记录四价应清零, got input=%f output=%f cache_read=%f cache_write=%f",
			nz.Input, nz.Output, nz.CacheRead, nz.CacheWrite)
	}

	// 旧零记录也应清零(幂等)
	var z model.LLMInfo
	gdb.Where("name = ?", "old-zero").First(&z)
	if z.Input != 0 || z.Output != 0 {
		t.Fatal("旧零记录四价应保持零")
	}

	// 重复执行应安全(幂等)
	if err := migrateLLMSource(gdb); err != nil {
		t.Fatal(err)
	}
	// 重复后仍正确
	gdb.Where("name = ?", "old-nonzero").First(&nz)
	if nz.Input != 0 {
		t.Fatal("重复执行后仍应清零")
	}
}
