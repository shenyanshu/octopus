package relay

import (
	"maps"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bestruirui/octopus/internal/model"
)

// groupGate 是分组变更(写)与转发选路(读)的共享读写锁。
// handlers 的变更编排(create/update/delete/toggle/channel mutation/import)持写锁覆盖
// DB→缓存→路由→SSE 的完整序列, 使最终状态反映提交序;
// Forward 选路后在网络发送前持读锁从最新快照复核成员可用性, 保证写期间的变更不穿透到旧缓存选路结果。
// 读锁在网络发送前释放(不持锁等上游响应), 已派发的请求不受后续禁用影响。
// 单实例: 无分布式抽象, 不新增依赖。
var groupGate sync.RWMutex

// GroupGateLock 获取分组变更写锁: handlers 调用以覆盖 DB→缓存→路由→SSE 的完整序列。
// 与 Forward 选路后的读锁互斥, 使变更期间新请求读到最新已发布状态。
func GroupGateLock()   { groupGate.Lock() }
func GroupGateUnlock() { groupGate.Unlock() }

// RouteState 是一个分组的进程内路由状态; 跨该分组的全部请求共享。
// 同时作为路由流的消息形状与分组读取响应中的 runtime 字段: 冷却, 探测与亲和都是本包路由算法的概念,
// 故状态形状由本包定义, 分组的持久化配置不含它; 内部标志未导出, 不会随消息出到 JSON。
// 三种模式共用 CurrentItemID: 手动模式下即人工指定的成员, 故障转移模式下由路由决定,
// 评分模式下是同分优先的现任成员, 前端由此只读这一个字段即可知道当前承载请求的成员, 无需再按模式分支。
type RouteState struct {
	GroupID       int           `json:"group_id"`        // 状态所属的分组 ID, 供状态流按分组定位。
	CurrentItemID int           `json:"current_item_id"` // 当前承载请求的成员 ID, 0 表示尚未建立路由或未人工指定。
	ProbeItemID   int           `json:"probe_item_id"`   // 当前占用恢复探测的成员 ID, 同一分组同时只允许一个成员被探测; 仅故障转移模式使用。
	AffinityUntil int64         `json:"affinity_until"`  // 当前路由的亲和截止 Unix 毫秒时间, 0 表示无亲和; 仅故障转移模式使用。
	Cooldowns     map[int]int64 `json:"cooldowns"`       // 失败成员 ID 对应的冷却截止 Unix 毫秒时间, 已到期的条目由前端按当前时间忽略; 仅故障转移模式写入。
	Scores        map[int]int   `json:"scores"`          // 评分模式下各成员当前分数; 未记录的成员视为初始分, 前端按同规则补齐。

	affinityArmed bool   // 当前路由下一次成功后是否开始亲和, 仅故障切换后为真。
	epoch         uint64 // 路由代数, 重建时递增; 迟到的成败结果携带旧代数时不允许写入, 防止污染重置后的新状态。
}

const routeStreamBuffer = 16 // 单个路由流连接的非阻塞消息缓冲容量。

var (
	routeMu       sync.Mutex                           // routeMu 保护全部分组路由状态。
	routes        = make(map[int]*RouteState)          // routes 按分组 ID 保存路由状态。
	routeStreams  = make(map[chan RouteState]struct{}) // 全部路由 SSE 连接。
	routeEpochSeq atomic.Uint64                        // 路由代数发生器, 进程内递增, 不持久化。
)

// RouteStateOf 返回分组当前的实时路由状态, 供读取接口随分组一并返回。
// 手动模式没有进程内路由: 当前成员即人工指定的成员, 冷却与亲和均不适用, 故直接由分组配置得出。
func RouteStateOf(group model.Group) RouteState {
	if group.Mode == model.GroupModeManual {
		return RouteState{
			GroupID:       group.ID,
			CurrentItemID: group.ActiveItemID,
			Cooldowns:     map[int]int64{},
			Scores:        map[int]int{},
		}
	}

	routeMu.Lock()
	defer routeMu.Unlock()

	route := routes[group.ID]
	if route == nil {
		// 未初始化状态也必须给出非空 map: runtime JSON 的稳定契约是空对象而非 null。
		return RouteState{GroupID: group.ID, Cooldowns: map[int]int64{}, Scores: map[int]int{}}
	}
	state := *route
	state.Cooldowns = maps.Clone(route.Cooldowns)
	state.Scores = maps.Clone(route.Scores)
	return state
}

// ResetRouteState 丢弃分组的进程内路由状态, 用于分组被删除。
// 不丢弃的话冷却与亲和会在 failover 切到 manual 再切回来之后复活并继续影响选路, 分组删除后其状态也会永久残留;
// 分组删除同时意味着成员身份消亡, 评分随之一并丢弃, 不存在任何保留场景。
func ResetRouteState(groupID int) {
	routeMu.Lock()
	defer routeMu.Unlock()

	delete(routes, groupID)
}

// RebuildRouteState 丢弃分组的瞬态路由状态但保留评分, 用于分组切换选择模式。
// 切模使旧路由代数失效: 切换前在途请求的迟到结果不得写回重建后的状态;
// 现任成员、冷却、探测与亲和都是模式相关的瞬态语义, 一并作废;
// 评分是成员健康度的累积、与模式无关, 原样保留(含尚未落库的最新值), 切回评分模式即恢复。
// 非评分模式下保留的评分不参与选路, 仅为切回时恢复。
func RebuildRouteState(groupID int) {
	routeMu.Lock()
	defer routeMu.Unlock()

	route := routes[groupID]
	if route == nil {
		return
	}
	route.CurrentItemID = 0
	route.ProbeItemID = 0
	route.AffinityUntil = 0
	route.affinityArmed = false
	route.Cooldowns = make(map[int]int64)
	route.epoch = routeEpochSeq.Add(1)
}

// PruneRouteMembers 按最新成员集合校正分组路由状态, 供所有会删除成员的变更入口调用
// (分组成员整体替换、渠道删除或渠道配置变更经外键级联删除成员)。
// 保留成员的评分原样保留(含未落库值), 被删成员的评分与瞬态引用一并清理;
// 路由代数必须无条件前进: 在途请求的迟到结果不得把已删成员写回评分表或弄脏落库集合,
// 即使该成员尚无评分条目也要靠代数挡住。
// 调用方必须仅在确有成员被删除时调用: 代数前进会丢弃该分组全部在途结果, 误报会白白丢失合法迟到记账。
func PruneRouteMembers(groupID int, itemIDs []int) {
	routeMu.Lock()
	defer routeMu.Unlock()

	route := routes[groupID]
	if route == nil {
		return
	}
	present := make(map[int]bool, len(itemIDs))
	for _, id := range itemIDs {
		present[id] = true
	}
	for itemID := range route.Scores {
		if !present[itemID] {
			delete(route.Scores, itemID)
		}
	}
	for itemID := range route.Cooldowns {
		if !present[itemID] {
			delete(route.Cooldowns, itemID)
		}
	}
	if route.ProbeItemID != 0 && !present[route.ProbeItemID] {
		route.ProbeItemID = 0
	}
	if route.CurrentItemID != 0 && !present[route.CurrentItemID] {
		route.CurrentItemID = 0
		route.AffinityUntil = 0
		route.affinityArmed = false
	}
	route.epoch = routeEpochSeq.Add(1)
}

// ToggleRouteMemberEnabled 调整成员级启用状态对路由的影响:
// 禁用当前承载成员时清出 CurrentItemID/ProbeItemID 与亲和, 但保留评分(成员仍存在于分组);
// 启用使成员重新可选。两种方向都前进代数, 使在途请求的迟到结果不写回旧状态。
// 与 PruneRouteMembers 不同: 后者删除成员的评分, 这里只清瞬态引用, 评分一律保留。
func ToggleRouteMemberEnabled(groupID, itemID int, enabled bool) {
	routeMu.Lock()
	defer routeMu.Unlock()

	route := routes[groupID]
	if route == nil {
		return
	}
	if !enabled {
		if route.ProbeItemID == itemID {
			route.ProbeItemID = 0
		}
		if route.CurrentItemID == itemID {
			route.CurrentItemID = 0
			route.AffinityUntil = 0
			route.affinityArmed = false
		}
	}
	route.epoch = routeEpochSeq.Add(1)
	publishRouteLocked(route)
}

// pickGroupItem 按分组模式选择本轮目标成员, 没有可用成员时返回零值; group.Items 已按 Priority 升序排列。
// excluded 是本请求内已确认本地不可用的成员, 故障转移模式跳过它们避免反复选同一不可用成员;
// 手动模式不自动重选故不使用 excluded。
// 所有模式统一以 item.Available 为选路门槛: 渠道/凭据/成员自身任一禁用即不参与选路。
// 返回路由代数供成败上报核对: 重置后的新路由不再接受旧代数的迟到结果。
func pickGroupItem(group model.Group, excluded map[int]bool) (model.GroupItem, uint64) {
	if group.Mode == model.GroupModeManual {
		for _, item := range group.Items {
			if item.ID == group.ActiveItemID {
				// 手动模式不自动重选: 指定成员不可用即立即失败(零值)。
				if !item.Available {
					return model.GroupItem{}, 0
				}
				return item, 0
			}
		}
		return model.GroupItem{}, 0
	}

	routeMu.Lock()
	defer routeMu.Unlock()

	route := groupRouteLocked(group)
	now := time.Now().UnixMilli()
	if route.AffinityUntil <= now {
		route.AffinityUntil = 0
	}

	// 亲和期内沿用当前成员, 不提前探测已恢复的高优先级成员; 成员不可用或已被本请求排除时不可沿用。
	if current := itemOf(group, route.CurrentItemID); current.ID != 0 && route.AffinityUntil > now && current.Available && !excluded[current.ID] {
		return current, route.epoch
	}

	for _, item := range group.Items {
		// 不可用的成员不参与选路: 渠道/凭据/成员自身任一禁用均跳过。
		if !item.Available {
			continue
		}
		// 本请求已确认本地不可用的成员跳过: 避免反复选同一成员形成空转。
		if excluded[item.ID] {
			continue
		}
		// 遍历到当前成员说明比它优先级更高的成员都不可选, 沿用当前成员。
		if item.ID == route.CurrentItemID {
			break
		}
		deadline, cooling := route.Cooldowns[item.ID]
		if cooling && deadline > now {
			continue
		}
		// 冷却已到期的成员只放行一个探测请求, 避免全部请求同时涌向尚未恢复的成员。
		if cooling {
			if route.ProbeItemID != 0 {
				continue
			}
			route.ProbeItemID = item.ID
			publishRouteLocked(route)
			return item, route.epoch
		}
		route.CurrentItemID = item.ID
		publishRouteLocked(route)
		return item, route.epoch
	}
	if route.CurrentItemID != 0 && itemOf(group, route.CurrentItemID).Available && !excluded[route.CurrentItemID] {
		return itemOf(group, route.CurrentItemID), route.epoch
	}
	return model.GroupItem{}, route.epoch
}

// recordRouteSuccess 上报故障转移模式的一轮成功: 结束该成员的冷却与探测占用, 并在故障切换后按配置开始亲和。
// 评分模式的成功记账见 recordScoredSuccess, 不经过冷却与亲和。
func recordRouteSuccess(group model.Group, itemID int, epoch uint64) {
	if group.Mode != model.GroupModeFailover {
		return
	}

	routeMu.Lock()
	defer routeMu.Unlock()

	route := routes[group.ID]
	if route == nil || route.epoch != epoch {
		return
	}
	now := time.Now().UnixMilli()
	changed := false

	// 探测成功说明该成员已恢复, 解除冷却; 若当前路由不在亲和期内则立即切回该成员。
	if route.ProbeItemID == itemID {
		route.ProbeItemID = 0
		delete(route.Cooldowns, itemID)
		if route.CurrentItemID == 0 || route.AffinityUntil <= now {
			route.CurrentItemID = itemID
			route.AffinityUntil = 0
		}
		changed = true
	}
	// 亲和只在故障切换后的首次成功时开始, 使请求在一段时间内稳定留在备用成员上。
	if route.CurrentItemID == itemID && route.affinityArmed {
		route.affinityArmed = false
		if group.RelayConfig.MemberAffinitySeconds > 0 {
			route.AffinityUntil = now + int64(group.RelayConfig.MemberAffinitySeconds)*1000
			changed = true
		}
	}
	if changed {
		publishRouteLocked(route)
	}
}

// recordRouteFailure 上报故障转移模式的一轮失败: 达到配置的总尝试次数后将该成员打入冷却并让出当前路由, 返回是否已冷却。
// failures 为该成员在本请求内包含首次请求的连续失败次数, 由调用方累计; 评分模式的失败记账见 recordScoredFailure。
func recordRouteFailure(group model.Group, itemID, failures int, epoch uint64) bool {
	if group.Mode != model.GroupModeFailover {
		return false
	}

	routeMu.Lock()
	defer routeMu.Unlock()

	route := routes[group.ID]
	if route == nil || route.epoch != epoch {
		return false
	}
	// 探测请求只有一次机会, 常规成员达到配置的总尝试次数后进入冷却。
	if route.ProbeItemID != itemID && failures < group.RelayConfig.MemberMaxAttempts {
		return false
	}

	now := time.Now().UnixMilli()
	route.Cooldowns[itemID] = now + int64(group.RelayConfig.MemberCooldownSeconds)*1000
	if route.ProbeItemID == itemID {
		route.ProbeItemID = 0
	}
	// 当前路由失败才需要下一个成员开始亲和; 独立探测失败不影响当前路由。
	if route.CurrentItemID == itemID {
		route.CurrentItemID = 0
		route.AffinityUntil = 0
		route.affinityArmed = true
	}
	publishRouteLocked(route)
	return true
}

// releaseRouteProbe 归还未产生成败结论的探测占用, 用于请求被人工中止或客户端断开; 仅故障转移模式存在探测占用。
func releaseRouteProbe(group model.Group, itemID int, epoch uint64) {
	if group.Mode != model.GroupModeFailover {
		return
	}
	routeMu.Lock()
	defer routeMu.Unlock()

	if route := routes[group.ID]; route != nil && route.epoch == epoch && route.ProbeItemID == itemID {
		route.ProbeItemID = 0
		publishRouteLocked(route)
	}
}

// abandonRoundBeforeDispatch 释放已选但尚未派发的本轮 probe 占用。
// dispatch 点定义为 groupGate.RUnlock(): 在此之前的所有放弃路径都需调此函数,
// 否则故障转移的 ProbeItemID 会永久泄漏。epoch 保证迟到的旧请求不会误清新 route 的 probe;
// 手动/评分模式为 no-op; 幂等: route 已不存在或 ProbeItemID 已被清时安全跳过。
func abandonRoundBeforeDispatch(group model.Group, itemID int, epoch uint64) {
	releaseRouteProbe(group, itemID, epoch)
}

// groupRouteLocked 取出分组路由状态并清理已删除成员的残留; 调用方必须持有锁。
func groupRouteLocked(group model.Group) *RouteState {
	route := routes[group.ID]
	if route == nil {
		route = &RouteState{GroupID: group.ID, Cooldowns: make(map[int]int64), Scores: make(map[int]int), epoch: routeEpochSeq.Add(1)}
		routes[group.ID] = route
	}
	items := make(map[int]bool, len(group.Items))
	for _, item := range group.Items {
		items[item.ID] = true
	}
	for itemID := range route.Cooldowns {
		if !items[itemID] {
			delete(route.Cooldowns, itemID)
		}
	}
	// 评分表同样随成员删除清理, 分数是成员的属性而不是分组的历史。
	for itemID := range route.Scores {
		if !items[itemID] {
			delete(route.Scores, itemID)
		}
	}
	if route.ProbeItemID != 0 && !items[route.ProbeItemID] {
		route.ProbeItemID = 0
	}
	if route.CurrentItemID != 0 && !items[route.CurrentItemID] {
		route.CurrentItemID = 0
		route.AffinityUntil = 0
		route.affinityArmed = false
	}
	return route
}

// itemOf 返回分组内指定 ID 的成员, 不存在时返回零值。
func itemOf(group model.Group, itemID int) model.GroupItem {
	for _, item := range group.Items {
		if item.ID == itemID {
			return item
		}
	}
	return model.GroupItem{}
}

// publishRouteLocked 非阻塞发布路由状态, 连接拥塞时关闭它并交给客户端重连获取全量快照; 冷却表与评分表按值复制以免前端读到后续变更; 调用方必须持有锁。
func publishRouteLocked(route *RouteState) {
	message := *route
	message.Cooldowns = maps.Clone(route.Cooldowns)
	message.Scores = maps.Clone(route.Scores)
	for stream := range routeStreams {
		select {
		case stream <- message:
		default:
			delete(routeStreams, stream)
			close(stream)
		}
	}
}

// OpenRouteStream 注册路由流连接, 返回后续增量通道。
// 不再返回快照: 分组读取接口已随分组带回当前路由状态, 前端由此拿到的初始值即全量, 连接只负责增量。
func OpenRouteStream() chan RouteState {
	routeMu.Lock()
	defer routeMu.Unlock()

	stream := make(chan RouteState, routeStreamBuffer)
	routeStreams[stream] = struct{}{}
	return stream
}

// CloseRouteStream 注销并关闭指定路由流连接。
func CloseRouteStream(stream chan RouteState) {
	routeMu.Lock()
	defer routeMu.Unlock()

	if _, exists := routeStreams[stream]; exists {
		delete(routeStreams, stream)
		close(stream)
	}
}
