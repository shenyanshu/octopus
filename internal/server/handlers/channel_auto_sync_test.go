package handlers

// 本文件验证 enableAllAutoSync handler 的 HTTP 契约与响应 envelope。
// 全部经真实 gin handler 与真实 op, 不联系上游、不 enqueue 同步。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/gin-gonic/gin"
)

// doEnableAllAutoSyncRequest 构造 gin 上下文并执行 enableAllAutoSync handler。
func doEnableAllAutoSyncRequest(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	if body == "" {
		body = "{}"
	}
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/channel/auto-sync/enable-all", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	enableAllAutoSync(c)
	return w
}

// autoSyncHandlerSeedChannel 建 auto_sync=false 的渠道供测试。
func autoSyncHandlerSeedChannel(t *testing.T, name string, autoSync bool) int {
	t.Helper()
	ch := model.Channel{
		Revision: "rev-" + name,
		ChannelConfig: model.ChannelConfig{
			Name:           name,
			Enabled:        true,
			BaseURL:        "http://" + name + ".example",
			AutoSyncModels: autoSync,
		},
	}
	if err := db.GetDB().Create(&ch).Error; err != nil {
		t.Fatalf("建渠道 %s 失败: %v", name, err)
	}
	return ch.ID
}

// autoSyncHandlerClearTables 清空渠道相关表并刷新缓存。
func autoSyncHandlerClearTables(t *testing.T) {
	t.Helper()
	for _, table := range []string{"channel_grants", "channel_models", "channel_keys", "channels"} {
		db.GetDB().Exec("DELETE FROM " + table)
	}
	db.GetDB().Exec("DELETE FROM sqlite_sequence WHERE name IN ('channels','channel_keys','channel_models','channel_grants')")
	if err := op.InitCache(); err != nil {
		t.Fatalf("刷新缓存失败: %v", err)
	}
}

// TestHandlerEnableAllAutoSyncSuccess 验证: 两个 auto=false 渠道被开启, 响应 envelope 正确。
func TestHandlerEnableAllAutoSyncSuccess(t *testing.T) {
	autoSyncHandlerClearTables(t)
	autoSyncHandlerSeedChannel(t, "h-auto-false-1", false)
	autoSyncHandlerSeedChannel(t, "h-auto-false-2", false)
	autoSyncHandlerSeedChannel(t, "h-auto-true", true)

	w := doEnableAllAutoSyncRequest(t, "{}")
	if w.Code != http.StatusOK {
		t.Fatalf("应返回 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Code    int                                  `json:"code"`
		Message string                               `json:"message"`
		Data    model.ChannelAutoSyncEnableAllResult `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v body=%s", err, w.Body.String())
	}
	if resp.Code != http.StatusOK {
		t.Errorf("code 应为 200, got %d", resp.Code)
	}
	if resp.Message != "success" {
		t.Errorf("message 应为 success, got %s", resp.Message)
	}
	if resp.Data.UpdatedCount != 2 {
		t.Errorf("updated_count 应为 2, got %d", resp.Data.UpdatedCount)
	}
}

// TestHandlerEnableAllAutoSyncNoOpCountZero 验证: 全部已 true 时 count=0。
func TestHandlerEnableAllAutoSyncNoOpCountZero(t *testing.T) {
	autoSyncHandlerClearTables(t)
	autoSyncHandlerSeedChannel(t, "h-noop-1", true)
	autoSyncHandlerSeedChannel(t, "h-noop-2", true)

	w := doEnableAllAutoSyncRequest(t, "{}")
	if w.Code != http.StatusOK {
		t.Fatalf("应返回 200, got %d", w.Code)
	}
	var resp struct {
		Data model.ChannelAutoSyncEnableAllResult `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Data.UpdatedCount != 0 {
		t.Errorf("no-op updated_count 应为 0, got %d", resp.Data.UpdatedCount)
	}
}

// TestHandlerEnableAllAutoSyncEmptyBodyRequest 验证: 请求 {} 正常处理(无参数必填)。
func TestHandlerEnableAllAutoSyncEmptyBodyRequest(t *testing.T) {
	autoSyncHandlerClearTables(t)
	autoSyncHandlerSeedChannel(t, "h-empty-body", false)

	w := doEnableAllAutoSyncRequest(t, "{}")
	if w.Code != http.StatusOK {
		t.Fatalf("应返回 200, got %d", w.Code)
	}
}

// TestHandlerEnableAllAutoSyncGetDetailConsistent 验证: 开启后 GET detail 新 token 一致。
func TestHandlerEnableAllAutoSyncGetDetailConsistent(t *testing.T) {
	autoSyncHandlerClearTables(t)
	chID := autoSyncHandlerSeedChannel(t, "h-get-detail", false)

	w := doEnableAllAutoSyncRequest(t, "{}")
	if w.Code != http.StatusOK {
		t.Fatalf("应返回 200, got %d", w.Code)
	}

	// 经 op.ChannelDetailGet 读取, revision 应与 DB 一致且 auto_sync=true。
	detail, err := op.ChannelDetailGet(context.Background(), chID)
	if err != nil {
		t.Fatalf("ChannelDetailGet 失败: %v", err)
	}
	var ch model.Channel
	db.GetDB().Where("id = ?", chID).First(&ch)
	if detail.Revision != ch.Revision {
		t.Errorf("detail revision=%s 与 DB=%s 不一致", detail.Revision, ch.Revision)
	}
	if !detail.AutoSyncModels {
		t.Error("detail AutoSyncModels 应为 true")
	}
}

// TestHandlerEnableAllThenStaleRevisionUpdate409 验证: HTTP enable-all 轮转 revision 后,
// 用旧 revision 提交 HTTP update 返回 409, DB 的 auto_sync/revision 不被覆盖。
// 经真实 gin handler(httptest Recorder), 不调 op.ChannelUpdate, 不启动外部服务。
func TestHandlerEnableAllThenStaleRevisionUpdate409(t *testing.T) {
	syncTestDB(t)
	// 建渠道 auto_sync=false, 获取旧 revision。
	chID, oldRev := revisionTestChannel(t, "enable-stale-rev")
	// HTTP enable-all。
	w := doEnableAllAutoSyncRequest(t, "{}")
	if w.Code != http.StatusOK {
		t.Fatalf("enable-all 应返回 200, got %d body=%s", w.Code, w.Body.String())
	}
	// 读取 enable-all 后的 detail(含新 revision)。
	detail, err := op.ChannelDetailGet(context.Background(), chID)
	if err != nil {
		t.Fatalf("ChannelDetailGet 失败: %v", err)
	}
	if !detail.AutoSyncModels {
		t.Fatal("enable-all 后 AutoSyncModels 应为 true")
	}
	if detail.Revision == oldRev {
		t.Fatal("enable-all 后 revision 应轮转")
	}
	// HTTP update: 完整有效 body + 旧 revision → 应返回 409。
	body := map[string]any{
		"id":       chID,
		"revision": oldRev,
		"name":     detail.Name,
		"enabled":  detail.Enabled,
		"base_url": detail.BaseURL,
		"keys":     detail.Keys,
		"models":   detail.Models,
		"grants":   detail.Grants,
	}
	w2 := doUpdateChannelRequest(t, body)
	if w2.Code != http.StatusConflict {
		t.Fatalf("旧 revision update 应返回 409, got %d body=%s", w2.Code, w2.Body.String())
	}
	// DB 未被覆盖: auto_sync 仍为 true, revision 仍为 enable-all 后的新值。
	var dbCh model.Channel
	if err := db.GetDB().Where("id = ?", chID).First(&dbCh).Error; err != nil {
		t.Fatalf("读 DB 渠道失败: %v", err)
	}
	if !dbCh.AutoSyncModels {
		t.Error("409 后 DB AutoSyncModels 应仍为 true (未被覆盖)")
	}
	if dbCh.Revision != detail.Revision {
		t.Errorf("409 后 DB revision=%s, 应为 enable-all 后的 %s", dbCh.Revision, detail.Revision)
	}
	if dbCh.Revision == oldRev {
		t.Error("409 后 DB revision 不应为旧值")
	}
}
