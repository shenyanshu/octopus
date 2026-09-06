package model

type LLMPrice struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// LLMSource 标识模型价格的来源: manual 为用户显式设定, auto 为系统自动生成(参考目录派生或零占位)。
type LLMSource string

const (
	LLMSourceManual LLMSource = "manual" // 用户在界面或 API 显式创建/更新的模型价格。
	LLMSourceAuto   LLMSource = "auto"   // 系统自动生成的模型价格, 四价为参考目录派生值或零占位。
)

type LLMInfo struct {
	Name   string    `json:"name" gorm:"primaryKey;not null"`
	Source LLMSource `json:"source" gorm:"type:varchar(10);not null;default:'auto'"` // 价格来源, auto 记录的四价仅为占位, 实际价格由 API 动态派生。
	LLMPrice
}

type OpenAIModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int    `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type OpenAIModelList struct {
	Object string        `json:"object"`
	Data   []OpenAIModel `json:"data"`
}
type AnthropicModel struct {
	ID          string `json:"id"`
	CreatedAt   string `json:"created_at"`
	DisplayName string `json:"display_name"`
	Type        string `json:"type"`
}

type AnthropicModelList struct {
	Data    []AnthropicModel `json:"data"`
	FirstID string           `json:"first_id"`
	HasMore bool             `json:"has_more"`
	LastID  string           `json:"last_id"`
}
