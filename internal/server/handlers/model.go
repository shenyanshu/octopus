package handlers

import (
	"fmt"
	"math"
	"net/http"
	"strings"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/price"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/server/router"
	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
)

func init() {
	router.NewGroupRouter("/api/v1/model").
		Use(middleware.Auth()).
		Use(middleware.RequireJSON()).
		AddRoute(
			router.NewRoute("/list", http.MethodGet).
				Handle(listLLM),
		).
		AddRoute(
			router.NewRoute("/create", http.MethodPost).
				Handle(createLLM),
		).
		AddRoute(
			router.NewRoute("/update", http.MethodPost).
				Handle(updateLLM),
		).
		AddRoute(
			router.NewRoute("/delete", http.MethodPost).
				Handle(deleteLLM),
		).
		AddRoute(
			router.NewRoute("/restore-auto", http.MethodPost).
				Handle(restoreAutoLLM),
		).
		AddRoute(
			router.NewRoute("/update-price", http.MethodPost).
				Handle(updateLLMPrice),
		).
		AddRoute(
			router.NewRoute("/rebuild-price", http.MethodPost).
				Handle(rebuildLLMPrice),
		).
		AddRoute(
			router.NewRoute("/last-update-time", http.MethodGet).
				Handle(getLastUpdateTime),
		)
	router.NewGroupRouter("/v1").
		Use(middleware.APIKeyAuth()).
		AddRoute(
			router.NewRoute("/models", http.MethodGet).
				Handle(getModelList),
		)
}

func getModelList(c *gin.Context) {
	models := op.GroupListModel()
	if allowed, ok := c.Get("supported_models"); ok {
		if names, _ := allowed.([]string); len(names) > 0 {
			models = lo.Filter(models, func(m string, _ int) bool {
				return lo.Contains(names, m)
			})
		}
	}

	if c.GetHeader("x-api-key") != "" {
		var anthropicModels []model.AnthropicModel
		for _, m := range models {
			anthropicModels = append(anthropicModels, model.AnthropicModel{
				ID:          m,
				CreatedAt:   "2024-01-01T00:00:00Z",
				DisplayName: m,
				Type:        "model",
			})
		}
		response := gin.H{
			"data":     anthropicModels,
			"has_more": false,
		}
		if len(anthropicModels) > 0 {
			response["first_id"] = anthropicModels[0].ID
			response["last_id"] = anthropicModels[len(anthropicModels)-1].ID
		}
		c.JSON(200, response)
	} else {
		var openAIModels []model.OpenAIModel
		for _, m := range models {
			openAIModels = append(openAIModels, model.OpenAIModel{
				ID:      m,
				Object:  "model",
				Created: 1763395200,
				OwnedBy: "octopus",
			})
		}
		c.JSON(200, gin.H{
			"success": true,
			"data":    openAIModels,
			"object":  "list",
		})
	}
}

// llmListItem 是 GET /api/v1/model/list 的每项格式, 替换旧 flat 响应。
// source=manual 时 price 为存储四价; source=auto 时 price 为参考目录派生价格或 null(unknown)。
type llmListItem struct {
	Name       string          `json:"name"`
	Source     string          `json:"source"`
	PriceKnown bool            `json:"price_known"`
	Price      *model.LLMPrice `json:"price"`
}

func listLLM(c *gin.Context) {
	infos := op.LLMList()
	items := make([]llmListItem, 0, len(infos))
	for _, info := range infos {
		p, known := op.LLMDerivePrice(info.Name)
		item := llmListItem{
			Name:       info.Name,
			Source:     string(info.Source),
			PriceKnown: known,
		}
		if known {
			item.Price = &p
		}
		items = append(items, item)
	}
	resp.Success(c, items)
}

// llmPriceRequest 是 create/update 的 flat 请求格式, 四价必须存在且 >=0。
// 不接收客户端 source/known 设定, 服务端强制 manual。
type llmPriceRequest struct {
	Name       string   `json:"name"`
	Input      *float64 `json:"input"`
	Output     *float64 `json:"output"`
	CacheRead  *float64 `json:"cache_read"`
	CacheWrite *float64 `json:"cache_write"`
}

// validateLLMPriceRequest 校验四价必须存在、有限且 >=0。
func validateLLMPriceRequest(req *llmPriceRequest) error {
	name := strings.ToLower(strings.TrimSpace(req.Name))
	if name == "" {
		return fmt.Errorf("name is required")
	}
	for _, field := range []struct {
		name string
		val  *float64
	}{
		{"input", req.Input},
		{"output", req.Output},
		{"cache_read", req.CacheRead},
		{"cache_write", req.CacheWrite},
	} {
		if field.val == nil {
			return fmt.Errorf("%s is required", field.name)
		}
		if math.IsNaN(*field.val) || math.IsInf(*field.val, 0) {
			return fmt.Errorf("%s must be finite", field.name)
		}
		if *field.val < 0 {
			return fmt.Errorf("%s must be >= 0", field.name)
		}
	}
	return nil
}

// createLLM 校验并创建自定义模型价格。
func createLLM(c *gin.Context) {
	var req llmPriceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateLLMPriceRequest(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	info := model.LLMInfo{
		Name: strings.ToLower(strings.TrimSpace(req.Name)),
		LLMPrice: model.LLMPrice{
			Input: *req.Input, Output: *req.Output,
			CacheRead: *req.CacheRead, CacheWrite: *req.CacheWrite,
		},
	}
	if err := op.LLMCreate(info, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, info)
}

// updateLLM 校验并更新自定义模型价格。
func updateLLM(c *gin.Context) {
	var req llmPriceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateLLMPriceRequest(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	info := model.LLMInfo{
		Name: strings.ToLower(strings.TrimSpace(req.Name)),
		LLMPrice: model.LLMPrice{
			Input: *req.Input, Output: *req.Output,
			CacheRead: *req.CacheRead, CacheWrite: *req.CacheWrite,
		},
	}
	if err := op.LLMUpdate(info, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, info)
}

// restoreAutoLLM 将指定模型从 manual 恢复为 auto, 返回恢复后的列表项。
func restoreAutoLLM(c *gin.Context) {
	var req struct {
		Name string `json:"name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.ToLower(strings.TrimSpace(req.Name))
	if name == "" {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
		return
	}
	info, err := op.LLMRestoreAuto(name, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusNotFound, err.Error())
		return
	}
	p, known := op.LLMDerivePrice(info.Name)
	item := llmListItem{
		Name:       info.Name,
		Source:     string(info.Source),
		PriceKnown: known,
	}
	if known {
		item.Price = &p
	}
	resp.Success(c, item)
}

// deleteLLM 校验模型名并删除自定义模型价格。
func deleteLLM(c *gin.Context) {
	var req struct {
		Name string `json:"name" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	req.Name = strings.ToLower(strings.TrimSpace(req.Name))
	if req.Name == "" {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
		return
	}
	if err := op.LLMDelete(req.Name, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, nil)
}

func updateLLMPrice(c *gin.Context) {
	err := price.UpdateLLMPrice(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, nil)
}

// rebuildLLMPrice 补齐渠道模型缺的价格记录, 清理无引用的 auto 记录, 保留所有 manual。
func rebuildLLMPrice(c *gin.Context) {
	ctx := c.Request.Context()
	if err := op.LLMRebuild(ctx); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, gin.H{"count": op.LLMListCount()})
}

func getLastUpdateTime(c *gin.Context) {
	time := price.GetLastUpdateTime()
	resp.Success(c, time)
}
