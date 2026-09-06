package catalog

import (
	"strings"
	"sync"

	"github.com/bestruirui/octopus/internal/model"
)

// Match 是参考目录匹配结果, 携带匹配到的标准模型 ID 与价格。
type Match struct {
	ModelID string         // 参考目录中的标准模型 ID。
	Price   model.LLMPrice // 匹配到的价格。
}

// refLock 保护 refPrices; Replace 写、Lookup 读, 保证并发安全。
var refLock sync.RWMutex

// refPrices 当前生效的参考价格目录, 初始为预置值的独立快照。
// init 复制 presetPrices, 使 Replace 不影响 presetPrices 原始快照。
var refPrices map[string]model.LLMPrice

func init() {
	refPrices = cloneNormalized(presetPrices)
}

// Lookup 按模型名在参考目录中查找价格, 返回匹配的标准模型 ID 与价格。
// 统一 lower/trim, 精确优先, 再用前缀/后缀分段匹配, 歧义时返回 false。
func Lookup(modelName string) (Match, bool) {
	refLock.RLock()
	defer refLock.RUnlock()
	return lookup(refPrices, modelName)
}

// Replace 以新价格目录原子替换当前参考目录。
// 复制入参避免调用方后续写入引发 race; key 统一 lower/trim 保持与 Lookup 一致。
func Replace(prices map[string]model.LLMPrice) {
	clone := cloneNormalized(prices)
	refLock.Lock()
	refPrices = clone
	refLock.Unlock()
}

// cloneNormalized 复制并规范化所有 key(lower+trim), 返回独立快照。
func cloneNormalized(src map[string]model.LLMPrice) map[string]model.LLMPrice {
	dst := make(map[string]model.LLMPrice, len(src))
	for k, v := range src {
		dst[normalize(k)] = v
	}
	return dst
}

// normalize 统一模型名为小写并去除首尾空白, 保持目录 key 与 Lookup 查询一致。
func normalize(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// lookup 在指定目录中执行匹配, 不持锁, 供持锁的 Lookup 与 init 复用。
// 将非字母、数字和小数点字符作为边界, 使标准模型名可出现在任意前缀或后缀之间;
// 优先选择分段最多的具体模型, 歧义(同分段数但不同价)时返回 false。
func lookup(refs map[string]model.LLMPrice, modelName string) (Match, bool) {
	name := normalize(modelName)
	if price, ok := refs[name]; ok {
		return Match{ModelID: name, Price: price}, true
	}
	nameSegments := strings.FieldsFunc(name, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '.'
	})

	matchedID := ""
	matchedSegCount := 0
	ambiguous := false
	for modelID, price := range refs {
		idSegments := strings.Split(modelID, "-")
		for start := 0; start+len(idSegments) <= len(nameSegments); start++ {
			matched := true
			for i, seg := range idSegments {
				if nameSegments[start+i] != seg {
					matched = false
					break
				}
			}
			if !matched {
				continue
			}
			if len(idSegments) > matchedSegCount {
				matchedID = modelID
				matchedSegCount = len(idSegments)
				ambiguous = false
			} else if len(idSegments) == matchedSegCount && refs[matchedID] != price {
				ambiguous = true
			}
			break
		}
	}
	if matchedID == "" || ambiguous {
		return Match{}, false
	}
	return Match{ModelID: matchedID, Price: refs[matchedID]}, true
}
