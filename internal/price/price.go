package price

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/price/catalog"
	"github.com/bestruirui/octopus/internal/rhttp"
	"github.com/charmbracelet/log"
)

const llmPriceUrl = "https://models.dev/api.json"

// ErrEmptyPriceCatalog 表示过滤后参考目录为空, 拒绝覆盖已有有效目录。
var ErrEmptyPriceCatalog = fmt.Errorf("filtered price catalog is empty, keeping existing")

// developerFamilies 定义研发商及其自研模型系列前缀。
var developerFamilies = map[string][]string{
	"openai":     {"gpt", "o"},
	"anthropic":  {"claude"},
	"google":     {"gemini", "gemma", "lyria", "veo"},
	"deepseek":   {"deepseek"},
	"xai":        {"grok"},
	"alibaba":    {"qwen", "qvq"},
	"zhipuai":    {"glm"},
	"minimax":    {"minimax"},
	"moonshotai": {"kimi"},
	"v0":         {"v0"},
	"xiaomi":     {"mimo"},
}

// lastUpdateLock 保护 lastUpdateTime; 价格目录已移至 catalog, 时间戳仍属网络层。
var lastUpdateLock sync.RWMutex

// lastUpdateTime 最近一次成功更新时间。
var lastUpdateTime time.Time

// modelsDevResponse 是 models.dev/api.json 的顶层结构, key 为研发商名。
type modelsDevResponse map[string]modelsDevProvider

type modelsDevProvider struct {
	Models map[string]modelsDevModel `json:"models"`
}

// modelsDevModel 是 models.dev/api.json 中单个模型的解析结构。
// Cost 为指针: JSON 中 cost 缺失或为 null 时为 nil, 与显式零价区分(显式 0 合法免费)。
// Input/Output/CacheRead/CacheWrite 为指针: 缺失为 nil, 使 validate 可区分缺失与显式零。
type modelsDevModel struct {
	ID         string              `json:"id"`     // 模型标识。
	Family     string              `json:"family"` // 模型所属系列。
	Modalities modelsDevModalities `json:"modalities"`
	Cost       *rawLLMCost         `json:"cost"` // 模型价格; nil 表示缺失或 null。
}

type modelsDevModalities struct {
	Output []string `json:"output"` // 模型支持的输出类型。
}

// rawLLMCost 是 models.dev cost 字段的解析结构, 字段缺失时为 nil 以区分显式零价。
// validateCost 将缺失 input/output 视为无效(拒绝); 缺失 cache_read/cache_write 沿0语义。
type rawLLMCost struct {
	Input      *float64 `json:"input"`       // 必需: 缺失或无效则拒绝整个条目。
	Output     *float64 `json:"output"`      // 必需: 缺失或无效则拒绝整个条目。
	CacheRead  *float64 `json:"cache_read"`  // 可选: 缺失沿0语义; 存在则须有效。
	CacheWrite *float64 `json:"cache_write"` // 可选: 缺失沿0语义; 存在则须有效。
}

// UpdateLLMPrice 从 models.dev 更新自研文本输出模型的价格。
// 过滤后若参考目录为空, 返回 ErrEmptyPriceCatalog 且不覆盖已有目录、不更新时间戳。
func UpdateLLMPrice(ctx context.Context) error {
	log.Debugf("update LLM price task started")
	startTime := time.Now()
	defer func() {
		log.Debugf("update LLM price task finished, update time: %s", time.Since(startTime))
	}()
	body, err := fetchPriceBody(ctx)
	if err != nil {
		return err
	}
	var rawPrice modelsDevResponse
	if err := json.Unmarshal(body, &rawPrice); err != nil {
		return fmt.Errorf("failed to parse LLM info: %w", err)
	}
	updatedPrices := filterDeveloperPrices(rawPrice)
	if len(updatedPrices) == 0 {
		// 空/全部过滤为空: 拒绝覆盖已有有效目录, 保留旧时间戳。
		return ErrEmptyPriceCatalog
	}
	catalog.Replace(updatedPrices)
	lastUpdateLock.Lock()
	lastUpdateTime = time.Now()
	lastUpdateLock.Unlock()
	return nil
}

// fetchPriceBody 按 direct→proxy 顺序获取 models.dev 响应体, 直连失败时回退代理。
func fetchPriceBody(ctx context.Context) ([]byte, error) {
	var body []byte
	httpClient, err := rhttp.Direct()
	if err == nil {
		req, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, llmPriceUrl, nil)
		if requestErr != nil {
			return nil, requestErr
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
		resp, requestErr := httpClient.Do(req)
		if requestErr != nil {
			err = requestErr
		} else {
			if resp.StatusCode != http.StatusOK {
				err = fmt.Errorf("failed to fetch LLM info: %s", resp.Status)
			} else {
				body, err = io.ReadAll(resp.Body)
				if err != nil {
					err = fmt.Errorf("failed to read response body: %w", err)
				}
			}
			resp.Body.Close()
		}
	}
	if err != nil {
		log.Warnf("direct request failed, trying with proxy: %v", err)
		httpClient, err = rhttp.Proxy()
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, llmPriceUrl, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("failed to fetch LLM info: %s", resp.Status)
		}
		body, err = io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read response body: %w", err)
		}
	}
	return body, nil
}

// filterDeveloperPrices 从 models.dev 原始响应中提取自研文本输出模型的价格。
// 仅保留包含 text 输出的非嵌入模型, 且模型系列须属于该研发商的自研前缀;
// 价格须通过 validateCost 验证: 缺失 input/output 或无效(negative/non-finite)的条目被拒绝,
// 显式 input=0/output=0 为合法免费, 缺失 cache 沿 0 语义。
func filterDeveloperPrices(rawPrice modelsDevResponse) map[string]model.LLMPrice {
	updatedPrices := make(map[string]model.LLMPrice)
	for provider, familyPrefixes := range developerFamilies {
		for _, priceModel := range rawPrice[provider].Models {
			modelID := strings.ToLower(priceModel.ID)
			modelFamily := strings.ToLower(priceModel.Family)

			// 仅保留包含文本输出的非嵌入模型。
			if modelID == "" || !slices.Contains(priceModel.Modalities.Output, "text") || strings.Contains(modelID, "embed") || strings.Contains(modelFamily, "embed") {
				continue
			}

			// 云平台可能同时托管第三方模型, 仅接受该研发商的自研系列。
			isDeveloperModel := false
			for _, familyPrefix := range familyPrefixes {
				if strings.HasPrefix(modelFamily, familyPrefix) {
					isDeveloperModel = true
					break
				}
			}
			if !isDeveloperModel {
				continue
			}

			// 价格有效性: 缺失/无效 cost 整条拒绝, 不作为 known free 流入目录。
			price, ok := validateCost(priceModel.Cost)
			if !ok {
				continue
			}
			updatedPrices[modelID] = price
		}
	}
	return updatedPrices
}

// validateCost 验证 models.dev cost 字段并返回规范化的 LLMPrice。
// cost 缺失或 null → 拒绝(未知≠免费); input/output 缺失或无效 → 拒绝;
// 显式 input=0/output=0 → 合法免费; cache_read/cache_write 缺失 → 0, 存在则须有效;
// 所有值须 >=0 且有限, negative/NaN/Inf 拒绝。返回 (price, ok)。
func validateCost(cost *rawLLMCost) (model.LLMPrice, bool) {
	if cost == nil || cost.Input == nil || cost.Output == nil {
		// cost 缺失/null 或必需的 input/output 缺失: 拒绝, 不作为 known free。
		return model.LLMPrice{}, false
	}
	if !validPrice(*cost.Input) || !validPrice(*cost.Output) {
		return model.LLMPrice{}, false
	}
	price := model.LLMPrice{Input: *cost.Input, Output: *cost.Output}
	// cache 字段可选: 缺失沿 0 语义, 存在则须有效。
	if cost.CacheRead != nil {
		if !validPrice(*cost.CacheRead) {
			return model.LLMPrice{}, false
		}
		price.CacheRead = *cost.CacheRead
	}
	if cost.CacheWrite != nil {
		if !validPrice(*cost.CacheWrite) {
			return model.LLMPrice{}, false
		}
		price.CacheWrite = *cost.CacheWrite
	}
	return price, true
}

// validPrice 判定价格值有效: 非负且有限(非 NaN/Inf)。
func validPrice(v float64) bool {
	return v >= 0 && !math.IsNaN(v) && !math.IsInf(v, 0)
}

// GetLastUpdateTime 返回最近一次价格更新时间。
func GetLastUpdateTime() time.Time {
	lastUpdateLock.RLock()
	defer lastUpdateLock.RUnlock()
	return lastUpdateTime
}
