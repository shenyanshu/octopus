package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/gin-gonic/gin"
)

func setupModelTestDB(t *testing.T) {
	t.Helper()
	if db.GetDB() == nil {
		t.Fatal("数据库未初始化")
	}
	for _, table := range []string{"llm_infos", "channel_models", "channels", "channel_keys", "channel_grants", "group_items", "groups"} {
		db.GetDB().Exec("DELETE FROM " + table)
	}
	// 刷新 LLM 缓存
	op.LLMRebuild(context.Background())
}

func doModelHandler(t *testing.T, handler gin.HandlerFunc, body any) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	var reqBody *bytes.Buffer
	if body != nil {
		data, _ := json.Marshal(body)
		reqBody = bytes.NewBuffer(data)
	} else {
		reqBody = bytes.NewBuffer(nil)
	}
	c.Request = httptest.NewRequest(http.MethodPost, "/", reqBody)
	c.Request.Header.Set("Content-Type", "application/json")
	handler(c)
	return w
}

func doModelHandlerGet(t *testing.T, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	handler(c)
	return w
}

func TestHandlerModelCreateMissingPrice400(t *testing.T) {
	setupModelTestDB(t)
	w := doModelHandler(t, createLLM, map[string]any{
		"name": "test-missing", "input": 10, "cache_read": 1, "cache_write": 5,
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("缺 output 应 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandlerModelCreateNullPrice400(t *testing.T) {
	setupModelTestDB(t)
	w := doModelHandler(t, createLLM, map[string]any{
		"name": "test-null", "input": nil, "output": 50, "cache_read": 1, "cache_write": 5,
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("null input 应 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandlerModelCreateNegativePrice400(t *testing.T) {
	setupModelTestDB(t)
	w := doModelHandler(t, createLLM, map[string]any{
		"name": "test-neg", "input": -1, "output": 50, "cache_read": 1, "cache_write": 5,
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("负数应 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandlerModelCreateValid(t *testing.T) {
	setupModelTestDB(t)
	w := doModelHandler(t, createLLM, map[string]any{
		"name": "valid-model", "input": 10, "output": 50, "cache_read": 1, "cache_write": 5,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("有效请求应 200, got %d: %s", w.Code, w.Body.String())
	}
	var info model.LLMInfo
	if err := db.GetDB().Where("name = ?", "valid-model").First(&info).Error; err != nil {
		t.Fatal(err)
	}
	if info.Source != model.LLMSourceManual {
		t.Fatalf("应为 manual, got %s", info.Source)
	}
}

func TestHandlerModelListNewFormat(t *testing.T) {
	setupModelTestDB(t)
	op.LLMCreate(model.LLMInfo{
		Name: "list-manual", LLMPrice: model.LLMPrice{Input: 10, Output: 50, CacheRead: 1, CacheWrite: 5},
	}, context.Background())
	db.GetDB().Create(&model.LLMInfo{Name: "list-auto", Source: model.LLMSourceAuto})
	op.LLMRebuild(context.Background())

	w := doModelHandlerGet(t, listLLM)
	if w.Code != http.StatusOK {
		t.Fatalf("应 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Data) == 0 {
		t.Fatal("应有记录")
	}
	for _, item := range resp.Data {
		if _, ok := item["name"]; !ok {
			t.Fatal("应有 name 字段")
		}
		if _, ok := item["source"]; !ok {
			t.Fatal("应有 source 字段")
		}
		if _, ok := item["price_known"]; !ok {
			t.Fatal("应有 price_known 字段")
		}
		if _, ok := item["price"]; !ok {
			t.Fatal("应有 price 字段")
		}
	}
	foundManual := false
	for _, item := range resp.Data {
		if item["name"] == "list-manual" {
			foundManual = true
			if item["source"] != "manual" {
				t.Fatalf("应为 manual, got %v", item["source"])
			}
			if item["price_known"] != true {
				t.Fatal("manual 应 price_known=true")
			}
		}
	}
	if !foundManual {
		t.Fatal("应找到 list-manual")
	}
}

func TestHandlerModelRestoreAuto(t *testing.T) {
	setupModelTestDB(t)
	op.LLMCreate(model.LLMInfo{
		Name: "restore-me", LLMPrice: model.LLMPrice{Input: 10, Output: 50, CacheRead: 1, CacheWrite: 5},
	}, context.Background())
	w := doModelHandler(t, restoreAutoLLM, map[string]any{
		"name": "restore-me",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("应 200, got %d: %s", w.Code, w.Body.String())
	}
	var info model.LLMInfo
	if err := db.GetDB().Where("name = ?", "restore-me").First(&info).Error; err != nil {
		t.Fatal(err)
	}
	if info.Source != model.LLMSourceAuto {
		t.Fatalf("恢复后应为 auto, got %s", info.Source)
	}
}

func TestHandlerModelRestoreAutoMissing404(t *testing.T) {
	setupModelTestDB(t)
	w := doModelHandler(t, restoreAutoLLM, map[string]any{
		"name": "nonexistent",
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("不存在应 404, got %d: %s", w.Code, w.Body.String())
	}
}
