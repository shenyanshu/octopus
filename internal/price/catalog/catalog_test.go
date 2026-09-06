package catalog

import (
	"sync"
	"testing"

	"github.com/bestruirui/octopus/internal/model"
)

// restorePresets 在测试结束后将目录恢复为预置值, 隔离 Replace 对后续测试的影响。
func restorePresets(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { Replace(presetPrices) })
}

// --- 预置值查找: 无需启动注册, 直接从 presetPrices 查找 ---

func TestPresetLookupExact(t *testing.T) {
	m, ok := Lookup("gpt-4o")
	if !ok {
		t.Fatal("预置 gpt-4o 应能查到")
	}
	if m.ModelID != "gpt-4o" {
		t.Fatalf("ModelID 应为 gpt-4o, got %s", m.ModelID)
	}
	if m.Price.Input != 2.5 || m.Price.Output != 10 {
		t.Fatalf("gpt-4o 价格应为 {2.5, 10, ...}, got %+v", m.Price)
	}
}

func TestPresetLookupCaseInsensitive(t *testing.T) {
	m, ok := Lookup("  GPT-4O  ")
	if !ok {
		t.Fatal("trim+lower 后应匹配 gpt-4o")
	}
	if m.ModelID != "gpt-4o" {
		t.Fatalf("规范化后 ModelID 应为 gpt-4o, got %s", m.ModelID)
	}
}

// --- normalize/分段匹配: 精确优先、前后缀分段、歧义、无匹配 ---

func TestSegmentedMatchPrefixSuffix(t *testing.T) {
	restorePresets(t)
	// "azure/gpt-4o/deploy" 按 / 分段后应匹配预置 "gpt-4o"
	m, ok := Lookup("azure/gpt-4o/deploy")
	if !ok {
		t.Fatal("前后缀分段应匹配 gpt-4o")
	}
	if m.ModelID != "gpt-4o" {
		t.Fatalf("应匹配 gpt-4o, got %s", m.ModelID)
	}
}

func TestSegmentedMatchLongestWins(t *testing.T) {
	restorePresets(t)
	Replace(map[string]model.LLMPrice{
		"foo-a":     {Input: 1},
		"foo-a-bar": {Input: 2},
	})
	m, ok := Lookup("x-foo-a-bar-y")
	if !ok {
		t.Fatal("应匹配分段最多的 foo-a-bar")
	}
	if m.ModelID != "foo-a-bar" {
		t.Fatalf("应选分段最多的 foo-a-bar, got %s", m.ModelID)
	}
	if m.Price.Input != 2 {
		t.Fatal("应返回 foo-a-bar 的价格")
	}
}

func TestSegmentedMatchAmbiguousReturnsFalse(t *testing.T) {
	restorePresets(t)
	// "foo-a" 和 "a-bar" 各 2 段, 均匹配 "foo-a-bar", 但价格不同 → 歧义
	Replace(map[string]model.LLMPrice{
		"foo-a": {Input: 1},
		"a-bar": {Input: 2},
	})
	_, ok := Lookup("foo-a-bar")
	if ok {
		t.Fatal("同分段数不同价应判定歧义, 返回 false")
	}
}

func TestSegmentedMatchSamePriceNotAmbiguous(t *testing.T) {
	restorePresets(t)
	// 同分段数同价不视为歧义, 返回其一即可
	Replace(map[string]model.LLMPrice{
		"foo-a": {Input: 1},
		"a-bar": {Input: 1},
	})
	m, ok := Lookup("foo-a-bar")
	if !ok {
		t.Fatal("同价不应判歧义, 应返回匹配")
	}
	if m.Price.Input != 1 {
		t.Fatal("应返回一致的价格")
	}
}

func TestNoMatch(t *testing.T) {
	restorePresets(t)
	_, ok := Lookup("zzz-not-a-real-model")
	if ok {
		t.Fatal("无匹配应返回 false")
	}
}

// --- Replace: 独立快照、更新可见、并发安全 ---

func TestReplaceHoldsIndependentSnapshot(t *testing.T) {
	restorePresets(t)
	src := map[string]model.LLMPrice{"snap-test": {Input: 1}}
	Replace(src)
	// 调用方后续写入原 map 不应影响目录(Replace 复制了快照)
	src["snap-test"] = model.LLMPrice{Input: 999}
	m, ok := Lookup("snap-test")
	if !ok || m.Price.Input != 1 {
		t.Fatal("Replace 应持有独立快照, 调用方后续写入不应影响目录")
	}
}

func TestReplaceUpdateVisible(t *testing.T) {
	restorePresets(t)
	Replace(map[string]model.LLMPrice{"new-model": {Input: 42}})
	m, ok := Lookup("new-model")
	if !ok || m.Price.Input != 42 {
		t.Fatal("Replace 后 Lookup 应见到新值")
	}
}

func TestReplaceNormalizesKeys(t *testing.T) {
	restorePresets(t)
	Replace(map[string]model.LLMPrice{"  GPT-X  ": {Input: 7}})
	m, ok := Lookup("gpt-x")
	if !ok || m.Price.Input != 7 {
		t.Fatal("Replace 应规范化 key, 使 Lookup 查到")
	}
}

func TestConcurrentLookupReplaceNoRace(t *testing.T) {
	restorePresets(t)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			Lookup("gpt-4o")
		}()
		go func(n int) {
			defer wg.Done()
			Replace(map[string]model.LLMPrice{"gpt-4o": {Input: float64(n % 10)}})
		}(i)
	}
	wg.Wait()
}
