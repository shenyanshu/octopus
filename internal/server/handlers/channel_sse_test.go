package handlers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/groupevents"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay"
	"github.com/gin-gonic/gin"
)

// 本文件证明渠道写操作 mutation 编排的 SSE 通知契约:
// 1. 已提交(含刷新失败)的事实必须发布 changed 事件, 不假定客户端重连;
// 2. 提交前失败(rollback) mutation 为 nil, 无事件;
// 3. 纯新增不前进路由代数, 不清既有 score 与 manual active。
// 测试订阅现有 groupEventStreams, 经真实 createChannel/enableChannel handler 编排,
// 并对 op 刷新失败注入场景单独验证 processChannelMutation(生产编排同一函数)。

// processChannelMutation 定义在 channel.go(生产), 测试经真实 handler 与直接调用两种方式验证它。
// 测试不重定义该函数: 避免与生产编排分叉。

// subscribeGroupEvents 订阅 groupevents 共享总线一条事件流, 返回通道与清理函数。
// 复用现有 SSE 设施: 与 streamGroupEvents 同一订阅入口, 只是不走 HTTP 长连接。
func subscribeGroupEvents(t *testing.T) (chan groupevents.Event, func()) {
	t.Helper()
	events := groupevents.Subscribe()
	return events, func() {
		groupevents.Unsubscribe(events)
	}
}

// receiveGroupEvent 等待一条事件, 最多让出调度若干次后超时返回 "", false。
func receiveGroupEvent(events <-chan groupevents.Event) (string, bool) {
	for i := 0; i < 8; i++ {
		yieldOnce()
		select {
		case ev, ok := <-events:
			if !ok {
				return "", false
			}
			return ev.Name, true
		default:
		}
	}
	return "", false
}

// yieldOnce 让出当前 goroutine 一次, 给发布侧机会。
func yieldOnce() {
	ch := make(chan struct{})
	go func() { close(ch) }()
	<-ch
}

// seedPatternChannelGrant 建一个启用渠道带单模型单凭据单授权, 返回渠道 ID 与授权主键。
// upstreamURL 非空时用于渠道地址(转发需真实上游); 空则用占位地址(仅测补齐不转发)。
func seedPatternChannelGrant(t *testing.T, name, modelName, upstreamURL string) (int, int) {
	t.Helper()
	if upstreamURL == "" {
		upstreamURL = "http://" + name + ".example"
	}
	channel := model.Channel{ChannelConfig: model.ChannelConfig{
		Name: name, Enabled: true, BaseURL: upstreamURL,
		OpenAIChatCompletionPath: "/chat", AnthropicMessagePath: "/msg",
	}}
	if err := db.GetDB().Create(&channel).Error; err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	key := model.ChannelKey{ChannelID: channel.ID, ChannelKeyConfig: model.ChannelKeyConfig{Name: "k", Key: "sk", Enabled: true}}
	if err := db.GetDB().Create(&key).Error; err != nil {
		t.Fatalf("建凭据失败: %v", err)
	}
	cm := model.ChannelModel{ChannelID: channel.ID, Name: modelName}
	if err := db.GetDB().Create(&cm).Error; err != nil {
		t.Fatalf("建模型失败: %v", err)
	}
	grant := model.ChannelGrant{ChannelModelID: cm.ID, ChannelKeyID: key.ID, Protocols: model.ProtocolOpenAIChatCompletion}
	if err := db.GetDB().Create(&grant).Error; err != nil {
		t.Fatalf("建授权失败: %v", err)
	}
	if err := op.InitCache(); err != nil {
		t.Fatalf("刷新缓存失败: %v", err)
	}
	return channel.ID, grant.ID
}

func callCreateChannelHandler(t *testing.T, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httptest.NewRequest(http.MethodPost, "/api/v1/channel/create", bytes.NewReader(body))
	gc.Request.Header.Set("Content-Type", "application/json")
	createChannel(gc)
	return rec
}

func callEnableChannelHandler(t *testing.T, channelID int, enabled bool) *httptest.ResponseRecorder {
	t.Helper()
	body := []byte(`{"id":` + strconv.Itoa(channelID) + `,"enabled":` + strconv.FormatBool(enabled) + `}`)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httptest.NewRequest(http.MethodPost, "/api/v1/channel/enable", bytes.NewReader(body))
	gc.Request.Header.Set("Content-Type", "application/json")
	enableChannel(gc)
	return rec
}

func clearChannelHandlerTables(t *testing.T) {
	t.Helper()
	for _, table := range []string{"groups", "group_items", "channel_grants", "channel_models", "channel_keys", "channels"} {
		if err := db.GetDB().Exec("DELETE FROM " + table).Error; err != nil {
			t.Fatalf("清理表 %s 失败: %v", table, err)
		}
	}
	if err := op.InitCache(); err != nil {
		t.Fatalf("刷新缓存失败: %v", err)
	}
}

// TestProcessChannelMutationCommittedPublishesChangedEvent 已提交事实(含刷新失败场景)必须发布 changed 事件。
// op 层在 PostCommitError.Mutation 携带的就是非 nil mutation; 此处直接验证 processChannelMutation 的契约,
// 边界声明: 此用例不模拟 op 刷新失败注入(op.refreshGroupsAfterCommit 未导出),
// 而是用一个真实存在的非 nil mutation(由预置渠道授权构建)证明编排函数发布事件。
func TestProcessChannelMutationCommittedPublishesChangedEvent(t *testing.T) {
	clearChannelHandlerTables(t)
	chID, grantID := seedPatternChannelGrant(t, "sse-proc", "gpt-4o", "")
	_ = chID
	// 建一个不带规则的分组(避免创建时自动补入该授权), 手动加成员代表"已提交存活集合"。
	created, err := op.GroupCreate(&model.GroupCreateRequest{
		Name: "sse-proc-rule", Mode: model.GroupModeScored,
	}, context.Background())
	if err != nil {
		t.Fatalf("建分组失败: %v", err)
	}
	if err := db.GetDB().Create(&model.GroupItem{GroupID: created.ID, ChannelGrantID: grantID, Priority: 1, Enabled: true}).Error; err != nil {
		t.Fatalf("加成员失败: %v", err)
	}
	if err := op.InitCache(); err != nil {
		t.Fatalf("刷新缓存失败: %v", err)
	}

	// 构造一个非 nil mutation 代表 op 在刷新失败时交出的提交事实: 该组存活成员含刚建的项。
	mutation := &op.ChannelMutation{
		GroupDeltas: []op.GroupMembersDelta{{
			GroupID: created.ID,
			ItemIDs: []int{findItemIDByGrantID(t, created.ID, grantID)},
		}},
	}

	events, cleanup := subscribeGroupEvents(t)
	defer cleanup()

	relay.GroupGateLock()
	processChannelMutation(mutation)
	relay.GroupGateUnlock()

	name, ok := receiveGroupEvent(events)
	if !ok {
		t.Fatalf("已提交事实应发布 changed 事件, 未收到")
	}
	if name != "changed" {
		t.Fatalf("事件名 = %q, 想要 changed", name)
	}
}

// TestProcessChannelMutationNilNoEvent nil mutation(提交前失败/rollback)无事件。
func TestProcessChannelMutationNilNoEvent(t *testing.T) {
	clearChannelHandlerTables(t)
	events, cleanup := subscribeGroupEvents(t)
	defer cleanup()

	relay.GroupGateLock()
	processChannelMutation(nil)
	relay.GroupGateUnlock()

	if name, ok := receiveGroupEvent(events); ok {
		t.Fatalf("nil mutation 不应发布事件: 收到 %q", name)
	}
}

// TestCreateChannelHandlerRollbackNoMutationNoEvent 经真实 createChannel handler:
// 提交前失败(name 校验)mutation 为 nil, 无事件。
func TestCreateChannelHandlerRollbackNoMutationNoEvent(t *testing.T) {
	clearChannelHandlerTables(t)
	events, cleanup := subscribeGroupEvents(t)
	defer cleanup()

	// name 缺失在 normalize 即报错, 事务未开始, mutation 为 nil。
	body := []byte(`{"name":"","base_url":"http://x.example","keys":[],"models":[],"grants":[]}`)
	rec := callCreateChannelHandler(t, body)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("响应码 = %d, 想要 500: %s", rec.Code, rec.Body.String())
	}
	if name, ok := receiveGroupEvent(events); ok {
		t.Fatalf("提交前失败不应发布事件: 收到 %q", name)
	}
}

// TestEnableChannelHandlerPureAddPreservesScoredRuntime 经真实 enableChannel handler:
// 纯新增补入成员, 但既有成员的 score 与现任不变(代数不前进)。
func TestEnableChannelHandlerPureAddPreservesScoredRuntime(t *testing.T) {
	clearChannelHandlerTables(t)
	// chA 用真实 passthrough 上游以支持转发建立现任; chB 不转发, 占位地址即可。
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	t.Cleanup(upstream.Close)
	chA, grantA := seedPatternChannelGrant(t, "chA", "gpt-4o", upstream.URL)
	_ = chA
	chB, grantB := seedPatternChannelGrant(t, "chB", "gpt-4o", "")
	// 先禁用 chB: GroupCreate 时只有 chA 被补入, 之后启用 chB 才是纯新增。
	if err := db.GetDB().Model(&model.Channel{}).Where("id = ?", chB).Update("enabled", false).Error; err != nil {
		t.Fatalf("禁用 chB 失败: %v", err)
	}
	if err := op.InitCache(); err != nil {
		t.Fatalf("刷新缓存失败: %v", err)
	}

	created, err := op.GroupCreate(&model.GroupCreateRequest{
		Name: "pure-add", Mode: model.GroupModeScored, AutoAddPattern: "^gpt-4o$",
	}, context.Background())
	if err != nil {
		t.Fatalf("建分组失败: %v", err)
	}
	groupID := created.ID
	itemA := findItemIDByGrantID(t, groupID, grantA)
	// 落非缺省分数(DB) + 转发建立现任(评分模式, 运行态满分=100)。
	if err := db.GetDB().Model(&model.GroupItem{}).Where("id = ?", itemA).Update("score", 88).Error; err != nil {
		t.Fatalf("改成员分数失败: %v", err)
	}
	forwardOK(t, "pure-add")
	loaded, _ := op.GroupGetByName("pure-add")
	currentBefore := relay.RouteStateOf(loaded).CurrentItemID
	scoreBefore := relay.RouteStateOf(loaded).Scores[itemA]

	events, cleanup := subscribeGroupEvents(t)
	defer cleanup()

	// 启用 chB: 规则分组补入 chB 的 gpt-4o(纯新增), chA 的 score 与现任应不变。
	rec := callEnableChannelHandler(t, chB, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("启用渠道响应码 = %d: %s", rec.Code, rec.Body.String())
	}
	name, ok := receiveGroupEvent(events)
	if !ok || name != "changed" {
		grpDiag, _ := op.GroupGetByName("pure-add")
		t.Fatalf("纯新增应发布 changed: ok=%v name=%q items=%d", ok, name, len(grpDiag.Items))
	}

	grp, _ := op.GroupGetByName("pure-add")
	state := relay.RouteStateOf(grp)
	if state.CurrentItemID != currentBefore {
		t.Fatalf("纯新增改了现任: %d -> %d", currentBefore, state.CurrentItemID)
	}
	if state.Scores[itemA] != scoreBefore {
		t.Fatalf("纯新增清了既有运行态 score: %d, 想要 %d", state.Scores[itemA], scoreBefore)
	}
	itemB := findItemIDByGrantID(t, groupID, grantB)
	found := false
	for _, item := range grp.Items {
		if item.ID == itemB {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("纯新增未补入 chB 成员")
	}
}

// TestEnableChannelHandlerNormalPathSingleEvent 正常启用路径只一次 changed 事件。
func TestEnableChannelHandlerNormalPathSingleEvent(t *testing.T) {
	clearChannelHandlerTables(t)
	chB, _ := seedPatternChannelGrant(t, "single", "gpt-4o", "")
	if err := db.GetDB().Model(&model.Channel{}).Where("id = ?", chB).Update("enabled", false).Error; err != nil {
		t.Fatalf("禁用失败: %v", err)
	}
	if err := op.InitCache(); err != nil {
		t.Fatalf("刷新缓存失败: %v", err)
	}
	if _, err := op.GroupCreate(&model.GroupCreateRequest{
		Name: "single-rule", Mode: model.GroupModeScored, AutoAddPattern: "^gpt-4o$",
	}, context.Background()); err != nil {
		t.Fatalf("建组失败: %v", err)
	}

	events, cleanup := subscribeGroupEvents(t)
	defer cleanup()

	rec := callEnableChannelHandler(t, chB, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("响应码 = %d: %s", rec.Code, rec.Body.String())
	}
	name, ok := receiveGroupEvent(events)
	if !ok || name != "changed" {
		t.Fatalf("应收到一次 changed: ok=%v name=%q", ok, name)
	}
	if name2, ok := receiveGroupEvent(events); ok {
		t.Fatalf("正常路径不应有第二条事件: 收到 %q", name2)
	}
}

func findItemIDByGrantID(t *testing.T, groupID, grantID int) int {
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

// 防止未用 import 误报。
var _ = sync.Mutex{}
