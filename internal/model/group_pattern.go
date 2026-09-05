package model

import (
	"errors"
	"regexp"
)

// MaxPatternBytes 是分组自动补充规则与授权预览共用的 pattern 字节上限。
// 上限按字节而非字符计: 足够覆盖真实上游模型名, 同时挡掉恶意巨长输入拖垮正则编译。
// 放在 model 而非 handler/op: 两个消费方共用同一规则, 谁也不依赖谁。
const MaxPatternBytes = 1024

// 规则非法的稳定错误文案: 不透传 regexp 的原始错误,
// 避免向前端泄漏实现细节与 Go 版本相关差异; 预览接口的历史文案与此保持一致。
var (
	ErrPatternTooLong = errors.New("pattern exceeds maximum length")
	ErrPatternInvalid = errors.New("invalid regex pattern")
)

// CompileGroupPattern 校验并编译规则表达式, 是 pattern 的单一判定入口:
// 分组创建/编辑、逻辑导入与授权预览都经由这里, 不各自另写一套长度与合法性口径。
// 空串是合法输入表示关闭规则, 返回 nil 正则; 默认大小写敏感, (?i) 由调用方显式声明;
// 不做 trim 与小写化: 表达式是用户意图本身, 收到什么就编译什么。
// 匹配语义与标准库 regexp(RE2) 一致: 不支持回溯与 lookaround, 编译失败即非法。
func CompileGroupPattern(pattern string) (*regexp.Regexp, error) {
	if pattern == "" {
		return nil, nil
	}
	if len(pattern) > MaxPatternBytes {
		return nil, ErrPatternTooLong
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, ErrPatternInvalid
	}
	return re, nil
}
