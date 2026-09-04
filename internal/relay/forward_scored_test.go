package relay

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
)

// 用例约定: fixture.members[0] 是配置顺序首位的 A, fixture.members[1] 是 B;
// 每个成员使用独立上游桩, 断言分数、渠道失败统计与命中数共同验证"谁跨越了网络边界、谁被记账"。

// passthroughGood 是透传链路的非流式成功响应。
func passthroughGood(w http.ResponseWriter, _ *http.Request) { openAICompletion(w, nil) }

// passthroughStreamGood 是透传链路的完整流式成功响应。
func passthroughStreamGood(w http.ResponseWriter, _ *http.Request) {
	serveSSE(w, []string{openAIStreamChunk}, true)
}

func TestForwardScoredSkipsUnavailableChannel(t *testing.T) {
	fixture := seedScoredGroup(t, model.GroupModeScored, model.ProtocolOpenAIChatCompletion,
		newUpstreamStub(t, passthroughGood), newUpstreamStub(t, passthroughGood))
	// A 渠道停用后不可用, 选路应直接落到 B, A 不产生任何网络请求。
	mutateChannelConfig(t, fixture, 0, "enabled", false)

	rec := postForward(newForwardRouter(), false, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("响应码 = %d, 想要 200, 响应体 %s", rec.Code, rec.Body.String())
	}
	if hits := fixture.members[0].upstream.hits.Load(); hits != 0 {
		t.Fatalf("不可用成员 A 收到 %d 次请求, 想要 0", hits)
	}
	if hits := fixture.members[1].upstream.hits.Load(); hits != 1 {
		t.Fatalf("成员 B 收到 %d 次请求, 想要 1", hits)
	}
	mustScore(t, fixture, 1, scoreMax)
}

func TestForwardScoredSkipsMemberWithMissingGrant(t *testing.T) {
	fixture := seedScoredGroup(t, model.GroupModeScored, model.ProtocolOpenAIChatCompletion,
		newUpstreamStub(t, passthroughGood), newUpstreamStub(t, passthroughGood))
	// A 的授权被删除后成员残缺, 选路应跳过 A。
	deleteGrantOfItem(t, fixture, 0)

	rec := postForward(newForwardRouter(), false, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("响应码 = %d, 想要 200, 响应体 %s", rec.Code, rec.Body.String())
	}
	if hits := fixture.members[0].upstream.hits.Load(); hits != 0 {
		t.Fatalf("残缺成员 A 收到 %d 次请求, 想要 0", hits)
	}
	mustScore(t, fixture, 1, scoreMax)
}

func TestForwardScoredAllMembersUnavailableWaits(t *testing.T) {
	fixture := seedScoredGroup(t, model.GroupModeScored, model.ProtocolOpenAIChatCompletion,
		newUpstreamStub(t, passthroughGood), newUpstreamStub(t, passthroughGood))
	mutateChannelConfig(t, fixture, 0, "enabled", false)
	mutateChannelConfig(t, fixture, 1, "enabled", false)

	// 全员本地不可用时沿用等待配置恢复的既有语义: 客户端取消即以取消终态返回, 不忙循环。
	router := newForwardRouter()
	ctx, cancel := context.WithCancel(context.Background())
	rec := httptest.NewRecorder()
	go router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(clientBody(false, false))).WithContext(ctx))
	time.Sleep(200 * time.Millisecond)
	cancel()
	waitLatestRequestStatus(t, StatusCanceled, 3*time.Second)
	if hits := fixture.members[0].upstream.hits.Load() + fixture.members[1].upstream.hits.Load(); hits != 0 {
		t.Fatalf("不可用成员收到 %d 次请求, 想要 0", hits)
	}
}

func TestForwardScoredExhaustionTerminates(t *testing.T) {
	internalError := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}
	fixture := seedScoredGroup(t, model.GroupModeScored, model.ProtocolOpenAIChatCompletion,
		newUpstreamStub(t, internalError), newUpstreamStub(t, internalError))

	rec := postForward(newForwardRouter(), false, false)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("响应码 = %d, 想要 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "all group members failed") {
		t.Fatalf("响应体未说明全员失败: %s", rec.Body.String())
	}
	if state := latestRequestState(); state.Status != StatusFailed {
		t.Fatalf("终态 = %s, 想要 failed", state.Status)
	}
	// 两个成员各真实失败一次: 扣分并写渠道失败统计。
	mustScore(t, fixture, 0, scoreInitial-scoreFailurePenalty)
	mustScore(t, fixture, 1, scoreInitial-scoreFailurePenalty)
	if got := channelFailedStats(t, fixture.members[0].channelID); got != 1 {
		t.Fatalf("A 渠道失败统计 = %d, 想要 1", got)
	}
	if got := channelFailedStats(t, fixture.members[1].channelID); got != 1 {
		t.Fatalf("B 渠道失败统计 = %d, 想要 1", got)
	}
	if hits := fixture.members[0].upstream.hits.Load() + fixture.members[1].upstream.hits.Load(); hits != 2 {
		t.Fatalf("上游命中 = %d, 想要各一次共 2", hits)
	}
}

func TestForwardScoredBuildOutboundLocalErrorExcluded(t *testing.T) {
	fixture := seedScoredGroup(t, model.GroupModeScored, model.ProtocolOpenAIChatCompletion,
		newUpstreamStub(t, passthroughGood), newUpstreamStub(t, passthroughGood))
	// A 的授权不支持任何已知协议: 协议构造在网络边界之前失败, 应排除换 B, 不扣分不写统计。
	mutateGrantProtocols(t, fixture, 0, 0)

	rec := postForward(newForwardRouter(), false, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("响应码 = %d, 想要 200, 响应体 %s", rec.Code, rec.Body.String())
	}
	mustScore(t, fixture, 0, scoreInitial)
	mustScore(t, fixture, 1, scoreMax)
	if got := channelFailedStats(t, fixture.members[0].channelID); got != 0 {
		t.Fatalf("本地错误写了渠道失败统计 = %d, 想要 0", got)
	}
	if hits := fixture.members[0].upstream.hits.Load(); hits != 0 {
		t.Fatalf("成员 A 收到 %d 次请求, 想要 0", hits)
	}
}

func TestForwardScoredParamOverrideLocalErrorExcluded(t *testing.T) {
	fixture := seedScoredGroup(t, model.GroupModeScored, model.ProtocolOpenAIChatCompletion,
		newUpstreamStub(t, passthroughGood), newUpstreamStub(t, passthroughGood))
	// A 的请求参数覆盖配置非法: 本地请求构造失败, 同样不扣分不写统计。
	mutateChannelConfig(t, fixture, 0, "param_override", "{invalid")

	rec := postForward(newForwardRouter(), false, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("响应码 = %d, 想要 200, 响应体 %s", rec.Code, rec.Body.String())
	}
	mustScore(t, fixture, 0, scoreInitial)
	mustScore(t, fixture, 1, scoreMax)
	if got := channelFailedStats(t, fixture.members[0].channelID); got != 0 {
		t.Fatalf("本地错误写了渠道失败统计 = %d, 想要 0", got)
	}
	if hits := fixture.members[0].upstream.hits.Load(); hits != 0 {
		t.Fatalf("成员 A 收到 %d 次请求, 想要 0", hits)
	}
}

func TestForwardScoredRequestInvalidTerminatesWithoutPollingMembers(t *testing.T) {
	fixture := seedScoredGroup(t, model.GroupModeScored, model.ProtocolAnthropicMessage,
		newUpstreamStub(t, anthropicMessage), newUpstreamStub(t, anthropicMessage))

	// 客户端请求形状无法被转换: 属请求级错误, 应直接终止, 不在成员间轮询。
	rec := postForward(newForwardRouter(), false, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("响应码 = %d, 想要 400, 响应体 %s", rec.Code, rec.Body.String())
	}
	if state := latestRequestState(); state.Status != StatusFailed {
		t.Fatalf("终态 = %s, 想要 failed", state.Status)
	}
	mustScore(t, fixture, 0, scoreInitial)
	mustScore(t, fixture, 1, scoreInitial)
	if got := channelFailedStats(t, fixture.members[0].channelID) + channelFailedStats(t, fixture.members[1].channelID); got != 0 {
		t.Fatalf("请求级错误写了渠道失败统计 = %d, 想要 0", got)
	}
	if hits := fixture.members[0].upstream.hits.Load() + fixture.members[1].upstream.hits.Load(); hits != 0 {
		t.Fatalf("上游命中 = %d, 想要 0", hits)
	}
}

// 鉴权失败矩阵: 两条协议链路 x 流式与非流式, 401/403 一律归零并换下一成员恢复请求。
func TestForwardScoredAuthFailureMatrix(t *testing.T) {
	unauthorized := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}
	forbidden := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"forbidden"}}`))
	}
	cases := []struct {
		name      string
		protocols model.Protocol
		stream    bool
		fail      http.HandlerFunc
	}{
		{"透传非流式 401", model.ProtocolOpenAIChatCompletion, false, unauthorized},
		{"透传流式 401", model.ProtocolOpenAIChatCompletion, true, unauthorized},
		{"透传非流式 403", model.ProtocolOpenAIChatCompletion, false, forbidden},
		{"转换非流式 401", model.ProtocolAnthropicMessage, false, unauthorized},
		{"转换流式 401", model.ProtocolAnthropicMessage, true, unauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			good := func(w http.ResponseWriter, r *http.Request) {
				if tc.stream {
					if tc.protocols == model.ProtocolAnthropicMessage {
						serveAnthropicStream(w)
					} else {
						passthroughStreamGood(w, r)
					}
					return
				}
				if tc.protocols == model.ProtocolAnthropicMessage {
					anthropicMessage(w, r)
				} else {
					openAICompletion(w, r)
				}
			}
			fixture := seedScoredGroup(t, model.GroupModeScored, tc.protocols,
				newUpstreamStub(t, tc.fail), newUpstreamStub(t, good))

			rec := postForward(newForwardRouter(), tc.stream, false)
			if rec.Code != http.StatusOK {
				t.Fatalf("响应码 = %d, 想要 200, 响应体 %s", rec.Code, rec.Body.String())
			}
			mustScore(t, fixture, 0, scoreFloor)
			mustScore(t, fixture, 1, scoreMax)
		})
	}
}

// 透传流式 401 且错误正文被截断: 状态码仍在错误链上, 鉴权归因不受正文读取失败影响。
func TestForwardScoredAuthFailureSurvivesTruncatedErrorBody(t *testing.T) {
	truncated := newUpstreamStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"nope"`))
		// 先把头与残缺正文推上网络再掐断连接, 模拟上游在错误正文中途断开。
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	})
	fixture := seedScoredGroup(t, model.GroupModeScored, model.ProtocolOpenAIChatCompletion,
		truncated, newUpstreamStub(t, passthroughStreamGood))

	rec := postForward(newForwardRouter(), true, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("响应码 = %d, 想要 200, 响应体 %s", rec.Code, rec.Body.String())
	}
	mustScore(t, fixture, 0, scoreFloor)
	mustScore(t, fixture, 1, scoreMax)
}

// 普通上游故障矩阵: 500, 连接拒绝与响应超时都按 -2 扣分并换下一成员恢复请求。
func TestForwardScoredOrdinaryUpstreamFailures(t *testing.T) {
	internalError := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}
	t.Run("500", func(t *testing.T) {
		fixture := seedScoredGroup(t, model.GroupModeScored, model.ProtocolOpenAIChatCompletion,
			newUpstreamStub(t, internalError), newUpstreamStub(t, passthroughGood))

		rec := postForward(newForwardRouter(), false, false)
		if rec.Code != http.StatusOK {
			t.Fatalf("响应码 = %d, 想要 200, 响应体 %s", rec.Code, rec.Body.String())
		}
		mustScore(t, fixture, 0, scoreInitial-scoreFailurePenalty)
		mustScore(t, fixture, 1, scoreMax)
	})
	t.Run("连接拒绝", func(t *testing.T) {
		// 先占一个端口再释放, 拨号必然被拒。
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("占端口失败: %v", err)
		}
		refusedURL := "http://" + listener.Addr().String()
		listener.Close()

		fixture := seedScoredGroup(t, model.GroupModeScored, model.ProtocolOpenAIChatCompletion,
			newUpstreamStub(t, passthroughGood), newUpstreamStub(t, passthroughGood))
		mutateChannelConfig(t, fixture, 0, "base_url", refusedURL)

		rec := postForward(newForwardRouter(), false, false)
		if rec.Code != http.StatusOK {
			t.Fatalf("响应码 = %d, 想要 200, 响应体 %s", rec.Code, rec.Body.String())
		}
		mustScore(t, fixture, 0, scoreInitial-scoreFailurePenalty)
		mustScore(t, fixture, 1, scoreMax)
	})
	t.Run("响应超时", func(t *testing.T) {
		slow := newUpstreamStub(t, func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(3 * time.Second)
			openAICompletion(w, nil)
		})
		fixture := seedScoredGroup(t, model.GroupModeScored, model.ProtocolOpenAIChatCompletion,
			slow, newUpstreamStub(t, passthroughGood))
		config := model.DefaultGroupRelayConfig()
		config.MemberNonStreamResponseTimeoutSeconds = 1
		setGroupRelayConfig(t, fixture, config)

		startedAt := time.Now()
		rec := postForward(newForwardRouter(), false, false)
		if rec.Code != http.StatusOK {
			t.Fatalf("响应码 = %d, 想要 200, 响应体 %s", rec.Code, rec.Body.String())
		}
		if elapsed := time.Since(startedAt); elapsed > 2500*time.Millisecond {
			t.Fatalf("超时换路耗时 %v, 想要约 1 秒", elapsed)
		}
		mustScore(t, fixture, 0, scoreInitial-scoreFailurePenalty)
		mustScore(t, fixture, 1, scoreMax)
	})
}

// 流式终态矩阵: 只有观察到协议成功终态才记满分, 其余一律按各自语义记账。
func TestForwardScoredStreamTerminalMatrix(t *testing.T) {
	t.Run("首事件前空流可换路", func(t *testing.T) {
		empty := newUpstreamStub(t, func(w http.ResponseWriter, _ *http.Request) { serveSSE(w, nil, false) })
		fixture := seedScoredGroup(t, model.GroupModeScored, model.ProtocolOpenAIChatCompletion,
			empty, newUpstreamStub(t, passthroughStreamGood))

		rec := postForward(newForwardRouter(), true, false)
		if rec.Code != http.StatusOK {
			t.Fatalf("响应码 = %d, 想要 200, 响应体 %s", rec.Code, rec.Body.String())
		}
		mustScore(t, fixture, 0, scoreInitial-scoreFailurePenalty)
		mustScore(t, fixture, 1, scoreMax)
	})
	t.Run("首事件即错误可换路", func(t *testing.T) {
		failing := newUpstreamStub(t, func(w http.ResponseWriter, _ *http.Request) {
			serveSSE(w, []string{`{"error":{"message":"boom","type":"server_error"}}`}, false)
		})
		fixture := seedScoredGroup(t, model.GroupModeScored, model.ProtocolOpenAIChatCompletion,
			failing, newUpstreamStub(t, passthroughStreamGood))

		rec := postForward(newForwardRouter(), true, false)
		if rec.Code != http.StatusOK {
			t.Fatalf("响应码 = %d, 想要 200, 响应体 %s", rec.Code, rec.Body.String())
		}
		mustScore(t, fixture, 0, scoreInitial-scoreFailurePenalty)
		mustScore(t, fixture, 1, scoreMax)
	})
	t.Run("提交后无终态截断记失败不可换路", func(t *testing.T) {
		truncated := newUpstreamStub(t, func(w http.ResponseWriter, _ *http.Request) {
			serveSSE(w, []string{openAIStreamChunk}, false)
		})
		fixture := seedScoredGroup(t, model.GroupModeScored, model.ProtocolOpenAIChatCompletion,
			truncated, newUpstreamStub(t, passthroughStreamGood))

		rec := postForward(newForwardRouter(), true, false)
		if !strings.Contains(rec.Body.String(), "hi") {
			t.Fatalf("已提交内容未转发: %s", rec.Body.String())
		}
		state := latestRequestState()
		if state.Status != StatusFailed {
			t.Fatalf("终态 = %s, 想要 failed", state.Status)
		}
		mustScore(t, fixture, 0, scoreInitial-scoreFailurePenalty)
		// 已提交的响应不可换路: B 不应收到请求, 也不得记满分。
		if hits := fixture.members[1].upstream.hits.Load(); hits != 0 {
			t.Fatalf("提交后换路了, B 收到 %d 次请求", hits)
		}
		mustScore(t, fixture, 1, scoreInitial)
	})
	t.Run("提交后错误终态记失败不可换路", func(t *testing.T) {
		failing := newUpstreamStub(t, func(w http.ResponseWriter, _ *http.Request) {
			serveSSE(w, []string{openAIStreamChunk, `{"error":{"message":"late boom","type":"server_error"}}`}, false)
		})
		fixture := seedScoredGroup(t, model.GroupModeScored, model.ProtocolOpenAIChatCompletion,
			failing, newUpstreamStub(t, passthroughStreamGood))

		rec := postForward(newForwardRouter(), true, false)
		if !strings.Contains(rec.Body.String(), "late boom") {
			t.Fatalf("错误终态未转发给客户端: %s", rec.Body.String())
		}
		if state := latestRequestState(); state.Status != StatusFailed {
			t.Fatalf("终态 = %s, 想要 failed", state.Status)
		}
		mustScore(t, fixture, 0, scoreInitial-scoreFailurePenalty)
		if hits := fixture.members[1].upstream.hits.Load(); hits != 0 {
			t.Fatalf("提交后换路了, B 收到 %d 次请求", hits)
		}
	})
	t.Run("成功终态记满分", func(t *testing.T) {
		fixture := seedScoredGroup(t, model.GroupModeScored, model.ProtocolOpenAIChatCompletion,
			newUpstreamStub(t, passthroughStreamGood), newUpstreamStub(t, passthroughStreamGood))

		rec := postForward(newForwardRouter(), true, false)
		if rec.Code != http.StatusOK {
			t.Fatalf("响应码 = %d, 想要 200", rec.Code)
		}
		if state := latestRequestState(); state.Status != StatusSuccess {
			t.Fatalf("终态 = %s, 想要 success", state.Status)
		}
		mustScore(t, fixture, 0, scoreMax)
		// 成员 A 满分后粘住: 同分现任优先, 第二次请求仍走 A。
		postForward(newForwardRouter(), true, false)
		if hits := fixture.members[0].upstream.hits.Load(); hits != 2 {
			t.Fatalf("满分成员未被粘住, A 命中 = %d, 想要 2", hits)
		}
	})
}

// 客户端在流式传输中途取消: 不扣分, 以取消终态收场。
func TestForwardScoredClientCancelNoDeduction(t *testing.T) {
	endless := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		// 写失败即退出: 网关断开后必须结束, 否则测试服务的关闭会一直等这只协程。
		for {
			if _, err := w.Write([]byte("data: " + openAIStreamChunk + "\n\n")); err != nil {
				return
			}
			flusher.Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}
	fixture := seedScoredGroup(t, model.GroupModeScored, model.ProtocolOpenAIChatCompletion,
		newUpstreamStub(t, endless), newUpstreamStub(t, endless))

	server := httptest.NewServer(newForwardRouter())
	t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/chat/completions", bytes.NewReader(clientBody(true, false)))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("发起请求失败: %v", err)
	}
	defer response.Body.Close()
	buf := make([]byte, 512)
	if _, err := response.Body.Read(buf); err != nil {
		t.Fatalf("读取首个事件失败: %v", err)
	}
	cancel()
	waitLatestRequestStatus(t, StatusCanceled, 5*time.Second)
	// 取消不归因成员: 分数保持初始。
	mustScore(t, fixture, 0, scoreInitial)
}

// 故障转移模式经真实转发链路的行为回归: 失败进冷却并切换, 不受评分逻辑影响。
func TestForwardFailoverBehaviorUnchanged(t *testing.T) {
	internalError := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}
	fixture := seedScoredGroup(t, model.GroupModeFailover, model.ProtocolOpenAIChatCompletion,
		newUpstreamStub(t, internalError), newUpstreamStub(t, passthroughGood))

	rec := postForward(newForwardRouter(), false, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("响应码 = %d, 想要 200, 响应体 %s", rec.Code, rec.Body.String())
	}
	state := RouteStateOf(mustGroupOf(t, fixture.id))
	if state.CurrentItemID != fixture.members[1].itemID {
		t.Fatalf("故障转移现任 = %d, 想要 B", state.CurrentItemID)
	}
	if len(state.Cooldowns) != 1 {
		t.Fatalf("失败成员未进冷却: %v", state.Cooldowns)
	}
	if len(state.Scores) != 0 {
		t.Fatalf("故障转移模式写入了评分: %v", state.Scores)
	}
}

// mustGroupOf 按主键读取分组, 供路由状态断言使用。
func mustGroupOf(t *testing.T, id int) model.Group {
	t.Helper()
	group, err := op.GroupGet(id)
	if err != nil {
		t.Fatalf("读分组失败: %v", err)
	}
	return group
}
