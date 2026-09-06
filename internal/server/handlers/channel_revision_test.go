package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay"
	"github.com/gin-gonic/gin"
)

// revisionTestChannel 建渠道并返回 (id, revision)。
func revisionTestChannel(t *testing.T, name string) (int, string) {
	t.Helper()
	detail := model.ChannelDetail{
		ChannelConfig: model.ChannelConfig{
			Name:    name,
			Enabled: true,
			BaseURL: "http://" + name + ".example",
		},
		Keys:   []model.ChannelKeyConfig{{Name: "k", Key: "sk", Enabled: true}},
		Models: []string{"m"},
		Grants: []model.ChannelGrantConfig{{ModelName: "m", KeyName: "k", Protocols: model.ProtocolOpenAIChatCompletion}},
	}
	created, _, err := op.ChannelCreate(&detail, context.Background())
	if err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	return created.ID, created.Revision
}

// doUpdateChannelRequest 构造 gin 上下文并执行 updateChannel handler。
func doUpdateChannelRequest(t *testing.T, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/channel", bytes.NewReader(raw))
	c.Request.Header.Set("Content-Type", "application/json")
	updateChannel(c)
	return w
}

// TestHandlerUpdateMissingRevision400 通过真实 gin handler 验证:
// 全量更新未提交 expected revision 返回 400。
func TestHandlerUpdateMissingRevision400(t *testing.T) {
	syncTestDB(t)
	chID, _ := revisionTestChannel(t, "rev-missing")
	detail, _ := op.ChannelDetailGet(context.Background(), chID)
	body := map[string]any{
		"id":       chID,
		"name":     detail.Name,
		"enabled":  detail.Enabled,
		"base_url": detail.BaseURL,
		"keys":     detail.Keys,
		"models":   detail.Models,
		"grants":   detail.Grants,
		// 故意不提交 revision
	}
	w := doUpdateChannelRequest(t, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("缺少 revision 应返回 400, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestHandlerUpdateStaleRevision409 通过真实 gin handler 验证:
// 提交过期 revision 返回 409。
func TestHandlerUpdateStaleRevision409(t *testing.T) {
	syncTestDB(t)
	chID, _ := revisionTestChannel(t, "rev-stale")
	detail, _ := op.ChannelDetailGet(context.Background(), chID)
	body := map[string]any{
		"id":       chID,
		"revision": "stale-token-not-in-db",
		"name":     detail.Name,
		"enabled":  detail.Enabled,
		"base_url": detail.BaseURL,
		"keys":     detail.Keys,
		"models":   detail.Models,
		"grants":   detail.Grants,
	}
	w := doUpdateChannelRequest(t, body)
	if w.Code != http.StatusConflict {
		t.Fatalf("过期 revision 应返回 409, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestHandlerUpdateSuccessReturnsNewToken 通过真实 gin handler 验证:
// 正确 revision 的更新成功并返回新 token。
func TestHandlerUpdateSuccessReturnsNewToken(t *testing.T) {
	syncTestDB(t)
	chID, oldRev := revisionTestChannel(t, "rev-success")
	if oldRev == "" {
		t.Fatal("初始 revision 为空")
	}
	detail, _ := op.ChannelDetailGet(context.Background(), chID)
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
	w := doUpdateChannelRequest(t, body)
	if w.Code != http.StatusOK {
		t.Fatalf("正确 revision 应返回 200, got %d body=%s", w.Code, w.Body.String())
	}
	var wrapper struct {
		Code int `json:"code"`
		Data struct {
			Revision string `json:"revision"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &wrapper); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if wrapper.Data.Revision == "" {
		t.Fatal("返回的新 revision 为空")
	}
	if wrapper.Data.Revision == oldRev {
		t.Fatal("返回的 revision 未轮转, 仍为旧值")
	}
}

// doGetChannelDetailRequest 构造 gin 上下文并执行 getChannelDetail handler。
func doGetChannelDetailRequest(t *testing.T, id int) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/channel/"+strconv.Itoa(id), nil)
	c.Params = gin.Params{{Key: "id", Value: strconv.Itoa(id)}}
	getChannelDetail(c)
	return w
}

// TestHandlerGetDetailMissing404 通过真实 gin handler 验证:
// GET 不存在的渠道返回 404。
func TestHandlerGetDetailMissing404(t *testing.T) {
	syncTestDB(t)
	w := doGetChannelDetailRequest(t, 99999)
	if w.Code != http.StatusNotFound {
		t.Fatalf("缺失渠道应返回 404, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestHandlerGetDetailSuccessReturnsRevision 通过真实 gin handler 验证:
// GET 返回的 detail 包含 revision 与子表一致。
func TestHandlerGetDetailSuccessReturnsRevision(t *testing.T) {
	syncTestDB(t)
	chID, expectedRev := revisionTestChannel(t, "rev-get")
	w := doGetChannelDetailRequest(t, chID)
	if w.Code != http.StatusOK {
		t.Fatalf("GET 应返回 200, got %d body=%s", w.Code, w.Body.String())
	}
	var wrapper struct {
		Code int `json:"code"`
		Data struct {
			ID       int                        `json:"id"`
			Revision string                     `json:"revision"`
			Models   []string                   `json:"models"`
			Keys     []model.ChannelKeyConfig   `json:"keys"`
			Grants   []model.ChannelGrantConfig `json:"grants"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &wrapper); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if wrapper.Data.Revision != expectedRev {
		t.Fatalf("GET revision %q != create revision %q", wrapper.Data.Revision, expectedRev)
	}
	if len(wrapper.Data.Models) != 1 || wrapper.Data.Models[0] != "m" {
		t.Fatalf("GET models 不匹配, got %v", wrapper.Data.Models)
	}
	if len(wrapper.Data.Keys) != 1 {
		t.Fatalf("GET keys 应有 1 条, got %d", len(wrapper.Data.Keys))
	}
	if len(wrapper.Data.Grants) != 1 {
		t.Fatalf("GET grants 应有 1 条, got %d", len(wrapper.Data.Grants))
	}
}

// TestHandlerCreateReturnsCoherentDetail 通过真实 gin handler 验证:
// create 返回的 detail 包含 revision 和子表, 与 DB 一致。
func TestHandlerCreateReturnsCoherentDetail(t *testing.T) {
	syncTestDB(t)
	body := map[string]any{
		"name":     "rev-create-coherent",
		"enabled":  true,
		"base_url": "http://create-coherent.example",
		"keys":     []map[string]any{{"name": "k", "key": "sk", "enabled": true}},
		"models":   []string{"m1", "m2"},
		"grants":   []map[string]any{{"model_name": "m1", "key_name": "k", "protocols": 2}},
	}
	raw, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/channel", bytes.NewReader(raw))
	c.Request.Header.Set("Content-Type", "application/json")
	createChannel(c)
	if w.Code != http.StatusOK {
		t.Fatalf("create 应返回 200, got %d body=%s", w.Code, w.Body.String())
	}
	var wrapper struct {
		Code int `json:"code"`
		Data struct {
			ID       int      `json:"id"`
			Revision string   `json:"revision"`
			Models   []string `json:"models"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &wrapper); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if wrapper.Data.Revision == "" {
		t.Fatal("create 返回的 revision 为空")
	}
	// 验证 DB 中 revision 与返回一致。
	var dbChannel model.Channel
	if err := db.GetDB().Where("id = ?", wrapper.Data.ID).First(&dbChannel).Error; err != nil {
		t.Fatalf("读 DB 渠道失败: %v", err)
	}
	if wrapper.Data.Revision != dbChannel.Revision {
		t.Fatalf("create 返回 revision %q != DB %q", wrapper.Data.Revision, dbChannel.Revision)
	}
	if len(wrapper.Data.Models) != 2 {
		t.Fatalf("create 返回 models 应有 2 条, got %d", len(wrapper.Data.Models))
	}
}

// TestHandlerGetDetailBlockedByWriterGate 通过 barrier 验证:
// 持写锁的 goroutine 提交 revision+模型新增后释放, GET 读到两者一致的快照。
// 不用 sleep, 用 channel 同步保证确定性。
func TestHandlerGetDetailBlockedByWriterGate(t *testing.T) {
	syncTestDB(t)
	chID, oldRev := revisionTestChannel(t, "rev-gate")
	detail, _ := op.ChannelDetailGet(context.Background(), chID)

	// writerStarted 在写锁获取后发送; writerCommitted 在事务提交后、释放写锁前发送;
	// getDone 在 GET 完成后发送。GET 必须等到写锁释放才能执行。
	writerStarted := make(chan struct{})
	writerCommitted := make(chan struct{})
	getDone := make(chan struct{})
	var getRevision string
	var getModels []string
	var getErr error

	// GET goroutine: 等写锁释放后执行, 读到提交后的状态。
	go func() {
		defer close(getDone)
		<-writerCommitted // 等写锁释放信号(实际由 GroupGateUnlock 释放)
		w := doGetChannelDetailRequest(t, chID)
		if w.Code != http.StatusOK {
			getErr = fmt.Errorf("GET status %d", w.Code)
			return
		}
		var wrapper struct {
			Code int `json:"code"`
			Data struct {
				Revision string   `json:"revision"`
				Models   []string `json:"models"`
			} `json:"data"`
		}
		json.Unmarshal(w.Body.Bytes(), &wrapper)
		getRevision = wrapper.Data.Revision
		getModels = wrapper.Data.Models
	}()

	// Writer goroutine: 获取写锁, 更新渠道(加模型+轮转revision), 提交后释放。
	go func() {
		relay.GroupGateLock()
		close(writerStarted)
		// 在写锁内添加模型并轮转 revision。
		detail.Models = append(detail.Models, "gate-new-model")
		detail.Grants = append(detail.Grants, model.ChannelGrantConfig{
			ModelName: "gate-new-model", KeyName: "k", Protocols: model.ProtocolOpenAIChatCompletion,
		})
		op.ChannelUpdate(&detail, context.Background())
		// 事务已提交, 释放写锁。GET 在此之后才能执行。
		relay.GroupGateUnlock()
		close(writerCommitted)
	}()

	<-writerStarted // 确认写锁已获取(GET 应被阻塞)
	<-getDone       // 等 GET 完成
	if getErr != nil {
		t.Fatalf("GET 失败: %v", getErr)
	}
	if getRevision == oldRev {
		t.Fatal("GET 读到的 revision 是旧值, 写锁未保护一致性")
	}
	if getRevision == "" {
		t.Fatal("GET 读到的 revision 为空")
	}
	found := false
	for _, m := range getModels {
		if m == "gate-new-model" {
			found = true
		}
	}
	if !found {
		t.Fatal("GET 未读到写锁内新增的模型 gate-new-model")
	}
}
