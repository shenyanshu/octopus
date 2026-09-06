package price

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/price/catalog"
)

// fixturePath 是 Go 与 Python 等价测试共用的固定 JSON fixture, 覆盖边界与过滤项。
const fixturePath = "testdata/models_dev_fixture.json"

// loadFixture 解析 fixture JSON 为 modelsDevResponse, 供 filterDeveloperPrices 真实解析。
func loadFixture(t *testing.T) modelsDevResponse {
	t.Helper()
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("读取 fixture 失败: %v", err)
	}
	var raw modelsDevResponse
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("解析 fixture 失败: %v", err)
	}
	return raw
}

// TestFilterDeveloperPricesFixture 验证 Go 侧真实解析 fixture 并比对完整四价。
// 保留项: m1-good(完整四价), m2-explicit-free(显式0=合法免费), m14-mixed-case(lower+trim),
// m7-cache-partial(非零 cache_read + 缺失 cache_write=0), a1-good(另一研发商)。
// 拒绝项: m3-missing-cost, m4-null-cost, m5-missing-input, m6-missing-output,
// m8-negative, m9-negcache, m10-negcw, m11-embed, m12-thirdparty, m13-image, m15-empty-cost。
func TestFilterDeveloperPricesFixture(t *testing.T) {
	raw := loadFixture(t)
	got := filterDeveloperPrices(raw)

	want := map[string]model.LLMPrice{
		"gpt-4o":           {Input: 2.5, Output: 10, CacheRead: 1.25, CacheWrite: 0.5},
		"gpt-free":         {Input: 0, Output: 0, CacheRead: 0, CacheWrite: 0},
		"gpt-mixed":        {Input: 3, Output: 6, CacheRead: 0, CacheWrite: 0},
		"gpt-cachepartial": {Input: 1, Output: 2, CacheRead: 0.5, CacheWrite: 0},
		"claude-3":         {Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75},
	}
	if len(got) != len(want) {
		t.Fatalf("过滤后应剩 %d 个, got %d: %+v", len(want), len(got), got)
	}
	for id, w := range want {
		g, ok := got[id]
		if !ok {
			t.Fatalf("过滤后应包含 %s; got keys: %+v", id, mapKeys(got))
		}
		if g != w {
			t.Fatalf("%s 完整四价不符: want %+v, got %+v", id, w, g)
		}
	}
}

// TestFilterDeveloperPricesRejectsInvalid 确认所有无效条目被拒绝(未流入目录)。
func TestFilterDeveloperPricesRejectsInvalid(t *testing.T) {
	raw := loadFixture(t)
	got := filterDeveloperPrices(raw)
	rejected := []string{
		"gpt-nocost", "gpt-nullcost", "gpt-noinput", "gpt-nooutput",
		"gpt-neg", "gpt-negcache", "gpt-negcw", "gpt-embed",
		"thirdparty-model", "gpt-image", "gpt-emptycost",
	}
	for _, id := range rejected {
		if _, ok := got[id]; ok {
			t.Fatalf("无效条目 %s 应被拒绝, 不应流入目录", id)
		}
	}
}

func mapKeys(m map[string]model.LLMPrice) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// TestFilterDeveloperPricesFeedsCatalog 验证过滤结果经 catalog.Replace 后可被 Lookup 查到,
// 串联运行时解析与参考目录, 含完整四价与 mixed-case 规范化。
func TestFilterDeveloperPricesFeedsCatalog(t *testing.T) {
	raw := loadFixture(t)
	filtered := filterDeveloperPrices(raw)
	catalog.Replace(filtered)
	t.Cleanup(func() { catalog.Replace(nil) })

	m, ok := catalog.Lookup("gpt-4o")
	if !ok {
		t.Fatal("catalog 应能查到 gpt-4o")
	}
	if m.Price != (model.LLMPrice{Input: 2.5, Output: 10, CacheRead: 1.25, CacheWrite: 0.5}) {
		t.Fatalf("gpt-4o 完整四价不符: %+v", m.Price)
	}
	// mixed-case: id "GPT-Mixed" lower 后 "gpt-mixed", family "GPT" 前缀匹配。
	mMixed, ok := catalog.Lookup("gpt-mixed")
	if !ok || mMixed.Price.Input != 3 {
		t.Fatalf("mixed-case 应规范化后保留, got %+v ok=%v", mMixed, ok)
	}
}

// TestUpdateLLMPrice 通过可控 RoundTripper(本地 TLS 测试服务器)验证真实 UpdateLLMPrice 公开路径。
// 不访问外网、不新增全局生产注入; 替换 http.DefaultTransport 须恢复, 故禁止 parallel。
//
// rhttp.Direct 克隆 http.DefaultTransport(*http.Transport) 并保留 DialContext + TLSClientConfig,
// 清掉 Proxy; 故用自定义 *http.Transport 的 DialContext 拨向本地测试服务器即可截获请求。
// directClient 在 rhttp 包内缓存, 首次 Direct 调用创建后复用; 所有子测试共享同一服务器与缓存客户端,
// 每个子测试通过 mockStatus/mockBody 变量切换响应内容。
func TestUpdateLLMPrice(t *testing.T) {
	// 替换全局 http.DefaultTransport, 须恢复; 禁止 parallel。
	var mockStatus int
	var mockBody []byte

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(mockStatus)
		w.Write(mockBody)
	}))
	defer ts.Close()

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", ts.Listener.Addr().String())
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // 测试用自签证书, 跳过验证
	}
	origTransport := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = origTransport })

	successBody, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("读取 fixture 失败: %v", err)
	}

	// 成功: 200 + 有效 fixture → catalog 可查询完整四价 + 时间戳更新。
	t.Run("success_populates_catalog_and_timestamp", func(t *testing.T) {
		mockStatus = http.StatusOK
		mockBody = successBody
		before := time.Now()
		if err := UpdateLLMPrice(context.Background()); err != nil {
			t.Fatalf("成功 fixture 不应返回错误: %v", err)
		}
		if !GetLastUpdateTime().After(before) {
			t.Fatal("成功后时间戳应更新")
		}
		m, ok := catalog.Lookup("gpt-4o")
		if !ok || m.Price != (model.LLMPrice{Input: 2.5, Output: 10, CacheRead: 1.25, CacheWrite: 0.5}) {
			t.Fatalf("成功后 catalog 应含 gpt-4o(完整四价): %+v ok=%v", m, ok)
		}
	})

	// 记录成功后的时间戳, 供失败用例验证不被覆盖。
	successTimestamp := GetLastUpdateTime()

	// 失败用例: 返回错误 + 旧目录和时间戳不变。所有响应 200 使 Direct 成功、不触发 proxy。
	allFilteredEmpty := `{"openai":{"models":{"m1":{"id":"gpt-embed","family":"gpt","modalities":{"output":["text"]},"cost":{"input":0.01,"output":0.01}}}}}`
	failureCases := []struct {
		name           string
		body           []byte
		expectEmptyErr bool // true=期望 ErrEmptyPriceCatalog; false=仅期望非 nil 错误(如解析失败)
	}{
		{"empty_object_preserves", []byte(`{}`), true},
		{"null_body_preserves", []byte(`null`), true},
		{"all_filtered_empty_preserves", []byte(allFilteredEmpty), true},
		{"malformed_preserves", []byte(`{{malformed`), false},
	}
	for _, c := range failureCases {
		t.Run(c.name, func(t *testing.T) {
			mockStatus = http.StatusOK
			mockBody = c.body
			err := UpdateLLMPrice(context.Background())
			if err == nil {
				t.Fatal("失败响应应返回错误")
			}
			if c.expectEmptyErr && !errors.Is(err, ErrEmptyPriceCatalog) {
				t.Fatalf("应返回 ErrEmptyPriceCatalog, got: %v", err)
			}
			// 时间戳不变: 失败路径不更新 lastUpdateTime。
			if !GetLastUpdateTime().Equal(successTimestamp) {
				t.Fatal("失败时时间戳不应更新")
			}
			// 旧目录不变: gpt-4o 仍为成功时写入的完整四价。
			m, ok := catalog.Lookup("gpt-4o")
			if !ok || m.Price.Input != 2.5 {
				t.Fatalf("失败时旧目录不应被覆盖: %+v ok=%v", m, ok)
			}
		})
	}
}

// --- 跨语言等价: Go filterDeveloperPrices 与 Python filter_models 在同一 fixture 上结果一致 ---

// TestFilterEquivalenceWithPython 通过 python3 标准库 CLI 执行 scripts/updatePrice.py 的
// filter_models 解析同一 fixture, 比对 Go 与 Python 保留的 (model_id, 完整四价) 一致。
// 无网络、无新增依赖; 使用临时内联 Python 脚本通过 importlib 加载 updatePrice 模块。
func TestFilterEquivalenceWithPython(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 不可用, 跳过跨语言等价测试")
	}
	script := `
import importlib.util, json, sys
spec = importlib.util.spec_from_file_location("updatePrice", sys.argv[1])
mod = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mod)
with open(sys.argv[2], encoding="utf-8") as f:
    raw = json.load(f)
for model_id, cost in mod.filter_models(raw):
    print(model_id + "\t" + json.dumps(cost, sort_keys=True))
`
	cmd := exec.Command("python3", "-c", script,
		"../../scripts/updatePrice.py", fixturePath)
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("python filter_models 执行失败: %v\nstderr: %s", err, string(out))
	}
	pyResult := parsePythonOutput(t, string(out))

	raw := loadFixture(t)
	goResult := filterDeveloperPrices(raw)

	if len(goResult) != len(pyResult) {
		t.Fatalf("Go(%d) 与 Python(%d) 保留条目数不一致\nGo: %+v\nPython: %+v",
			len(goResult), len(pyResult), goResult, pyResult)
	}
	for id, pyCost := range pyResult {
		gp, ok := goResult[id]
		if !ok {
			t.Fatalf("Go 缺少 Python 保留的 %s", id)
		}
		if !floatEq(gp.Input, pyCost.Input) || !floatEq(gp.Output, pyCost.Output) ||
			!floatEq(gp.CacheRead, pyCost.CacheRead) || !floatEq(gp.CacheWrite, pyCost.CacheWrite) {
			t.Fatalf("%s 四价不一致: Go %+v vs Python %+v", id, gp, pyCost)
		}
	}
}

// parsePythonOutput 解析 python filter_models 输出的 "id\t{json}" 行为 map。
func parsePythonOutput(t *testing.T, out string) map[string]model.LLMPrice {
	t.Helper()
	result := make(map[string]model.LLMPrice)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			t.Fatalf("无法解析 Python 输出行: %q", line)
		}
		var c model.LLMPrice
		// Python json.dumps 产生 {"cache_read":..., "input":..., "output":...},
		// model.LLMPrice 的 json tag 与之匹配。
		if err := json.Unmarshal([]byte(parts[1]), &c); err != nil {
			t.Fatalf("解析 Python cost JSON 失败: %v", err)
		}
		result[parts[0]] = c
	}
	return result
}

// floatEq 判定两个 float64 在价格精度内相等。
func floatEq(a, b float64) bool {
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return diff < 1e-9
}

// TestValidateCostRejectsNonFinite 验证 validPrice 对 Inf/NaN 的深度防御拒绝。
// 标准 JSON 不会产生 Inf/NaN, 但验证函数仍须拒绝以防其他来源流入。
func TestValidateCostRejectsNonFinite(t *testing.T) {
	inf := math.Inf(1)
	nan := math.NaN()
	cases := []struct {
		name string
		cost *rawLLMCost
		ok   bool
	}{
		{"nil cost", nil, false},
		{"nil input", &rawLLMCost{Output: ptr(1)}, false},
		{"nil output", &rawLLMCost{Input: ptr(1)}, false},
		{"negative input", &rawLLMCost{Input: ptr(-1), Output: ptr(1)}, false},
		{"negative output", &rawLLMCost{Input: ptr(1), Output: ptr(-1)}, false},
		{"inf input", &rawLLMCost{Input: ptr(inf), Output: ptr(1)}, false},
		{"nan output", &rawLLMCost{Input: ptr(1), Output: ptr(nan)}, false},
		{"inf cache_read", &rawLLMCost{Input: ptr(1), Output: ptr(1), CacheRead: ptr(inf)}, false},
		{"negative cache_write", &rawLLMCost{Input: ptr(1), Output: ptr(1), CacheWrite: ptr(-1)}, false},
		{"explicit zero free", &rawLLMCost{Input: ptr(0), Output: ptr(0)}, true},
		{"valid with cache", &rawLLMCost{Input: ptr(2), Output: ptr(6), CacheRead: ptr(0.5), CacheWrite: ptr(5)}, true},
		{"valid cache missing defaults 0", &rawLLMCost{Input: ptr(2), Output: ptr(6)}, true},
	}
	for _, c := range cases {
		_, ok := validateCost(c.cost)
		if ok != c.ok {
			t.Fatalf("%s: want ok=%v, got ok=%v", c.name, c.ok, ok)
		}
	}
}

// ptr 返回 v 的地址, 供 validateCost 测试构造指针字段。
func ptr(v float64) *float64 { return &v }
