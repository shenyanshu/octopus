package task

import (
	"sync"
	"time"

	"github.com/charmbracelet/log"
)

type taskEntry struct {
	name            string
	interval        time.Duration
	fn              func()
	runOnStart      bool
	ticker          *time.Ticker
	stopCh          chan struct{}
	updateCh        chan time.Duration
	mu              sync.Mutex
	pendingInterval time.Duration
}

var (
	tasks   = make(map[string]*taskEntry)
	tasksMu sync.RWMutex
)

// Register 注册一个定时任务
// runOnStart: 是否在启动时立即执行一次
func Register(name string, interval time.Duration, runOnStart bool, fn func()) {
	if interval <= 0 {
		log.Debugf("task %s not registered: interval is 0", name)
		return
	}

	tasksMu.Lock()
	defer tasksMu.Unlock()

	if _, exists := tasks[name]; exists {
		log.Warnf("task %s already registered, skipping", name)
		return
	}

	tasks[name] = &taskEntry{
		name:       name,
		interval:   interval,
		fn:         fn,
		runOnStart: runOnStart,
		stopCh:     make(chan struct{}),
		updateCh:   make(chan time.Duration, 1),
	}
	log.Debugf("task %s registered with interval %v, runOnStart: %v", name, interval, runOnStart)
}

// Update 更新任务的执行间隔。
// 当 interval 为 0 时，删除任务。
// updateCh 带有 1 个缓冲槽, 保证连续多次更新中最后一次能被接收: 无缓冲的 select default
// 在 runTask 恰好不在 select 等待时会丢失更新, 使设置保存后运行时间隔与持久值不一致。
func Update(name string, interval time.Duration) {
	tasksMu.Lock()
	entry, exists := tasks[name]
	if !exists {
		tasksMu.Unlock()
		log.Warnf("task %s not found", name)
		return
	}

	if interval <= 0 {
		delete(tasks, name)
		tasksMu.Unlock()
		close(entry.stopCh)
		log.Infof("task %s removed: interval is 0", name)
		return
	}
	entry.mu.Lock()
	entry.interval = interval
	entry.mu.Unlock()
	tasksMu.Unlock()

	select {
	case entry.updateCh <- interval:
		log.Infof("task %s interval updated to %v", name, interval)
	default:
		// 通道已有待消费的更新: 覆盖为最新值, runTask 消费后会用 entry.interval 重建 ticker,
		// 而非消费队列中可能已过期的旧值。
		entry.mu.Lock()
		entry.pendingInterval = interval
		entry.mu.Unlock()
		log.Warnf("task %s update queued, latest interval %v will take effect", name, interval)
	}
}

// RUN 启动所有注册的任务
func RUN() {
	tasksMu.RLock()
	for _, entry := range tasks {
		go runTask(entry)
	}
	tasksMu.RUnlock()

	// 阻塞主协程
	select {}
}

func runTask(entry *taskEntry) {
	// 根据配置决定是否在启动时立即执行
	if entry.runOnStart {
		go entry.fn()
	}

	// 初始化 ticker 前持锁读取 interval, 与 Update 的写入互斥。
	entry.mu.Lock()
	entry.ticker = time.NewTicker(entry.interval)
	entry.mu.Unlock()
	defer entry.ticker.Stop()

	for {
		select {
		case <-entry.ticker.C:
			go entry.fn()
		case newInterval := <-entry.updateCh:
			entry.ticker.Stop()
			entry.mu.Lock()
			entry.interval = newInterval
			entry.mu.Unlock()
			entry.ticker = time.NewTicker(newInterval)
		case <-entry.stopCh:
			return
		}
		// 消费 updateCh 后检查是否有 Update 在通道满时写入的 pendingInterval: 保证最后一次更新生效。
		entry.mu.Lock()
		pending := entry.pendingInterval
		currentInterval := entry.interval
		entry.pendingInterval = 0
		entry.mu.Unlock()
		if pending > 0 && pending != currentInterval {
			entry.ticker.Stop()
			entry.mu.Lock()
			entry.interval = pending
			entry.mu.Unlock()
			entry.ticker = time.NewTicker(pending)
		}
	}
}
