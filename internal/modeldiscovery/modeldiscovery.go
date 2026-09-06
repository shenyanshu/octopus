// Package modeldiscovery 提供上游模型列表探测的共享实现。
// handlers 的手动 fetch-model 与 channelsync 的后台同步共用同一组函数,
// 使两侧的探测行为完全一致, 不出现"手动能看到、自动看不到"的分歧。
package modeldiscovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/dlclark/regexp2"
)

// 探测过程中的边界常量: 不引入用户配置, 仅作为防御性上限, 防止上游异常拖垮服务。
const (
	maxAnthropicPages     = 1000            // Anthropic 分页最大页数, 超过视为上游异常。
	maxDiscoveredModels   = 10000           // 单侧协议返回的最大模型总数, 超过视为上游异常。
	maxSuccessBodyBytes   = int64(4 << 20)  // 成功响应体最大字节数(4 MiB), 超过直接拒绝。
	maxUpstreamErrorDrain = int64(512)      // 非 2xx 响应体最多丢弃字节数, 仅用于排空连接。
	regexMatchTimeout     = 5 * time.Second // 单次正则匹配的最长时间, 防止灾难性回溯。
)

// Result 是一次探测的合并结果: 按模型名称有序排列, 协议位由两侧成功结果取并集。
// Partial 为真表示恰好一侧协议失败, 另一侧成功的结果仍可使用;
// 两侧都成功或两侧都失败时为假。
type Result struct {
	Models  []model.ChannelFetchModel
	Partial bool
}

// Discover 同时探测 OpenAI 与 Anthropic 两侧, 合并成功结果并按 matchRegex 过滤。
// 两侧并发执行; 恰好一侧失败时另一侧结果仍可用(Partial=true), 两侧都失败时返回错误。
// matchRegex 使用 regexp2.ECMAScript 语法, 空串表示不过滤;
// 正则先于网络编译, 非法模式立即失败, 不会触达上游。
func Discover(ctx context.Context, httpClient *http.Client, config model.ChannelConfig, key, matchRegex string) (Result, error) {
	// 先编译正则: 非法模式在发任何上游请求前就失败, 避免无意义的网络副作用。
	re, err := compileMatchRegex(matchRegex)
	if err != nil {
		return Result{}, err
	}

	// 两侧各自写入独立变量, 协议归属由调用点决定而非完成顺序。
	// 旧实现把两个结果塞进同一 channel 再按到达顺序赋值, 会出现
	// "先到的当 OpenAI, 后到的当 Anthropic" 的错配 —— 协议位会贴到错误的模型上。
	// 用 WaitGroup + 独立槽位, 即使 Anthropic 先返回, OpenAI 的结果仍归 OpenAI 槽。
	var openaiModels, anthropicModels []string
	var openaiErr, anthropicErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		openaiModels, openaiErr = fetchOpenAIModels(httpClient, ctx, config, key, modelsURL(config.BaseURL, config.OpenAIResponsePath))
	}()
	go func() {
		defer wg.Done()
		anthropicModels, anthropicErr = fetchAnthropicModels(httpClient, ctx, config, key, modelsURL(config.BaseURL, config.AnthropicMessagePath))
	}()
	wg.Wait()

	if openaiErr != nil && anthropicErr != nil {
		// 两侧错误经 sanitize 后不含 token/上游原文/URL, 可安全拼接;
		// 用 %w 串联, 使 errors.Is(err, context.Canceled) 等能穿透到任一侧。
		return Result{}, fmt.Errorf("openai: %w; anthropic: %w", openaiErr, anthropicErr)
	}
	partial := openaiErr != nil || anthropicErr != nil

	models, err := mergeModels(openaiModels, anthropicModels, re, ctx)
	if err != nil {
		return Result{}, err
	}
	return Result{Models: models, Partial: partial}, nil
}

// mergeModels 把两侧模型名按"先 OpenAI 后 Anthropic"的固定顺序并入同一有序集合,
// 同名模型在两侧都出现时协议位取并集。顺序写死而非按 map 遍历, 使界面排序稳定。
func mergeModels(openaiModels, anthropicModels []string, re *regexp2.Regexp, ctx context.Context) ([]model.ChannelFetchModel, error) {
	protocolsByModel := make(map[string]model.Protocol, len(openaiModels)+len(anthropicModels))
	order := make([]string, 0, len(openaiModels)+len(anthropicModels))
	appendModel := func(name string, proto model.Protocol) error {
		// 遵循现有校验约束: 名称为空白即非法, 跳过; 不做大小写或名称归一化。
		if strings.TrimSpace(name) == "" {
			return nil
		}
		if re != nil {
			// 每条模型名匹配前先看 ctx 是否已取消, 整体取消能尽早结束而非被正则卡住。
			if err := ctx.Err(); err != nil {
				return err
			}
			matched, err := re.MatchString(name)
			if err != nil {
				// regexp2 的 MatchString 仅在超时时返回错误, 且错误串会带上原始输入(上游模型名);
				// 返回固定安全消息, 不把上游模型名透出给调用方。
				return fmt.Errorf("match regex timed out")
			}
			if !matched {
				return nil
			}
		}
		if _, ok := protocolsByModel[name]; !ok {
			order = append(order, name)
		}
		protocolsByModel[name] |= proto
		return nil
	}
	for _, name := range openaiModels {
		if err := appendModel(name, model.ProtocolOpenAIResponse); err != nil {
			return nil, err
		}
	}
	for _, name := range anthropicModels {
		if err := appendModel(name, model.ProtocolAnthropicMessage); err != nil {
			return nil, err
		}
	}
	models := make([]model.ChannelFetchModel, 0, len(order))
	for _, name := range order {
		models = append(models, model.ChannelFetchModel{Name: name, Protocols: protocolsByModel[name]})
	}
	return models, nil
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
		return nil, sanitizeTransportError(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	applyCustomHeaders(req, target.CustomHeader)
	response, err := httpClient.Do(req)
	if err != nil {
		return nil, sanitizeTransportError(err)
	}
	result, err := decodeModelList[model.OpenAIModelList](response)
	if err != nil {
		return nil, err
	}
	return collectModelNames(len(result.Data), func(i int) string { return result.Data[i].ID }, "openai")
}

// refer: https://platform.claude.com/docs
func fetchAnthropicModels(httpClient *http.Client, ctx context.Context, target model.ChannelConfig, key, url string) ([]string, error) {
	var allModels []string
	var afterID string
	// 记录已访问过的 cursor, 检测 A→B→A 这类回环; 单纯 A→A 是其特例, 同样被覆盖。
	seen := make(map[string]struct{}, maxAnthropicPages)
	for page := 0; ; page++ {
		if page >= maxAnthropicPages {
			return nil, fmt.Errorf("anthropic pagination exceeded %d pages", maxAnthropicPages)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if afterID != "" {
			if _, dup := seen[afterID]; dup {
				return nil, fmt.Errorf("anthropic pagination cycle at after_id")
			}
			seen[afterID] = struct{}{}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, sanitizeTransportError(err)
		}
		req.Header.Set("X-Api-Key", key)
		req.Header.Set("Anthropic-Version", "2023-06-01")
		applyCustomHeaders(req, target.CustomHeader)
		if afterID != "" {
			q := req.URL.Query()
			q.Set("after_id", afterID)
			req.URL.RawQuery = q.Encode()
		}
		response, err := httpClient.Do(req)
		if err != nil {
			return nil, sanitizeTransportError(err)
		}
		result, err := decodeModelList[model.AnthropicModelList](response)
		if err != nil {
			return nil, err
		}
		for _, m := range result.Data {
			if strings.TrimSpace(m.ID) == "" {
				continue
			}
			if len(allModels) >= maxDiscoveredModels {
				return nil, fmt.Errorf("anthropic returned too many models (limit %d)", maxDiscoveredModels)
			}
			allModels = append(allModels, m.ID)
		}
		if !result.HasMore {
			break
		}
		// HasMore 为真但 LastID 为空: 上游分页异常, 继续会以空 cursor 原地空转。
		if result.LastID == "" {
			return nil, fmt.Errorf("anthropic pagination stuck: empty last_id with has_more")
		}
		afterID = result.LastID
	}
	return allModels, nil
}

// collectModelNames 按索引取模型名, 跳过空白名, 并对总数设防。
// 上游可能返回带空白或空串的条目, 与现有渠道校验一致地视为非法并丢弃。
func collectModelNames(total int, idAt func(int) string, proto string) ([]string, error) {
	names := make([]string, 0, total)
	for i := 0; i < total; i++ {
		id := idAt(i)
		if strings.TrimSpace(id) == "" {
			continue
		}
		if len(names) >= maxDiscoveredModels {
			return nil, fmt.Errorf("%s returned too many models (limit %d)", proto, maxDiscoveredModels)
		}
		names = append(names, id)
	}
	return names, nil
}

// applyCustomHeaders 把渠道自定义 Header 追加到请求, 顺序在协议鉴权之后,
// 与基线一致: 自定义 Header 可有意覆盖默认鉴权头。
func applyCustomHeaders(req *http.Request, headers []model.CustomHeader) {
	for _, header := range headers {
		if header.HeaderKey != "" {
			req.Header.Set(header.HeaderKey, header.HeaderValue)
		}
	}
}

// decodeModelList 关闭响应体并把成功响应解成模型列表。
// 所有对外错误消息均为固定安全文本, 不含上游响应体、reason phrase、JSON 字段名或读取错误原文:
// 鉴权失败的上游常回整页 HTML 或带 token 的 JSON, reason phrase 也可能被恶意上游篡改为泄露凭据,
// 故非 2xx 只回数字状态码与 http.StatusText; JSON 形状不符或读取失败只回固定原因。
// 成功体先按 maxSuccessBodyBytes 限长读入, 超长直接拒绝。
// 成功响应必须是带 data 数组的对象: 空数组 [] 合法(零模型), 缺少 data 或 data 为 null 视为形状不符。
func decodeModelList[T any](response *http.Response) (T, error) {
	defer response.Body.Close()
	var result T
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		// 排空以便连接复用; 字节数有上限, 避免上游塞超大错误体消耗内存。
		_, _ = io.CopyN(io.Discard, response.Body, maxUpstreamErrorDrain)
		var zero T
		// 只用数字状态码与标准 reason phrase, 不回上游可能篡改的 response.Status。
		return zero, fmt.Errorf("upstream %d %s", response.StatusCode, http.StatusText(response.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxSuccessBodyBytes+1))
	if err != nil {
		// body 读取错误可能携带上游连接细节, 不透出。
		var zero T
		return zero, errors.New("read upstream response failed")
	}
	if int64(len(body)) > maxSuccessBodyBytes {
		var zero T
		return zero, errors.New("upstream response too large")
	}
	if err := json.Unmarshal(body, &result); err != nil {
		// UnmarshalTypeError 会暴露上游 JSON 字段名/类型, 语法错误会暴露 body 片段, 一律脱敏。
		var zero T
		return zero, errors.New("upstream response malformed")
	}
	if err := requireDataArray(body); err != nil {
		var zero T
		return zero, err
	}
	return result, nil
}

// sanitizeTransportError 剥离 *url.Error 外层带有的 URL 与任何底层文本,
// 只保留可被 errors.Is 识别的已知 context 错误(Canceled/DeadlineExceeded)。
// 上游 URL、非法 header 值(可能是用户填入的凭据)、自定义 RoundTripper 的任意错误文本
// 都不透出给手动 fetch-model 的调用方 —— 只回固定 "upstream unreachable" 摘要。
// context 的两个哨兵错误经 %w 包裹, errors.Is(err, context.Canceled) 仍可穿透。
func sanitizeTransportError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("upstream unreachable: %w", context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("upstream unreachable: %w", context.DeadlineExceeded)
	}
	return errors.New("upstream unreachable")
}

// requireDataArray 校验成功响应体必须是带 data 数组的对象:
// 空数组 [] 合法(零模型成功), 缺少 data 或 data 为 null 视为形状不符。
// 用最小探针结构解码, 只看 data 是否为数组类型, 不依赖具体模型的字段定义。
func requireDataArray(body []byte) error {
	var probe struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return errors.New("upstream response malformed")
	}
	// data 缺失或 null: RawMessage 为空, 不合法。
	if len(probe.Data) == 0 {
		return errors.New("upstream response missing data array")
	}
	// data 必须是数组; 任意其他类型(JSON 对象/字符串/数字)拒绝。
	if probe.Data[0] != '[' {
		return errors.New("upstream response data is not an array")
	}
	return nil
}
