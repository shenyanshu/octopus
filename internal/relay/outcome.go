package relay

import (
	"errors"
)

// roundOutcome 一轮成员尝试失败的归因分类, 决定评分与渠道统计的记账方式。
// 只服务于评分模式的记账决策, 手动与故障转移模式沿用既有路径, 不经过本分类。
type roundOutcome int

const (
	roundOutcomeMemberLocal     roundOutcome = iota // 成员本地配置不可用(端点, 代理, 参数覆盖, 协议构造): 请求内排除并换下一个, 不扣分不写渠道统计。
	roundOutcomeRequestInvalid                      // 客户端请求级错误: 任何成员都无法成功, 不扣分并终止请求。
	roundOutcomeUpstreamFailure                     // 真实上游普通故障: 扣分并换下一个。
	roundOutcomeUpstreamAuth                        // 真实上游鉴权失败: 归零并换下一个。
)

// memberLocalError 标记网络边界之前的成员本地配置错误。
// 这类错误不归因上游: 不写渠道失败统计, 评分模式只在请求内排除该成员。
type memberLocalError struct{ err error }

func (e *memberLocalError) Error() string { return e.err.Error() }
func (e *memberLocalError) Unwrap() error { return e.err }

// markMemberLocal 包装网络边界之前发生的成员本地错误, 供 classifyRoundFailure 识别。
func markMemberLocal(err error) error {
	if err == nil {
		return nil
	}
	var marked *memberLocalError
	if errors.As(err, &marked) {
		return err
	}
	return &memberLocalError{err: err}
}

// requestInvalidError 标记客户端请求本身的错误: 换任何成员都不会成功, 应直接终止请求。
type requestInvalidError struct{ err error }

func (e *requestInvalidError) Error() string { return e.err.Error() }
func (e *requestInvalidError) Unwrap() error { return e.err }

// markRequestInvalid 包装客户端请求级错误, 供 classifyRoundFailure 识别。
func markRequestInvalid(err error) error {
	if err == nil {
		return nil
	}
	var marked *requestInvalidError
	if errors.As(err, &marked) {
		return err
	}
	return &requestInvalidError{err: err}
}

// classifyRoundFailure 按错误链上的结构化标记归因一轮失败, 不做任何错误文本匹配。
// 两条发送路径各自保证: 网络边界之前的错误都带 memberLocalError 标记,
// 请求级错误带 requestInvalidError 标记, 其余一律来自网络边界之后。
func classifyRoundFailure(err error) roundOutcome {
	var requestErr *requestInvalidError
	if errors.As(err, &requestErr) {
		return roundOutcomeRequestInvalid
	}
	var localErr *memberLocalError
	if errors.As(err, &localErr) {
		return roundOutcomeMemberLocal
	}
	if authFailure(err) {
		return roundOutcomeUpstreamAuth
	}
	return roundOutcomeUpstreamFailure
}
