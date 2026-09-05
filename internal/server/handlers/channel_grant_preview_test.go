package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/gin-gonic/gin"
)

// grantPreviewResponse 解析预览响应的标准信封, 取 data 为候选切片。
type grantPreviewResponse struct {
	Code    int                           `json:"code"`
	Message string                        `json:"message"`
	Data    []model.ChannelGrantCandidate `json:"data"`
}

// callPreviewGrants 以真实 gin.CreateTestContext 调用 previewChannelGrants。
// 与 group_test.go 一致: 直调 handler 绕过中间件, 聚焦于 handler 自身行为。
func callPreviewGrants(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(rec)
	ginContext.Request = httptest.NewRequest(http.MethodPost, "/api/v1/channel/grants/preview", bytes.NewReader([]byte(body)))
	ginContext.Request.Header.Set("Content-Type", "application/json")
	previewChannelGrants(ginContext)
	return rec
}

// seedGrantPreviewFixtures 建立一批带可区分模型名的授权候选, 覆盖可用/不可用、重名模型等场景。
// 复用 group_test.go 的建表清理逻辑但独立成函数: 预览用例不经分组, 不需要 forward 预热。
// 返回建出的候选(按 op.ChannelGrantCandidates 的稳定顺序)供断言对照。
func seedGrantPreviewFixtures(t *testing.T) []model.ChannelGrantCandidate {
	t.Helper()
	dbConn := db.GetDB()
	for _, table := range []string{"groups", "group_items", "channel_grants", "channel_models", "channel_keys", "channels"} {
		if err := dbConn.Exec("DELETE FROM " + table).Error; err != nil {
			t.Fatalf("清理表 %s 失败: %v", table, err)
		}
	}
	// 两个渠道各带两个模型。beta 建后再禁用: Enabled 带默认值 true, 零值 false 会被 GORM 跳过而落库为 true。
	// 模型名刻意混合大小写与后缀, 以测大小写敏感、子串、交替、锚点。
	channels := []struct {
		name    string
		enabled bool
		models  []string
	}{
		{"alpha", true, []string{"gpt-4o", "gpt-4o-mini"}},
		{"beta", false, []string{"claude-3-opus", "claude-3-sonnet"}},
	}
	for _, ch := range channels {
		channel := model.Channel{ChannelConfig: model.ChannelConfig{
			Name: ch.name, Enabled: true, BaseURL: "http://example.invalid",
			OpenAIChatCompletionPath: "/chat", AnthropicMessagePath: "/msg",
		}}
		if err := dbConn.Create(&channel).Error; err != nil {
			t.Fatalf("建渠道 %s 失败: %v", ch.name, err)
		}
		if !ch.enabled {
			if err := dbConn.Model(&model.Channel{}).Where("id = ?", channel.ID).Update("enabled", false).Error; err != nil {
				t.Fatalf("禁用渠道 %s 失败: %v", ch.name, err)
			}
		}
		key := model.ChannelKey{ChannelID: channel.ID, ChannelKeyConfig: model.ChannelKeyConfig{Name: "k", Key: "sk-test", Enabled: true}}
		if err := dbConn.Create(&key).Error; err != nil {
			t.Fatalf("建凭据失败: %v", err)
		}
		for _, modelName := range ch.models {
			channelModel := model.ChannelModel{ChannelID: channel.ID, Name: modelName}
			if err := dbConn.Create(&channelModel).Error; err != nil {
				t.Fatalf("建模型 %s 失败: %v", modelName, err)
			}
			grant := model.ChannelGrant{ChannelModelID: channelModel.ID, ChannelKeyID: key.ID, Protocols: model.ProtocolOpenAIChatCompletion}
			if err := dbConn.Create(&grant).Error; err != nil {
				t.Fatalf("建授权失败: %v", err)
			}
		}
	}
	if err := op.InitCache(); err != nil {
		t.Fatalf("刷新缓存失败: %v", err)
	}
	return op.ChannelGrantCandidates()
}

func assertPreviewOK(t *testing.T, rec *httptest.ResponseRecorder) grantPreviewResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("响应码 = %d, 想要 200: %s", rec.Code, rec.Body.String())
	}
	var resp grantPreviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v: %s", err, rec.Body.String())
	}
	if resp.Data == nil {
		t.Fatalf("data 为 null, 想要非 nil 数组")
	}
	return resp
}

func assertPreviewBadRequest(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("响应码 = %d, 想要 400: %s", rec.Code, rec.Body.String())
	}
}

func modelNames(candidates []model.ChannelGrantCandidate) []string {
	names := make([]string, 0, len(candidates))
	for _, c := range candidates {
		names = append(names, c.ModelName)
	}
	return names
}

func assertEqualStringSlices(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("切片长度 = %d, 想要 %d: got=%v want=%v", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("第 %d 项 = %q, 想要 %q: got=%v want=%v", i, got[i], want[i], got, want)
		}
	}
}

// TestPreviewGrantsEmptyPatternReturnsEmpty 空模式返回空集而非全集: 避免误把"没填"当"全选"。
func TestPreviewGrantsEmptyPatternReturnsEmpty(t *testing.T) {
	seedGrantPreviewFixtures(t)
	rec := callPreviewGrants(t, `{"pattern":""}`)
	resp := assertPreviewOK(t, rec)
	if len(resp.Data) != 0 {
		t.Fatalf("空模式返回了 %d 条, 想要 0: %+v", len(resp.Data), resp.Data)
	}
}

// TestPreviewGrantsMissingPatternReturnsEmpty 缺省 pattern 字段等同于空, 返回空集。
func TestPreviewGrantsMissingPatternReturnsEmpty(t *testing.T) {
	seedGrantPreviewFixtures(t)
	rec := callPreviewGrants(t, `{}`)
	resp := assertPreviewOK(t, rec)
	if len(resp.Data) != 0 {
		t.Fatalf("缺省 pattern 返回了 %d 条, 想要 0", len(resp.Data))
	}
}

// TestPreviewGrantsExactMatch 精确匹配用 ^$ 锚点: 只命中完全相等的模型名。
func TestPreviewGrantsExactMatch(t *testing.T) {
	seedGrantPreviewFixtures(t)
	rec := callPreviewGrants(t, `{"pattern":"^gpt-4o$"}`)
	resp := assertPreviewOK(t, rec)
	assertEqualStringSlices(t, modelNames(resp.Data), []string{"gpt-4o"})
}

// TestPreviewGrantsSubstringMatch 子串匹配(无锚点): gpt-4o 命中 gpt-4o 与 gpt-4o-mini。
func TestPreviewGrantsSubstringMatch(t *testing.T) {
	seedGrantPreviewFixtures(t)
	rec := callPreviewGrants(t, `{"pattern":"gpt-4o"}`)
	resp := assertPreviewOK(t, rec)
	assertEqualStringSlices(t, modelNames(resp.Data), []string{"gpt-4o", "gpt-4o-mini"})
}

// TestPreviewGrantsAlternationMatch 交替表达式命中多个前缀。
func TestPreviewGrantsAlternationMatch(t *testing.T) {
	seedGrantPreviewFixtures(t)
	rec := callPreviewGrants(t, `{"pattern":"^(gpt-4o|claude-3-opus)$"}`)
	resp := assertPreviewOK(t, rec)
	assertEqualStringSlices(t, modelNames(resp.Data), []string{"gpt-4o", "claude-3-opus"})
}

// TestPreviewGrantsCaseSensitive 默认大小写敏感: 大写模式不匹配小写模型名。
func TestPreviewGrantsCaseSensitive(t *testing.T) {
	seedGrantPreviewFixtures(t)
	rec := callPreviewGrants(t, `{"pattern":"^GPT-4O$"}`)
	resp := assertPreviewOK(t, rec)
	if len(resp.Data) != 0 {
		t.Fatalf("大小写敏感失效, 大写模式命中了: %+v", resp.Data)
	}
}

// TestPreviewGrantsCaseInsensitiveFlag (?i) 标志使匹配忽略大小写。
func TestPreviewGrantsCaseInsensitiveFlag(t *testing.T) {
	seedGrantPreviewFixtures(t)
	rec := callPreviewGrants(t, `{"pattern":"(?i)^GPT-4O$"}`)
	resp := assertPreviewOK(t, rec)
	assertEqualStringSlices(t, modelNames(resp.Data), []string{"gpt-4o"})
}

// TestPreviewGrantsLookaroundRejected RE2 不支持 lookaround, 编译失败即 400。
func TestPreviewGrantsLookaroundRejected(t *testing.T) {
	seedGrantPreviewFixtures(t)
	rec := callPreviewGrants(t, `{"pattern":"(?<=gpt)-4o"}`)
	assertPreviewBadRequest(t, rec)
}

// TestPreviewGrantsInvalidRegexRejected 非法正则(未闭合括号)即 400。
func TestPreviewGrantsInvalidRegexRejected(t *testing.T) {
	seedGrantPreviewFixtures(t)
	rec := callPreviewGrants(t, `{"pattern":"[gpt"}`)
	assertPreviewBadRequest(t, rec)
}

// TestPreviewGrantsOverlongPatternRejected 超过 1024 字节的 pattern 即 400, 不进编译。
func TestPreviewGrantsOverlongPatternRejected(t *testing.T) {
	seedGrantPreviewFixtures(t)
	long := strings.Repeat("a", model.MaxPatternBytes+1)
	body, _ := json.Marshal(map[string]string{"pattern": long})
	rec := callPreviewGrants(t, string(body))
	assertPreviewBadRequest(t, rec)
}

// TestPreviewGrantsMaxLengthBoundary 恰好 1024 字节的 pattern 不被拒, 能编译则返回结果。
func TestPreviewGrantsMaxLengthBoundary(t *testing.T) {
	seedGrantPreviewFixtures(t)
	// 1024 个 'a' 是合法正则(字面量), 不命中任何模型名, 但不应被长度拦截。
	pattern := strings.Repeat("a", model.MaxPatternBytes)
	body, _ := json.Marshal(map[string]string{"pattern": pattern})
	rec := callPreviewGrants(t, string(body))
	resp := assertPreviewOK(t, rec)
	if len(resp.Data) != 0 {
		t.Fatalf("全 a 模式不应命中任何模型: %+v", resp.Data)
	}
}

// TestPreviewGrantsInvalidJSONRejected 非 JSON 体即 400。
func TestPreviewGrantsInvalidJSONRejected(t *testing.T) {
	seedGrantPreviewFixtures(t)
	rec := callPreviewGrants(t, `not json`)
	assertPreviewBadRequest(t, rec)
}

// TestPreviewGrantsNonStringPatternRejected pattern 为非字符串类型即 400。
func TestPreviewGrantsNonStringPatternRejected(t *testing.T) {
	seedGrantPreviewFixtures(t)
	rec := callPreviewGrants(t, `{"pattern":123}`)
	assertPreviewBadRequest(t, rec)
}

// TestPreviewGrantsIncludesUnavailable 不可用候选照常返回: 预览看"会命中什么", 不过滤可用性。
func TestPreviewGrantsIncludesUnavailable(t *testing.T) {
	seedGrantPreviewFixtures(t)
	rec := callPreviewGrants(t, `{"pattern":"claude"}`)
	resp := assertPreviewOK(t, rec)
	if len(resp.Data) != 2 {
		t.Fatalf("claude 应命中 2 条(含不可用), 实际 %d: %+v", len(resp.Data), resp.Data)
	}
	for _, c := range resp.Data {
		if c.Available {
			t.Fatalf("禁用渠道的候选不应 Available=true: %+v", c)
		}
	}
}

// TestPreviewGrantsPreservesOrder 匹配结果保持 ChannelGrantCandidates 的稳定顺序, 不重排。
func TestPreviewGrantsPreservesOrder(t *testing.T) {
	all := seedGrantPreviewFixtures(t)
	rec := callPreviewGrants(t, `{"pattern":"."}`)
	resp := assertPreviewOK(t, rec)
	if len(resp.Data) != len(all) {
		t.Fatalf("'. ' 命中数 %d 与全集 %d 不符", len(resp.Data), len(all))
	}
	for i := range resp.Data {
		if resp.Data[i].ID != all[i].ID {
			t.Fatalf("第 %d 项 ID = %d, 想要 %d (顺序被打乱)", i, resp.Data[i].ID, all[i].ID)
		}
	}
}

// TestPreviewGrantsDuplicateModelDifferentGrantIDs 同名模型的不同授权 ID 都保留。
// 在 alpha 渠道的 gpt-4o 模型上再加一把凭据与授权: 同一模型名出现两条不同 ID 的候选。
func TestPreviewGrantsDuplicateModelDifferentGrantIDs(t *testing.T) {
	seedGrantPreviewFixtures(t)
	dbConn := db.GetDB()
	var alpha model.Channel
	if err := dbConn.Where("name = ?", "alpha").First(&alpha).Error; err != nil {
		t.Fatalf("读 alpha 渠道失败: %v", err)
	}
	var gpt4o model.ChannelModel
	if err := dbConn.Where("channel_id = ? AND name = ?", alpha.ID, "gpt-4o").First(&gpt4o).Error; err != nil {
		t.Fatalf("读 gpt-4o 模型失败: %v", err)
	}
	// 第二把凭据: 与原凭据同名区分用 k2, 让候选的 KeyName 不同以稳定排序。
	key2 := model.ChannelKey{ChannelID: alpha.ID, ChannelKeyConfig: model.ChannelKeyConfig{Name: "k2", Key: "sk-test2", Enabled: true}}
	if err := dbConn.Create(&key2).Error; err != nil {
		t.Fatalf("建凭据失败: %v", err)
	}
	grant2 := model.ChannelGrant{ChannelModelID: gpt4o.ID, ChannelKeyID: key2.ID, Protocols: model.ProtocolOpenAIChatCompletion}
	if err := dbConn.Create(&grant2).Error; err != nil {
		t.Fatalf("建授权失败: %v", err)
	}
	if err := op.InitCache(); err != nil {
		t.Fatalf("刷新缓存失败: %v", err)
	}

	rec := callPreviewGrants(t, `{"pattern":"^gpt-4o$"}`)
	resp := assertPreviewOK(t, rec)
	if len(resp.Data) != 2 {
		t.Fatalf("同名模型应命中 2 条不同授权, 实际 %d: %+v", len(resp.Data), resp.Data)
	}
	if resp.Data[0].ID == resp.Data[1].ID {
		t.Fatalf("两条候选 ID 相同: %+v", resp.Data)
	}
	if resp.Data[0].ModelName != "gpt-4o" || resp.Data[1].ModelName != "gpt-4o" {
		t.Fatalf("候选模型名不符: %+v", resp.Data)
	}
}

// TestPreviewGrantsNoMutation 预览是只读: 调用前后全集不变, 不改缓存/状态。
func TestPreviewGrantsNoMutation(t *testing.T) {
	before := seedGrantPreviewFixtures(t)
	// 多次预览后全集仍与首次一致。
	for _, pat := range []string{`{"pattern":"gpt"}`, `{"pattern":"claude"}`, `{"pattern":""}`, `{"pattern":"^gpt-4o$"}`} {
		rec := callPreviewGrants(t, pat)
		assertPreviewOK(t, rec)
	}
	after := op.ChannelGrantCandidates()
	if len(after) != len(before) {
		t.Fatalf("预览改了候选数量: before=%d after=%d", len(before), len(after))
	}
	for i := range before {
		if before[i].ID != after[i].ID {
			t.Fatalf("预览改了候选顺序或内容: before[%d].ID=%d after[%d].ID=%d", i, before[i].ID, i, after[i].ID)
		}
	}
}

// TestPreviewGrantsCompileOncePerRequest 每请求独立编译: 同一 pattern 多次请求结果一致。
func TestPreviewGrantsCompileOncePerRequest(t *testing.T) {
	seedGrantPreviewFixtures(t)
	for i := 0; i < 3; i++ {
		rec := callPreviewGrants(t, `{"pattern":"gpt-4o"}`)
		resp := assertPreviewOK(t, rec)
		assertEqualStringSlices(t, modelNames(resp.Data), []string{"gpt-4o", "gpt-4o-mini"})
	}
}

// TestPreviewGrantsRespondsEnvelopeShape 成功响应是标准信封, data 是数组而非 null。
func TestPreviewGrantsRespondsEnvelopeShape(t *testing.T) {
	seedGrantPreviewFixtures(t)
	rec := callPreviewGrants(t, `{"pattern":"nonexistent-model-xyz"}`)
	resp := assertPreviewOK(t, rec)
	if resp.Code != http.StatusOK {
		t.Fatalf("code = %d, 想要 200", resp.Code)
	}
	if len(resp.Data) != 0 {
		t.Fatalf("无命中应返回空数组, 实际 %d 条", len(resp.Data))
	}
	// 验证 JSON 原文里 data 是 [] 而非 null。
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"data":[]`)) {
		t.Fatalf("响应体 data 不是 []: %s", rec.Body.String())
	}
}
