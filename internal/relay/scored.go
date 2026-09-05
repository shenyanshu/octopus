package relay

import (
	"errors"
	"net/http"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

// 评分模式的分值边界与惩罚幅度, 单点维护: 任何调整都只改这里。
// 分数表达成员的相对健康度而非可用性开关, 故 0 分成员仍可被选中。
const (
	scoreInitial        = 99  // 尚未记录过分量的成员的默认分数。
	scoreMax            = 100 // 完整成功后的满分。
	scoreFailurePenalty = 2   // 普通可归因失败的扣分幅度。
	scoreFloor          = 0   // 分数下限, 不做时间恢复, 只能靠真实成功回升。
)

// pickScoredItem 评分模式选路: 在当前可用且本请求尚未排除的成员里选分数最高者。
// 同分时现任成员优先, 其余同分按配置顺序先到先得; 没有可选成员时返回零值。
// 换路只发生在此处: 成败上报不得改动现任成员, 否则并发迟到的结果会抢回或清掉新换的路由。
// 返回的路由代数供成败上报核对, 重置后的新路由不再接受旧代数的迟到结果。
func pickScoredItem(group model.Group, excluded map[int]bool) (model.GroupItem, uint64) {
	routeMu.Lock()
	defer routeMu.Unlock()

	route := groupRouteLocked(group)
	best := model.GroupItem{}
	bestScore := -1
	for _, item := range group.Items {
		// 渠道或凭据已停用、授权两侧缺失的成员不参与选路, 由配置恢复或等待路径处理。
		if excluded[item.ID] || !item.Available {
			continue
		}
		score := scoreOfLocked(route, item.ID)
		// 只有严格更高分才能取代已有候选; 现任成员例外, 同分即胜出以保持粘性。
		if score > bestScore || (score == bestScore && item.ID == route.CurrentItemID) {
			best, bestScore = item, score
		}
	}
	if best.ID == 0 {
		return best, route.epoch
	}
	if route.CurrentItemID != best.ID {
		route.CurrentItemID = best.ID
		publishRouteLocked(route)
	}
	return best, route.epoch
}

// recordScoredSuccess 记录评分模式下的一次完整成功: 该成员记满分。
// 只恢复分数, 不改现任成员: 迟到的成功不能推翻本请求已经换路建立的新路由;
// 路由代数不符或成员已删除时直接丢弃, 不污染重置后的新状态。
func recordScoredSuccess(group model.Group, itemID int, epoch uint64) {
	if group.Mode != model.GroupModeScored || itemOf(group, itemID).ID == 0 {
		return
	}
	routeMu.Lock()
	defer routeMu.Unlock()

	// 停机冻结核对与改分同处一个临界区: 收尾建立冻结时持有同一把锁,
	// 冻结后的记账必然看到冻结而放弃, 保证收尾终写的就是最终值。
	if scoreFrozen.Load() {
		return
	}

	route := routes[group.ID]
	if route == nil || route.epoch != epoch || scoreOfLocked(route, itemID) == scoreMax {
		return
	}
	route.Scores[itemID] = scoreMax
	markScoreDirty(itemID)
	publishRouteLocked(route)
}

// recordScoredFailure 记录评分模式下的一次可归因上游失败: 鉴权失败直接归零, 其余按惩罚扣分且下探到 0。
// 与成功同理只动分数: 迟到的失败不能清掉新的现任成员, 也不得把已删除成员的分数写回新状态。
func recordScoredFailure(group model.Group, itemID int, epoch uint64, err error) {
	if group.Mode != model.GroupModeScored || itemOf(group, itemID).ID == 0 {
		return
	}
	routeMu.Lock()
	defer routeMu.Unlock()

	// 与成功记账同一冻结边界: 停机收尾建立冻结后, 失败也不得再改分或弄脏。
	if scoreFrozen.Load() {
		return
	}

	route := routes[group.ID]
	if route == nil || route.epoch != epoch {
		return
	}
	current := scoreOfLocked(route, itemID)
	score := scoreFloor
	if !authFailure(err) {
		score = max(scoreFloor, current-scoreFailurePenalty)
	}
	// 分数无变化则不写不发, 避免重复失败刷爆状态流。
	if score == current {
		return
	}
	route.Scores[itemID] = score
	markScoreDirty(itemID)
	publishRouteLocked(route)
}

// recordCommittedStreamFailure 评分模式下已提交流式响应后的失败记账边界:
// 客户端已取消或失败不归因上游(本地编码, 客户端写失败)时不扣分, 其余按普通失败扣分。
// 已提交的响应不可换路, 扣分只影响后续请求。
func recordCommittedStreamFailure(group model.Group, itemID int, epoch uint64, err error, upstreamFault, clientGone bool) {
	if clientGone || !upstreamFault {
		return
	}
	recordScoredFailure(group, itemID, epoch, err)
}

// scoreOfLocked 返回成员当前分数, 尚未记录过的成员视为初始分; 调用方必须持有锁。
func scoreOfLocked(route *RouteState, itemID int) int {
	if score, ok := route.Scores[itemID]; ok {
		return score
	}
	return scoreInitial
}

// authFailure 结构化识别上游鉴权失败: 凭据被拒(401/403)意味着换路也救不了该成员, 直接归零。
// 只认错误链上的结构化状态码, 不做任何错误文本匹配, 以免上游措辞变化造成误判。
func authFailure(err error) bool {
	var httpErr *httpclient.Error
	if errors.As(err, &httpErr) && authFailureStatus(httpErr.StatusCode) {
		return true
	}
	var llmErr *llm.ResponseError
	if errors.As(err, &llmErr) && authFailureStatus(llmErr.StatusCode) {
		return true
	}
	return false
}

// authFailureStatus 判定 HTTP 状态码是否属于鉴权失败。
func authFailureStatus(statusCode int) bool {
	return statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden
}
