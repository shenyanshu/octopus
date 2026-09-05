package relay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/gin-gonic/gin"
	"github.com/looplj/axonhub/llm"
)

// 本文件是评分模式经真实 Forward 控制流的行为测试: 真实数据库与缓存, httptest 假上游, gin 转发链路。
// 上游桩按成员独立计数, 断言"哪个成员真正跨越了网络边界"是全部用例的公共底线。

// TestMain 建立测试数据库与缓存, 数据行由各用例自行播种。
func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	dir, err := os.MkdirTemp("", "octopus-relay-test")
	if err != nil {
		panic(err)
	}
	code := func() int {
		if err := db.InitDB("sqlite", filepath.Join(dir, "relay.db"), false); err != nil {
			panic(err)
		}
		if err := op.InitCache(); err != nil {
			panic(err)
		}
		return m.Run()
	}()
	_ = db.Close()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// upstreamStub 是一个带请求计数的假上游。
type upstreamStub struct {
	server *httptest.Server
	hits   *atomic.Int32
}

// newUpstreamStub 按给定处理函数建立假上游, 测试结束时自动关闭。
func newUpstreamStub(t *testing.T, handler http.HandlerFunc) upstreamStub {
	t.Helper()
	hits := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	return upstreamStub{server: server, hits: hits}
}

// memberFixture 是一个已播种的分组成员: 独立渠道, 模型, 凭据与授权。
type memberFixture struct {
	itemID    int
	channelID int
	upstream  upstreamStub
}

// scoredGroupFixture 是一次播种完成后的评分分组。
type scoredGroupFixture struct {
	id      int
	members []memberFixture
}

// seedScoredGroup 清库后播种一个分组, members 顺序即配置顺序。
// 每个成员的协议由 protocols 决定, 仅 Anthropic 时走跨协议转换链路, 仅 OpenAI Chat 时走透传链路。
func seedScoredGroup(t *testing.T, mode model.GroupMode, protocols model.Protocol, members ...upstreamStub) scoredGroupFixture {
	t.Helper()
	dbConn := db.GetDB()
	for _, table := range []string{"groups", "group_items", "channel_grants", "channel_models", "channel_keys", "channels"} {
		if err := dbConn.Exec("DELETE FROM " + table).Error; err != nil {
			t.Fatalf("清理表 %s 失败: %v", table, err)
		}
	}
	resetRoutesForTest()

	group := model.Group{Name: "scored-fixture", Mode: mode, RelayConfig: model.GroupRelayConfig{
		MemberMaxAttempts:                     1,
		MemberRetryIntervalSeconds:            1,
		MemberNonStreamResponseTimeoutSeconds: 5,
		MemberStreamFirstEventTimeoutSeconds:  5,
		MemberCooldownSeconds:                 60,
	}}
	fixture := scoredGroupFixture{}
	for i, upstream := range members {
		channel := model.Channel{ChannelConfig: model.ChannelConfig{
			Name:                     "ch-" + strconv.Itoa(i),
			Enabled:                  true,
			BaseURL:                  upstream.server.URL,
			OpenAIChatCompletionPath: "/chat",
			AnthropicMessagePath:     "/msg",
		}}
		if err := dbConn.Create(&channel).Error; err != nil {
			t.Fatalf("建渠道失败: %v", err)
		}
		key := model.ChannelKey{ChannelID: channel.ID, ChannelKeyConfig: model.ChannelKeyConfig{Name: "k", Key: "sk-test", Enabled: true}}
		if err := dbConn.Create(&key).Error; err != nil {
			t.Fatalf("建凭据失败: %v", err)
		}
		channelModel := model.ChannelModel{ChannelID: channel.ID, Name: "up-model"}
		if err := dbConn.Create(&channelModel).Error; err != nil {
			t.Fatalf("建模型失败: %v", err)
		}
		grant := model.ChannelGrant{ChannelModelID: channelModel.ID, ChannelKeyID: key.ID, Protocols: protocols}
		if err := dbConn.Create(&grant).Error; err != nil {
			t.Fatalf("建授权失败: %v", err)
		}
		group.Items = append(group.Items, model.GroupItem{ChannelGrantID: grant.ID, Priority: i + 1, Enabled: true})
		fixture.members = append(fixture.members, memberFixture{channelID: channel.ID, upstream: upstream})
	}
	if err := dbConn.Create(&group).Error; err != nil {
		t.Fatalf("建分组失败: %v", err)
	}
	var reloaded model.Group
	if err := dbConn.Preload("Items").First(&reloaded, "name = ?", group.Name).Error; err != nil {
		t.Fatalf("重载分组失败: %v", err)
	}
	fixture.id = reloaded.ID
	for i := range fixture.members {
		fixture.members[i].itemID = reloaded.Items[i].ID
	}
	if err := op.InitCache(); err != nil {
		t.Fatalf("刷新缓存失败: %v", err)
	}
	return fixture
}

// newForwardRouter 建立走 OpenAI Chat 协议的转发路由。
func newForwardRouter() *gin.Engine {
	router := gin.New()
	router.POST("/v1/chat/completions", Forward(llm.APIFormatOpenAIChatCompletion))
	return router
}

// clientBody 构造合法的客户端请求体; invalid 为真时给出无法被任何成员转换的请求形状。
func clientBody(stream, invalid bool) []byte {
	if invalid {
		return []byte(`{"model":"scored-fixture","stream":false,"messages":"not-an-array"}`)
	}
	body := map[string]any{"model": "scored-fixture", "messages": []map[string]string{{"role": "user", "content": "hi"}}}
	if stream {
		body["stream"] = true
	}
	raw, _ := json.Marshal(body)
	return raw
}

// postForward 同步发起一次转发请求。
func postForward(router *gin.Engine, stream, invalid bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(clientBody(stream, invalid)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// latestRequestState 返回最近一条请求状态, 用于终态断言。
func latestRequestState() RequestState {
	mu.Lock()
	defer mu.Unlock()
	var latest *RequestState
	for _, request := range requests {
		if latest == nil || request.ID > latest.ID {
			latest = request
		}
	}
	return *latest
}

// waitLatestRequestStatus 轮询等待最近请求到达指定终态。
func waitLatestRequestStatus(t *testing.T, status Status, timeout time.Duration) RequestState {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if state := latestRequestState(); state.Status == status {
			return state
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("请求未在 %v 内到达终态 %s, 当前 %s", timeout, status, latestRequestState().Status)
	return RequestState{}
}

// channelFailedStats 读取渠道累计失败次数。
func channelFailedStats(t *testing.T, channelID int) int64 {
	t.Helper()
	channel, err := op.ChannelGet(channelID)
	if err != nil {
		t.Fatalf("读渠道失败: %v", err)
	}
	return channel.StatsMetrics.RequestFailed
}

// mustScore 断言成员分数, 未记录视为初始分。
func mustScore(t *testing.T, fixture scoredGroupFixture, index, want int) {
	t.Helper()
	got := scoreOfItem(fixture.id, fixture.members[index].itemID)
	if got != want {
		t.Fatalf("成员 %d 分数 = %d, 想要 %d", index, got, want)
	}
}

// openAICompletion 写出一份完整的 OpenAI Chat 非流式成功响应。
func openAICompletion(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
}

// anthropicMessage 写出一份完整的 Anthropic 非流式成功响应。
func anthropicMessage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"up-model","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
}

// serveSSE 以事件流写出给定事件, done 为真时附上 OpenAI 终止事件。
func serveSSE(w http.ResponseWriter, events []string, done bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for _, event := range events {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
		if flusher != nil {
			flusher.Flush()
		}
	}
	if done {
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// openAIStreamChunk 是一条合法的 OpenAI 流内容事件。
const openAIStreamChunk = `{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"}}]}`

// anthropicStreamEvents 是一组能完整终结的 Anthropic 流事件。
var anthropicStreamEvents = []string{
	`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"up-model","usage":{"input_tokens":1,"output_tokens":1}}}`,
	`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
	`{"type":"content_block_stop","index":0}`,
	`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
	`{"type":"message_stop"}`,
}

// serveAnthropicStream 写出事件名携带的 Anthropic 事件流。
func serveAnthropicStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for _, event := range anthropicStreamEvents {
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", anthropicEventName(event), event)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// anthropicEventName 从事件正文中提取事件名, Anthropic SSE 要求事件名与正文类型一致。
func anthropicEventName(event string) string {
	var parsed struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal([]byte(event), &parsed)
	return parsed.Type
}

// refreshGroupCache 在直接改库后刷新分组缓存, 让转发选路看到最新配置。
func refreshGroupCache(t *testing.T) {
	t.Helper()
	if err := op.InitCache(); err != nil {
		t.Fatalf("刷新缓存失败: %v", err)
	}
}

// mutateChannelConfig 直接改成员渠道配置并刷新缓存。
func mutateChannelConfig(t *testing.T, fixture scoredGroupFixture, index int, column string, value any) {
	t.Helper()
	if err := db.GetDB().Model(&model.Channel{}).Where("id = ?", fixture.members[index].channelID).Update(column, value).Error; err != nil {
		t.Fatalf("更新渠道 %s 失败: %v", column, err)
	}
	refreshGroupCache(t)
}

// mutateGrantProtocols 直接改成员授权协议并刷新缓存。
func mutateGrantProtocols(t *testing.T, fixture scoredGroupFixture, index int, protocols model.Protocol) {
	t.Helper()
	grantID := grantIDOfItem(t, fixture.members[index].itemID)
	if err := db.GetDB().Model(&model.ChannelGrant{}).Where("id = ?", grantID).Update("protocols", protocols).Error; err != nil {
		t.Fatalf("更新授权协议失败: %v", err)
	}
	refreshGroupCache(t)
}

// deleteGrantOfItem 删除成员指向的授权并刷新缓存, 模拟授权被移除后的残缺配置。
func deleteGrantOfItem(t *testing.T, fixture scoredGroupFixture, index int) {
	t.Helper()
	grantID := grantIDOfItem(t, fixture.members[index].itemID)
	if err := db.GetDB().Delete(&model.ChannelGrant{}, grantID).Error; err != nil {
		t.Fatalf("删除授权失败: %v", err)
	}
	refreshGroupCache(t)
}

// grantIDOfItem 查分组成员当前指向的授权 ID。
func grantIDOfItem(t *testing.T, itemID int) int {
	t.Helper()
	var item model.GroupItem
	if err := db.GetDB().First(&item, itemID).Error; err != nil {
		t.Fatalf("查分组成员失败: %v", err)
	}
	return item.ChannelGrantID
}

// setGroupRelayConfig 覆盖分组的转发配置并刷新缓存。
func setGroupRelayConfig(t *testing.T, fixture scoredGroupFixture, config model.GroupRelayConfig) {
	t.Helper()
	// 配置列带 serializer:json 标签, 直接传结构体不会走序列化器, 需要先编成 JSON 字符串。
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("编码转发配置失败: %v", err)
	}
	if err := db.GetDB().Model(&model.Group{}).Where("id = ?", fixture.id).Update("relay_config", string(raw)).Error; err != nil {
		t.Fatalf("更新分组转发配置失败: %v", err)
	}
	refreshGroupCache(t)
}
