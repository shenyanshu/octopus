package handlers

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/server/router"
	"github.com/gin-contrib/sse"
	"github.com/gin-gonic/gin"
)

func init() {
	router.NewGroupRouter("/api/v1/group").
		Use(middleware.Auth()).
		Use(middleware.RequireJSON()).
		AddRoute(
			router.NewRoute("/list", http.MethodGet).
				Handle(getGroupList),
		).
		AddRoute(
			router.NewRoute("/get/:id", http.MethodGet).
				Handle(getGroup),
		).
		AddRoute(
			router.NewRoute("/events", http.MethodGet).
				Handle(streamGroupEvents),
		).
		AddRoute(
			router.NewRoute("/create", http.MethodPost).
				Handle(createGroup),
		).
		AddRoute(
			router.NewRoute("/update/:id", http.MethodPost).
				Handle(updateGroup),
		).
		AddRoute(
			router.NewRoute("/delete/:id", http.MethodDelete).
				Handle(deleteGroup),
		).
		AddRoute(
			router.NewRoute("/item/enabled", http.MethodPost).
				Handle(toggleGroupItemEnabled),
		)
}

// groupResponse 是分组读取响应: 分组配置加其当前实时路由状态。
// 路由状态由 Relay 持有且不落库, 不属于分组配置, 故在此拼接而不作为 model.Group 的字段;
// 嵌入的分组字段在 JSON 中展平, 前端看到的仍是一层对象。
type groupResponse struct {
	model.Group
	// ActiveItemID 恒为零值, 只为遮蔽嵌入分组的同名字段: 当前成员一律从 runtime.current_item_id 读,
	// 出两份会让消费方无从选择, 而故障转移模式下嵌入的那份只是写入侧的陈旧值。
	// model.Group 上的 tag 供数据库转储使用不能删, 遮蔽也不能用 json:"-" ——
	// 带该 tag 的字段被 encoding/json 直接跳过, 不进候选集也就不参与同名冲突消解, 嵌入的那份仍会输出;
	// 同名加 omitempty 才既胜出又因零值被省略。
	ActiveItemID int              `json:"active_item_id,omitempty"`
	Runtime      relay.RouteState `json:"runtime"` // 分组当前的实时路由状态。
}

// 分组变更事件, SSE 收到后按事件名原样转发。
type groupEvent struct {
	Name string // SSE 事件名: changed 表示分组配置或成员发生变更, deleted 表示分组已删除。
	Data any    // changed 携带完整的 groupResponse, deleted 携带分组 ID。
}

const groupEventBuffer = 16 // 单个分组事件流连接的非阻塞消息缓冲容量。

var (
	groupEventMu sync.Mutex // groupEventMu 保护全部分组事件流连接。
	// groupMutationMu 已统一到 relay.GroupGate(共享读写锁)。
	// 变更编排持 groupGate 写锁覆盖 DB→缓存→路由→SSE 的完整序列; Forward 选路后持读锁复核。
	groupEventStreams = make(map[chan groupEvent]struct{}) // 全部分组事件流 SSE 连接。
)

// publishGroupEvent 非阻塞发布一条分组变更事件, 连接拥塞时关闭它并交给客户端重连后重新拉取对齐。
// 发布点在此而非 op: 事件携带的运行状态取自 Relay, 而 Relay 依赖 op, 放进 op 会形成循环依赖。
func publishGroupEvent(event groupEvent) {
	groupEventMu.Lock()
	defer groupEventMu.Unlock()

	for stream := range groupEventStreams {
		select {
		case stream <- event:
		default:
			delete(groupEventStreams, stream)
			close(stream)
		}
	}
}

// streamGroupEvents 向前端发送分组的变更事件与实时运行状态。
// 一条连接承载两个来源: 分组的增删改由本包发布, 故障转移模式的运行状态增量由 Relay 的路由自身发布。
// 不发初始快照: 分组读取接口已随分组带回当前状态, 前端由此拿到的初始值即全量。
func streamGroupEvents(c *gin.Context) {
	prepareSSE(c)
	routeUpdates := relay.OpenRouteStream()
	defer relay.CloseRouteStream(routeUpdates)

	events := make(chan groupEvent, groupEventBuffer)
	groupEventMu.Lock()
	groupEventStreams[events] = struct{}{}
	groupEventMu.Unlock()
	defer func() {
		groupEventMu.Lock()
		defer groupEventMu.Unlock()
		// 通道可能已被拥塞时的发布方关闭, 重复关闭会 panic。
		if _, exists := groupEventStreams[events]; exists {
			delete(groupEventStreams, events)
			close(events)
		}
	}()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := c.Writer.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			c.Writer.Flush()
		case update, ok := <-routeUpdates:
			if !ok {
				return
			}
			if err := sse.Encode(c.Writer, sse.Event{Event: "runtime", Data: update}); err != nil {
				return
			}
			c.Writer.Flush()
		case event, ok := <-events:
			if !ok {
				return
			}
			if err := sse.Encode(c.Writer, sse.Event{Event: event.Name, Data: event.Data}); err != nil {
				return
			}
			c.Writer.Flush()
		}
	}
}

// getGroupList 返回全部分组, 并为每个分组补齐当前的实时路由状态。
// 路由状态由 Relay 持有而 Relay 依赖 op, 故补齐点放在此处而非 op.GroupList 内。
func getGroupList(c *gin.Context) {
	groups := op.GroupList()
	responses := make([]groupResponse, len(groups))
	for i, group := range groups {
		responses[i] = groupResponse{Group: group, Runtime: relay.RouteStateOf(group)}
	}
	resp.Success(c, responses)
}

// getGroup 返回单个分组及其当前实时路由状态。
// 日志详情只关心承载该请求的那一个分组, 由此无需拉取整份分组列表。
func getGroup(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	group, err := op.GroupGet(id)
	if err != nil {
		resp.Error(c, http.StatusNotFound, err.Error())
		return
	}
	resp.Success(c, groupResponse{Group: group, Runtime: relay.RouteStateOf(group)})
}

func createGroup(c *gin.Context) {
	var req model.GroupCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	// 与 update/delete/toggle 同一写锁: 覆盖 DB→缓存→路由→SSE 发布,
	// 防止 delete/create ID reuse 时旧 delete cleanup 晚于新 create publication。
	relay.GroupGateLock()
	defer relay.GroupGateUnlock()
	group, err := op.GroupCreate(&req, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	response := groupResponse{Group: *group, Runtime: relay.RouteStateOf(*group)}
	publishGroupEvent(groupEvent{Name: "changed", Data: response})
	resp.Success(c, response)
}

// updateGroup 更新分组配置, 成员和手动模式的当前成员。
// 变更后的完整分组一并推入事件流: 其他会话由此同步到新的成员与配置, 无需各自重新拉取列表;
// 手动模式的当前成员也随之出去, 它由分组配置定稿, Relay 自身不会为它发运行状态增量。
func updateGroup(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	var req model.GroupUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	relay.GroupGateLock()
	defer relay.GroupGateUnlock()
	oldGroup, err := op.GroupGet(id)
	if err != nil {
		resp.Error(c, http.StatusNotFound, err.Error())
		return
	}
	group, err := op.GroupUpdate(id, &req, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	// 选择模式变化后瞬态路由不再适用: 重建使旧代数失效, 避免旧的冷却与亲和在切回故障转移时复活;
	// 评分与模式无关, 跨模式保留, 切回评分模式即恢复(含尚未落库的最新值)。
	if oldGroup.Mode != group.Mode {
		relay.RebuildRouteState(id)
	}
	// 成员被删除时按最终成员集合校正路由状态, 在生成更新响应前完成:
	// 保留成员的评分原样保留, 被删成员的评分与瞬态引用清理, 代数随之失效,
	// 在途请求的迟到结果既写不回新状态, 也弄脏不了已删成员。
	if req.Items != nil && groupMemberRemoved(oldGroup.Items, group.Items) {
		relay.PruneRouteMembers(id, groupItemIDs(group.Items))
	}
	response := groupResponse{Group: *group, Runtime: relay.RouteStateOf(*group)}
	publishGroupEvent(groupEvent{Name: "changed", Data: response})
	resp.Success(c, response)
}

// groupMemberRemoved 判断成员集合更新后是否有旧成员被删除; 新增与重排不属于删除。
func groupMemberRemoved(oldItems, newItems []model.GroupItem) bool {
	present := make(map[int]bool, len(newItems))
	for _, item := range newItems {
		present[item.ID] = true
	}
	for _, item := range oldItems {
		if !present[item.ID] {
			return true
		}
	}
	return false
}

// groupItemIDs 提取成员主键集合, 供路由状态按最新成员校正。
func groupItemIDs(items []model.GroupItem) []int {
	ids := make([]int, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func deleteGroup(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	relay.GroupGateLock()
	defer relay.GroupGateUnlock()
	if err := op.GroupDel(id, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	relay.ResetRouteState(id)
	publishGroupEvent(groupEvent{Name: "deleted", Data: id})
	resp.Success(c, "group deleted successfully")
}

// toggleGroupItemEnabled 切换分组成员的启用状态: 禁用使成员不参与选路但仍留在分组中,
// 启用使其重新可选; 两者都前进路由代数并刷新分组缓存, 路由状态与事件同步发布。
// 同一分组的并发切换由 groupGate(共享读写锁) 串行化, 保证 DB→缓存→路由的最终状态反映提交序。
func toggleGroupItemEnabled(c *gin.Context) {
	var req struct {
		GroupID int  `json:"group_id" binding:"required"`
		ItemID  int  `json:"item_id" binding:"required"`
		Enabled bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	relay.GroupGateLock()
	defer relay.GroupGateUnlock()

	group, err := op.GroupItemSetEnabled(c.Request.Context(), req.GroupID, req.ItemID, req.Enabled)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	// 启用切换不复用 PruneRouteMembers(后者会删评分): 评分是成员属性, 禁用只清瞬态引用。
	relay.ToggleRouteMemberEnabled(req.GroupID, req.ItemID, req.Enabled)
	response := groupResponse{Group: group, Runtime: relay.RouteStateOf(group)}
	publishGroupEvent(groupEvent{Name: "changed", Data: response})
	resp.Success(c, response)
}
