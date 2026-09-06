package task

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestUpdateIntervalLastWriteWins 证明连续多次 Update 中最后一次值生效。
// 修复前 select default 会在 runTask 不在 select 等待时丢失更新, 使运行时间隔与持久值不一致。
func TestUpdateIntervalLastWriteWins(t *testing.T) {
	resetTasksForTest()
	const name = "test-update-last-wins"
	var callCount atomic.Int64
	Register(name, 1*time.Hour, false, func() {
		callCount.Add(1)
	})
	tasksMu.RLock()
	entry := tasks[name]
	tasksMu.RUnlock()
	go runTask(entry)
	defer close(entry.stopCh)

	// 快速连续更新: 100ms → 200ms → 50ms。
	Update(name, 100*time.Millisecond)
	Update(name, 200*time.Millisecond)
	Update(name, 50*time.Millisecond)

	// 等待至少 3 个周期, 证明间隔确实是 50ms。
	time.Sleep(200 * time.Millisecond)
	before := callCount.Load()
	time.Sleep(150 * time.Millisecond)
	after := callCount.Load()
	if after-before < 2 {
		t.Fatalf("最后更新 50ms 未生效: 150ms 内只调用了 %d 次(期望 >=2), 总调用 %d", after-before, after)
	}
}

// TestUpdateIntervalRuntimeConsistent 证明设置保存后运行时间隔立即生效。
func TestUpdateIntervalRuntimeConsistent(t *testing.T) {
	resetTasksForTest()
	const name = "test-runtime-consistent"
	Register(name, 1*time.Hour, false, func() {})
	tasksMu.RLock()
	entry := tasks[name]
	tasksMu.RUnlock()
	go runTask(entry)
	defer close(entry.stopCh)

	Update(name, 80*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	entry.mu.Lock()
	got := entry.interval
	entry.mu.Unlock()
	if got != 80*time.Millisecond {
		t.Fatalf("runtime interval = %v, want 80ms", got)
	}
}
