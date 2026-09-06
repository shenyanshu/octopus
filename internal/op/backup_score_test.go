package op

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/google/uuid"
)

// TestMain 建立测试数据库: 逻辑导出/导入只经数据库, 不依赖运行时缓存。
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "octopus-op-test")
	if err != nil {
		panic(err)
	}
	code := func() int {
		if err := db.InitDB("sqlite", filepath.Join(dir, "op.db"), false); err != nil {
			panic(err)
		}
		return m.Run()
	}()
	_ = db.Close()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// seedGroupItemForBackup 建立渠道-凭据-模型-授权-分组-成员的最小闭环, 返回成员行。
func seedGroupItemForBackup(t *testing.T, name string) model.GroupItem {
	t.Helper()
	gormDB := db.GetDB()
	channel := model.Channel{Revision: uuid.NewString(), ChannelConfig: model.ChannelConfig{
		Name: "backup-seed-" + name, Enabled: true, BaseURL: "http://backup-seed.example",
	}}
	if err := gormDB.Create(&channel).Error; err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	key := model.ChannelKey{ChannelID: channel.ID, ChannelKeyConfig: model.ChannelKeyConfig{Name: name, Key: "secret", Enabled: true}}
	if err := gormDB.Create(&key).Error; err != nil {
		t.Fatalf("建凭据失败: %v", err)
	}
	channelModel := model.ChannelModel{ChannelID: channel.ID, Name: "m-" + name}
	if err := gormDB.Create(&channelModel).Error; err != nil {
		t.Fatalf("建模型失败: %v", err)
	}
	grant := model.ChannelGrant{ChannelModelID: channelModel.ID, ChannelKeyID: key.ID, Protocols: model.ProtocolOpenAIChatCompletion}
	if err := gormDB.Create(&grant).Error; err != nil {
		t.Fatalf("建授权失败: %v", err)
	}
	group := model.Group{Name: name, Mode: model.GroupModeScored}
	if err := gormDB.Create(&group).Error; err != nil {
		t.Fatalf("建分组失败: %v", err)
	}
	item := model.GroupItem{GroupID: group.ID, ChannelGrantID: grant.ID, Priority: 1}
	if err := gormDB.Create(&item).Error; err != nil {
		t.Fatalf("建成员失败: %v", err)
	}
	return item
}

// 逻辑导出不透出成员的持久分数: 该列只随物理数据库/卷备份保留。
func TestDBExportOmitsGroupItemScore(t *testing.T) {
	item := seedGroupItemForBackup(t, "export-omit")
	gormDB := db.GetDB()
	if err := gormDB.Model(&model.GroupItem{}).Where("id = ?", item.ID).Update("score", 13).Error; err != nil {
		t.Fatalf("写入持久分数失败: %v", err)
	}

	dump, err := DBExportAll(context.Background())
	if err != nil {
		t.Fatalf("逻辑导出失败: %v", err)
	}
	encoded, err := json.Marshal(dump)
	if err != nil {
		t.Fatalf("编码导出失败: %v", err)
	}
	// 用键名匹配而不是裸子串: 分组模式取值 "scored" 恰好包含 score 字样。
	if strings.Contains(string(encoded), `"score":`) {
		t.Fatalf("逻辑导出透出了持久分数: %s", encoded)
	}
}

// 逻辑导入无法注入持久分数: 载荷携带 score 的"真实新增"成员落库取默认 99。
func TestDBImportCannotInjectGroupItemScore(t *testing.T) {
	item := seedGroupItemForBackup(t, "import-inject")
	// 先删掉已存在的成员行, 使导入成为真实插入: 分组与授权外键仍然合法。
	if err := db.GetDB().Delete(&model.GroupItem{}, item.ID).Error; err != nil {
		t.Fatalf("预删成员失败: %v", err)
	}
	payload := `{"version":5,` +
		`"groups":[{"id":` + strconv.Itoa(item.GroupID) + `,"name":"inject","mode":"scored"}],` +
		`"group_items":[{"id":` + strconv.Itoa(item.ID) + `,"group_id":` + strconv.Itoa(item.GroupID) +
		`,"channel_grant_id":` + strconv.Itoa(item.ChannelGrantID) + `,"priority":1,"score":13}]}`

	var dump model.DBDump
	if err := json.Unmarshal([]byte(payload), &dump); err != nil {
		t.Fatalf("解码载荷失败: %v", err)
	}
	if _, err := DBImportIncremental(context.Background(), &dump); err != nil {
		t.Fatalf("逻辑导入失败: %v", err)
	}

	var reloaded model.GroupItem
	if err := db.GetDB().First(&reloaded, item.ID).Error; err != nil {
		t.Fatalf("重读成员失败: %v", err)
	}
	if reloaded.Score != 99 {
		t.Fatalf("载荷注入的持久分数生效: %d, 想要默认 99", reloaded.Score)
	}
}
