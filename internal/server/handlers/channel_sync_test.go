package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/channelsync"
	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/gin-gonic/gin"
)

// channelsyncInitOnce 确保 channelsync 协调器在 handler 测试进程中被初始化一次。
// handlers 包的 TestMain(在 group_test.go)已初始化 DB 与 cache, 但未初始化 channelsync;
// 本测试文件验证 sync handler 契约, 需要协调器可用, 用 init guard 避免与 group_test 重复初始化。
var channelsyncInitOnce sync.Once

func ensureChannelsyncInit() {
	channelsyncInitOnce.Do(func() {
		channelsync.Init(context.Background())
	})
}

// syncTestDB 清空渠道相关表并刷新缓存, 为每个 sync handler 测试提供干净起点。
func syncTestDB(t *testing.T) {
	t.Helper()
	ensureChannelsyncInit()
	for _, tbl := range []string{"group_items", "groups", "channel_grants", "channel_models", "channel_keys", "channels"} {
		db.GetDB().Exec("DELETE FROM " + tbl)
	}
	db.GetDB().Exec("DELETE FROM sqlite_sequence WHERE name IN ('channels','channel_keys','channel_models','channel_grants','groups','group_items')")
	if err := op.InitCache(); err != nil {
		t.Fatalf("刷新缓存失败: %v", err)
	}
}

// syncTestChannel 建渠道+单启用凭据, 返回渠道 ID。
func syncTestChannel(t *testing.T, name, baseURL, key string, autoSync bool) int {
	t.Helper()
	detail := model.ChannelDetail{
		ChannelConfig: model.ChannelConfig{
			Name:                 name,
			Enabled:              true,
			BaseURL:              baseURL,
			OpenAIResponsePath:   "/v1/responses",
			AnthropicMessagePath: "/v1/messages",
			AutoSyncModels:       autoSync,
		},
		Keys:   []model.ChannelKeyConfig{{Name: "default", Key: key, Enabled: true}},
		Models: []string{},
		Grants: []model.ChannelGrantConfig{},
	}
	created, _, err := op.ChannelCreate(&detail, context.Background())
	if err != nil {
		t.Fatalf("建渠道 %s 失败: %v", name, err)
	}
	return created.ID
}

// --- getSyncStatus handler ---

func TestHandlerGetSyncStatusEmpty(t *testing.T) {
	syncTestDB(t)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/channel/sync-status", nil)
	getSyncStatus(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", w.Code)
	}
	var wrapper struct {
		Data []model.ChannelModelSyncStatus `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &wrapper); err != nil {
		t.Fatalf("解析包装失败: %v body=%s", err, w.Body.String())
	}
	if wrapper.Data == nil {
		t.Error("sync-status 应返回非 nil 数组")
	}
	if len(wrapper.Data) != 0 {
		t.Errorf("清空后应 0 条, 得到 %d", len(wrapper.Data))
	}
}

func TestHandlerGetSyncStatusRunningNullLastSyncAt(t *testing.T) {
	syncTestDB(t)
	// 阻塞上游: 让 sync 进入 running 后检查 last_sync_at 为 null。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // 阻塞到请求被取消
	}))
	defer srv.Close()
	chID := syncTestChannel(t, "sync-running", srv.URL, "sk", false)
	started, _, _, _ := channelsync.StartSingle(chID)
	if !started {
		t.Fatal("应启动")
	}
	// 等 running 状态出现。
	var raw []byte
	for i := 0; i < 50; i++ {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/channel/sync-status", nil)
		getSyncStatus(c)
		body := w.Body.String()
		if strings.Contains(body, `"running"`) {
			raw = w.Body.Bytes()
			break
		}
	}
	if raw == nil {
		t.Fatal("未看到 running 状态")
	}
	if !strings.Contains(string(raw), `"last_sync_at":null`) {
		t.Errorf("running 状态 last_sync_at 应为 null: %s", string(raw))
	}
}

// --- syncChannelModels handler ---

func TestHandlerSyncChannelModelsInvalidID(t *testing.T) {
	syncTestDB(t)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "abc"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/channel/sync/abc", nil)
	syncChannelModels(c)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400 (invalid ID)", w.Code)
	}
}

func TestHandlerSyncChannelModelsNotFound(t *testing.T) {
	syncTestDB(t)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "99999"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/channel/sync/99999", nil)
	syncChannelModels(c)
	if w.Code != http.StatusNotFound {
		t.Errorf("status=%d, want 404", w.Code)
	}
}

func TestHandlerSyncChannelModelsStopped503(t *testing.T) {
	syncTestDB(t)
	// 停止协调器, 验证 handler 返回 503。
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	channelsync.Stop(ctx)
	defer channelsync.Init(context.Background()) // 恢复供后续测试
	chID := syncTestChannel(t, "sync-stopped", "http://unused", "sk", false)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: strconv.Itoa(chID)}}
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/channel/sync/"+strconv.Itoa(chID), nil)
	syncChannelModels(c)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d, want 503 (coordinator stopped)", w.Code)
	}
}

// --- syncAllChannelModels handler ---

func TestHandlerSyncAllChannelModelsStopped503(t *testing.T) {
	syncTestDB(t)
	// 先建一个 auto sync 渠道, 使 StartBatch 有候选, 然后停止协调器触发 ErrStopped。
	chID := syncTestChannel(t, "sync-all-stopped", "http://unused", "sk", true)
	_ = chID
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	channelsync.Stop(ctx)
	defer channelsync.Init(context.Background())
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/channel/sync-all", nil)
	syncAllChannelModels(c)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d, want 503 (coordinator stopped)", w.Code)
	}
}

func TestHandlerSyncAllChannelModelsEmptyArrays(t *testing.T) {
	syncTestDB(t)
	// 无 auto sync 渠道, batch 返回空数组(非 null)。
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/channel/sync-all", nil)
	syncAllChannelModels(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", w.Code)
	}
	var wrapper struct {
		Data model.ChannelSyncStartResult `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &wrapper); err != nil {
		t.Fatalf("解析失败: %v body=%s", err, w.Body.String())
	}
	if wrapper.Data.StartedIDs == nil {
		t.Error("StartedIDs 不应为 nil")
	}
	if wrapper.Data.BusyIDs == nil {
		t.Error("BusyIDs 不应为 nil")
	}
	if wrapper.Data.SkippedIDs == nil {
		t.Error("SkippedIDs 不应为 nil")
	}
}
