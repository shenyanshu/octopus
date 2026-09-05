package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/price"
	"github.com/bestruirui/octopus/internal/relay"
	"github.com/bestruirui/octopus/internal/rhttp"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/server/router"
	"github.com/charmbracelet/log"
	"github.com/dlclark/regexp2"
	"github.com/gin-gonic/gin"
)

func init() {
	router.NewGroupRouter("/api/v1/channel").
		Use(middleware.Auth()).
		Use(middleware.RequireJSON()).
		AddRoute(
			router.NewRoute("/detail/:id", http.MethodGet).
				Handle(getChannelDetail),
		).
		AddRoute(
			router.NewRoute("/stats", http.MethodGet).
				Handle(listChannelStats),
		).
		AddRoute(
			router.NewRoute("/grants", http.MethodGet).
				Handle(listChannelGrant),
		).
		AddRoute(
			router.NewRoute("/grants/preview", http.MethodPost).
				Handle(previewChannelGrants),
		).
		AddRoute(
			router.NewRoute("/create", http.MethodPost).
				Handle(createChannel),
		).
		AddRoute(
			router.NewRoute("/update", http.MethodPost).
				Handle(updateChannel),
		).
		AddRoute(
			router.NewRoute("/enable", http.MethodPost).
				Handle(enableChannel),
		).
		AddRoute(
			router.NewRoute("/delete/:id", http.MethodDelete).
				Handle(deleteChannel),
		).
		AddRoute(
			router.NewRoute("/fetch-model", http.MethodPost).
				Handle(fetchModel),
		)
}

// getChannelDetail 返回单个渠道的完整配置, 供编辑表单打开时读取。
// 与列表分开: 整份配置带着路径, 代理与凭据明文, 只有正在编辑的那一个渠道用得上。
func getChannelDetail(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
		return
	}
	detail, err := op.ChannelDetailGet(id)
	if err != nil {
		resp.Error(c, http.StatusNotFound, err.Error())
		return
	}
	resp.Success(c, detail)
}

// listChannelStats 返回全部渠道及其模型的累计统计, 也是渠道列表页的数据来源。
// 不带整份配置: 统计每次转发都在变, 界面按更短的间隔刷新它, 而路径, 代理与凭据明文只在编辑时用得上。
func listChannelStats(c *gin.Context) {
	resp.Success(c, op.ChannelStatsList())
}

// listChannelGrant 返回全部渠道授权候选, 供分组页选取成员。
func listChannelGrant(c *gin.Context) {
	resp.Success(c, op.ChannelGrantCandidates())
}

func createChannel(c *gin.Context) {
	var req model.ChannelDetail
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	// 新建渠道可能被现有分组成员引用(授权引用渠道), 与 Forward 复核的读路径共享锁。
	relay.GroupGateLock()
	defer relay.GroupGateUnlock()
	channel, mutation, err := op.ChannelCreate(&req, c.Request.Context())
	// 已提交事实先校正缓存与路由, 再按受影响分组发布 SSE, 最后上报错误;
	// 提交前失败 mutation 为 nil, 无事可校正也无事件可发。整个序列在 groupGate 写锁内。
	processChannelMutation(mutation)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	if err := addChannelModelPrices(channel.Models, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, channel)
}

func updateChannel(c *gin.Context) {
	var req model.ChannelDetail
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	if req.ID == 0 {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
		return
	}
	// 渠道变更与分组变更共享读写锁: 持写锁覆盖 DB→缓存→路由→SSE 的完整序列,
	// 使 Forward 选路后的复核读到最新已发布状态, 避免级联删除的旧缓存穿透。
	relay.GroupGateLock()
	defer relay.GroupGateUnlock()
	// 渠道全量替换会删除未列出的凭据、模型与授权, 经外键级联删除分组成员;
	// 启用自动补充规则的分组在同一事务内补入本渠道匹配授权, mutation 同时携带新增与删除两类事实。
	// 提交前失败 mutation 为 nil, 无从也无需校正; 提交后失败(缓存刷新失败)时携带提交事实,
	// 级联删除不可回滚, 必须先按事实校正路由并发布事件再报错, 否则已打开客户端会等到下次拉取才对齐。
	channel, mutation, err := op.ChannelUpdate(&req, c.Request.Context())
	processChannelMutation(mutation)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	if err := addChannelModelPrices(channel.Models, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	if err := op.LLMCleanupGhosts(c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, channel)
}

func enableChannel(c *gin.Context) {
	var request struct {
		ID      int  `json:"id"`
		Enabled bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	// 渠道启停直接影响成员 Available, 与 Forward 复核的读路径共享锁使状态发布线性化。
	relay.GroupGateLock()
	defer relay.GroupGateUnlock()
	mutation, err := op.ChannelEnabled(request.ID, request.Enabled, c.Request.Context())
	// 已提交事实先校正缓存与路由, 再发布 SSE, 最后上报错误; 与 create/update 同一编排。
	processChannelMutation(mutation)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, nil)
}

func deleteChannel(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
		return
	}
	// 与 updateChannel 同一读写锁: 级联删除的缓存与路由校正不被 Forward 读路径穿透。
	relay.GroupGateLock()
	defer relay.GroupGateUnlock()
	// 删除渠道会经外键级联删除其授权与引用它的分组成员; mutation 语义与更新入口相同。
	mutation, err := op.ChannelDel(id, c.Request.Context())
	// 已提交事实先校正缓存与路由, 再按受影响分组发布 SSE, 最后上报错误; 与 create/update/enable 同一编排。
	processChannelMutation(mutation)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	if err := op.LLMCleanupGhosts(c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, nil)
}

// processChannelMutation 是渠道写操作 mutation 的统一编排:
// 先按提交事实校正缓存与路由, 再按受影响分组发布 SSE。
// 所有渠道 handler(create/update/enable/delete)在拿到 op 返回的 mutation 后调用本函数,
// 使"已提交即通知"这一契约不依赖客户端重连 —— 提交前失败 mutation 为 nil 时无事可校正也无事件可发。
// 必须在 groupGate 写锁内调用: 校正与发布共享缓存与路由状态。
func processChannelMutation(mutation *op.ChannelMutation) {
	reconcileChannelMutation(mutation)
	publishGroupMutationEvent(mutation)
}

// reconcileChannelMutation 按渠道写操作已提交的级联事实精确校正受影响分组的路由状态。
// 只动成员集合实际变化的分组, 未受影响的分组不前进代数, 免得误丢其合法的迟到记账。
func reconcileChannelMutation(mutation *op.ChannelMutation) {
	if mutation == nil {
		return
	}
	// 先按提交事实校正分组缓存, 再校正路由状态: 提交后刷新失败时缓存陈旧,
	// 不校正缓存的话, 下一次请求会从陈旧缓存里选到已删成员或漏掉新成员。
	op.ApplyGroupMemberDeltas(mutation.GroupDeltas)
	for _, delta := range mutation.GroupDeltas {
		// 纯新增不前进代数: 路由代数前进会丢弃该分组全部在途结果, 误报会白白丢失合法迟到记账;
		// 只有确有成员删除时才修剪路由, 新增成员由后续选路自然纳入, 评分/活跃成员一律不动。
		if delta.Removed {
			relay.PruneRouteMembers(delta.GroupID, delta.ItemIDs)
		}
	}
}

// publishGroupMutationEvent 为受渠道变更影响的分组发布 changed 事件, 复用现有 SSE 事件不另立协议。
// handler 已持 GroupGate 写锁: 事件携带的运行状态取自 Relay 与刚校正的分组缓存, 与其他会话对齐到完整成员。
// GroupGet 读不到时不可静默: 已提交的成员变更不会因读取失败而回滚, 必须带上下文上报而不是吞掉;
// 此处不重试也不 panic —— 仅记录错误上下文, 事件本身被跳过, 后续校正依赖已发布的缓存校正事实。
func publishGroupMutationEvent(mutation *op.ChannelMutation) {
	if mutation == nil {
		return
	}
	for _, delta := range mutation.GroupDeltas {
		group, err := op.GroupGet(delta.GroupID)
		if err != nil {
			// 不写不真实的"并发删除"理由: 读不到就是读不到, 提交已发生不可回滚。
			// 记录带 groupID 的错误上下文, 让运维可定位为何某受影响组的事件漏发;
			// 事件本身跳过, 缓存已由 reconcileChannelMutation 校正, 客户端下次拉取即对齐。
			log.Warnf("publishGroupMutationEvent: skip group %d, group read failed after committed mutation: %v", delta.GroupID, err)
			continue
		}
		publishGroupEvent(groupEvent{Name: "changed", Data: groupResponse{Group: group, Runtime: relay.RouteStateOf(group)}})
	}
}

// addChannelModelPrices 为渠道模型匹配校准价格，并批量写入尚不存在的价格记录。
func addChannelModelPrices(modelNames []string, ctx context.Context) error {
	seen := make(map[string]struct{}, len(modelNames))
	llmInfos := make([]model.LLMInfo, 0, len(modelNames))
	for _, modelName := range modelNames {
		modelName = strings.ToLower(modelName)
		if _, ok := seen[modelName]; ok {
			continue
		}
		seen[modelName] = struct{}{}
		llmInfo := model.LLMInfo{Name: modelName}
		if modelPrice := price.GetLLMPrice(modelName); modelPrice != nil {
			llmInfo.LLMPrice = *modelPrice
		}
		llmInfos = append(llmInfos, llmInfo)
	}
	return op.LLMBatchCreate(llmInfos, ctx)
}

// fetchModel 按提交的渠道配置与凭据拉取上游模型列表, 并按过滤表达式筛选后返回。
// 同时探测 OpenAI 与 Anthropic 两侧, 谁返回了哪些模型, 就给对应协议位打勾: 协议支持由探测结果决定, 无需用户声明。
// OpenAI 侧记为 Responses 而不是 Chat: Chat Completions 已被官方标记弃用, 新渠道应默认走 Responses,
// 仍需 Chat 的渠道由用户在界面上手动勾选。两侧的 /models 地址与认证形态不同, 故必须分别探测:
// 单协议上游只有一侧会成功, "哪侧成功" 本身就是协议支持的证据。
// 只有两侧都失败才算失败; 一侧失败属正常情况, 单协议上游本就只有一侧讲得通, 按成功那侧的结果返回。
func fetchModel(c *gin.Context) {
	var request model.ChannelFetchModelRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	ctx := c.Request.Context()
	// 探测收的是尚未落库的提交配置, 不经 normalizeChannelConfig, 故在此自行去空白;
	// 其中只有地址是硬需求: 渠道尚未命名时也可试拉, 故名称不在此校验。
	target := request.Channel
	target.BaseURL = strings.TrimSpace(target.BaseURL)
	target.ChannelProxy = strings.TrimSpace(target.ChannelProxy)
	target.MatchRegex = strings.TrimSpace(target.MatchRegex)
	if target.BaseURL == "" {
		resp.Error(c, http.StatusBadRequest, "channel base url is required")
		return
	}

	var httpClient *http.Client
	var err error
	switch {
	case !target.Proxy:
		httpClient, err = rhttp.Direct()
	case target.ChannelProxy == "":
		httpClient, err = rhttp.Proxy()
	default:
		httpClient, err = rhttp.New(target.ChannelProxy)
		// 渠道专用代理的客户端不再共享, 探测完就得关掉空闲连接; 探测收的是未落库的输入, 留着也无从复用。
		if httpClient != nil {
			defer httpClient.CloseIdleConnections()
		}
	}
	if err != nil {
		resp.Error(c, http.StatusBadGateway, err.Error())
		return
	}

	var openaiModels, anthropicModels []string
	var openaiErr, anthropicErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		openaiModels, openaiErr = fetchOpenAIModels(httpClient, ctx, target, request.Key, modelsURL(target.BaseURL, target.OpenAIResponsePath))
	}()
	go func() {
		defer wg.Done()
		anthropicModels, anthropicErr = fetchAnthropicModels(httpClient, ctx, target, request.Key, modelsURL(target.BaseURL, target.AnthropicMessagePath))
	}()
	wg.Wait()

	if openaiErr != nil && anthropicErr != nil {
		// 上游鉴权失败或地址不通属于调用方配置问题, 按 502 返回并带上上游原文, 便于在界面上直接看到原因。
		resp.Error(c, http.StatusBadGateway, fmt.Sprintf("openai: %v; anthropic: %v", openaiErr, anthropicErr))
		return
	}

	var re *regexp2.Regexp
	if target.MatchRegex != "" {
		if re, err = regexp2.Compile(target.MatchRegex, regexp2.ECMAScript); err != nil {
			resp.Error(c, http.StatusBadRequest, err.Error())
			return
		}
	}

	// 两侧结果按名称合并成一份有序集合: 同名模型在两侧都出现时, 协议位取并集。
	// 保持首次出现的顺序, 界面上模型的排列才与上游返回的一致;
	// 先并入 OpenAI 再并入 Anthropic, 顺序写死而不用 map 遍历, 否则界面上的模型排列会随每次刷新变化。
	protocolsByModel := make(map[string]model.Protocol, len(openaiModels)+len(anthropicModels))
	order := make([]string, 0, len(openaiModels)+len(anthropicModels))
	for _, name := range openaiModels {
		if re != nil {
			matched, err := re.MatchString(name)
			if err != nil {
				resp.Error(c, http.StatusBadRequest, err.Error())
				return
			}
			if !matched {
				continue
			}
		}
		if _, ok := protocolsByModel[name]; !ok {
			order = append(order, name)
		}
		protocolsByModel[name] |= model.ProtocolOpenAIResponse
	}
	for _, name := range anthropicModels {
		if re != nil {
			matched, err := re.MatchString(name)
			if err != nil {
				resp.Error(c, http.StatusBadRequest, err.Error())
				return
			}
			if !matched {
				continue
			}
		}
		if _, ok := protocolsByModel[name]; !ok {
			order = append(order, name)
		}
		protocolsByModel[name] |= model.ProtocolAnthropicMessage
	}

	models := make([]model.ChannelFetchModel, 0, len(order))
	for _, name := range order {
		models = append(models, model.ChannelFetchModel{Name: name, Protocols: protocolsByModel[name]})
	}
	resp.Success(c, models)
}

// modelsURL 取协议请求路径的父级目录, 与地址拼成同级的 /models 地址。
// 例如 /v1/chat/completions 与 /v1/messages 都得到 /v1/models, /chat/completions 得到 /models。
func modelsURL(baseURL, protocolPath string) string {
	parent := path.Dir(strings.TrimRight(protocolPath, "/"))
	// Anthropic 的 /v1/messages 只有一层, 父级即 /v1; Chat 的 /v1/chat/completions 需要再上一层。
	if strings.HasSuffix(parent, "/chat") {
		parent = path.Dir(parent)
	}
	if parent == "." || parent == "/" {
		parent = ""
	}
	return strings.TrimRight(baseURL, "/") + parent + "/models"
}

// refer: https://platform.openai.com/docs/api-reference/models/list
func fetchOpenAIModels(httpClient *http.Client, ctx context.Context, target model.ChannelConfig, key, url string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	for _, header := range target.CustomHeader {
		if header.HeaderKey != "" {
			req.Header.Set(header.HeaderKey, header.HeaderValue)
		}
	}

	response, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	result, err := decodeModelList[model.OpenAIModelList](response)
	if err != nil {
		return nil, err
	}

	models := make([]string, 0, len(result.Data))
	for _, m := range result.Data {
		models = append(models, m.ID)
	}
	return models, nil
}

// refer: https://platform.claude.com/docs
func fetchAnthropicModels(httpClient *http.Client, ctx context.Context, target model.ChannelConfig, key, url string) ([]string, error) {
	var allModels []string
	var afterID string
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-Api-Key", key)
		req.Header.Set("Anthropic-Version", "2023-06-01")
		for _, header := range target.CustomHeader {
			if header.HeaderKey != "" {
				req.Header.Set(header.HeaderKey, header.HeaderValue)
			}
		}
		if afterID != "" {
			q := req.URL.Query()
			q.Set("after_id", afterID)
			req.URL.RawQuery = q.Encode()
		}

		response, err := httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		// 分页时每轮都会新建响应, 必须当轮读完即关; 用 defer 会攒到整个函数返回才释放。
		result, err := decodeModelList[model.AnthropicModelList](response)
		if err != nil {
			return nil, err
		}
		for _, m := range result.Data {
			allModels = append(allModels, m.ID)
		}
		if !result.HasMore {
			break
		}
		afterID = result.LastID
	}
	return allModels, nil
}

// decodeModelList 关闭响应并把响应体解成模型列表; 非 2xx 时按上游错误返回。
// 两侧解析流程一致, 只有目标结构不同, 故用类型参数收敛; 分页调用要求当轮读完即关, 关闭点放在此处最稳。
func decodeModelList[T any](response *http.Response) (T, error) {
	defer response.Body.Close()
	var result T
	// 上游报错时响应体常是能被正常解码的 JSON, 若不先拦下, 模型列表会解成空列表并当作成功;
	// 响应体截断到 512 字节: 部分上游在鉴权失败时返回整页 HTML, 全文带到界面上无用。
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body, err := io.ReadAll(io.LimitReader(response.Body, 512))
		if err != nil {
			return result, fmt.Errorf("upstream %s", response.Status)
		}
		return result, fmt.Errorf("upstream %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return result, err
	}
	return result, nil
}
