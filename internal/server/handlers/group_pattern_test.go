package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/gin-gonic/gin"
)

// 本文件证明分组自动补充规则的 handler 层行为: 非法输入 400 映射、规则创建经真实 handler 往返。
// 业务补齐的正确性由 op 层测试单独证明, 此处聚焦 handler 的状态码映射与响应信封。

// callCreateGroup 以真实 gin context 调用 createGroup handler。
func callCreateGroup(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(rec)
	ginContext.Request = httptest.NewRequest(http.MethodPost, "/api/v1/group/create", bytes.NewReader([]byte(body)))
	ginContext.Request.Header.Set("Content-Type", "application/json")
	createGroup(ginContext)
	return rec
}

// assertBadRequest 断言响应为 400。
func assertBadRequest(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("响应码 = %d, 想要 400: %s", rec.Code, rec.Body.String())
	}
}

// seedHandlerChannel 建一个启用渠道带单模型单凭据单授权, 返回授权主键。
func seedHandlerChannel(t *testing.T, name, modelName string) int {
	t.Helper()
	channel := model.Channel{ChannelConfig: model.ChannelConfig{
		Name: name, Enabled: true, BaseURL: "http://" + name + ".example",
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
	return grant.ID
}

// TestCreateGroupHandlerRejectsInvalidPattern 非法正则经 handler 映射为 400, 文案稳定。
func TestCreateGroupHandlerRejectsInvalidPattern(t *testing.T) {
	clearHandlerTables(t)
	rec := callCreateGroup(t, `{"name":"bad","mode":"scored","auto_add_pattern":"[unclosed"}`)
	assertBadRequest(t, rec)
	if !bytes.Contains(rec.Body.Bytes(), []byte("invalid regex pattern")) {
		t.Fatalf("错误文案应为 invalid regex pattern: %s", rec.Body.String())
	}
}

// TestCreateGroupHandlerRejectsOverlongPattern 超长 pattern 经 handler 映射为 400。
func TestCreateGroupHandlerRejectsOverlongPattern(t *testing.T) {
	clearHandlerTables(t)
	long := strings.Repeat("a", model.MaxPatternBytes+1)
	body, _ := json.Marshal(map[string]string{"name": "long", "auto_add_pattern": long})
	rec := callCreateGroup(t, string(body))
	assertBadRequest(t, rec)
	if !bytes.Contains(rec.Body.Bytes(), []byte("pattern exceeds maximum length")) {
		t.Fatalf("错误文案应为 pattern exceeds maximum length: %s", rec.Body.String())
	}
}

// TestUpdateGroupHandlerRejectsInvalidPattern 更新入口同样映射非法 pattern 为 400。
func TestUpdateGroupHandlerRejectsInvalidPattern(t *testing.T) {
	clearHandlerTables(t)
	group, err := op.GroupCreate(&model.GroupCreateRequest{Name: "upd-bad", Mode: model.GroupModeScored}, context.Background())
	if err != nil {
		t.Fatalf("建分组失败: %v", err)
	}
	rec := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(rec)
	ginContext.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`{"auto_add_pattern":"[unclosed"}`)))
	ginContext.Request.Header.Set("Content-Type", "application/json")
	ginContext.Params = gin.Params{{Key: "id", Value: strconv.Itoa(group.ID)}}
	updateGroup(ginContext)
	assertBadRequest(t, rec)
}

// TestCreateGroupHandlerWithRuleAutoSupplements 经真实 createGroup handler 创建带规则的分组,
// 响应信封含 auto_add_pattern, 且匹配授权被自动补入成员。
func TestCreateGroupHandlerWithRuleAutoSupplements(t *testing.T) {
	clearHandlerTables(t)
	grantID := seedHandlerChannel(t, "hc", "gpt-4o")

	rec := callCreateGroup(t, `{"name":"rule-handler","mode":"scored","auto_add_pattern":"^gpt-4o$"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("响应码 = %d, 想要 200: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Code int `json:"code"`
		Data struct {
			ID             int               `json:"id"`
			AutoAddPattern string            `json:"auto_add_pattern"`
			Items          []model.GroupItem `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v: %s", err, rec.Body.String())
	}
	if resp.Data.AutoAddPattern != "^gpt-4o$" {
		t.Fatalf("响应未带 auto_add_pattern: %q", resp.Data.AutoAddPattern)
	}
	if len(resp.Data.Items) != 1 || resp.Data.Items[0].ChannelGrantID != grantID {
		t.Fatalf("规则应自动补入匹配授权 %d: %+v", grantID, resp.Data.Items)
	}
}

// clearHandlerTables 清空六张主表, 与 group_test.go 的清理同口径。
func clearHandlerTables(t *testing.T) {
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
