// Package channelsync 协调渠道模型自动同步: 按全局周期或手动触发拉取上游模型列表,
// 将缺失的模型与授权增量补齐, 并在同事务内触发规则分组的自动补成员。
// task 定时任务与 handler 手动触发共用同一协调器入口, 不重复实现。
package channelsync

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bestruirui/octopus/internal/model"
)

const (
	perKeyTimeout    = 30 * time.Second // 单条凭据探测的网络超时。
	perChannelBudget = 5 * time.Minute  // 单个渠道同步(含全部凭据迭代)的总预算, 不含等待 slot。
	maxConcurrent    = 3                // 全局最多同时同步的渠道数。
)

var (
	statusesMu sync.Mutex
	statuses   = make(map[int]*statusEntry)

	// lifecycle 统一停止/claim/Add 的线性化: Stop 的 Wait 不会与新的 Add 竞争,
	// stopped 标记设置后任何新 claim 都被拒绝, 已有 worker 的 Wait 在全部结束后才返回。
	lifecycle sync.Mutex
	slots     = make(chan struct{}, maxConcurrent)
	running   = make(map[int]struct{})
	rootCtx   context.Context
	rootCxl   context.CancelFunc
	rootWG    sync.WaitGroup
	stopped   atomic.Bool
)

// statusEntry 单渠道最近一次同步结果, 带 mutex 供并发读写。
type statusEntry struct {
	mu     sync.Mutex
	status model.ChannelModelSyncStatus
}

// ErrStopped 在协调器已停止后拒绝新同步时返回。
var ErrStopped = errors.New("channelsync: coordinator stopped")

// ErrChannelNotFound 渠道不存在时返回, handler 据此映射 404。
var ErrChannelNotFound = errors.New("channelsync: channel not found")

// ErrChannelBusy 渠道正在同步中, handler 据此映射 busy。
var ErrChannelBusy = errors.New("channelsync: channel already syncing")
