package handlers

import (
	"net/http"
	"strconv"
	"time"

	"github.com/bestruirui/octopus/internal/groupevents"
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

// publishGroupEvent 非阻塞发布一条分组变更事件, 复用 groupevents 共享总线。
func publishGroupEvent(event groupevents.Event) {
	groupevents.Publish(event)
}

// streamGroupEvents 向前端发送分组的变更事件与实时运行状态。
// 一条连接承载两个来源: 分组的增删改由本包发布, 故障转移模式的运行状态增量由 Relay 的路由自身发布。
// 不发初始快照: 分组读取接口已随分组带回当前状态, 前端由此拿到的初始值即全量。
func streamGroupEvents(c *gin.Context) {
	prepareSSE(c)
	routeUpdates := relay.OpenRouteStream()
	defer relay.CloseRouteStream(routeUpdates)

	events := groupevents.Subscribe()
	defer groupevents.Unsubscribe(events)

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
	responses := make([]groupevents.ChangedData, len(groups))
	for i, group := range groups {
		responses[i] = groupevents.ChangedData{Group: group, Runtime: relay.RouteStateOf(group)}
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
	resp.Success(c, groupevents.ChangedData{Group: group, Runtime: relay.RouteStateOf(group)})
}

func createGroup(c *gin.Context) {
	var req model.GroupCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	// 规则合法性在入口校验: handler 只做错误到状态码的映射, 单一判定口径仍是 model.CompileGroupPattern。
	if _, err := model.CompileGroupPattern(req.AutoAddPattern); err != nil {
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
	response := groupevents.ChangedData{Group: *group, Runtime: relay.RouteStateOf(*group)}
	publishGroupEvent(groupevents.Event{Name: "changed", Data: response})
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
	// 规则变更时校验: 指针区分"未提交保持不变"与"空串清除", 两者都不判为非法。
	if req.AutoAddPattern != nil {
		if _, err := model.CompileGroupPattern(*req.AutoAddPattern); err != nil {
			resp.Error(c, http.StatusBadRequest, err.Error())
			return
		}
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
	response := groupevents.ChangedData{Group: *group, Runtime: relay.RouteStateOf(*group)}
	publishGroupEvent(groupevents.Event{Name: "changed", Data: response})
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
	publishGroupEvent(groupevents.Event{Name: "deleted", Data: id})
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
	response := groupevents.ChangedData{Group: group, Runtime: relay.RouteStateOf(group)}
	publishGroupEvent(groupevents.Event{Name: "changed", Data: response})
	resp.Success(c, response)
}
