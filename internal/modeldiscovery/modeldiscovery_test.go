package modeldiscovery

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/model"
)

// 合法的探测渠道配置: OpenAI 与 Anthropic 两协议路径由 modelsURL 推导出同一 /v1/models 端点,
// 测试服务端按请求头区分两侧 —— Authorization 归 OpenAI, X-Api-Key 归 Anthropic。
const (
	testOpenAIResponsePath   = "/v1/responses"
	testAnthropicMessagePath = "/v1/messages"
	testKey                  = "test-secret-key"
)

func newTestConfig(baseURL string) model.ChannelConfig {
	return model.ChannelConfig{
		BaseURL:              baseURL,
		OpenAIResponsePath:   testOpenAIResponsePath,
		AnthropicMessagePath: testAnthropicMessagePath,
	}
}

// openAIList 构造 OpenAI 模型列表响应体。
func openAIList(ids ...string) string {
	var b strings.Builder
	b.WriteString(`{"object":"list","data":[`)
	for i, id := range ids {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":%q,"object":"model"}`, id)
	}
	b.WriteString(`]}`)
	return b.String()
}

// anthropicList 构造 Anthropic 模型列表响应体, has_more/last_id 控制分页。
func anthropicList(hasMore bool, lastID string, ids ...string) string {
	var b strings.Builder
	b.WriteString(`{"data":[`)
	for i, id := range ids {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":%q,"type":"model"}`, id)
	}
	b.WriteString(`],"first_id":"f","has_more":`)
	if hasMore {
		b.WriteString("true")
	} else {
		b.WriteString("false")
	}
	fmt.Fprintf(&b, `,"last_id":%q}`, lastID)
	return b.String()
}

// gatedHandler 返回一个固定 JSON 响应; gate 非 nil 时先阻塞到 gate 关闭,
// 用来强制另一侧先返回, 验证协议归属不依赖完成顺序。
func gatedHandler(status int, body string, gate <-chan struct{}, hits *int32) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if gate != nil {
			<-gate
		}
		if hits != nil {
			atomic.AddInt32(hits, 1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body != "" {
			_, _ = w.Write([]byte(body))
		}
	}
}

// dispatchServer 按请求头把 /v1/models 分流到两侧 handler。
func dispatchServer(t *testing.T, openai, anthropic http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Header.Get("Authorization") != "":
			openai(w, r)
		case r.Header.Get("X-Api-Key") != "":
			anthropic(w, r)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func protocolsOf(models []model.ChannelFetchModel, name string) (model.Protocol, bool) {
	for _, m := range models {
		if m.Name == name {
			return m.Protocols, true
		}
	}
	return 0, false
}

// --- 协议归属: 不依赖完成顺序 -----------------------------------------------

func TestDiscoverAttributesProtocolByEndpointNotCompletionOrder(t *testing.T) {
	// Anthropic 立即返回, OpenAI 阻塞到 gate 关闭 —— 旧实现会把 Anthropic 的结果错当 OpenAI。
	openaiGate := make(chan struct{})
	var openaiHits int32
	srv := dispatchServer(t,
		gatedHandler(http.StatusOK, openAIList("gpt-4o", "gpt-3.5"), openaiGate, &openaiHits),
		gatedHandler(http.StatusOK, anthropicList(false, "", "claude-3", "claude-opus"), nil, nil),
	)
	cfg := newTestConfig(srv.URL)

	type out struct {
		r   Result
		err error
	}
	got := make(chan out, 1)
	go func() {
		r, err := Discover(context.Background(), srv.Client(), cfg, testKey, "", "")
		got <- out{r, err}
	}()

	// 让 Anthropic 先有机会响应; OpenAI 还在阻塞。关闭 gate 放行 OpenAI 完成探测。
	close(openaiGate)
	res := <-got
	if res.err != nil {
		t.Fatalf("Discover err: %v", res.err)
	}
	if atomic.LoadInt32(&openaiHits) == 0 {
		t.Fatal("OpenAI 端点未被调用, 测试无效")
	}
	// OpenAI 模型必须挂 OpenAIResponse, 不能因为 Anthropic 先到就贴成 AnthropicMessage。
	if p, ok := protocolsOf(res.r.Models, "gpt-4o"); !ok || p != model.ProtocolOpenAIResponse {
		t.Errorf("gpt-4o protocols = %v, want ProtocolOpenAIResponse", p)
	}
	if p, ok := protocolsOf(res.r.Models, "claude-3"); !ok || p != model.ProtocolAnthropicMessage {
		t.Errorf("claude-3 protocols = %v, want ProtocolAnthropicMessage", p)
	}
	if res.r.Partial {
		t.Errorf("Partial = true, want false (两侧都成功)")
	}
}

func TestDiscoverAttributesProtocolWhenOpenAIReturnsFirst(t *testing.T) {
	// 反过来: OpenAI 立即返回, Anthropic 阻塞。
	anthropicGate := make(chan struct{})
	var anthropicHits int32
	srv := dispatchServer(t,
		gatedHandler(http.StatusOK, openAIList("gpt-4o"), nil, nil),
		gatedHandler(http.StatusOK, anthropicList(false, "", "claude-3"), anthropicGate, &anthropicHits),
	)
	cfg := newTestConfig(srv.URL)

	got := make(chan struct {
		r   Result
		err error
	}, 1)
	go func() {
		r, err := Discover(context.Background(), srv.Client(), cfg, testKey, "", "")
		got <- struct {
			r   Result
			err error
		}{r, err}
	}()
	close(anthropicGate)
	res := <-got
	if res.err != nil {
		t.Fatalf("Discover err: %v", res.err)
	}
	if atomic.LoadInt32(&anthropicHits) == 0 {
		t.Fatal("Anthropic 端点未被调用, 测试无效")
	}
	if p, ok := protocolsOf(res.r.Models, "gpt-4o"); !ok || p != model.ProtocolOpenAIResponse {
		t.Errorf("gpt-4o protocols = %v, want ProtocolOpenAIResponse", p)
	}
	if p, ok := protocolsOf(res.r.Models, "claude-3"); !ok || p != model.ProtocolAnthropicMessage {
		t.Errorf("claude-3 protocols = %v, want ProtocolAnthropicMessage", p)
	}
}

// --- 同名模型协议位取并集 ----------------------------------------------------

func TestDiscoverSharedModelUnionsProtocols(t *testing.T) {
	srv := dispatchServer(t,
		gatedHandler(http.StatusOK, openAIList("shared-model"), nil, nil),
		gatedHandler(http.StatusOK, anthropicList(false, "", "shared-model"), nil, nil),
	)
	cfg := newTestConfig(srv.URL)
	r, err := Discover(context.Background(), srv.Client(), cfg, testKey, "", "")
	if err != nil {
		t.Fatalf("Discover err: %v", err)
	}
	want := model.ProtocolOpenAIResponse | model.ProtocolAnthropicMessage
	if p, ok := protocolsOf(r.Models, "shared-model"); !ok || p != want {
		t.Errorf("shared-model protocols = %v, want union %v", p, want)
	}
	if r.Partial {
		t.Errorf("Partial = true, want false")
	}
}

// --- 单侧失败: Partial=true, 仅成功侧模型 -----------------------------------

func TestDiscoverOpenAI401AnthropicOKIsPartial(t *testing.T) {
	srv := dispatchServer(t,
		gatedHandler(http.StatusUnauthorized, `{"error":"invalid api key sk-leaked-token"}`, nil, nil),
		gatedHandler(http.StatusOK, anthropicList(false, "", "claude-3"), nil, nil),
	)
	cfg := newTestConfig(srv.URL)
	r, err := Discover(context.Background(), srv.Client(), cfg, testKey, "", "")
	if err != nil {
		t.Fatalf("Discover err: %v", err)
	}
	if !r.Partial {
		t.Fatal("Partial = false, want true (仅 OpenAI 失败)")
	}
	if len(r.Models) != 1 {
		t.Fatalf("Models len = %d, want 1 (仅成功侧)", len(r.Models))
	}
	if p := r.Models[0].Protocols; p != model.ProtocolAnthropicMessage {
		t.Errorf("claude-3 protocols = %v, want ProtocolAnthropicMessage", p)
	}
}

func TestDiscoverAnthropic401OpenAIOKIsPartial(t *testing.T) {
	srv := dispatchServer(t,
		gatedHandler(http.StatusOK, openAIList("gpt-4o"), nil, nil),
		gatedHandler(http.StatusUnauthorized, `{"error":"bad key"}`, nil, nil),
	)
	cfg := newTestConfig(srv.URL)
	r, err := Discover(context.Background(), srv.Client(), cfg, testKey, "", "")
	if err != nil {
		t.Fatalf("Discover err: %v", err)
	}
	if !r.Partial {
		t.Fatal("Partial = false, want true (仅 Anthropic 失败)")
	}
	if len(r.Models) != 1 {
		t.Fatalf("Models len = %d, want 1", len(r.Models))
	}
	if r.Models[0].Name != "gpt-4o" || r.Models[0].Protocols != model.ProtocolOpenAIResponse {
		t.Errorf("got %+v, want gpt-4o / ProtocolOpenAIResponse", r.Models[0])
	}
}

func TestDiscoverBoth401ReturnsError(t *testing.T) {
	srv := dispatchServer(t,
		gatedHandler(http.StatusUnauthorized, `{"error":"openai-secret"}`, nil, nil),
		gatedHandler(http.StatusUnauthorized, `{"error":"anthropic-secret"}`, nil, nil),
	)
	cfg := newTestConfig(srv.URL)
	r, err := Discover(context.Background(), srv.Client(), cfg, testKey, "", "")
	if err == nil {
		t.Fatal("err = nil, want error (两侧都失败)")
	}
	// 错误消息不得泄露上游原文里的疑似凭据。
	if strings.Contains(err.Error(), "openai-secret") || strings.Contains(err.Error(), "anthropic-secret") {
		t.Errorf("error leaks upstream body: %v", err)
	}
	if r.Partial {
		t.Errorf("Partial = true on both-fail, want false")
	}
}

// --- 空结果数组仍为非 nil ----------------------------------------------------

func TestDiscoverEmptyButSuccessfulEndpointsYieldEmptyNonNilModels(t *testing.T) {
	srv := dispatchServer(t,
		gatedHandler(http.StatusOK, openAIList(), nil, nil),
		gatedHandler(http.StatusOK, anthropicList(false, ""), nil, nil),
	)
	cfg := newTestConfig(srv.URL)
	r, err := Discover(context.Background(), srv.Client(), cfg, testKey, "", "")
	if err != nil {
		t.Fatalf("Discover err: %v", err)
	}
	if r.Models == nil {
		t.Fatal("Models = nil, want non-nil empty slice")
	}
	if len(r.Models) != 0 {
		t.Errorf("Models len = %d, want 0", len(r.Models))
	}
	if r.Partial {
		t.Errorf("Partial = true, want false (两侧都成功, 即使零模型)")
	}
}

// --- 正则: 非法/超时/取消 ---------------------------------------------------

func TestDiscoverInvalidRegexFailsBeforeNetwork(t *testing.T) {
	var hits int32
	srv := dispatchServer(t,
		gatedHandler(http.StatusOK, openAIList("gpt-4o"), nil, &hits),
		gatedHandler(http.StatusOK, anthropicList(false, "", "claude-3"), nil, &hits),
	)
	cfg := newTestConfig(srv.URL)
	// 未闭合的分组是 ECMAScript 非法模式。
	_, err := Discover(context.Background(), srv.Client(), cfg, testKey, "(unclosed", "")
	if err == nil {
		t.Fatal("err = nil, want regex compile error")
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Errorf("上游被调用 %d 次, 非法正则应先于网络失败", hits)
	}
}

func TestDiscoverRegexTimeoutBoundedAndSafe(t *testing.T) {
	// 灾难性回溯: (a+)+$ 对"全 a 后跟非 a 字符"的输入会指数膨胀。
	// 上游模型名由探测给出, 这里放一个足够触发回溯的名以验证 MatchTimeout 边界。
	backtrackName := strings.Repeat("a", 30) + "b"
	srv := dispatchServer(t,
		gatedHandler(http.StatusOK, openAIList(backtrackName), nil, nil),
		gatedHandler(http.StatusOK, anthropicList(false, "", "c"), nil, nil),
	)
	cfg := newTestConfig(srv.URL)
	start := time.Now()
	_, err := Discover(context.Background(), srv.Client(), cfg, testKey, `(a+)+$`, "")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("err = nil, want match timeout error")
	}
	// 必须在合理时间内返回, 不应被回溯卡死(超时 5s + 网络/合并余量)。
	if elapsed > 15*time.Second {
		t.Errorf("Discover 耗时 %v, 正则超时未起作用", elapsed)
	}
	// 错误消息不得泄露上游模型名(regexp2 超时错误默认带原始输入)。
	if strings.Contains(err.Error(), backtrackName) {
		t.Errorf("error leaks upstream input: %v", err)
	}
}

func TestDiscoverContextCancelTerminatesWithoutLeak(t *testing.T) {
	// 两侧都阻塞, 取消 ctx 后应整体终止, 不泄漏 goroutine。
	openaiGate := make(chan struct{})
	anthropicGate := make(chan struct{})
	srv := dispatchServer(t,
		gatedHandler(http.StatusOK, openAIList("gpt-4o"), openaiGate, nil),
		gatedHandler(http.StatusOK, anthropicList(false, "", "claude"), anthropicGate, nil),
	)
	cfg := newTestConfig(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() {
		_, err := Discover(ctx, srv.Client(), cfg, testKey, "", "")
		got <- err
	}()
	// 给探测一点时间发起请求(然后阻塞在 gate 上), 再取消。
	time.Sleep(50 * time.Millisecond)
	cancel()
	// 关闭 gate 让被取消的请求立刻返回(若实现正确, http 会因 ctx 报错而不等响应体)。
	close(openaiGate)
	close(anthropicGate)
	select {
	case err := <-got:
		if err == nil {
			// 取消时两侧都失败, 应返回错误。
			t.Fatal("err = nil, want context error on cancel")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Discover 未在取消后返回, goroutine 泄漏")
	}
}

// --- 分页: 多页/回环/取消 ----------------------------------------------------

func TestDiscoverAnthropicPaginationMultiPage(t *testing.T) {
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		after := r.URL.Query().Get("after_id")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		switch after {
		case "":
			_, _ = w.Write([]byte(anthropicList(true, "p1", "m1", "m2")))
		case "p1":
			_, _ = w.Write([]byte(anthropicList(false, "", "m3")))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	// OpenAI 侧空成功, 确保 Anthropic 分页路径被单独验证。
	srv := dispatchServer(t, gatedHandler(http.StatusOK, openAIList(), nil, nil), mux)
	cfg := newTestConfig(srv.URL)
	r, err := Discover(context.Background(), srv.Client(), cfg, testKey, "", "")
	if err != nil {
		t.Fatalf("Discover err: %v", err)
	}
	wantNames := map[string]bool{"m1": true, "m2": true, "m3": true}
	if len(r.Models) != 3 {
		t.Fatalf("Models len = %d, want 3", len(r.Models))
	}
	for _, m := range r.Models {
		if !wantNames[m.Name] {
			t.Errorf("unexpected model %q", m.Name)
		}
		if m.Protocols != model.ProtocolAnthropicMessage {
			t.Errorf("%q protocols = %v, want AnthropicMessage", m.Name, m.Protocols)
		}
	}
}

func TestDiscoverAnthropicPaginationCycleDetected(t *testing.T) {
	// 上游在 p1 与 p2 之间来回跳, 形不成 A→A 而是 A→B→A 回环。
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		after := r.URL.Query().Get("after_id")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		switch after {
		case "":
			_, _ = w.Write([]byte(anthropicList(true, "p1", "m1")))
		case "p1":
			_, _ = w.Write([]byte(anthropicList(true, "p2", "m2")))
		case "p2":
			// 回到 p1, 形成 p1→p2→p1 循环。
			_, _ = w.Write([]byte(anthropicList(true, "p1", "m3")))
		default:
			_, _ = w.Write([]byte(anthropicList(false, "", "x")))
		}
	})
	// OpenAI 也失败: 两侧都失败时 Discover 才回错误, 才能在错误里看到分页回环。
	srv := dispatchServer(t,
		gatedHandler(http.StatusUnauthorized, `{"error":"x"}`, nil, nil),
		mux)
	cfg := newTestConfig(srv.URL)
	_, err := Discover(context.Background(), srv.Client(), cfg, testKey, "", "")
	if err == nil {
		t.Fatal("err = nil, want pagination cycle error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("err = %v, want cycle error", err)
	}
}

// blockingHandler 永不主动响应, 仅在请求上下文取消时返回; 用来让两侧都处于在途,
// 使取消 ctx 时两侧同时失败, Discover 回带 context.Canceled 可被 errors.Is 识别。
func blockingHandler() http.HandlerFunc {
	return func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}
}

func TestDiscoverAnthropicPaginationCancelled(t *testing.T) {
	// 第一页返回 has_more=true, 第二页阻塞到请求上下文取消(客户端断开后服务端即返回);
	// OpenAI 同样阻塞, 使取消时两侧都失败, 错误可被 errors.Is(., context.Canceled) 识别。
	page := int32(0)
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&page, 1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(anthropicList(true, "p1", "m1")))
			return
		}
		// 第二页阻塞到请求被取消; 不能用 select{}, 否则 srv.Close() 会等到永远。
		<-r.Context().Done()
	})
	srv := dispatchServer(t, blockingHandler(), mux)
	cfg := newTestConfig(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() {
		_, err := Discover(ctx, srv.Client(), cfg, testKey, "", "")
		got <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-got:
		if err == nil {
			t.Fatal("err = nil, want context error")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want errors.Is(., context.Canceled)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("分页取消未终止, goroutine 泄漏")
	}
}

// --- 成功体过大被拒绝, 上游错误体不泄露 --------------------------------------

func TestDiscoverRejectsOversizedSuccessBody(t *testing.T) {
	// 构造一个超过 maxSuccessBodyBytes 的合法 JSON 响应。
	huge := strings.Repeat(" ", int(maxSuccessBodyBytes)+8)
	body := `{"object":"list","data":[{"id":"` + strings.Repeat("x", int(maxSuccessBodyBytes)) + `"}` + huge + `]}`
	srv := dispatchServer(t,
		gatedHandler(http.StatusOK, body, nil, nil),
		gatedHandler(http.StatusOK, anthropicList(false, "", "claude"), nil, nil),
	)
	cfg := newTestConfig(srv.URL)
	r, err := Discover(context.Background(), srv.Client(), cfg, testKey, "", "")
	// Anthropic 侧成功, OpenAI 侧超大被拒 —— 属单侧失败, Partial=true, 仅 Anthropic 模型可用。
	if err != nil {
		t.Fatalf("Discover err: %v", err)
	}
	if !r.Partial {
		t.Fatal("Partial = false, want true (OpenAI 超大被拒, Anthropic 成功)")
	}
	for _, m := range r.Models {
		if m.Protocols != model.ProtocolAnthropicMessage {
			t.Errorf("model %q protocols = %v, 仅应含 Anthropic 成功侧", m.Name, m.Protocols)
		}
	}
}

func TestDiscoverUpstreamErrorDoesNotLeakSecrets(t *testing.T) {
	// 上游 401 响应体里塞入疑似 token 与内部 URL, 错误消息不得回带这些内容。
	leakedToken := "sk-super-secret-leaked-abcdef"
	leakedURL := "https://internal.upstream.corp/v1/admin/debug"
	body := `{"error":"` + leakedToken + `","url":"` + leakedURL + `"}`
	srv := dispatchServer(t,
		gatedHandler(http.StatusUnauthorized, body, nil, nil),
		gatedHandler(http.StatusOK, anthropicList(false, "", "claude-3"), nil, nil),
	)
	cfg := newTestConfig(srv.URL)
	r, err := Discover(context.Background(), srv.Client(), cfg, testKey, "", "")
	if err != nil {
		t.Fatalf("Discover err (单侧失败应返回 nil): %v", err)
	}
	if !r.Partial {
		t.Fatal("Partial = false, want true")
	}
	// 同时验证两侧都失败时, 拼接错误也不含泄漏内容。
	srv2 := dispatchServer(t,
		gatedHandler(http.StatusUnauthorized, body, nil, nil),
		gatedHandler(http.StatusUnauthorized, body, nil, nil),
	)
	cfg2 := newTestConfig(srv2.URL)
	_, bothErr := Discover(context.Background(), srv2.Client(), cfg2, testKey, "", "")
	if bothErr == nil {
		t.Fatal("both-fail err = nil")
	}
	if strings.Contains(bothErr.Error(), leakedToken) {
		t.Errorf("both-fail error leaks token: %v", bothErr)
	}
	if strings.Contains(bothErr.Error(), leakedURL) {
		t.Errorf("both-fail error leaks internal URL: %v", bothErr)
	}
}

// --- 传输错误脱敏 ------------------------------------------------------------

func TestDiscoverTransportErrorSanitized(t *testing.T) {
	// 用一个保证不可达的地址, 触发连接拒绝; 错误不应回带完整派生 URL(含 /v1/models 路径)。
	cfg := model.ChannelConfig{
		BaseURL:              "http://127.0.0.1:1", // 端口 1 通常不可达
		OpenAIResponsePath:   testOpenAIResponsePath,
		AnthropicMessagePath: testAnthropicMessagePath,
	}
	_, err := Discover(context.Background(), http.DefaultClient, cfg, testKey, "", "")
	if err == nil {
		t.Fatal("err = nil, want transport error")
	}
	// 不应回带具体路径片段 /v1/models; "upstream unreachable" 摘要是允许的。
	if strings.Contains(err.Error(), "/v1/models") {
		t.Errorf("transport error leaks derived URL path: %v", err)
	}
}

// --- 自定义 Header 行为保留 --------------------------------------------------

func TestDiscoverCustomHeadersApplied(t *testing.T) {
	var sawXCustom, sawAuth string
	var mu sync.Mutex
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.Header.Get("Authorization") != "" {
			sawAuth = r.Header.Get("X-Custom-H")
		} else if r.Header.Get("X-Api-Key") != "" {
			sawXCustom = r.Header.Get("X-Custom-H")
		}
		mu.Unlock()
		if r.Header.Get("Authorization") != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(openAIList("gpt-4o")))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(anthropicList(false, "", "claude")))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cfg := newTestConfig(srv.URL)
	cfg.CustomHeader = []model.CustomHeader{{HeaderKey: "X-Custom-H", HeaderValue: "val-123"}}
	_, err := Discover(context.Background(), srv.Client(), cfg, testKey, "", "")
	if err != nil {
		t.Fatalf("Discover err: %v", err)
	}
	if sawAuth != "val-123" || sawXCustom != "val-123" {
		t.Errorf("custom header not applied to both sides: openai=%q anthropic=%q", sawAuth, sawXCustom)
	}
}

// --- 空白模型名被丢弃 --------------------------------------------------------

func TestDiscoverDropsBlankModelNames(t *testing.T) {
	srv := dispatchServer(t,
		gatedHandler(http.StatusOK, openAIList("  ", "", "gpt-4o"), nil, nil),
		gatedHandler(http.StatusOK, anthropicList(false, "", "", "claude"), nil, nil),
	)
	cfg := newTestConfig(srv.URL)
	r, err := Discover(context.Background(), srv.Client(), cfg, testKey, "", "")
	if err != nil {
		t.Fatalf("Discover err: %v", err)
	}
	if len(r.Models) != 2 {
		t.Fatalf("Models len = %d, want 2 (空白名被丢弃)", len(r.Models))
	}
	for _, m := range r.Models {
		if strings.TrimSpace(m.Name) == "" {
			t.Errorf("空白模型名未丢弃: %q", m.Name)
		}
	}
}

// --- 安全: reason phrase 不泄漏 ---------------------------------------------

func TestDiscoverUpstreamReasonPhraseNotLeaked(t *testing.T) {
	// 恶意上游在 reason phrase 里塞疑似 token, 错误消息不得回带。
	// httptest 不允许直接设置 reason phrase, 用自定义 ResponseWriter 控制 WriteHeader 的短语。
	leakedToken := "Bearer sk-leaked-in-reason-phrase"
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 直接劫持连接写原始状态行, 含篡改的 reason phrase。
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("server does not support hijack")
		}
		conn, _, _ := hj.Hijack()
		defer conn.Close()
		fmt.Fprintf(conn, "HTTP/1.1 401 %s\r\nContent-Length: 0\r\n\r\n", leakedToken)
	})
	srv := dispatchServer(t, mux, mux)
	cfg := newTestConfig(srv.URL)
	_, err := Discover(context.Background(), srv.Client(), cfg, testKey, "", "")
	if err == nil {
		t.Fatal("err = nil, want 401 error")
	}
	if strings.Contains(err.Error(), leakedToken) {
		t.Errorf("error leaks attacker-controlled reason phrase: %v", err)
	}
	// 应只含数字状态码与标准 StatusText。
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should contain numeric status code 401: %v", err)
	}
}

// --- 安全: JSON 字段类型错误不泄漏字段名 ------------------------------------

func TestDiscoverMalformedJSONFieldTypeNotLeaked(t *testing.T) {
	// data 字段返回字符串而非数组, 触发 UnmarshalTypeError, 字段名 "data" 不得出现在错误里。
	body := `{"data":"not-an-array"}`
	srv := dispatchServer(t,
		gatedHandler(http.StatusOK, body, nil, nil),
		gatedHandler(http.StatusOK, anthropicList(false, "", "claude"), nil, nil),
	)
	cfg := newTestConfig(srv.URL)
	_, err := Discover(context.Background(), srv.Client(), cfg, testKey, "", "")
	if err != nil {
		t.Fatalf("Discover err (单侧失败应 nil): %v", err)
	}
	// 两边都失败时, 验证错误不含字段名。
	srv2 := dispatchServer(t,
		gatedHandler(http.StatusOK, body, nil, nil),
		gatedHandler(http.StatusOK, body, nil, nil),
	)
	cfg2 := newTestConfig(srv2.URL)
	_, bothErr := Discover(context.Background(), srv2.Client(), cfg2, testKey, "", "")
	if bothErr == nil {
		t.Fatal("both-fail err = nil")
	}
	if strings.Contains(bothErr.Error(), "data") {
		t.Errorf("error leaks JSON field name: %v", bothErr)
	}
}

// --- 安全: body reader 错误不泄漏 --------------------------------------------

func TestDiscoverBodyReadErrorSanitized(t *testing.T) {
	// 服务端写合法状态码后立即断连, body 读取会出错; 错误不应回带 io 原文。
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("server does not support hijack")
		}
		conn, _, _ := hj.Hijack()
		// 声明有 body 但不写完就关, 客户端 ReadAll 会报 unexpected EOF。
		fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Length: 999\r\n\r\n")
		conn.Close()
	})
	srv := dispatchServer(t, mux, mux)
	cfg := newTestConfig(srv.URL)
	_, err := Discover(context.Background(), srv.Client(), cfg, testKey, "", "")
	if err == nil {
		// 两边都断连 → 应返回错误。
		t.Fatal("err = nil, want body read error")
	}
	// 不应含 "EOF"/"unexpected"/"read" 等底层原文, 只回固定 "read upstream response failed"。
	for _, frag := range []string{"EOF", "unexpected", "chunked", "transfer"} {
		if strings.Contains(err.Error(), frag) {
			t.Errorf("error leaks body read detail (%q): %v", frag, err)
		}
	}
}

// --- 安全: 非法自定义 header 值不泄漏凭据 -----------------------------------

func TestDiscoverMalformedCustomHeaderSecretNotLeaked(t *testing.T) {
	// 用户在 CustomHeader 里填入了带 CRLF 的非法值(可能是误粘的凭据)。
	// http.NewRequestWithContext 不会因此失败, 但 httpClient.Do 会因 header 校验报错;
	// 错误消息不应回带 header 值原文。
	secretValue := "sk-secret-in-header-value\r\nX-Injected: bad"
	srv := dispatchServer(t,
		gatedHandler(http.StatusOK, openAIList("gpt-4o"), nil, nil),
		gatedHandler(http.StatusOK, anthropicList(false, "", "claude"), nil, nil),
	)
	cfg := newTestConfig(srv.URL)
	cfg.CustomHeader = []model.CustomHeader{{HeaderKey: "X-Custom", HeaderValue: secretValue}}
	_, err := Discover(context.Background(), srv.Client(), cfg, testKey, "", "")
	if err == nil {
		t.Skip("当前 http client 未拒绝该 header 值, 跳过泄漏校验")
	}
	if strings.Contains(err.Error(), "sk-secret-in-header-value") {
		t.Errorf("error leaks custom header secret value: %v", err)
	}
}

// --- 安全: 自定义 RoundTripper 任意错误不泄漏 -------------------------------

func TestDiscoverCustomRoundTripperErrorSanitized(t *testing.T) {
	// 用注入的 Transport 让 RoundTrip 返回带任意文本的错误, 验证不透出。
	leakedDetail := "internal-upstream-internal.corp:9090 token=sk-rt-leaked"
	rt := &errRoundTripper{err: errors.New(leakedDetail)}
	client := &http.Client{Transport: rt}
	cfg := newTestConfig("http://example.test")
	_, err := Discover(context.Background(), client, cfg, testKey, "", "")
	if err == nil {
		t.Fatal("err = nil, want transport error")
	}
	if strings.Contains(err.Error(), leakedDetail) {
		t.Errorf("error leaks RoundTripper detail: %v", err)
	}
	if strings.Contains(err.Error(), "sk-rt-leaked") {
		t.Errorf("error leaks token from RoundTripper: %v", err)
	}
}

// errRoundTripper 总是返回预设错误, 用来模拟自定义 Transport 的任意失败。
type errRoundTripper struct{ err error }

func (e *errRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, e.err
}

// --- 形状校验: 缺少 data / null 失败, 空数组成功 ----------------------------

func TestDiscoverMissingDataArrayRejected(t *testing.T) {
	// 顶层对象无 data 字段, 不应静默当作零模型成功。
	body := `{"object":"list"}`
	srv := dispatchServer(t,
		gatedHandler(http.StatusOK, body, nil, nil),
		gatedHandler(http.StatusOK, anthropicList(false, "", "claude"), nil, nil),
	)
	cfg := newTestConfig(srv.URL)
	r, err := Discover(context.Background(), srv.Client(), cfg, testKey, "", "")
	// 单侧失败: Anthropic 成功, OpenAI 形状不符 → Partial=true, 不返回错误。
	if err != nil {
		t.Fatalf("Discover err: %v", err)
	}
	if !r.Partial {
		t.Fatal("Partial = false, want true (OpenAI 形状不符)")
	}
	for _, m := range r.Models {
		if m.Protocols != model.ProtocolAnthropicMessage {
			t.Errorf("仅应含 Anthropic 成功侧, got %q protocols=%v", m.Name, m.Protocols)
		}
	}
}

func TestDiscoverNullDataRejected(t *testing.T) {
	body := `{"data":null}`
	srv := dispatchServer(t,
		gatedHandler(http.StatusOK, body, nil, nil),
		gatedHandler(http.StatusOK, body, nil, nil),
	)
	cfg := newTestConfig(srv.URL)
	_, err := Discover(context.Background(), srv.Client(), cfg, testKey, "", "")
	if err == nil {
		t.Fatal("err = nil, want error (data:null 两侧都形状不符)")
	}
	// null 既不是合法数组也不算缺失, 任一拒绝消息即可; 关键是不静默成功。
	if !strings.Contains(err.Error(), "data") {
		t.Errorf("err = %v, want data-related rejection", err)
	}
}

func TestDiscoverDataNotArrayRejected(t *testing.T) {
	body := `{"data":{"id":"wrong"}}`
	srv := dispatchServer(t,
		gatedHandler(http.StatusOK, body, nil, nil),
		gatedHandler(http.StatusOK, body, nil, nil),
	)
	cfg := newTestConfig(srv.URL)
	_, err := Discover(context.Background(), srv.Client(), cfg, testKey, "", "")
	if err == nil {
		t.Fatal("err = nil, want error (data 非数组)")
	}
}

func TestDiscoverEmptyDataArraySuccess(t *testing.T) {
	// 空数组是合法的零模型成功, 与 missing/null 区分。
	srv := dispatchServer(t,
		gatedHandler(http.StatusOK, `{"data":[]}`, nil, nil),
		gatedHandler(http.StatusOK, `{"data":[]}`, nil, nil),
	)
	cfg := newTestConfig(srv.URL)
	r, err := Discover(context.Background(), srv.Client(), cfg, testKey, "", "")
	if err != nil {
		t.Fatalf("Discover err: %v", err)
	}
	if r.Models == nil || len(r.Models) != 0 {
		t.Errorf("Models = %v, want non-nil empty", r.Models)
	}
	if r.Partial {
		t.Errorf("Partial = true, want false (两侧都成功)")
	}
}

// --- context 取消/超时 errors.Is 穿透 --------------------------------------

func TestDiscoverContextCanceledErrorsIsPreserved(t *testing.T) {
	srv := dispatchServer(t, blockingHandler(), blockingHandler())
	cfg := newTestConfig(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() {
		_, err := Discover(ctx, srv.Client(), cfg, testKey, "", "")
		got <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-got:
		if err == nil {
			t.Fatal("err = nil, want context error")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("errors.Is(err, context.Canceled) = false, err=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("取消未终止, goroutine 泄漏")
	}
}

func TestDiscoverContextDeadlineErrorsIsPreserved(t *testing.T) {
	srv := dispatchServer(t, blockingHandler(), blockingHandler())
	cfg := newTestConfig(srv.URL)
	// 极短超时, 让两侧都因 DeadlineExceeded 失败。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := Discover(ctx, srv.Client(), cfg, testKey, "", "")
	if err == nil {
		t.Fatal("err = nil, want deadline error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("errors.Is(err, context.DeadlineExceeded) = false, err=%v", err)
	}
}
