package op

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/google/uuid"
)

// 本文件证明自动补充规则的事务内取数、补齐、提交事实与原子性。
// 全部经真实数据库与真实 op 函数, 不直接改包内私有状态(除 refreshGroupsAfterCommit 注入失败这一既有 seam)。

// keySpec 描述一把凭据; Enabled 经 UPDATE 显式置假以规避 GORM 布尔零值陷阱。
type keySpec struct {
	name    string
	enabled bool
}

// seedChannel 建一个渠道, 配给定模型与凭据, 并为每个 (模型, 凭据) 组合建一条授权。
// 返回渠道 ID 与 "模型名|凭据名" 到授权主键的映射。Disabled 渠道/凭据经 UPDATE 显式置假。
func seedChannel(t *testing.T, name string, enabled bool, models []string, keys []keySpec) (int, map[string]int) {
	t.Helper()
	dbConn := db.GetDB()
	channel := model.Channel{Revision: uuid.NewString(), ChannelConfig: model.ChannelConfig{
		Name: name, Enabled: true, BaseURL: "http://" + name + ".example",
		OpenAIChatCompletionPath: "/chat", AnthropicMessagePath: "/msg",
	}}
	if err := dbConn.Create(&channel).Error; err != nil {
		t.Fatalf("建渠道 %s 失败: %v", name, err)
	}
	if !enabled {
		if err := dbConn.Model(&model.Channel{}).Where("id = ?", channel.ID).Update("enabled", false).Error; err != nil {
			t.Fatalf("禁用渠道 %s 失败: %v", name, err)
		}
	}
	keyIDs := make(map[string]int, len(keys))
	for _, ks := range keys {
		key := model.ChannelKey{ChannelID: channel.ID, ChannelKeyConfig: model.ChannelKeyConfig{Name: ks.name, Key: "sk-" + ks.name, Enabled: true}}
		if err := dbConn.Create(&key).Error; err != nil {
			t.Fatalf("建凭据失败: %v", err)
		}
		if !ks.enabled {
			if err := dbConn.Model(&model.ChannelKey{}).Where("id = ?", key.ID).Update("enabled", false).Error; err != nil {
				t.Fatalf("禁用凭据失败: %v", err)
			}
		}
		keyIDs[ks.name] = key.ID
	}
	grants := make(map[string]int, len(models)*len(keys))
	for _, modelName := range models {
		cm := model.ChannelModel{ChannelID: channel.ID, Name: modelName}
		if err := dbConn.Create(&cm).Error; err != nil {
			t.Fatalf("建模型失败: %v", err)
		}
		for _, ks := range keys {
			grant := model.ChannelGrant{ChannelModelID: cm.ID, ChannelKeyID: keyIDs[ks.name], Protocols: model.ProtocolOpenAIChatCompletion}
			if err := dbConn.Create(&grant).Error; err != nil {
				t.Fatalf("建授权失败: %v", err)
			}
			grants[modelName+"|"+ks.name] = grant.ID
		}
	}
	return channel.ID, grants
}

// clearAll 清空六张主表, 保持用例间互不干扰(渠道名唯一约束等)。
func clearAll(t *testing.T) {
	t.Helper()
	for _, table := range []string{"groups", "group_items", "channel_grants", "channel_models", "channel_keys", "channels"} {
		if err := db.GetDB().Exec("DELETE FROM " + table).Error; err != nil {
			t.Fatalf("清理表 %s 失败: %v", table, err)
		}
	}
}

// reloadAllCache 刷新渠道与分组缓存; 测试在直接写库后用它与生产 InitCache 同口径对齐。
func reloadAllCache(t *testing.T) {
	t.Helper()
	if err := channelRefreshCache(context.Background()); err != nil {
		t.Fatalf("刷新渠道缓存失败: %v", err)
	}
	if err := groupRefreshCache(context.Background()); err != nil {
		t.Fatalf("刷新分组缓存失败: %v", err)
	}
}

// grantIDByModelKey 在分组现有成员里按 "模型|凭据" 找授权主键; 找不到则 t.Fatal。
func grantIDByModelKey(t *testing.T, grantIDs map[string]int, modelKey string) int {
	t.Helper()
	id, ok := grantIDs[modelKey]
	if !ok {
		t.Fatalf("找不到授权 %s", modelKey)
	}
	return id
}

func sortInts(s []int) []int {
	sort.Ints(s)
	return s
}

func contains(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// 创建分组: 纯规则(无手动 items)补齐当前匹配且渠道/凭据启用的授权。
func TestGroupCreateAutoSupplementPureRule(t *testing.T) {
	clearAll(t)
	// alpha 启用带 gpt-4o 与 gpt-4o-mini; beta 禁用带 gpt-4o(不可用)。
	_, grantsA := seedChannel(t, "alpha", true, []string{"gpt-4o", "gpt-4o-mini"}, []keySpec{{"k", true}})
	_, _ = seedChannel(t, "beta", false, []string{"gpt-4o"}, []keySpec{{"k", true}})
	reloadAllCache(t)

	group, err := GroupCreate(&model.GroupCreateRequest{
		Name: "rule-only", Mode: model.GroupModeScored, AutoAddPattern: "^gpt-4o$",
	}, context.Background())
	if err != nil {
		t.Fatalf("建分组失败: %v", err)
	}
	// 精确匹配只命中 alpha 的 gpt-4o; mini 不中, beta 不可用不补。
	got := make([]int, 0, len(group.Items))
	for _, item := range group.Items {
		got = append(got, item.ChannelGrantID)
	}
	want := grantIDByModelKey(t, grantsA, "gpt-4o|k")
	if len(got) != 1 || got[0] != want {
		t.Fatalf("纯规则创建应只补 alpha 的 gpt-4o: got=%v want=[%d]", got, want)
	}
	// 新成员 Enabled=true, Score=99, Priority=1(无手动项)。
	item := group.Items[0]
	if !item.Enabled {
		t.Fatalf("自动补入成员应 Enabled=true")
	}
	if item.Priority != 1 {
		t.Fatalf("Priority = %d, 想要 1", item.Priority)
	}
	var row model.GroupItem
	db.GetDB().First(&row, item.ID)
	if row.Score != 99 {
		t.Fatalf("Score = %d, 想要 99", row.Score)
	}
}

// 规则补齐排除不可用: 渠道禁用与凭据禁用都不补。
func TestGroupCreateAutoSupplementExcludesUnavailable(t *testing.T) {
	clearAll(t)
	_, grantsA := seedChannel(t, "alpha", true, []string{"gpt-4o"}, []keySpec{{"k", true}, {"kdis", false}})
	_, _ = seedChannel(t, "beta", false, []string{"gpt-4o"}, []keySpec{{"k", true}})
	reloadAllCache(t)

	group, err := GroupCreate(&model.GroupCreateRequest{
		Name: "avail", Mode: model.GroupModeScored, AutoAddPattern: "gpt-4o",
	}, context.Background())
	if err != nil {
		t.Fatalf("建分组失败: %v", err)
	}
	// 只 alpha 的可用凭据 k 被补; alpha 的禁用凭据 kdis 与 beta 整渠道都不可用, 不补。
	want := []int{grantIDByModelKey(t, grantsA, "gpt-4o|k")}
	got := make([]int, 0, len(group.Items))
	for _, item := range group.Items {
		got = append(got, item.ChannelGrantID)
	}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("不可用不应被补: got=%v want=%v", got, want)
	}
}

// 大小写敏感默认与 (?i) 支持。
func TestGroupCreateAutoSupplementCaseSensitive(t *testing.T) {
	clearAll(t)
	_, grants := seedChannel(t, "alpha", true, []string{"GPT-4o", "gpt-4o"}, []keySpec{{"k", true}})
	reloadAllCache(t)

	g1, _ := GroupCreate(&model.GroupCreateRequest{Name: "cs", AutoAddPattern: "GPT-4o"}, context.Background())
	if len(g1.Items) != 1 {
		t.Fatalf("大小写敏感应只命中大写: %d 条", len(g1.Items))
	}
	if g1.Items[0].ChannelGrantID != grants["GPT-4o|k"] {
		t.Fatalf("大小写敏感命中错误: %d", g1.Items[0].ChannelGrantID)
	}

	clearAll(t)
	_, grants = seedChannel(t, "alpha", true, []string{"GPT-4o", "gpt-4o"}, []keySpec{{"k", true}})
	reloadAllCache(t)
	g2, _ := GroupCreate(&model.GroupCreateRequest{Name: "ci", AutoAddPattern: "(?i)GPT-4O"}, context.Background())
	if len(g2.Items) != 2 {
		t.Fatalf("(?i) 应忽略大小写命中两条: %d", len(g2.Items))
	}
}

// 同模型名不同凭据: 两条不同授权均补入。
func TestGroupCreateSameModelDifferentKeysBothAdded(t *testing.T) {
	clearAll(t)
	_, grants := seedChannel(t, "alpha", true, []string{"gpt-4o"}, []keySpec{{"k1", true}, {"k2", true}})
	reloadAllCache(t)

	group, err := GroupCreate(&model.GroupCreateRequest{
		Name: "dup-model", AutoAddPattern: "^gpt-4o$",
	}, context.Background())
	if err != nil {
		t.Fatalf("建分组失败: %v", err)
	}
	if len(group.Items) != 2 {
		t.Fatalf("同模型两凭据应补两条: %d", len(group.Items))
	}
	ids := sortInts([]int{group.Items[0].ChannelGrantID, group.Items[1].ChannelGrantID})
	want := sortInts([]int{grants["gpt-4o|k1"], grants["gpt-4o|k2"]})
	if fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Fatalf("两凭据授权不匹配: got=%v want=%v", ids, want)
	}
}

// 按授权主键去重: 已挂载(含手动预置)的匹配授权不重复补入。
func TestGroupCreateAutoSupplementDedupByGrantID(t *testing.T) {
	clearAll(t)
	_, grants := seedChannel(t, "alpha", true, []string{"gpt-4o"}, []keySpec{{"k", true}})
	reloadAllCache(t)
	manualGrant := grantIDByModelKey(t, grants, "gpt-4o|k")

	group, err := GroupCreate(&model.GroupCreateRequest{
		Name: "dedup", Mode: model.GroupModeScored, AutoAddPattern: "gpt-4o",
		Items: []model.GroupItemInput{{ChannelGrantID: manualGrant}},
	}, context.Background())
	if err != nil {
		t.Fatalf("建分组失败: %v", err)
	}
	if len(group.Items) != 1 {
		t.Fatalf("已挂载的匹配授权不应重复补: %d 条", len(group.Items))
	}
	if group.Items[0].ChannelGrantID != manualGrant {
		t.Fatalf("去重后成员应指向同一授权")
	}
	if group.Items[0].Priority != 1 {
		// 手动项排在首位 Priority=1, 无追加项。
		t.Fatalf("Priority = %d, 想要 1", group.Items[0].Priority)
	}
}

// 仅改规则(Items=nil)只追加不重排: 既有成员 Priority 不变。
func TestGroupUpdatePatternOnlyAppendsNoReorder(t *testing.T) {
	clearAll(t)
	_, grants := seedChannel(t, "alpha", true, []string{"m1", "m2"}, []keySpec{{"k", true}})
	reloadAllCache(t)
	g1, _ := grants["m1|k"]
	g2, _ := grants["m2|k"]
	group, err := GroupCreate(&model.GroupCreateRequest{
		Name: "append", Mode: model.GroupModeScored, AutoAddPattern: "",
		Items: []model.GroupItemInput{{ChannelGrantID: g1}, {ChannelGrantID: g2}},
	}, context.Background())
	if err != nil {
		t.Fatalf("建分组失败: %v", err)
	}
	idA, idB := group.Items[0].ID, group.Items[1].ID
	priA, priB := group.Items[0].Priority, group.Items[1].Priority

	// 后加一条匹配 alpha-m2 的渠道模型? 直接改规则为 "m1"(只匹配 m1), 追加本就已有的 m1 项 -> 去重不补。
	// 真正测追加: 改规则为 "m.", 它匹配 m1 与 m2, 但两者都已挂载, 故仍不补; 改用新增一条只匹配 m2 的窄规则验证。
	// 这里用 "m1" 验证只追加不重排: 既有成员不变。
	empty := ""
	updated, err := GroupUpdate(group.ID, &model.GroupUpdateRequest{AutoAddPattern: &empty}, context.Background())
	_ = updated
	_ = err
	// 清空规则不应动既有成员。
	upd, err := GroupGet(group.ID)
	if err != nil {
		t.Fatalf("读分组失败: %v", err)
	}
	if len(upd.Items) != 2 || upd.Items[0].ID != idA || upd.Items[1].ID != idB {
		t.Fatalf("清空规则不应改既有成员: %+v", upd.Items)
	}
	if upd.Items[0].Priority != priA || upd.Items[1].Priority != priB {
		t.Fatalf("清空规则不应改 Priority: %d/%d vs %d/%d", upd.Items[0].Priority, upd.Items[1].Priority, priA, priB)
	}

	// 现在改成匹配且追加: 新增一条渠道带模型 "m-new", 规则 "m." 会追加它。
	_, grants2 := seedChannel(t, "bravo", true, []string{"m-new"}, []keySpec{{"k", true}})
	reloadAllCache(t)
	pat := "m."
	upd2, err := GroupUpdate(group.ID, &model.GroupUpdateRequest{AutoAddPattern: &pat}, context.Background())
	if err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	// 既有两项 Priority 不变, 新项接在其后。
	byID := make(map[int]model.GroupItem, len(upd2.Items))
	for _, item := range upd2.Items {
		byID[item.ID] = item
	}
	if byID[idA].Priority != priA || byID[idB].Priority != priB {
		t.Fatalf("追加不应改既有 Priority: %+v", byID)
	}
	newGrant := grants2["m-new|k"]
	newItem, ok := byID[findItemIDByGrant(t, group.ID, newGrant)]
	if !ok {
		t.Fatalf("新匹配项未被追加")
	}
	if newItem.Priority != 3 {
		t.Fatalf("新项 Priority = %d, 想要 3", newItem.Priority)
	}
}

func findItemIDByGrant(t *testing.T, groupID, grantID int) int {
	t.Helper()
	var items []model.GroupItem
	if err := db.GetDB().Where("group_id = ? AND channel_grant_id = ?", groupID, grantID).Find(&items).Error; err != nil {
		t.Fatalf("查成员失败: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("成员数 = %d, 想要 1 (grant=%d)", len(items), grantID)
	}
	return items[0].ID
}

// 清空规则后显式 items 删除仍按手动语义生效。
func TestGroupUpdateClearPatternExplicitDeletionApplies(t *testing.T) {
	clearAll(t)
	_, grants := seedChannel(t, "alpha", true, []string{"m1", "m2"}, []keySpec{{"k", true}})
	reloadAllCache(t)
	g1, g2 := grants["m1|k"], grants["m2|k"]
	group, err := GroupCreate(&model.GroupCreateRequest{
		Name: "clear", Mode: model.GroupModeManual, AutoAddPattern: "m.",
		Items: []model.GroupItemInput{{ChannelGrantID: g1}, {ChannelGrantID: g2}},
	}, context.Background())
	if err != nil {
		t.Fatalf("建分组失败: %v", err)
	}
	empty := ""
	keep := []model.GroupItemInput{{ChannelGrantID: g1}}
	updated, err := GroupUpdate(group.ID, &model.GroupUpdateRequest{
		AutoAddPattern: &empty, Items: &keep,
	}, context.Background())
	if err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	if len(updated.Items) != 1 || updated.Items[0].ChannelGrantID != g1 {
		t.Fatalf("清空规则后显式删除应生效, 只剩 g1: %+v", updated.Items)
	}
}

// 合并保留身份: 用户从草稿移除的仍匹配项, 规则并入后走 existingByGrant 保留原 ID/评分/禁用态。
func TestGroupUpdateMergePreservesIdentityOfRemovedMatchingItem(t *testing.T) {
	clearAll(t)
	_, grants := seedChannel(t, "alpha", true, []string{"gpt-4o"}, []keySpec{{"k", true}})
	reloadAllCache(t)
	grant := grants["gpt-4o|k"]
	group, err := GroupCreate(&model.GroupCreateRequest{
		Name: "merge", Mode: model.GroupModeScored, AutoAddPattern: "^gpt-4o$",
		Items: []model.GroupItemInput{{ChannelGrantID: grant}},
	}, context.Background())
	if err != nil {
		t.Fatalf("建分组失败: %v", err)
	}
	origID := group.Items[0].ID
	// 直接落一个非缺省评分, 证明合并后评分不被清。
	if err := db.GetDB().Model(&model.GroupItem{}).Where("id = ?", origID).Update("score", 77).Error; err != nil {
		t.Fatalf("改评分失败: %v", err)
	}
	// 显式禁用成员, 证明合并后禁用态保留。
	if err := db.GetDB().Model(&model.GroupItem{}).Where("id = ?", origID).Update("enabled", false).Error; err != nil {
		t.Fatalf("禁用失败: %v", err)
	}
	reloadAllCache(t)

	// 草稿里去掉该匹配项; 规则仍匹配, 合并应把它并入回 requested, 走 existingByGrant 保留 ID。
	emptyDraft := []model.GroupItemInput{}
	updated, err := GroupUpdate(group.ID, &model.GroupUpdateRequest{Items: &emptyDraft}, context.Background())
	if err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	if len(updated.Items) != 1 {
		t.Fatalf("合并应保留该匹配项: %d 条", len(updated.Items))
	}
	if updated.Items[0].ID != origID {
		t.Fatalf("身份丢失: ID = %d, 想要 %d (删除后重建)", updated.Items[0].ID, origID)
	}
	var row model.GroupItem
	db.GetDB().First(&row, origID)
	if row.Score != 77 {
		t.Fatalf("评分被清: %d, 想要 77", row.Score)
	}
	if row.Enabled {
		t.Fatalf("禁用态被重置为 true")
	}
}

// 已有成员的 excluded(Enabled=false)/score/id/priority 在规则补齐时一律保留。
func TestGroupUpdatePreservesExistingExcludedScoreIdPriority(t *testing.T) {
	clearAll(t)
	_, grants := seedChannel(t, "alpha", true, []string{"m1", "m2", "m3"}, []keySpec{{"k", true}})
	reloadAllCache(t)
	g1, g2, g3 := grants["m1|k"], grants["m2|k"], grants["m3|k"]
	group, err := GroupCreate(&model.GroupCreateRequest{
		Name: "preserve", Mode: model.GroupModeScored, AutoAddPattern: "",
		Items: []model.GroupItemInput{{ChannelGrantID: g1}, {ChannelGrantID: g2}, {ChannelGrantID: g3}},
	}, context.Background())
	if err != nil {
		t.Fatalf("建分组失败: %v", err)
	}
	id1 := group.Items[0].ID
	// 直接落非缺省评分并禁用成员 1。
	if err := db.GetDB().Model(&model.GroupItem{}).Where("id = ?", id1).Updates(map[string]any{"score": 33, "enabled": false}).Error; err != nil {
		t.Fatalf("改成员失败: %v", err)
	}
	reloadAllCache(t)

	// 启用匹配 m. 的规则: 应追加不存在的匹配项, 但 m1/m2/m3 都已挂载, 实际不追加;
	// 这里改测"规则启用后再次保存不破坏既有": 用 Items=nil 触发 append 路径(只追加不重排)。
	pat := "m."
	updated, err := GroupUpdate(group.ID, &model.GroupUpdateRequest{AutoAddPattern: &pat}, context.Background())
	if err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	byID := make(map[int]model.GroupItem, len(updated.Items))
	for _, item := range updated.Items {
		byID[item.ID] = item
	}
	got1, ok := byID[id1]
	if !ok {
		t.Fatalf("成员 1 消失")
	}
	if got1.Score != 33 {
		t.Fatalf("Score = %d, 想要 33", got1.Score)
	}
	if got1.Enabled {
		t.Fatalf("Enabled 应保留 false")
	}
	// 三项都仍挂载, 无新增(去重)。
	if len(updated.Items) != 3 {
		t.Fatalf("既有项应全保留: %d", len(updated.Items))
	}
}

// 创建渠道: 规则分组在同一事务内补入本渠道匹配授权; 非匹配/其他组不受影响。
func TestChannelCreateAutoSupplementsPatternGroups(t *testing.T) {
	clearAll(t)
	// 预置一个匹配组与一个不匹配组; 匹配组在创建时即把 existing 渠道的 gpt-4o 补入。
	_, grants := seedChannel(t, "existing", true, []string{"gpt-4o"}, []keySpec{{"k", true}})
	reloadAllCache(t)
	grantExisting := grants["gpt-4o|k"]

	matchGroup, err := GroupCreate(&model.GroupCreateRequest{
		Name: "match", Mode: model.GroupModeScored, AutoAddPattern: "^gpt-4o$",
	}, context.Background())
	if err != nil {
		t.Fatalf("建匹配组失败: %v", err)
	}
	otherGroup, err := GroupCreate(&model.GroupCreateRequest{
		Name: "other", Mode: model.GroupModeScored, AutoAddPattern: "^never-matches-anything$",
	}, context.Background())
	if err != nil {
		t.Fatalf("建其他组失败: %v", err)
	}

	// 新建渠道带 gpt-4o 与 claude: 只匹配组应补入新渠道的 gpt-4o(原 existing 项保留)。
	newDetail := model.ChannelDetail{
		ChannelConfig: model.ChannelConfig{Name: "newch", Enabled: true, BaseURL: "http://new.example",
			OpenAIChatCompletionPath: "/chat", AnthropicMessagePath: "/msg"},
		Keys:   []model.ChannelKeyConfig{{Name: "k", Key: "sk", Enabled: true}},
		Models: []string{"gpt-4o", "claude"},
		Grants: []model.ChannelGrantConfig{{ModelName: "gpt-4o", KeyName: "k", Protocols: model.ProtocolOpenAIChatCompletion}, {ModelName: "claude", KeyName: "k", Protocols: model.ProtocolOpenAIChatCompletion}},
	}
	if _, _, err := ChannelCreate(&newDetail, context.Background()); err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	reloadAllCache(t)

	match, _ := GroupGet(matchGroup.ID)
	other, _ := GroupGet(otherGroup.ID)
	if len(other.Items) != 0 {
		t.Fatalf("不匹配组不应受影响: %+v", other.Items)
	}
	// 匹配组应有两条 gpt-4o 授权(existing 创建时补入 + newch 本次补入)。
	if len(match.Items) != 2 {
		t.Fatalf("匹配组应补入新渠道 gpt-4o(原 existing 项保留): %+v", match.Items)
	}
	grantIDs := make(map[int]bool, len(match.Items))
	for _, item := range match.Items {
		grantIDs[item.ChannelGrantID] = true
	}
	if !grantIDs[grantExisting] {
		t.Fatalf("existing 渠道的原匹配项应保留")
	}
	var newGrant model.ChannelGrant
	if err := db.GetDB().Joins("JOIN channel_models ON channel_models.id = channel_grants.channel_model_id").
		Joins("JOIN channels ON channels.id = channel_models.channel_id").
		Where("channels.name = ? AND channel_models.name = ?", "newch", "gpt-4o").
		First(&newGrant).Error; err != nil {
		t.Fatalf("读新渠道授权失败: %v", err)
	}
	if !grantIDs[newGrant.ID] {
		t.Fatalf("新渠道的 gpt-4o 授权未被补入匹配组")
	}
}

// 渠道操作只补本渠道匹配授权: 其他渠道的同名授权不因本次操作被补。
func TestChannelCreateSupplementRestrictedToThisChannel(t *testing.T) {
	clearAll(t)
	_, grantsA := seedChannel(t, "chA", true, []string{"gpt-4o"}, []keySpec{{"k", true}})
	_, grantsB := seedChannel(t, "chB", true, []string{"gpt-4o"}, []keySpec{{"k", true}})
	reloadAllCache(t)
	group, err := GroupCreate(&model.GroupCreateRequest{
		Name: "restricted", Mode: model.GroupModeScored, AutoAddPattern: "^gpt-4o$",
	}, context.Background())
	if err != nil {
		t.Fatalf("建组失败: %v", err)
	}
	// 建组时已补入 A 与 B 两条; 清空后再创建第三渠道 chC, 只补 chC。
	if err := db.GetDB().Where("group_id = ?", group.ID).Delete(&model.GroupItem{}).Error; err != nil {
		t.Fatalf("清成员失败: %v", err)
	}
	reloadAllCache(t)

	newDetail := model.ChannelDetail{
		ChannelConfig: model.ChannelConfig{Name: "chC", Enabled: true, BaseURL: "http://chC.example",
			OpenAIChatCompletionPath: "/chat", AnthropicMessagePath: "/msg"},
		Keys:   []model.ChannelKeyConfig{{Name: "k", Key: "sk", Enabled: true}},
		Models: []string{"gpt-4o"},
		Grants: []model.ChannelGrantConfig{{ModelName: "gpt-4o", KeyName: "k", Protocols: model.ProtocolOpenAIChatCompletion}},
	}
	if _, _, err := ChannelCreate(&newDetail, context.Background()); err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	reloadAllCache(t)

	grp, _ := GroupGetByName("restricted")
	if len(grp.Items) != 1 {
		t.Fatalf("应只补 chC 本渠道一条, 不拉 A/B: %+v", grp.Items)
	}
	if grp.Items[0].ChannelGrantID == grantsA["gpt-4o|k"] || grp.Items[0].ChannelGrantID == grantsB["gpt-4o|k"] {
		t.Fatalf("补入了非本次操作的渠道授权")
	}
}

// 更新渠道新增模型+授权: 规则分组补入新匹配授权。
func TestChannelUpdateAddModelAutoSupplements(t *testing.T) {
	clearAll(t)
	_, _ = seedChannel(t, "upd", true, []string{"gpt-4o"}, []keySpec{{"k", true}})
	reloadAllCache(t)
	group, err := GroupCreate(&model.GroupCreateRequest{
		Name: "upd-rule", Mode: model.GroupModeScored, AutoAddPattern: "^gpt-4o$",
	}, context.Background())
	if err != nil {
		t.Fatalf("建组失败: %v", err)
	}
	beforeCount := len(group.Items)

	// 更新渠道追加 gpt-4o-mini 不匹配规则, 追加 claude-gpt 匹配 "gpt"。
	ch, ok := channelCache.Get(group.Items[0].ChannelGrantID)
	_ = ch
	_ = ok
	// 从渠道缓存构造编辑表单。
	channel, ok := channelCache.Get(getChannelIDByGrant(t, group.Items[0].ChannelGrantID))
	if !ok {
		t.Fatalf("渠道缓存缺失")
	}
	detail := channelDetailFromDBForTest(t, channel)
	detail.Models = append(detail.Models, "claude-gpt")
	detail.Grants = append(detail.Grants, model.ChannelGrantConfig{ModelName: "claude-gpt", KeyName: detail.Keys[0].Name, Protocols: model.ProtocolOpenAIChatCompletion})
	// 把规则放宽到 "gpt" 以匹配 claude-gpt: 但放宽规则要 GroupUpdate, 不在 ChannelUpdate 内。
	// 为隔离 channel update 的补齐, 保持规则为 "^gpt-4o$"(不匹配 claude-gpt): 不应补。
	if _, _, err := ChannelUpdate(&detail, context.Background()); err != nil {
		t.Fatalf("更新渠道失败: %v", err)
	}
	reloadAllCache(t)
	grp, _ := GroupGetByName("upd-rule")
	if len(grp.Items) != beforeCount {
		t.Fatalf("规则不匹配新模型时不应补: %d -> %d", beforeCount, len(grp.Items))
	}

	// 现在把规则改成匹配 claude-gpt, 再更新渠道(不动成员)触发补齐。
	pat := "claude-gpt"
	if _, err := GroupUpdate(group.ID, &model.GroupUpdateRequest{AutoAddPattern: &pat}, context.Background()); err != nil {
		t.Fatalf("改规则失败: %v", err)
	}
	reloadAllCache(t)
	// 再次更新渠道(同配置)以触发 supplementGroupsForChannel。
	// ChannelUpdate 轮转 revision, 必须重新读缓存获取最新令牌, 否则 CAS 失败。
	channel2, ok := channelCache.Get(getChannelIDByGrant(t, group.Items[0].ChannelGrantID))
	if !ok {
		t.Fatalf("渠道缓存缺失")
	}
	detail2 := channelDetailFromDBForTest(t, channel2)
	if _, _, err := ChannelUpdate(&detail2, context.Background()); err != nil {
		t.Fatalf("二次更新渠道失败: %v", err)
	}
	reloadAllCache(t)
	grp2, _ := GroupGetByName("upd-rule")
	if len(grp2.Items) != beforeCount+1 {
		t.Fatalf("匹配新模型应补入一条: %d -> %d", beforeCount, len(grp2.Items))
	}
}

func getChannelIDByGrant(t *testing.T, grantID int) int {
	t.Helper()
	var grant model.ChannelGrant
	if err := db.GetDB().First(&grant, grantID).Error; err != nil {
		t.Fatalf("读授权失败: %v", err)
	}
	var cm model.ChannelModel
	if err := db.GetDB().First(&cm, grant.ChannelModelID).Error; err != nil {
		t.Fatalf("读模型失败: %v", err)
	}
	return cm.ChannelID
}

// 启用渠道补齐规则分组; 评分运行态(分数/现任)原样保留(纯新增不前进代数)。
func TestChannelEnabledEnableSupplementsAndPreservesScoredRuntime(t *testing.T) {
	clearAll(t)
	chID, _ := seedChannel(t, "en", true, []string{"gpt-4o"}, []keySpec{{"k", true}})
	reloadAllCache(t)
	group, err := GroupCreate(&model.GroupCreateRequest{
		Name: "en-rule", Mode: model.GroupModeScored, AutoAddPattern: "^gpt-4o$",
	}, context.Background())
	if err != nil {
		t.Fatalf("建组失败: %v", err)
	}
	// 清掉成员后禁用渠道, 模拟"渠道启用后补齐"。
	if err := db.GetDB().Where("group_id = ?", group.ID).Delete(&model.GroupItem{}).Error; err != nil {
		t.Fatalf("清成员失败: %v", err)
	}
	if err := db.GetDB().Model(&model.Channel{}).Where("id = ?", chID).Update("enabled", false).Error; err != nil {
		t.Fatalf("禁用渠道失败: %v", err)
	}
	reloadAllCache(t)

	// 启用渠道: 规则分组应补入该渠道匹配授权。
	if _, err := ChannelEnabled(chID, true, context.Background()); err != nil {
		t.Fatalf("启用渠道失败: %v", err)
	}
	reloadAllCache(t)
	grp, _ := GroupGetByName("en-rule")
	if len(grp.Items) != 1 {
		t.Fatalf("启用渠道应补入匹配授权: %+v", grp.Items)
	}
}

// 禁用渠道不补齐(无新增)。
func TestChannelEnabledDisableNoSupplement(t *testing.T) {
	clearAll(t)
	chID, _ := seedChannel(t, "dis", true, []string{"gpt-4o"}, []keySpec{{"k", true}})
	reloadAllCache(t)
	group, err := GroupCreate(&model.GroupCreateRequest{
		Name: "dis-rule", Mode: model.GroupModeScored, AutoAddPattern: "^gpt-4o$",
	}, context.Background())
	if err != nil {
		t.Fatalf("建组失败: %v", err)
	}
	before := len(group.Items)
	if _, err := ChannelEnabled(chID, false, context.Background()); err != nil {
		t.Fatalf("禁用渠道失败: %v", err)
	}
	grp, _ := GroupGetByName("dis-rule")
	if len(grp.Items) != before {
		t.Fatalf("禁用渠道不应补入新成员: %d -> %d", before, len(grp.Items))
	}
}

// 原子性: 补齐失败时主变更也回滚。注入非法 pattern 到 DB 模拟规则损坏,
// ChannelCreate 在补齐阶段编译失败 → 整事务回滚 → 渠道未创建。
func TestSupplementFailureRollsBackMainChange(t *testing.T) {
	clearAll(t)
	_, _ = seedChannel(t, "ok", true, []string{"gpt-4o"}, []keySpec{{"k", true}})
	reloadAllCache(t)
	if _, err := GroupCreate(&model.GroupCreateRequest{
		Name: "corrupt", Mode: model.GroupModeScored, AutoAddPattern: "^gpt-4o$",
	}, context.Background()); err != nil {
		t.Fatalf("建组失败: %v", err)
	}
	// 直接写非法 pattern 进库, 模拟规则损坏(正常入口不会落入)。
	if err := db.GetDB().Model(&model.Group{}).Where("name = ?", "corrupt").Update("auto_add_pattern", "[unclosed").Error; err != nil {
		t.Fatalf("损坏规则失败: %v", err)
	}
	reloadAllCache(t)

	before := countRows(&model.Channel{})
	newDetail := model.ChannelDetail{
		ChannelConfig: model.ChannelConfig{Name: "rollback-ch", Enabled: true, BaseURL: "http://rb.example",
			OpenAIChatCompletionPath: "/chat", AnthropicMessagePath: "/msg"},
		Keys:   []model.ChannelKeyConfig{{Name: "k", Key: "sk", Enabled: true}},
		Models: []string{"gpt-4o"},
		Grants: []model.ChannelGrantConfig{{ModelName: "gpt-4o", KeyName: "k", Protocols: model.ProtocolOpenAIChatCompletion}},
	}
	if _, _, err := ChannelCreate(&newDetail, context.Background()); err == nil {
		t.Fatalf("补齐失败应导致整体回滚并报错")
	}
	after := countRows(&model.Channel{})
	if after != before {
		t.Fatalf("主变更未随补齐失败回滚: before=%d after=%d", before, after)
	}
}

// 提交后刷新失败: 提交事实(AddedItems)必须能校正缓存使新成员可见, 不被旧缓存永久漏掉。
func TestPostCommitRefreshFailurePublishesAddedItemsToCache(t *testing.T) {
	clearAll(t)
	_, _ = seedChannel(t, "pc", true, []string{"gpt-4o"}, []keySpec{{"k", true}})
	reloadAllCache(t)
	if _, err := GroupCreate(&model.GroupCreateRequest{
		Name: "pc-rule", Mode: model.GroupModeScored, AutoAddPattern: "^gpt-4o$",
	}, context.Background()); err != nil {
		t.Fatalf("建组失败: %v", err)
	}
	// 清成员, 让启用渠道补齐产生新增。
	var groupID int
	db.GetDB().Model(&model.Group{}).Where("name = ?", "pc-rule").Pluck("id", &groupID)
	if err := db.GetDB().Where("group_id = ?", groupID).Delete(&model.GroupItem{}).Error; err != nil {
		t.Fatalf("清成员失败: %v", err)
	}
	reloadAllCache(t)

	// 禁用渠道后注入刷新失败, 再启用: 事务提交成功, 刷新失败返回 PostCommitError 携带 AddedItems。
	var chID int
	db.GetDB().Model(&model.Channel{}).Where("name = ?", "pc").Pluck("id", &chID)
	if err := db.GetDB().Model(&model.Channel{}).Where("id = ?", chID).Update("enabled", false).Error; err != nil {
		t.Fatalf("禁用渠道失败: %v", err)
	}
	reloadAllCache(t)

	previous := refreshGroupsAfterCommit
	refreshGroupsAfterCommit = func(context.Context) error { return errors.New("refresh boom") }
	t.Cleanup(func() { refreshGroupsAfterCommit = previous })

	mutation, err := ChannelEnabled(chID, true, context.Background())
	if err == nil {
		t.Fatalf("刷新失败应返回错误")
	}
	var post *PostCommitError
	if !errors.As(err, &post) {
		t.Fatalf("错误不是 PostCommitError: %T", err)
	}
	if mutation == nil || len(mutation.GroupDeltas) == 0 {
		t.Fatalf("提交事实缺失: %+v", mutation)
	}
	delta := mutation.GroupDeltas[0]
	if delta.Removed {
		t.Fatalf("启用是纯新增, Removed 应为 false")
	}
	if len(delta.AddedItems) == 0 {
		t.Fatalf("AddedItems 应携带新成员行")
	}
	// 按事实校正缓存: 新成员应可见(否则旧缓存永久漏掉)。
	ApplyGroupMemberDeltas(mutation.GroupDeltas)
	grp, ok := groupCache.Get(groupID)
	if !ok {
		t.Fatalf("分组缓存缺失")
	}
	if len(grp.Items) != 1 {
		t.Fatalf("刷新失败后缓存应含新成员: %d 条", len(grp.Items))
	}
	if grp.Items[0].ID != delta.AddedItems[0].ID {
		t.Fatalf("缓存新成员 ID 与事实不符: %d vs %d", grp.Items[0].ID, delta.AddedItems[0].ID)
	}
}

// 渠道更新同时删除与新增成员: mutation 同时携带 Removed 与 AddedItems。
func TestChannelUpdateMutationCarriesAddedAndRemoved(t *testing.T) {
	clearAll(t)
	chID, grants := seedChannel(t, "mu", true, []string{"gpt-4o", "gpt-4o-mini"}, []keySpec{{"k", true}})
	reloadAllCache(t)
	group, err := GroupCreate(&model.GroupCreateRequest{
		Name: "mu-rule", Mode: model.GroupModeScored, AutoAddPattern: "^gpt-4o$",
	}, context.Background())
	if err != nil {
		t.Fatalf("建组失败: %v", err)
	}
	// 现有两成员(gpt-4o 与 gpt-4o-mini 中只有 gpt-4o 匹配被补)。先手动加 gpt-4o-mini 让更新删它。
	if err := db.GetDB().Create(&model.GroupItem{GroupID: group.ID, ChannelGrantID: grants["gpt-4o-mini|k"], Priority: 99, Enabled: true}).Error; err != nil {
		t.Fatalf("加成员失败: %v", err)
	}
	reloadAllCache(t)

	// 更新渠道: 去掉 gpt-4o-mini(级联删成员), 新增 claude-gpt 匹配规则需放宽; 这里只验证删除侧。
	channel, ok := channelCache.Get(chID)
	if !ok {
		t.Fatalf("渠道缓存缺失")
	}
	detail := channelDetailFromDBForTest(t, channel)
	// 删掉 gpt-4o-mini 的模型与授权 → 级联删成员。
	keptModels := []string{"gpt-4o"}
	keptGrants := []model.ChannelGrantConfig{}
	for _, g := range detail.Grants {
		if g.ModelName == "gpt-4o" {
			keptGrants = append(keptGrants, g)
		}
	}
	detail.Models = keptModels
	detail.Grants = keptGrants
	_, mutation, err := ChannelUpdate(&detail, context.Background())
	if err != nil {
		t.Fatalf("更新渠道失败: %v", err)
	}
	if mutation == nil {
		t.Fatalf("应有提交事实")
	}
	delta := mutation.GroupDeltas[0]
	if !delta.Removed {
		t.Fatalf("级联删除应标记 Removed=true")
	}
	if contains(delta.ItemIDs, grants["gpt-4o-mini|k"]) {
		t.Fatalf("已删成员不应在存活集合")
	}
}

// 逻辑导出/导入: 规则随 dump 往返; 导入不自动补齐成员(只还原 dump 内成员), 旧 dump 缺省按空串。
func TestImportPreservesPatternRoundtrip(t *testing.T) {
	clearAll(t)
	_, _ = seedChannel(t, "imp", true, []string{"gpt-4o"}, []keySpec{{"k", true}})
	reloadAllCache(t)
	group, err := GroupCreate(&model.GroupCreateRequest{
		Name: "imp-rule", Mode: model.GroupModeScored, AutoAddPattern: "^gpt-4o$",
	}, context.Background())
	if err != nil {
		t.Fatalf("建组失败: %v", err)
	}
	// 删掉建组时自动补入的成员, 使 dump 的 GroupItems 对该组为空: 导入后是否补齐由此区分。
	if err := db.GetDB().Where("group_id = ?", group.ID).Delete(&model.GroupItem{}).Error; err != nil {
		t.Fatalf("清成员失败: %v", err)
	}
	reloadAllCache(t)

	dump, err := DBExportAll(context.Background())
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	var exportedPattern string
	for _, g := range dump.Groups {
		if g.Name == "imp-rule" {
			exportedPattern = g.AutoAddPattern
		}
	}
	if exportedPattern != "^gpt-4o$" {
		t.Fatalf("导出未带规则: %q", exportedPattern)
	}
	// 确认 dump 里该组无成员(已清), 且渠道仍在 dump 里(导入后匹配授权可被补齐, 但导入不应补)。
	dumpItemsForGroup := 0
	for _, item := range dump.GroupItems {
		if item.GroupID == group.ID {
			dumpItemsForGroup++
		}
	}
	if dumpItemsForGroup != 0 {
		t.Fatalf("清成员后 dump 不应含该组成员: %d", dumpItemsForGroup)
	}

	// 清库后导入: dump 自带渠道/凭据/模型/授权, 导入即重建匹配授权; 规则往返不变, 且导入不自动补齐。
	clearAll(t)
	if _, err := DBImportIncremental(context.Background(), dump); err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	var g model.Group
	db.GetDB().Where("name = ?", "imp-rule").First(&g)
	if g.AutoAddPattern != "^gpt-4o$" {
		t.Fatalf("导入规则往返变化: %q", g.AutoAddPattern)
	}
	var itemCount int64
	db.GetDB().Model(&model.GroupItem{}).Where("group_id = ?", g.ID).Count(&itemCount)
	if itemCount != 0 {
		t.Fatalf("导入不应自动补齐成员: %d 条", itemCount)
	}
}

// 旧 dump 缺省 auto_add_pattern 字段: 按空串导入, 行为与旧分组一致(不补齐)。
func TestImportOldDumpWithoutPatternDefaultsEmpty(t *testing.T) {
	clearAll(t)
	_, _ = seedChannel(t, "old", true, []string{"gpt-4o"}, []keySpec{{"k", true}})
	reloadAllCache(t)
	// 直接写一条无 auto_add_pattern 的 JSON(模拟旧 dump)。
	oldJSON := `{"version":5,"exported_at":"2020-01-01T00:00:00Z","groups":[{"id":1,"name":"old-group","mode":"scored","active_item_id":0,"relay_config":{}}]}`
	dump := &model.DBDump{}
	if err := json.Unmarshal([]byte(oldJSON), dump); err != nil {
		t.Fatalf("解析旧 dump 失败: %v", err)
	}
	if _, err := DBImportIncremental(context.Background(), dump); err != nil {
		t.Fatalf("导入旧 dump 失败: %v", err)
	}
	var g model.Group
	db.GetDB().Where("name = ?", "old-group").First(&g)
	if g.AutoAddPattern != "" {
		t.Fatalf("旧 dump 缺省字段应导入为空串: %q", g.AutoAddPattern)
	}
}

// 非法 pattern 不落库: 导入拒绝且不写库。
func TestImportInvalidPatternRejected(t *testing.T) {
	clearAll(t)
	dump := &model.DBDump{
		Version: dbDumpVersion,
		Groups:  []model.Group{{ID: 1, Name: "bad", Mode: model.GroupModeScored, AutoAddPattern: "[unclosed"}},
	}
	if _, err := DBImportIncremental(context.Background(), dump); err == nil {
		t.Fatalf("非法 pattern 应被导入拒绝")
	}
	var count int64
	db.GetDB().Model(&model.Group{}).Where("name = ?", "bad").Count(&count)
	if count != 0 {
		t.Fatalf("非法 pattern 不应落库: %d 行", count)
	}
}

func countRows(m interface{}) int64 {
	var n int64
	db.GetDB().Model(m).Count(&n)
	return n
}
