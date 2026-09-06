// Package groupevents 提供分组变更事件的发布与订阅总线。
// handlers 的 SSE 端点与 channelsync 的渠道同步协调器共用同一总线,
// 使"已提交即通知"不依赖客户端重连。
package groupevents

import (
	"sync"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/relay"
)

// ChangedData 是分组 changed 事件携带的响应载荷。
// handlers.groupResponse 与此同构: SSE 客户端无需区分事件来源。
// ActiveItemID 恒为零值, 只为遮蔽嵌入分组同名字段: 当前成员一律从 runtime.current_item_id 读。
type ChangedData struct {
	model.Group
	ActiveItemID int              `json:"active_item_id,omitempty"`
	Runtime      relay.RouteState `json:"runtime"`
}

// Event 是一条分组变更事件, SSE 收到后按事件名原样转发。
type Event struct {
	Name string // SSE 事件名: changed 表示分组配置或成员发生变更, deleted 表示分组已删除。
	Data any    // changed 携带完整分组响应, deleted 携带分组 ID。
}

// Buffer 是单个事件流连接的非阻塞消息缓冲容量。
const Buffer = 16

var (
	mu      sync.Mutex
	streams = make(map[chan Event]struct{})
)

// Publish 非阻塞发布一条事件, 连接拥塞时关闭它并交给客户端重连后重新拉取对齐。
func Publish(event Event) {
	mu.Lock()
	defer mu.Unlock()
	for stream := range streams {
		select {
		case stream <- event:
		default:
			delete(streams, stream)
			close(stream)
		}
	}
}

// Subscribe 注册一条新的事件流并返回它; 不再使用时必须调用 Unsubscribe 清理。
func Subscribe() chan Event {
	events := make(chan Event, Buffer)
	mu.Lock()
	streams[events] = struct{}{}
	mu.Unlock()
	return events
}

// Unsubscribe 注销一条事件流; 通道可能已被拥塞时的 Publish 关闭, 重复关闭会 panic, 故先检查存在性。
func Unsubscribe(events chan Event) {
	mu.Lock()
	defer mu.Unlock()
	if _, exists := streams[events]; exists {
		delete(streams, events)
		close(events)
	}
}
