package relay

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
)

func TestClassifyRoundFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want roundOutcome
	}{
		// 请求级: 客户端请求无法被任何成员服务。
		{"请求级标记", markRequestInvalid(errors.New("bad payload")), roundOutcomeRequestInvalid},
		{"包装链中的请求级标记", fmt.Errorf("round: %w", markRequestInvalid(errors.New("bad payload"))), roundOutcomeRequestInvalid},

		// 成员本地: 网络边界之前的配置错误。
		{"成员本地标记", markMemberLocal(errors.New("resolve upstream endpoint")), roundOutcomeMemberLocal},
		{"包装链中的成员本地标记", fmt.Errorf("round: %w", markMemberLocal(errors.New("invalid proxy"))), roundOutcomeMemberLocal},

		// 上游: 透传路径的网络与响应错误。
		{"透传网络错误", errors.New("connection refused"), roundOutcomeUpstreamFailure},
		{"透传 500", &httpclient.Error{StatusCode: http.StatusInternalServerError}, roundOutcomeUpstreamFailure},
		{"透传 401", &httpclient.Error{StatusCode: http.StatusUnauthorized}, roundOutcomeUpstreamAuth},
		{"透传 403", fmt.Errorf("%w: body", &httpclient.Error{StatusCode: http.StatusForbidden}), roundOutcomeUpstreamAuth},

		// 上游: 转换路径由 pipeline 的 UpstreamError 标记表达。
		{"转换路径上游故障", pipeline.WrapUpstreamError(errors.New("failed to do request: connection reset")), roundOutcomeUpstreamFailure},
		{"转换路径上游 401", pipeline.WrapUpstreamError(fmt.Errorf("transformed: %w", &llm.ResponseError{StatusCode: http.StatusUnauthorized})), roundOutcomeUpstreamAuth},
		{"包装后的转换路径上游 403", fmt.Errorf("round: %w", pipeline.WrapUpstreamError(&llm.ResponseError{StatusCode: http.StatusForbidden})), roundOutcomeUpstreamAuth},
		{"转换路径超时", errors.New("upstream non-stream response timeout"), roundOutcomeUpstreamFailure},
	}
	for _, tc := range cases {
		if got := classifyRoundFailure(tc.err); got != tc.want {
			t.Errorf("%s: classify = %v, 想要 %v", tc.name, got, tc.want)
		}
	}
}

// 成员本地标记必须优先于链上可能同时存在的上游状态码, 否则本地构造失败会被误记为上游故障。
func TestClassifyRoundFailureLocalMarkBeatsStatusCode(t *testing.T) {
	err := markMemberLocal(fmt.Errorf("build: %w", &httpclient.Error{StatusCode: http.StatusUnauthorized}))
	if got := classifyRoundFailure(err); got != roundOutcomeMemberLocal {
		t.Fatalf("classify = %v, 想要 %v", got, roundOutcomeMemberLocal)
	}
}

// 错误正文读取失败的透传 401 仍保留状态码在错误链上, 鉴权归因不受正文丢失影响。
func TestAuthFailureSurvivesBodyReadError(t *testing.T) {
	appErr := &httpclient.Error{Method: "POST", URL: "http://upstream", StatusCode: http.StatusUnauthorized, Status: "401 Unauthorized"}
	err := fmt.Errorf("%w: %w", appErr, errors.New("unexpected EOF"))
	if got := classifyRoundFailure(err); got != roundOutcomeUpstreamAuth {
		t.Fatalf("classify = %v, 想要 %v", got, roundOutcomeUpstreamAuth)
	}
}
