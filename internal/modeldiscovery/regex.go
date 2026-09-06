package modeldiscovery

import "github.com/dlclark/regexp2"

// compileMatchRegex 编译 matchRegex 为 regexp2 引擎(ECMAScript 语法)。
// 空串返回 nil 表示不过滤; 编译失败返回 *syntax.Error, 供上层按 400 区分非法模式。
// 编译在发起任何网络请求之前完成, 非法模式直接失败, 不会产生上游副作用。
func compileMatchRegex(pattern string) (*regexp2.Regexp, error) {
	if pattern == "" {
		return nil, nil
	}
	re, err := regexp2.Compile(pattern, regexp2.ECMAScript)
	if err != nil {
		return nil, err
	}
	// 给编译好的引擎单独设一个匹配超时, 防止灾难性回溯在单条模型名上无限耗时;
	// regexp2 仅暴露字段而非构造参数, 这里按官方支持的 MatchTimeout 字段赋值。
	re.MatchTimeout = regexMatchTimeout
	return re, nil
}
