package op

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
)

// seedGroupForEnabled 建一个双成员评分分组并刷新缓存, 返回分组与两成员 ID。
func seedGroupForEnabled(t *testing.T) (model.Group, int, int) {
	t.Helper()
	dbConn := db.GetDB()
	for _, table := range []string{"groups", "group_items", "channel_grants", "channel_models", "channel_keys", "channels"} {
		if err := dbConn.Exec("DELETE FROM " + table).Error; err != nil {
			t.Fatalf("清理表 %s 失败: %v", table, err)
		}
	}
	var channels []model.Channel
	var grants []int
	for i := 0; i < 2; i++ {
		ch := model.Channel{ChannelConfig: model.ChannelConfig{
			Name: "enabled-ch-" + string(rune('a'+i)), Enabled: true, BaseURL: "http://example",
		}}
		dbConn.Create(&ch)
		key := model.ChannelKey{ChannelID: ch.ID, ChannelKeyConfig: model.ChannelKeyConfig{Name: "k", Key: "sk", Enabled: true}}
		dbConn.Create(&key)
		cm := model.ChannelModel{ChannelID: ch.ID, Name: "m"}
		dbConn.Create(&cm)
		grant := model.ChannelGrant{ChannelModelID: cm.ID, ChannelKeyID: key.ID, Protocols: model.ProtocolOpenAIChatCompletion}
		dbConn.Create(&grant)
		channels = append(channels, ch)
		grants = append(grants, grant.ID)
	}
	group := model.Group{Name: "enabled-group", Mode: model.GroupModeScored, Items: []model.GroupItem{
		{ChannelGrantID: grants[0], Priority: 1, Enabled: true},
		{ChannelGrantID: grants[1], Priority: 2, Enabled: true},
	}}
	if err := dbConn.Create(&group).Error; err != nil {
		t.Fatalf("建分组失败: %v", err)
	}
	if err := InitCache(); err != nil {
		t.Fatalf("刷新缓存失败: %v", err)
	}
	loaded, err := GroupGetByName("enabled-group")
	if err != nil {
		t.Fatal(err)
	}
	return loaded, loaded.Items[0].ID, loaded.Items[1].ID
}

// 归属校验: 不属于该分组的 item_id 被拒。
func TestGroupItemSetEnabledRejectsForeignItem(t *testing.T) {
	group, itemA, itemB := seedGroupForEnabled(t)
	_ = itemA

	// itemB 属于 group, 应成功。
	if _, err := GroupItemSetEnabled(context.Background(), group.ID, itemB, false); err != nil {
		t.Fatalf("禁用本组成员失败: %v", err)
	}
	if _, err := GroupItemSetEnabled(context.Background(), group.ID, itemB, true); err != nil {
		t.Fatalf("启用本组成员失败: %v", err)
	}

	// 不存在的分组 ID。
	if _, err := GroupItemSetEnabled(context.Background(), 99999, itemB, false); err == nil {
		t.Fatalf("不存在的分组未报错")
	}

	// 不存在的成员 ID。
	if _, err := GroupItemSetEnabled(context.Background(), group.ID, 99999, false); err == nil {
		t.Fatalf("不存在的成员未报错")
	}
}

// 持久化往返: 禁用→读库→启用→读库, 值正确。
func TestGroupItemSetEnabledPersists(t *testing.T) {
	group, itemA, _ := seedGroupForEnabled(t)

	if _, err := GroupItemSetEnabled(context.Background(), group.ID, itemA, false); err != nil {
		t.Fatalf("禁用失败: %v", err)
	}
	var row model.GroupItem
	db.GetDB().First(&row, itemA)
	if row.Enabled {
		t.Fatalf("禁用后 enabled = true")
	}
	if row.Score != 99 {
		t.Fatalf("禁用后 score = %d, 想要 99 (评分不被改)", row.Score)
	}

	if _, err := GroupItemSetEnabled(context.Background(), group.ID, itemA, true); err != nil {
		t.Fatalf("启用失败: %v", err)
	}
	db.GetDB().First(&row, itemA)
	if !row.Enabled {
		t.Fatalf("启用后 enabled = false")
	}
}

// 生产导出→导入往返: 禁用成员经真实 DBExportAll/DBImportIncremental 后
// 新库中保持 false; 旧版备份(缺 enabled 字段)导入后得 true。
func TestGroupItemEnabledProductionRoundTrip(t *testing.T) {
	group, itemA, _ := seedGroupForEnabled(t)

	// 禁用成员 A。
	if _, err := GroupItemSetEnabled(context.Background(), group.ID, itemA, false); err != nil {
		t.Fatalf("禁用失败: %v", err)
	}

	// 真实导出: GroupItem 的 JSON 含 enabled=false。
	dump, err := DBExportAll(context.Background())
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	foundDisabled := false
	for _, item := range dump.GroupItems {
		if item.ID == itemA && !item.Enabled {
			foundDisabled = true
		}
	}
	if !foundDisabled {
		t.Fatalf("导出未含禁用成员")
	}

	// 在同一库内删除该行后重新导入, 验证 createDoNothing → 新插入 → UPDATE false 链路。
	db.GetDB().Delete(&model.GroupItem{}, itemA)
	if _, err := DBImportIncremental(context.Background(), dump); err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	var row model.GroupItem
	db.GetDB().First(&row, itemA)
	if row.Enabled {
		t.Fatalf("生产导入后禁用成员 enabled = true, 想要 false")
	}

	// 旧版备份模拟: JSON 不含 enabled 字段时, UnmarshalJSON 补 true, 导入后得 true。
	db.GetDB().Delete(&model.GroupItem{}, itemA)
	legacyJSON := stripEnabledFromGroupItems(t, dump)
	if _, err := DBImportIncremental(context.Background(), legacyJSON); err != nil {
		t.Fatalf("导入旧版备份失败: %v", err)
	}
	var legacyRow model.GroupItem
	db.GetDB().First(&legacyRow, itemA)
	if !legacyRow.Enabled {
		t.Fatalf("旧版备份导入后 enabled = false, 想要 true")
	}
}

// stripEnabledFromGroupItems 把 dump 中 GroupItems 的 enabled 字段从 JSON 中去掉, 模拟旧版备份。
func stripEnabledFromGroupItems(t *testing.T, dump *model.DBDump) *model.DBDump {
	t.Helper()
	for i := range dump.GroupItems {
		dump.GroupItems[i].Enabled = true // 设 true 后 JSON 会出 "enabled":true
	}
	// 序列化全 dump, 去掉 GroupItems 中的 enabled 键, 再反序列化触发 UnmarshalJSON。
	jsonBytes, err := json.Marshal(dump)
	if err != nil {
		t.Fatal(err)
	}
	s := string(jsonBytes)
	s = strings.ReplaceAll(s, `"enabled":true,`, ``)
	s = strings.ReplaceAll(s, `,"enabled":true`, ``)
	var result model.DBDump
	if err := json.Unmarshal([]byte(s), &result); err != nil {
		t.Fatalf("反序列化旧版 JSON 失败: %v", err)
	}
	return &result
}
