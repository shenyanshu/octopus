package op

import (
	"context"
	"fmt"
	"strings"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/price/catalog"
)

// RegisterLookupReference 已删除: 生产直接使用 catalog.Lookup, 不再全局注入。

// PriceResolution 是一次用量费用解析的完整结果, 供日志记账统一携带。
// cost_known=false 时费用为 null, 日志展示 unknown; running(无 usage) 同样 null。
type PriceResolution struct {
	CostKnown          bool           // 是否有可靠价格来源。
	CostSource         string         // "manual" | "actual_reference" | "group_reference" | "unknown"
	CostReferenceModel *string        // actual/group 参考匹配到的标准模型 ID; manual/unknown 为 nil。
	Price              model.LLMPrice // 解析出的单价, 用于费用计算。
}

// ResolveUsagePrice 是费用记账的单入口: 接收 actual(上游实际模型名)与 group(客户端请求的分组名),
// 按 manual → actual_reference → group_reference → unknown 顺序解析模型价格。
// manual: 缓存中 source=manual 的记录, 四价即最终价格(含显式零价), cost_known=true。
// actual_reference: 参考目录按上游实际模型名匹配, 无论缓存有无记录; cost_known=true。
// group_reference: actual 未匹配但分组名匹配参考目录, cost_known=true。
// unknown: 无任何匹配, cost_known=false, 四价零占位。
func ResolveUsagePrice(actualModel, groupModel string) PriceResolution {
	return resolveModelPrice(actualModel, groupModel, catalog.Lookup)
}

// resolveModelPrice 是纯解析函数, 接收显式 lookup 参数供测试注入可控目录。
// 生产唯一入口 ResolveUsagePrice 固定使用 catalog.Lookup。
func resolveModelPrice(actualModel, groupModel string, lookup func(string) (catalog.Match, bool)) PriceResolution {
	key := strings.ToLower(strings.TrimSpace(actualModel))
	if entry, ok := llmModelCache.Get(key); ok && entry.Source == model.LLMSourceManual {
		// manual 四价即最终价格, 含显式零价: 用户明确设定 0 表示免费。
		return PriceResolution{
			CostKnown:  true,
			CostSource: "manual",
			Price:      entry.LLMPrice,
		}
	}
	// actual_reference: 无论缓存有无记录, 优先按上游实际模型名匹配参考目录。
	if match, found := lookup(actualModel); found {
		return PriceResolution{
			CostKnown:          true,
			CostSource:         "actual_reference",
			CostReferenceModel: &match.ModelID,
			Price:              match.Price,
		}
	}
	// group_reference: actual 未匹配, 按分组名(客户端请求的模型名)匹配参考目录。
	if match, found := lookup(groupModel); found {
		return PriceResolution{
			CostKnown:          true,
			CostSource:         "group_reference",
			CostReferenceModel: &match.ModelID,
			Price:              match.Price,
		}
	}
	return PriceResolution{
		CostKnown:  false,
		CostSource: "unknown",
	}
}

// LLMRestoreAuto 将指定模型从 manual 恢复为 auto, 返回恢复后的列表项信息。
// 恢复后价格由参考目录动态派生, 无参考则为零占位。
func LLMRestoreAuto(modelName string, ctx context.Context) (model.LLMInfo, error) {
	key := strings.ToLower(strings.TrimSpace(modelName))
	entry, ok := llmModelCache.Get(key)
	if !ok {
		return model.LLMInfo{}, fmt.Errorf("model not found")
	}
	if entry.Source != model.LLMSourceManual {
		// 已是 auto, 无需恢复但仍返回当前状态。
		return model.LLMInfo{Name: key, Source: entry.Source, LLMPrice: entry.LLMPrice}, nil
	}
	// 更新 DB 为 auto, 四价清零占位。
	info := model.LLMInfo{Name: key, Source: model.LLMSourceAuto}
	if err := db.GetDB().WithContext(ctx).Model(&model.LLMInfo{}).Where("name = ?", key).
		Updates(map[string]any{"source": model.LLMSourceAuto, "input": 0, "output": 0, "cache_read": 0, "cache_write": 0}).Error; err != nil {
		return model.LLMInfo{}, err
	}
	llmModelCache.Set(key, llmCacheEntry{LLMPrice: model.LLMPrice{}, Source: model.LLMSourceAuto})
	return info, nil
}

// LLMDerivePrice 动态派生模型价格供 API 列表返回。
// manual 返回存储的四价(含零); auto 返回参考目录匹配价格或零占位, 同时返回 known 标记。
func LLMDerivePrice(modelName string) (model.LLMPrice, bool) {
	key := strings.ToLower(strings.TrimSpace(modelName))
	entry, ok := llmModelCache.Get(key)
	if !ok {
		return model.LLMPrice{}, false
	}
	if entry.Source == model.LLMSourceManual {
		return entry.LLMPrice, true
	}
	// auto: 参考目录派生。
	if match, found := catalog.Lookup(modelName); found {
		return match.Price, true
	}
	return model.LLMPrice{}, false
}
