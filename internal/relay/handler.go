package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/gin-contrib/sse"
	"github.com/gin-gonic/gin"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer"
	"github.com/looplj/axonhub/llm/transformer/anthropic"
	"github.com/looplj/axonhub/llm/transformer/openai"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
	"github.com/tidwall/sjson"
)

// Forward 按客户端协议承载一个请求的完整转发过程: 解析请求, 定位分组, 循环选目标请求上游, 直至提交响应或请求结束。
// afterPickHook 是选路后、授权/渠道复核前的测试注入点; 生产为 nil, 不影响热路径。
// 测试在此注入渠道禁用等竞态场景, 验证 handler 在快照与查询之间发现不可用时立即跳过。
var afterPickHook func(group model.Group, item model.GroupItem)

func Forward(format llm.APIFormat) gin.HandlerFunc {
	// 客户端协议同时定出入站转换器和请求协议位: 后者随请求状态推给界面, 也是每轮选择上游协议的首选。
	var inbound transformer.Inbound
	requestProtocol := model.ProtocolOpenAIChatCompletion
	switch format {
	case llm.APIFormatOpenAIResponse:
		inbound = responses.NewInboundTransformer()
		requestProtocol = model.ProtocolOpenAIResponse
	case llm.APIFormatAnthropicMessage:
		inbound = anthropic.NewInboundTransformer()
		requestProtocol = model.ProtocolAnthropicMessage
	default:
		inbound = openai.NewInboundTransformer()
	}

	return func(c *gin.Context) {
		// 完整读取客户端请求, 正文先登记到请求状态, 后续每轮直接改写为当前目标请求。
		raw, err := httpclient.ReadHTTPRequest(c.Request)
		if err != nil {
			rejectRequest(c, inbound, err)
			return
		}

		// 此处只读取选组和分流所需字段; 完整协议校验由同协议上游或跨协议 pipeline 完成。
		var metadata struct {
			Model     string `json:"model"`  // 客户端请求的分组名称。
			Streaming bool   `json:"stream"` // 客户端是否请求流式响应。
		}
		if err := json.Unmarshal(raw.Body, &metadata); err != nil {
			rejectRequest(c, inbound, err)
			return
		}

		// API Key 限定了模型范围时只放行范围内的模型, 为空表示不限制。
		if allowed, ok := c.Get("supported_models"); ok {
			if names, _ := allowed.([]string); len(names) > 0 && !slices.Contains(names, metadata.Model) {
				rejectRequest(c, inbound, errors.New("model not supported by this api key"))
				return
			}
		}

		// 客户端请求的模型名称即分组名称; 分组不存在说明模型名错误, 等待也不会出现该分组。
		// 分组主键随请求状态一并登记, 界面由此可直接按主键取分组而不必按名称回查。
		group, err := op.GroupGetByName(metadata.Model)
		if err != nil {
			rejectRequest(c, inbound, errors.New("model not found"))
			return
		}

		// 登记进程内请求状态, 返回的记录是后续全部状态写入和前端可视化推送的入口。
		request := newRequestState(c.Request.Context(), metadata.Model, group.ID, requestProtocol, string(raw.Body), c.GetInt("api_key_id"))
		ctx := request.requestCtx
		failedItemID := 0              // 当前累计连续失败次数的成员 ID。
		failures := 0                  // 该成员包含首次请求的连续失败次数。
		excluded := make(map[int]bool) // 本请求内不再选择的成员: 已跨网络边界尝试过或已确认本地不可用。

		// 本地不可用连续计数: 超过成员数时说明缓存与数据库稳定不一致, 终止而非空转。
		localUnavailableStreak := 0
		for {
			if ctx.Err() != nil {
				request.markCanceled(ctx.Err(), "", nil)
				return
			}

			// 分组配置和成员随时可改, 故每轮重新读取; 分组被删除时等待它重新出现。
			group, err = op.GroupGetByName(metadata.Model)
			if err != nil {
				if !request.wait(ctx, model.DefaultGroupRelayConfig().MemberRetryIntervalSeconds) {
					return
				}
				continue
			}

			// 手动模式取人工指定的成员, 故障转移模式按优先级选择未禁用且不在冷却中的成员,
			// 评分模式在可用成员里选分数最高且本请求未排除的。
			// 无目标时: 手动模式立即失败; 全员不可用(含空分组)立即失败;
			// 仅故障转移存在冷却中的可用成员时等待, 让探测或冷却到期后继续。
			var item model.GroupItem
			var routeEpoch uint64
			if group.Mode == model.GroupModeScored {
				item, routeEpoch = pickScoredItem(group, excluded)
			} else {
				item, routeEpoch = pickGroupItem(group, excluded)
			}
			if item.ID == 0 {
				// 手动模式: 指定成员不可用时立即失败, 不自动重选也不等待,
				// 即使其他成员可用也不切换(手动模式的核心语义)。
				if group.Mode == model.GroupModeManual {
					failure := errors.New("manual active member unavailable")
					request.markFailed(failure, "", nil)
					rejectRequest(c, inbound, failure)
					return
				}
				// 全员本地不可用(含空分组)时立即失败: 禁用/缺凭据不会自愈,
				// 等待只会对同一批不可用成员形成无界循环。
				// 故障转移模式存在冷却中的可用成员时仍走等待, 让冷却到期后请求继续。
				anyAvailable := false
				for _, m := range group.Items {
					if m.Available {
						anyAvailable = true
						break
					}
				}
				if !anyAvailable {
					failure := errors.New("no available group member")
					request.markFailed(failure, "", nil)
					rejectRequest(c, inbound, failure)
					return
				}
				// 评分模式: 可用成员被本请求排除后全部试完, 走已有的终态。
				if group.Mode == model.GroupModeScored && len(excluded) > 0 {
					failure := errors.New("all group members failed")
					request.markFailed(failure, "", nil)
					rejectRequest(c, inbound, failure)
					return
				}
				if !request.wait(ctx, group.RelayConfig.MemberRetryIntervalSeconds) {
					return
				}
				continue
			}

			// 测试注入点: 选路已选中 item 但尚未做授权/渠道复核。
			// 注入渠道禁用等竞态, 验证复核时发现不可用即立即跳过。
			if afterPickHook != nil {
				afterPickHook(group, item)
			}

			// 持共享读锁从最新分组快照复核成员: 变更编排持写锁期间, 此处读到的是已发布的最新状态。
			// 成员被禁用/删除/移除后, 快照不再含它或 Available=false → 本地不可用, 不发送请求。
			// 读锁在网络发送前释放, 已派发的请求不受后续禁用影响。
			groupGate.RLock()
			latestGroup, latestErr := op.GroupGetByName(metadata.Model)
			if latestErr != nil || latestGroup.ID != group.ID {
				groupGate.RUnlock()
				abandonRoundBeforeDispatch(group, item.ID, routeEpoch)
				localUnavailableStreak++
				if localUnavailableStreak > len(group.Items) {
					failure := errors.New("no available group member (group changed during dispatch)")
					request.markFailed(failure, "", nil)
					rejectRequest(c, inbound, failure)
					return
				}
				if group.Mode == model.GroupModeScored {
					excluded[item.ID] = true
				}
				continue
			}
			var latestItem model.GroupItem
			for _, m := range latestGroup.Items {
				if m.ID == item.ID {
					latestItem = m
					break
				}
			}
			if latestItem.ID == 0 || !latestItem.Available || !latestItem.Enabled {
				// 成员在快照与复核之间被禁用或删除: 本地不可用, 不计任何统计/冷却/分数。
				groupGate.RUnlock()
				abandonRoundBeforeDispatch(group, item.ID, routeEpoch)
				localUnavailableStreak++
				if localUnavailableStreak > len(group.Items) {
					failure := errors.New("no available group member (cache inconsistency)")
					request.markFailed(failure, "", nil)
					rejectRequest(c, inbound, failure)
					return
				}
				if group.Mode == model.GroupModeScored {
					excluded[item.ID] = true
				} else if group.Mode == model.GroupModeFailover {
					excluded[item.ID] = true
				}
				continue
			}
			item = latestItem
			group = latestGroup

			// 成员本地不可用(授权缺失/凭据停用/渠道被删或禁用): 发生在网络边界之前,
			// 对所有模式都不计失败、不写统计、不扣分、不冷却, 释放读锁后重新选路。
			// 手动模式重选后仍指向同一不可用成员 → 立即失败; 故障转移/评分排除该成员选下一个。
			grant, err := op.ChannelGrantGet(item.ChannelGrantID)
			if err != nil {
				groupGate.RUnlock()
				abandonRoundBeforeDispatch(group, item.ID, routeEpoch)
				localUnavailableStreak++
				if localUnavailableStreak > len(group.Items) {
					failure := errors.New("no available group member (cache inconsistency)")
					request.markFailed(failure, "", nil)
					rejectRequest(c, inbound, failure)
					return
				}
				if group.Mode == model.GroupModeScored || group.Mode == model.GroupModeFailover {
					excluded[item.ID] = true
				}
				if group.Mode == model.GroupModeManual {
					failure := errors.New("manual active member unavailable")
					request.markFailed(failure, "", nil)
					rejectRequest(c, inbound, failure)
					return
				}
				continue
			}
			channelModel := grant.ChannelModel
			channelKey := grant.ChannelKey

			// 渠道在快照与查询之间被禁用时同样为成员本地不可用: 不发送请求, 不计任何统计/冷却/分数。
			channel, err := op.ChannelGet(channelModel.ChannelID)
			if err != nil || !channel.Enabled {
				groupGate.RUnlock()
				abandonRoundBeforeDispatch(group, item.ID, routeEpoch)
				localUnavailableStreak++
				if localUnavailableStreak > len(group.Items) {
					failure := errors.New("no available group member (cache inconsistency)")
					request.markFailed(failure, "", nil)
					rejectRequest(c, inbound, failure)
					return
				}
				if group.Mode == model.GroupModeScored || group.Mode == model.GroupModeFailover {
					excluded[item.ID] = true
				}
				if group.Mode == model.GroupModeManual {
					failure := errors.New("manual active member unavailable")
					request.markFailed(failure, "", nil)
					rejectRequest(c, inbound, failure)
					return
				}
				continue
			}
			localUnavailableStreak = 0

			// 将分组成员配置的真实模型写入本轮上游请求。
			raw.Body, err = sjson.SetBytes(raw.Body, "model", channelModel.Name)
			if err != nil {
				groupGate.RUnlock()
				abandonRoundBeforeDispatch(group, item.ID, routeEpoch)
				request.markFailed(err, "", nil)
				rejectRequest(c, inbound, err)
				return
			}
			// OpenAI Chat 流式响应需显式要求上游在末尾附带用量。
			if metadata.Streaming && format == llm.APIFormatOpenAIChatCompletion {
				raw.Body, err = sjson.SetBytes(raw.Body, "stream_options.include_usage", true)
				if err != nil {
					groupGate.RUnlock()
					abandonRoundBeforeDispatch(group, item.ID, routeEpoch)
					request.markFailed(err, "", nil)
					rejectRequest(c, inbound, err)
					return
				}
			}

			// 在渠道授权支持的协议内选出本轮上游协议, 按该协议的路径与授权绑定的凭据构造出站转换器。
			// 先于登记本轮目标: 选中的协议是本轮目标的一部分, 需与渠道和模型一并推给界面。
			outbound, targetProtocol, passthrough, err := buildOutbound(channel, grant, *channelKey, requestProtocol)
			if err != nil {
				// 协议构造失败发生在网络边界之前, 归为成员本地错误: 不计统计/冷却/分数, 不消耗上游尝试。
				// 手动模式立即失败; 故障转移/评分排除该成员选下一个。
				groupGate.RUnlock()
				abandonRoundBeforeDispatch(group, item.ID, routeEpoch)
				if group.Mode == model.GroupModeManual {
					failure := errors.New("manual active member unavailable")
					request.markFailed(failure, "", nil)
					rejectRequest(c, inbound, failure)
					return
				}
				excluded[item.ID] = true
				continue
			}

			// 本轮即将跨越网络边界请求上游: 登记为本请求已排除的成员, 后续轮次不再选它。
			excluded[item.ID] = true

			// 已从最新快照复核成员可用性并构造出站转换器, 请求即将派发: 释放读锁。
			// 释放后发生的禁用只影响后续请求, 不中断已派发的本轮请求。
			groupGate.RUnlock()

			// 为本轮上游调用建立独立取消入口并登记当前目标; 取消原因用于区分人工中止与响应超时。
			roundCtx, cancelRoundCause := context.WithCancelCause(ctx)
			// 人工中止和本轮完成都使用普通 canceled 原因, 超时回调则写入具体的超时错误。
			cancelRound := func() {
				cancelRoundCause(context.Canceled)
			}
			request.startRound(cancelRound, channel.Name, channelModel.Name, targetProtocol)

			roundStartedAt := time.Now() // 本轮上游调用的开始时间, 用于统计首个有效响应耗时。

			// 请求上游并等待首个有效响应: 非流式等待完整响应, 流式等待首个事件。
			// 同协议渠道原样直通, 跨协议渠道经转换后请求; 此时尚未写给客户端, 失败仍可换目标重试。
			var result *upstreamResponse
			if err == nil {
				timeoutSeconds := group.RelayConfig.MemberNonStreamResponseTimeoutSeconds // 非流式等待完整响应, 流式分支改为首事件超时。
				timeoutErr := errors.New("upstream non-stream response timeout")          // 具体错误用于区分超时与人工中止。
				if metadata.Streaming {
					timeoutSeconds = group.RelayConfig.MemberStreamFirstEventTimeoutSeconds
					timeoutErr = errors.New("upstream stream first event timeout")
				}
				// 计时器取消本轮上下文, 让正在等待 HTTP 响应或首个流事件的调用及时返回。
				timeoutTimer := time.AfterFunc(time.Duration(timeoutSeconds)*time.Second, func() {
					cancelRoundCause(timeoutErr)
				})
				// 客户端与渠道协议一致时直接透传, 其余组合通过 pipeline 转换。
				if passthrough {
					result, err = sendPassthrough(roundCtx, format, raw, channel, outbound, metadata.Streaming, channelModel.Name)
				} else {
					result, err = sendConverted(roundCtx, format, raw, channel, outbound, metadata.Streaming)
				}
				// 上游调用返回即结束首响应等待; Stop 失败说明已到期, 主动取消可避免等待异步回调完成。
				if !timeoutTimer.Stop() {
					cancelRoundCause(timeoutErr)
				}
				if context.Cause(roundCtx) == timeoutErr {
					err = timeoutErr
					// 超时与响应返回同时发生时舍弃尚未提交的流结果, 避免把超时误记为成功。
					if result != nil && result.events != nil {
						result.events.Close()
						if result.closeIdle != nil {
							result.closeIdle()
						}
					}
				}
			}

			if err != nil {
				// 记录本轮上游调用已经结束及其失败原因。
				request.finishRound(err.Error())
				// 父上下文结束说明客户端已经取消, 归还探测占用并以取消终态结束请求。
				if ctx.Err() != nil {
					releaseRouteProbe(group, item.ID, routeEpoch)
					request.markCanceled(ctx.Err(), "", nil)
					return
				}
				// 仅人工中止本轮时不计失败也不等待; 响应超时属于真实失败并消耗尝试次数。
				if context.Cause(roundCtx) == context.Canceled {
					releaseRouteProbe(group, item.ID, routeEpoch)
					continue
				}
				cancelRound()

				// 统一处理成员本地错误(网络边界之前): 不写统计/冷却/分数, 不消耗上游尝试。
				// 手动模式立即失败; 故障转移排除该成员选下一个; 评分模式排除该成员选下一个。
				if classifyRoundFailure(err) == roundOutcomeMemberLocal {
					abandonRoundBeforeDispatch(group, item.ID, routeEpoch)
					if group.Mode == model.GroupModeManual {
						failure := errors.New("manual active member unavailable")
						request.markFailed(failure, "", nil)
						rejectRequest(c, inbound, failure)
						return
					}
					excluded[item.ID] = true
					continue
				}

				// 评分模式按归因分类记账: 请求级与成员本地错误不写渠道统计也不扣分,
				// 只有真实跨越网络边界的失败才归因渠道并影响评分。
				if group.Mode == model.GroupModeScored {
					outcome := classifyRoundFailure(err)
					if outcome == roundOutcomeUpstreamFailure || outcome == roundOutcomeUpstreamAuth {
						metrics := model.StatsMetrics{WaitTime: time.Since(roundStartedAt).Milliseconds(), RequestFailed: 1}
						_ = op.ChannelStatsUpdate(channel.ID, metrics)
						_ = op.ChannelModelStatsUpdate(channelModel.ID, metrics)
						_ = op.ChannelKeyStatsUpdate(channelKey.ID, metrics)
					}
					switch outcome {
					case roundOutcomeRequestInvalid:
						request.markFailed(err, "", nil)
						rejectRequest(c, inbound, err)
						return
					case roundOutcomeMemberLocal:
						excluded[item.ID] = true
						continue
					default:
						recordScoredFailure(group, item.ID, routeEpoch, err)
						continue
					}
				}

				// 故障转移模式的既有记账路径, 行为不变。
				metrics := model.StatsMetrics{WaitTime: time.Since(roundStartedAt).Milliseconds(), RequestFailed: 1}
				_ = op.ChannelStatsUpdate(channel.ID, metrics)
				_ = op.ChannelModelStatsUpdate(channelModel.ID, metrics)
				_ = op.ChannelKeyStatsUpdate(channelKey.ID, metrics)

				// 成员改变时重新开始累计该成员在本请求内的连续失败次数。
				if failedItemID == item.ID {
					failures++
				} else {
					failedItemID = item.ID
					failures = 1
				}
				// 达到总尝试次数时成员进入冷却并立即重新选路, 否则等待后重试。
				if recordRouteFailure(group, item.ID, failures, routeEpoch) {
					continue
				}
				if !request.wait(ctx, group.RelayConfig.MemberRetryIntervalSeconds) {
					return
				}
				continue
			}
			// 记录本轮已经取得可提交的上游响应。
			request.finishRound("")
			// 请求级取消可能与上游成功同时到达, 此时不应提交响应或继续重试。
			if ctx.Err() != nil {
				releaseRouteProbe(group, item.ID, routeEpoch)
				request.markCanceled(ctx.Err(), "", computeUsageAccounting(channelModel.Name, request.Model, result.usage))
				return
			}
			roundWaitTime := time.Since(roundStartedAt).Milliseconds() // 流式响应只统计等待首帧的时间。
			// 上游成功后解除该成员的冷却与探测占用, 并按路由配置开始亲和。
			recordRouteSuccess(group, item.ID, routeEpoch)
			// 同协议透传时原样返回上游响应头; 跨协议响应没有需要透传的响应头。
			for key, values := range result.header {
				c.Writer.Header()[key] = values
			}

			// 非流式响应已经完整取得, 提交后一次写给客户端。
			if !metadata.Streaming {
				cancelRound()
				if c.Writer.Header().Get("Content-Type") == "" {
					c.Header("Content-Type", "application/json")
				}
				// 非流式响应已有完整用量, 一次性计算费用快照, 渠道统计与请求级记账复用。
				// usage 可能为 nil(上游错误等无用量场景), 此时 accounting 为 nil, 用零值 metrics 统计。
				accounting := computeUsageAccounting(channelModel.Name, request.Model, result.usage)
				var metrics model.StatsMetrics
				if accounting != nil {
					metrics = accounting.Metrics
				}
				metrics.WaitTime = roundWaitTime
				metrics.RequestSuccess = 1
				_ = op.ChannelStatsUpdate(channel.ID, metrics)
				_ = op.ChannelModelStatsUpdate(channelModel.ID, metrics)
				_ = op.ChannelKeyStatsUpdate(channelKey.ID, metrics)
				// 评分模式: 非流式响应完整取得即上游完整成功, 此后客户端写失败不改变该成员的健康结论。
				recordScoredSuccess(group, item.ID, routeEpoch)
				request.markCommitted(false)
				n, err := c.Writer.Write(result.body)
				if err == nil && n != len(result.body) {
					err = io.ErrShortWrite
				}
				if err != nil {
					if ctx.Err() != nil {
						request.markCanceled(ctx.Err(), string(result.body), accounting)
					} else {
						request.markFailed(err, string(result.body), accounting)
					}
					return
				}
				request.markSucceeded(string(result.body), accounting)
				return
			}

			// 首帧提交后仍需逐个事件判断协议终态: 上游发出结束事件后未必立即关闭响应体, 继续读取会一直阻塞到
			// 客户端断开, 从而把已完整交付的响应误判为 context canceled。
			if c.Writer.Header().Get("Content-Type") == "" {
				c.Header("Content-Type", "text/event-stream")
			}
			var encoded bytes.Buffer
			var chunks []*httpclient.StreamEvent
			event := result.first
			last := result.last // 已转发的最后一个事件是否已按客户端协议结束整个响应流。
			committed := false
			upstreamFault := false // 失败是否归因上游: 读流中断或结束事件携带的失败为真, 本地编码与客户端写失败为假。
			for {
				if event != nil {
					chunks = append(chunks, event)
					// 每个事件计一个输出字符供日志页展示, 按节流间隔发布。
					request.addOutput()
					encoded.Reset()
					if encodeErr := sse.Encode(&encoded, sse.Event{Id: event.LastEventID, Event: event.Type, Data: event.Data}); encodeErr != nil {
						err = encodeErr
						break
					}
					if !committed {
						request.markCommitted(true)
						committed = true
					}
					n, writeErr := c.Writer.Write(encoded.Bytes())
					if writeErr == nil && n != encoded.Len() {
						writeErr = io.ErrShortWrite
					}
					if writeErr != nil {
						err = writeErr
						break
					}
					c.Writer.Flush()
				}
				if last {
					break
				}
				if !result.events.Next() {
					err = result.events.Err()
					upstreamFault = true // 读流中断属于上游侧结果; err 为空时本标志不会被消费。
					break
				}
				event = result.events.Current()
				// 已提交的响应不能再换目标重试, 结束事件自身携带的失败原样转发给客户端, 并在转发后作为本请求终态。
				last, err = inspectStreamEvent(format, event)
				if err != nil {
					upstreamFault = true
				}
			}
			// 评分模式严格要求观察到协议成功终态: 已提交内容后流在无终态处结束属于上游截断,
			// 记为上游失败并终结请求, 不再换路; 其余模式沿用历史语义。
			if err == nil && !last && group.Mode == model.GroupModeScored {
				err = errors.New("upstream stream ended without terminal event")
				upstreamFault = true
			}
			request.finishStream()
			result.events.Close()
			// 事件流已读完, 渠道专用代理的独占连接池到此归还。
			if result.closeIdle != nil {
				result.closeIdle()
			}
			cancelRound()
			// 使用客户端协议转换器聚合已转发事件, 统一取得最终响应正文和用量。
			responseBody, meta, aggregateErr := inbound.AggregateStreamChunks(context.WithoutCancel(ctx), chunks)
			if aggregateErr == nil {
				result.usage = meta.Usage
			}
			// 评分模式下聚合失败即请求失败且不记满分: 已提交的响应无法重取, 只能以失败定稿;
			// 聚合发生在本地, 属转换问题, 不归因上游故不扣分。其余模式沿用忽略聚合错误的历史语义。
			if aggregateErr != nil && err == nil && group.Mode == model.GroupModeScored {
				err = aggregateErr
			}
			// 流式响应结束并聚合出用量后, 一次性计算费用快照, 渠道统计与请求级记账复用。
			// usage 可能为 nil(上游 401 等无用量场景), 此时 accounting 为 nil, 用零值 metrics 统计失败次数。
			accounting := computeUsageAccounting(channelModel.Name, request.Model, result.usage)
			var metrics model.StatsMetrics
			if accounting != nil {
				metrics = accounting.Metrics
			}
			metrics.WaitTime = roundWaitTime
			if err == nil {
				metrics.RequestSuccess = 1
			} else {
				metrics.RequestFailed = 1
			}
			_ = op.ChannelStatsUpdate(channel.ID, metrics)
			_ = op.ChannelModelStatsUpdate(channelModel.ID, metrics)
			_ = op.ChannelKeyStatsUpdate(channelKey.ID, metrics)
			if err != nil {
				// 评分记账内部会忽略客户端取消与本地错误, 终态判定不受影响。
				recordCommittedStreamFailure(group, item.ID, routeEpoch, err, upstreamFault, ctx.Err() != nil)
				if ctx.Err() != nil {
					request.markCanceled(ctx.Err(), string(responseBody), accounting)
					return
				}
				request.markFailed(err, string(responseBody), accounting)
				return
			}
			// 评分模式: 流式响应完整交付客户端才算完整成功。
			recordScoredSuccess(group, item.ID, routeEpoch)
			request.markSucceeded(string(responseBody), accounting)
			return
		}
	}
}

// rejectRequest 以客户端协议的错误格式返回请求级失败, 用于尚未登记状态因而无需定稿的请求。
func rejectRequest(c *gin.Context, inbound transformer.Inbound, err error) {
	response := inbound.TransformError(c.Request.Context(), &llm.ResponseError{
		StatusCode: http.StatusBadRequest,
		Detail:     llm.ErrorDetail{Message: err.Error(), Type: "invalid_request_error"},
	})
	c.Data(response.StatusCode, "application/json", response.Body)
	c.Abort()
}
