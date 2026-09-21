package relay

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/looplj/axonhub/llm"
)

// 客户端请求在转发过程中的当前状态。
type Status string

const (
	StatusRunning   Status = "running"   // 循环中: 正在选目标, 等待或请求上游。
	StatusCommitted Status = "committed" // 首字节已写出客户端, 此后不可再重试。
	StatusSuccess   Status = "success"   // 响应已完整交付客户端。
	StatusFailed    Status = "failed"    // 请求以错误结束。
	StatusCanceled  Status = "canceled"  // 客户端提前断开或取消。
)

// 客户端请求的完整进程内状态, 同时作为状态流的消息形状; 上半部分在请求到达时写入并在结束时定稿, 下半部分每轮循环覆盖。
type RequestState struct {
	ID                 uint64         `json:"id"`                   // 请求在当前进程内的唯一标识。
	Status             Status         `json:"status"`               // 请求当前状态。
	StartedAt          time.Time      `json:"started_at"`           // 请求到达时间。
	Duration           time.Duration  `json:"duration"`             // 请求从到达到结束的总耗时, 未结束时为零。
	FirstTokenDuration time.Duration  `json:"first_token_duration"` // 流式正确响应轮次开始到首字节提交的耗时, 非流式响应为零。
	StreamDuration     time.Duration  `json:"stream_duration"`      // 流式响应从首字节提交到响应结束的耗时, 非流式响应为零。
	ResponseDuration   time.Duration  `json:"response_duration"`    // 非流式正确响应轮次开始到完整响应提交的耗时, 流式响应为零。
	Model              string         `json:"model"`                // 客户端请求的模型名称, 即分组名称。
	Protocol           model.Protocol `json:"protocol"`             // 客户端请求使用的协议, 由入站格式定出, 单个协议位而非掩码组合。
	GroupID            int            `json:"group_id"`             // 承载本请求的分组 ID, 供界面按主键直接定位分组而不必按名称回查。
	APIKeyName         string         `json:"api_key_name"`         // 发起请求时的 API Key 名称。
	Usage              llm.Usage      `json:"usage"`                // 请求结束时写入的展示用量。
	Cost               *float64       `json:"cost"`                 // 请求结束时写入的累计费用; running 或未知时为 null。
	CostKnown          bool           `json:"cost_known"`           // 费用是否有可靠价格来源。
	CostSource         string         `json:"cost_source"`          // "manual" | "actual_reference" | "group_reference" | "unknown"
	CostReferenceModel *string        `json:"cost_reference_model"` // actual/group 参考匹配到的标准模型 ID; manual/unknown 为 null。
	OutputChars        int            `json:"output_chars"`         // 流式过程中按事件数量估算并实时累计的输出字符数, 仅用于界面展示, 不参与结算。

	Round          int            `json:"round"`            // 最新一轮循环的递增序号, 人工中止按此匹配以免误杀下一轮。
	RoundStartedAt time.Time      `json:"round_started_at"` // 最新一轮上游请求的开始时间。
	TargetChannel  string         `json:"target_channel"`   // 最新一轮选中的渠道名称。
	TargetModel    string         `json:"target_model"`     // 最新一轮实际请求上游的模型名称。
	TargetProtocol model.Protocol `json:"target_protocol"`  // 最新一轮实际请求上游的协议, 与 Protocol 不同即本轮做了跨协议转换; 0 表示尚未选出。
	Sending        bool           `json:"sending"`          // 最新一轮是否仍在等待上游响应。
	Error          string         `json:"error,omitempty"`  // 最新一轮的失败原因, 请求结束后即为最终错误。

	requestBody  string // 客户端原始请求体, 体积大故不进状态流, 由独立接口按需拉取。
	responseBody string // 聚合后的完整最终响应体, 同样按需拉取。
	apiKeyID     int    // 发起请求的 API Key ID, 用于请求完成后的归属统计。

	requestCtx    context.Context    // 请求级上下文, 同时约束等待、当前轮次和后续重试。
	requestCancel context.CancelFunc // 中止整个请求, 同时打断等待、当前轮次和后续重试。
	roundCancel   context.CancelFunc // 中止最新一轮上游请求, 仅在该轮等待响应期间非空。
	lastPublish   time.Time          // 上次向状态流发布快照的时间, 输出字符数按此节流发布。
	streamStarted time.Time          // 流式响应首字节提交时间, 用于计算实际流式传输耗时。
}

const streamBuffer = 16                              // 单个状态流连接的非阻塞消息缓冲容量。
const maxFinished = 50                               // 进程内最多保留的已结束请求数量。
const outputPublishInterval = 500 * time.Millisecond // 输出字符数实时推送的最短发布间隔。

var (
	idSeq    atomic.Uint64                          // 进程内严格递增的请求 ID。
	mu       sync.Mutex                             // 全部共享状态的互斥锁。
	requests = make(map[uint64]*RequestState)       // 按请求 ID 保存的全部请求状态。
	watchers = make(map[chan RequestState]struct{}) // 全部状态流 SSE 连接。
)

// newRequestState 分配请求 ID 并登记初始运行状态; 返回的记录是本请求后续全部状态写入的入口。
func newRequestState(ctx context.Context, modelName string, groupID int, protocol model.Protocol, body string, apiKeyID int) *RequestState {
	requestCtx, requestCancel := context.WithCancel(ctx)
	mu.Lock()
	defer mu.Unlock()

	request := &RequestState{
		ID:                 idSeq.Add(1),
		Status:             StatusRunning,
		StartedAt:          time.Now(),
		Model:              modelName,
		Protocol:           protocol,
		GroupID:            groupID,
		CostSource:         "unknown", // running 状态尚未记账, 枚举契约要求非空串。
		CostKnown:          false,
		Cost:               nil,
		CostReferenceModel: nil,
		requestBody:        body,
		apiKeyID:           apiKeyID,
		requestCtx:         requestCtx,
		requestCancel:      requestCancel,
	}
	// 登记时保存名称快照, 查询失败时留空。
	if apiKey, err := op.APIKeyGet(apiKeyID, ctx); err == nil {
		request.APIKeyName = apiKey.Name
	}
	requests[request.ID] = request
	publishRequestLocked(request)
	return request
}

// startRound 记录本轮选中的目标并进入上游请求, cancel 供人工中止本轮, 返回递增的轮次序号。
func (r *RequestState) startRound(cancel context.CancelFunc, channel, modelName string, protocol model.Protocol) int {
	mu.Lock()
	defer mu.Unlock()

	r.Round++
	r.RoundStartedAt = time.Now()
	r.OutputChars = 0 // 新一轮从头计数, 避免累计上一轮未提交的输出。
	r.lastPublish = time.Time{}
	r.TargetChannel = channel
	r.TargetModel = modelName
	r.TargetProtocol = protocol
	r.Sending = true
	r.Error = ""
	r.roundCancel = cancel
	publishRequestLocked(r)
	return r.Round
}

// finishRound 记录本轮上游结果, errText 为空表示已取得可提交响应。
func (r *RequestState) finishRound(errText string) {
	mu.Lock()
	defer mu.Unlock()

	r.Sending = false
	r.Error = errText
	r.roundCancel = nil
	publishRequestLocked(r)
}

// addOutput 每个转发事件累加一个输出字符并按节流间隔发布快照; 距上次发布不足阈值时只累加不出流。
func (r *RequestState) addOutput() {
	mu.Lock()
	defer mu.Unlock()

	r.OutputChars++
	if time.Since(r.lastPublish) >= outputPublishInterval {
		r.lastPublish = time.Now()
		if !r.streamStarted.IsZero() {
			r.StreamDuration = time.Since(r.streamStarted)
		}
		publishRequestLocked(r)
	}
}

// Interrupt 中止指定请求仍在等待响应且轮次匹配的上游请求; 轮次不匹配说明该轮已结束, 不影响后续轮次。
func Interrupt(id uint64, round int) {
	mu.Lock()
	request := requests[id]
	if request == nil || request.Round != round || request.roundCancel == nil {
		mu.Unlock()
		return
	}
	cancel := request.roundCancel
	request.roundCancel = nil
	mu.Unlock()

	cancel()
}

// CancelRequest 取消指定的完整请求; 已结束请求不会被重新改写状态。
func CancelRequest(id uint64) {
	mu.Lock()
	request := requests[id]
	if request == nil {
		mu.Unlock()
		return
	}
	cancel := request.requestCancel
	request.requestCancel = nil
	mu.Unlock()

	if cancel != nil {
		cancel()
	}
}

// wait 在重新选择目标之前退避 seconds 秒; 客户端在退避期间断开时以取消终态定稿并返回 false。
func (r *RequestState) wait(ctx context.Context, seconds int) bool {
	select {
	case <-ctx.Done():
		r.markCanceled(ctx.Err(), "", nil)
		return false
	case <-time.After(time.Duration(seconds) * time.Second):
		return true
	}
}

// markCommitted 标记响应已提交, 并按响应方式记录最终正确轮次的首字或完整响应耗时。
func (r *RequestState) markCommitted(streaming bool) {
	mu.Lock()
	defer mu.Unlock()

	now := time.Now()
	r.Status = StatusCommitted
	if streaming {
		r.FirstTokenDuration = now.Sub(r.RoundStartedAt)
		r.streamStarted = now
	} else {
		r.ResponseDuration = now.Sub(r.RoundStartedAt)
	}
	publishRequestLocked(r)
}

// finishStream 记录首字节提交至流式响应实际结束的耗时。
func (r *RequestState) finishStream() {
	mu.Lock()
	defer mu.Unlock()

	if !r.streamStarted.IsZero() {
		r.StreamDuration = time.Since(r.streamStarted)
	}
}

// markSucceeded 以成功终态定稿请求。
func (r *RequestState) markSucceeded(responseBody string, accounting *UsageAccounting) {
	mu.Lock()
	defer mu.Unlock()

	r.Status = StatusSuccess
	r.Error = ""
	r.responseBody = responseBody
	r.finishLocked(accounting)
}

// markFailed 以失败终态定稿请求, 最终错误取自本次失败原因。
func (r *RequestState) markFailed(err error, responseBody string, accounting *UsageAccounting) {
	mu.Lock()
	defer mu.Unlock()

	if r.requestCtx.Err() != nil {
		r.Status = StatusCanceled
		r.Error = r.requestCtx.Err().Error()
	} else {
		r.Status = StatusFailed
		r.Error = err.Error()
	}
	if responseBody != "" {
		r.responseBody = responseBody
	}
	r.finishLocked(accounting)
}

// markCanceled 以取消终态定稿请求, 用于客户端提前断开或主动取消。
func (r *RequestState) markCanceled(err error, responseBody string, accounting *UsageAccounting) {
	mu.Lock()
	defer mu.Unlock()

	r.Status = StatusCanceled
	r.Error = err.Error()
	if responseBody != "" {
		r.responseBody = responseBody
	}
	r.finishLocked(accounting)
}

// finishLocked 写入用量和费用, 发布终态, 更新请求级统计并裁剪历史; 调用方必须持有锁。
func (r *RequestState) finishLocked(accounting *UsageAccounting) {
	r.Sending = false
	r.roundCancel = nil
	if r.requestCancel != nil {
		r.requestCancel()
	}
	r.requestCancel = nil
	if accounting != nil {
		r.Usage = accounting.Usage
	}
	var metrics model.StatsMetrics
	var resolution op.PriceResolution
	if accounting != nil {
		metrics = accounting.Metrics
		resolution = accounting.Resolution
	} else {
		resolution = op.PriceResolution{CostSource: "unknown"}
	}
	if resolution.CostKnown {
		cost := metrics.InputCost + metrics.OutputCost
		r.Cost = &cost
	} else {
		r.Cost = nil
	}
	r.CostKnown = resolution.CostKnown
	r.CostSource = resolution.CostSource
	r.CostReferenceModel = resolution.CostReferenceModel
	r.Duration = time.Since(r.StartedAt)
	metrics.WaitTime = r.Duration.Milliseconds()
	if r.Status == StatusSuccess {
		metrics.RequestSuccess = 1
	} else {
		metrics.RequestFailed = 1
	}
	_ = op.StatsTotalUpdate(metrics)
	_ = op.StatsHourlyUpdate(metrics)
	_ = op.StatsDailyUpdate(context.Background(), metrics)
	if r.apiKeyID > 0 {
		_ = op.StatsAPIKeyUpdate(r.apiKeyID, metrics)
	}
	publishRequestLocked(r)

	finished := 0
	oldest := uint64(0)
	for id, request := range requests {
		if request.Status == StatusRunning || request.Status == StatusCommitted {
			continue
		}
		finished++
		if oldest == 0 || id < oldest {
			oldest = id
		}
	}
	if finished > maxFinished {
		delete(requests, oldest)
	}
}

// UsageAccounting 是一次用量费用计算的完整快照, 渠道统计与请求级记账复用同一结果, 避免重复查价。
type UsageAccounting struct {
	Usage      llm.Usage          // 统一用量。
	Metrics    model.StatsMetrics // 按单价转换的 Token 与费用统计。
	Resolution op.PriceResolution // 价格解析结果(manual/actual/group/unknown)。
}

// computeUsageAccounting 一次性解析价格并计算费用, 供渠道统计与 finishLocked 复用。
// usage 为 nil 时返回 nil, finishLocked 据此设 cost=null + unknown 元数据。
func computeUsageAccounting(actualModel, groupModel string, usage *llm.Usage) *UsageAccounting {
	if usage == nil {
		return nil
	}
	accounting := &UsageAccounting{Usage: *usage}
	accounting.Metrics = model.StatsMetrics{InputToken: usage.PromptTokens, OutputToken: usage.CompletionTokens}
	accounting.Resolution = op.ResolveUsagePrice(actualModel, groupModel)
	if !accounting.Resolution.CostKnown {
		return accounting
	}
	price := accounting.Resolution.Price
	cachedTokens, writeCachedTokens := int64(0), int64(0)
	if usage.PromptTokensDetails != nil {
		cachedTokens = usage.PromptTokensDetails.CachedTokens
		writeCachedTokens = usage.PromptTokensDetails.WriteCachedTokens
	}
	inputTokens := max(int64(0), usage.PromptTokens-cachedTokens-writeCachedTokens)
	accounting.Metrics.InputCost = (float64(inputTokens)*price.Input + float64(cachedTokens)*price.CacheRead + float64(writeCachedTokens)*price.CacheWrite) / 1_000_000
	accounting.Metrics.OutputCost = float64(usage.CompletionTokens) * price.Output / 1_000_000
	return accounting
}

// publishRequestLocked 非阻塞发布最新请求状态, 连接拥塞时关闭它并交给客户端重连获取全量快照; 调用方必须持有锁。
func publishRequestLocked(request *RequestState) {
	for stream := range watchers {
		select {
		case stream <- *request:
		default:
			delete(watchers, stream)
			close(stream)
		}
	}
}

// OpenRequestStream 注册请求状态流连接, 返回按请求 ID 倒序的全部快照和后续增量通道。
// 日志页不提供排序开关, 而 requests 是 map, 遍历顺序随机, 故顺序须由此处定稿。
func OpenRequestStream() ([]RequestState, chan RequestState) {
	mu.Lock()
	defer mu.Unlock()

	stream := make(chan RequestState, streamBuffer)
	watchers[stream] = struct{}{}

	snapshot := make([]RequestState, 0, len(requests))
	for _, request := range requests {
		snapshot = append(snapshot, *request)
	}
	sort.Slice(snapshot, func(i, j int) bool { return snapshot[i].ID > snapshot[j].ID })
	return snapshot, stream
}

// CloseRequestStream 注销并关闭指定请求状态流连接。
func CloseRequestStream(stream chan RequestState) {
	mu.Lock()
	defer mu.Unlock()

	if _, exists := watchers[stream]; exists {
		delete(watchers, stream)
		close(stream)
	}
}

// RequestBody 返回指定请求保存的原始请求体, 记录不存在时返回空串。
func RequestBody(id uint64) string {
	mu.Lock()
	defer mu.Unlock()

	if request := requests[id]; request != nil {
		return request.requestBody
	}
	return ""
}

// ResponseBody 返回指定请求当前保存的响应体, 记录不存在或响应未完成时返回空串。
func ResponseBody(id uint64) string {
	mu.Lock()
	defer mu.Unlock()

	if request := requests[id]; request != nil {
		return request.responseBody
	}
	return ""
}

// Clear 删除全部已结束的请求记录。
func Clear() {
	mu.Lock()
	defer mu.Unlock()

	for id, request := range requests {
		if request.Status != StatusRunning && request.Status != StatusCommitted {
			delete(requests, id)
		}
	}
}
