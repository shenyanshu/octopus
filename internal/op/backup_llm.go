package op

import (
	"fmt"

	"github.com/bestruirui/octopus/internal/model"
)

// splitLLMInfosForImport 将导入的 LLMInfos 按 source 分流为 manual 和 auto 两组。
// 旧 dump 缺 source 字段时 JSON 解码为零值 "", 视为 auto 并清零四价。
// 非法 source 值拒绝导入。auto 不覆盖目标 manual(manual 优先), manual 遵循显式覆盖。
func splitLLMInfosForImport(infos []model.LLMInfo) (manual, auto []model.LLMInfo, err error) {
	for _, info := range infos {
		switch model.LLMSource(info.Source) {
		case model.LLMSourceManual:
			manual = append(manual, info)
		case model.LLMSourceAuto, "":
			// 旧 dump 缺 source 视为 auto, 清零四价为零占位。
			info.Source = model.LLMSourceAuto
			info.LLMPrice = model.LLMPrice{}
			auto = append(auto, info)
		default:
			return nil, nil, fmt.Errorf("illegal llm_info source %q for model %q", info.Source, info.Name)
		}
	}
	return manual, auto, nil
}
