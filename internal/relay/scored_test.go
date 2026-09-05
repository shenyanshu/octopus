package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

// resetRoutesForTest 清空全局路由状态, 保证用例互不渗透。
func resetRoutesForTest() {
	routeMu.Lock()
	routes = make(map[int]*RouteState)
	routeMu.Unlock()
}

// scoredGroupOf 构造评分模式的分组, 可变参数顺序即成员配置顺序, 成员默认可用。
func scoredGroupOf(itemIDs ...int) model.Group {
	group := model.Group{ID: 1, Mode: model.GroupModeScored, Name: "scored-test"}
	for _, id := range itemIDs {
		group.Items = append(group.Items, model.GroupItem{ID: id, GroupID: group.ID, Available: true, Enabled: true})
	}
	return group
}

// scoreOfItem 断言辅助: 返回成员当前分数, 无路由状态时即初始分。
func scoreOfItem(groupID, itemID int) int {
	routeMu.Lock()
	defer routeMu.Unlock()
	if route := routes[groupID]; route != nil {
		return scoreOfLocked(route, itemID)
	}
	return scoreInitial
}

// incumbentOf 断言辅助: 返回分组当前成员。
func incumbentOf(groupID int) int {
	routeMu.Lock()
	defer routeMu.Unlock()
	if route := routes[groupID]; route != nil {
		return route.CurrentItemID
	}
	return 0
}

// pickScored 断言辅助: 执行一次选路并返回成员与路由代数。
func pickScored(group model.Group, excluded map[int]bool) (model.GroupItem, uint64) {
	return pickScoredItem(group, excluded)
}

func TestPickScoredItemInitialFollowsConfigOrder(t *testing.T) {
	resetRoutesForTest()
	group := scoredGroupOf(11, 22, 33)

	item, _ := pickScored(group, nil)
	if item.ID != 11 {
		t.Fatalf("初始选路 = %d, 想要配置顺序首位 11", item.ID)
	}
	if got := incumbentOf(group.ID); got != 11 {
		t.Fatalf("现任成员 = %d, 想要 11", got)
	}
}

func TestPickScoredItemTiePrefersIncumbent(t *testing.T) {
	resetRoutesForTest()
	group := scoredGroupOf(11, 22, 33)

	// 排除 11 后选路 22, 使其成为现任。
	pickScored(group, map[int]bool{11: true})
	if got := incumbentOf(group.ID); got != 22 {
		t.Fatalf("现任成员 = %d, 想要 22", got)
	}
	// 三家同分: 现任成员 22 必须胜出, 否则粘性失效。
	if item, _ := pickScored(group, nil); item.ID != 22 {
		t.Fatalf("同分选路 = %d, 想要现任成员 22", item.ID)
	}
}

func TestPickScoredItemPrefersHigherScoreOverConfigOrder(t *testing.T) {
	resetRoutesForTest()
	group := scoredGroupOf(11, 22)

	item, epoch := pickScored(group, nil) // 11 成为现任。
	recordScoredSuccess(group, 22, epoch)
	if item, _ = pickScored(group, nil); item.ID != 22 {
		t.Fatalf("选路 = %d, 想要更高分的 22", item.ID)
	}
}

func TestPickScoredItemSkipsUnavailable(t *testing.T) {
	resetRoutesForTest()
	group := scoredGroupOf(11, 22)
	group.Items[0].Available = false // 渠道停用或授权残缺的成员不参与选路。

	item, _ := pickScored(group, nil)
	if item.ID != 22 {
		t.Fatalf("选路 = %d, 想要跳过不可用成员后的 22", item.ID)
	}
}

func TestScoredSuccessSetsMaxScore(t *testing.T) {
	resetRoutesForTest()
	group := scoredGroupOf(11)

	_, epoch := pickScored(group, nil)
	recordScoredSuccess(group, 11, epoch)
	if got := scoreOfItem(group.ID, 11); got != scoreMax {
		t.Fatalf("成功后分数 = %d, 想要 %d", got, scoreMax)
	}
}

func TestScoredOrdinaryFailureDeductsAndFloors(t *testing.T) {
	resetRoutesForTest()
	group := scoredGroupOf(11)
	plainErr := errors.New("upstream non-stream response timeout")

	_, epoch := pickScored(group, nil)
	recordScoredFailure(group, 11, epoch, plainErr)
	if got := scoreOfItem(group.ID, 11); got != scoreInitial-scoreFailurePenalty {
		t.Fatalf("一次普通失败后分数 = %d, 想要 %d", got, scoreInitial-scoreFailurePenalty)
	}

	// 从满分连续失败超过可扣完的次数, 分数停在 0 不下探为负。
	recordScoredSuccess(group, 11, epoch)
	for i := 0; i < scoreMax/scoreFailurePenalty+10; i++ {
		recordScoredFailure(group, 11, epoch, plainErr)
	}
	if got := scoreOfItem(group.ID, 11); got != scoreFloor {
		t.Fatalf("连续失败后分数 = %d, 想要下限 %d", got, scoreFloor)
	}
}

func TestScoredAuthFailureZeroesScore(t *testing.T) {
	resetRoutesForTest()
	group := scoredGroupOf(11)
	authErr := fmt.Errorf("round failed: %w: body", &httpclient.Error{
		StatusCode: http.StatusUnauthorized,
		Status:     "401 Unauthorized",
	})

	_, epoch := pickScored(group, nil)
	recordScoredSuccess(group, 11, epoch)
	recordScoredFailure(group, 11, epoch, authErr)
	if got := scoreOfItem(group.ID, 11); got != scoreFloor {
		t.Fatalf("鉴权失败后分数 = %d, 想要直接归零 %d", got, scoreFloor)
	}
}

func TestPickScoredItemZeroScoreStillSelectable(t *testing.T) {
	resetRoutesForTest()
	group := scoredGroupOf(11, 22)
	authErr := &httpclient.Error{StatusCode: http.StatusForbidden}

	_, epoch := pickScored(group, nil)
	pickScored(group, map[int]bool{11: true}) // 22 成为现任。
	recordScoredFailure(group, 11, epoch, authErr)
	recordScoredFailure(group, 22, epoch, authErr)
	if got := scoreOfItem(group.ID, 22); got != scoreFloor {
		t.Fatalf("预备条件失败: 22 分数 = %d, 想要 0", got)
	}
	// 全员 0 分仍必须可选: 分数是相对健康度而非可用性开关; 同分沿用现任。
	if item, _ := pickScored(group, nil); item.ID != 22 {
		t.Fatalf("全员 0 分选路 = %d, 想要现任成员 22", item.ID)
	}
}

func TestPickScoredItemSkipsExcludedWithinRequest(t *testing.T) {
	resetRoutesForTest()
	group := scoredGroupOf(11, 22, 33)

	if item, _ := pickScored(group, map[int]bool{11: true}); item.ID != 22 {
		t.Fatalf("排除 11 后选路 = %d, 想要 22", item.ID)
	}
	if item, _ := pickScored(group, map[int]bool{11: true, 22: true}); item.ID != 33 {
		t.Fatalf("排除 11,22 后选路 = %d, 想要 33", item.ID)
	}
	if item, _ := pickScored(group, map[int]bool{11: true, 22: true, 33: true}); item.ID != 0 {
		t.Fatalf("全部成员已排除仍选路 = %d, 想要零值以终结请求", item.ID)
	}
}

func TestScoredMemberDeletionCleansState(t *testing.T) {
	resetRoutesForTest()
	group := scoredGroupOf(11, 22)

	_, epoch := pickScored(group, nil) // 11 成为现任。
	recordScoredFailure(group, 11, epoch, errors.New("boom"))
	recordScoredSuccess(group, 22, epoch)

	// 删除成员 11 后再选路: 其分数与现任引用都应被清理。
	group.Items = group.Items[1:]
	if item, _ := pickScored(group, nil); item.ID != 22 {
		t.Fatalf("删除成员后选路 = %d, 想要 22", item.ID)
	}
	routeMu.Lock()
	_, staleScore := routes[group.ID].Scores[11]
	routeMu.Unlock()
	if staleScore {
		t.Fatalf("已删除成员的分数未被清理")
	}
	if got := incumbentOf(group.ID); got != 22 {
		t.Fatalf("删除现任成员后现任 = %d, 想要 22", got)
	}

	// 成员删除后的迟到结果不得把已删成员的分数写回状态。
	recordScoredSuccess(group, 11, epoch)
	recordScoredFailure(group, 11, epoch, errors.New("late boom"))
	routeMu.Lock()
	_, reinserted := routes[group.ID].Scores[11]
	routeMu.Unlock()
	if reinserted {
		t.Fatalf("已删除成员的分数被迟到结果重新写入")
	}
}

func TestScoredLateSuccessRestoresScoreWithoutStealingIncumbent(t *testing.T) {
	resetRoutesForTest()
	group := scoredGroupOf(11, 22)

	_, epoch := pickScored(group, nil)                        // 11 现任。
	recordScoredFailure(group, 11, epoch, errors.New("boom")) // 11 扣分。
	pickScored(group, map[int]bool{11: true})                 // 本请求换路 22。
	recordScoredSuccess(group, 22, epoch)                     // 22 满分且现任。
	recordScoredSuccess(group, 11, epoch)                     // 11 上迟到的成功。
	if got := scoreOfItem(group.ID, 11); got != scoreMax {
		t.Fatalf("迟到成功后 11 分数 = %d, 想要 %d", got, scoreMax)
	}
	if got := incumbentOf(group.ID); got != 22 {
		t.Fatalf("迟到的成功抢回了现任成员: 现任 = %d, 想要 22", got)
	}
}

func TestScoredLateFailureKeepsNewIncumbent(t *testing.T) {
	resetRoutesForTest()
	group := scoredGroupOf(11, 22)

	_, epoch := pickScored(group, nil)
	recordScoredFailure(group, 11, epoch, errors.New("boom"))
	pickScored(group, map[int]bool{11: true})
	recordScoredSuccess(group, 22, epoch)

	recordScoredFailure(group, 11, epoch, errors.New("late boom"))
	if got := incumbentOf(group.ID); got != 22 {
		t.Fatalf("迟到的失败清掉了新现任: 现任 = %d, 想要 22", got)
	}
	if got := scoreOfItem(group.ID, 22); got != scoreMax {
		t.Fatalf("迟到的失败影响了无关成员: 22 分数 = %d, 想要 %d", got, scoreMax)
	}
	if got := scoreOfItem(group.ID, 11); got != scoreInitial-2*scoreFailurePenalty {
		t.Fatalf("迟到失败后 11 分数 = %d, 想要 %d", got, scoreInitial-2*scoreFailurePenalty)
	}
}

func TestResetRouteStateInvalidatesOldEpoch(t *testing.T) {
	resetRoutesForTest()
	group := scoredGroupOf(11)

	_, oldEpoch := pickScored(group, nil)
	recordScoredSuccess(group, 11, oldEpoch)
	ResetRouteState(group.ID)
	if got := scoreOfItem(group.ID, 11); got != scoreInitial {
		t.Fatalf("重置后分数 = %d, 想要初始分 %d", got, scoreInitial)
	}
	if got := incumbentOf(group.ID); got != 0 {
		t.Fatalf("重置后现任 = %d, 想要 0", got)
	}

	// 重建后的新路由代数不同: 旧代数的迟到成功与失败都必须被丢弃, 不得复活或污染新状态。
	item, newEpoch := pickScored(group, nil)
	if item.ID != 11 || newEpoch == oldEpoch {
		t.Fatalf("重建后代数 = %d/%d, 想要新代数且重新选路 11", newEpoch, oldEpoch)
	}
	recordScoredSuccess(group, 11, oldEpoch)
	recordScoredFailure(group, 11, oldEpoch, errors.New("late boom"))
	if got := scoreOfItem(group.ID, 11); got != scoreInitial {
		t.Fatalf("旧代数迟到结果写入 = %d, 想要仍为初始分 %d", got, scoreInitial)
	}
}

func TestScoredModeSwitchRejectsOldResults(t *testing.T) {
	resetRoutesForTest()
	group := scoredGroupOf(11)

	_, epoch := pickScored(group, nil)
	// 请求在途期间分组切到故障转移: 旧结果不得再写入评分状态。
	switched := group
	switched.Mode = model.GroupModeFailover
	recordScoredSuccess(switched, 11, epoch)
	recordScoredFailure(switched, 11, epoch, errors.New("late boom"))
	if got := scoreOfItem(group.ID, 11); got != scoreInitial {
		t.Fatalf("切模后旧结果写入 = %d, 想要初始分 %d", got, scoreInitial)
	}
}

func TestRecordCommittedStreamFailureBoundary(t *testing.T) {
	cases := []struct {
		name          string
		upstreamFault bool
		clientGone    bool
		wantScore     int
	}{
		{"上游流失败扣分", true, false, scoreInitial - scoreFailurePenalty},
		{"客户端取消不扣分", true, true, scoreInitial},
		{"本地编码或写失败不扣分", false, false, scoreInitial},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetRoutesForTest()
			group := scoredGroupOf(11)
			_, epoch := pickScored(group, nil)
			recordCommittedStreamFailure(group, 11, epoch, errors.New("stream broken"), tc.upstreamFault, tc.clientGone)
			if got := scoreOfItem(group.ID, 11); got != tc.wantScore {
				t.Fatalf("分数 = %d, 想要 %d", got, tc.wantScore)
			}
		})
	}
}

func TestRouteStateSnapshotIsCloned(t *testing.T) {
	resetRoutesForTest()
	group := scoredGroupOf(11)
	_, epoch := pickScored(group, nil)
	recordScoredSuccess(group, 11, epoch)

	stream := OpenRouteStream()
	defer CloseRouteStream(stream)
	publishRouteLocked(routes[group.ID])

	// 快照与增量都必须是内部评分表的副本: 修改它们不得影响路由状态。
	snapshot := RouteStateOf(group)
	snapshot.Scores[11] = 0
	if got := scoreOfItem(group.ID, 11); got != scoreMax {
		t.Fatalf("读取快照与内部状态共享底层 map, 修改后内部分数 = %d", got)
	}
	message := <-stream
	message.Scores[11] = 0
	if got := scoreOfItem(group.ID, 11); got != scoreMax {
		t.Fatalf("SSE 增量与内部状态共享底层 map, 修改后内部分数 = %d", got)
	}
}

func TestAuthFailureClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"httpclient 401", &httpclient.Error{StatusCode: http.StatusUnauthorized}, true},
		{"httpclient 403", &httpclient.Error{StatusCode: http.StatusForbidden}, true},
		{"包装链中的 httpclient 401", fmt.Errorf("round failed: %w: body", &httpclient.Error{StatusCode: http.StatusUnauthorized}), true},
		{"httpclient 500", &httpclient.Error{StatusCode: http.StatusInternalServerError}, false},
		{"httpclient 429", &httpclient.Error{StatusCode: http.StatusTooManyRequests}, false},
		{"llm 响应错误 403", &llm.ResponseError{StatusCode: http.StatusForbidden}, true},
		{"llm 响应错误无状态码", &llm.ResponseError{}, false},
		{"普通错误", errors.New("connection reset by peer"), false},
		{"含 401 字样的普通错误", errors.New("upstream responded 401 Unauthorized"), false},
	}
	for _, tc := range cases {
		if got := authFailure(tc.err); got != tc.want {
			t.Errorf("%s: authFailure = %v, 想要 %v", tc.name, got, tc.want)
		}
	}
}

func TestScoredRecordingIgnoresOtherModes(t *testing.T) {
	resetRoutesForTest()
	manual := model.Group{ID: 2, Mode: model.GroupModeManual, ActiveItemID: 7, Items: []model.GroupItem{{ID: 7, Available: true, Enabled: true}, {ID: 8, Available: true, Enabled: true}}}
	if item, _ := pickGroupItem(manual, nil); item.ID != 7 {
		t.Fatalf("手动模式选路 = %d, 想要人工指定的 7", item.ID)
	}

	failover := model.Group{ID: 3, Mode: model.GroupModeFailover, Items: []model.GroupItem{{ID: 31, Available: true, Enabled: true}, {ID: 32, Available: true, Enabled: true}}}
	if item, _ := pickGroupItem(failover, nil); item.ID != 31 {
		t.Fatalf("故障转移选路 = %d, 想要配置顺序首位 31", item.ID)
	}

	// 非 scored 模式下评分记账必须是纯无操作: 不建分数, 不动路由。
	recordScoredSuccess(manual, 7, 0)
	recordScoredSuccess(failover, 31, 0)
	recordScoredFailure(failover, 31, 0, &httpclient.Error{StatusCode: http.StatusUnauthorized})
	recordCommittedStreamFailure(failover, 31, 0, errors.New("stream broken"), true, false)
	if got := incumbentOf(3); got != 31 {
		t.Fatalf("评分记账改动了故障转移路由: 现任 = %d, 想要 31", got)
	}
	routeMu.Lock()
	cooldowns := len(routes[3].Cooldowns)
	routeMu.Unlock()
	if cooldowns != 0 {
		t.Fatalf("评分记账写入了冷却: 冷却条目 = %d, 想要 0", cooldowns)
	}
}

func TestFailoverCooldownSkipUnchanged(t *testing.T) {
	resetRoutesForTest()
	group := model.Group{
		ID:          4,
		Mode:        model.GroupModeFailover,
		RelayConfig: model.GroupRelayConfig{MemberMaxAttempts: 1, MemberCooldownSeconds: 60},
		Items:       []model.GroupItem{{ID: 41, Available: true, Enabled: true}, {ID: 42, Available: true, Enabled: true}},
	}
	item, foEpoch := pickGroupItem(group, nil)
	if item.ID != 41 {
		t.Fatalf("故障转移首次选路 = %d, 想要 41", item.ID)
	}
	if !recordRouteFailure(group, 41, 1, foEpoch) {
		t.Fatalf("达到总尝试次数应进入冷却")
	}
	if item, _ := pickGroupItem(group, nil); item.ID != 42 {
		t.Fatalf("冷却中的成员应被跳过, 选路 = %d, 想要 42", item.ID)
	}
	if item, _ := pickGroupItem(group, nil); item.ID != 42 {
		t.Fatalf("故障转移重复选路 = %d, 想要现任 42", item.ID)
	}
}

// Runtime 状态的 JSON 稳定契约: scores 恒为非空对象, 前端按 Record 处理不接受 null。
func TestRouteStateScoresJSONContract(t *testing.T) {
	resetRoutesForTest()
	group := scoredGroupOf(11, 22)

	// 未路由过的评分分组: scores 必须序列化为 {} 而非 null。
	encoded, err := json.Marshal(RouteStateOf(group))
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	if !strings.Contains(string(encoded), `"scores":{}`) {
		t.Fatalf("未路由分组的 scores 不是空对象: %s", encoded)
	}

	// 已有 0 分的成员序列化后必须保留 0, 不被缺省语义吞掉。
	_, routeEpoch := pickScoredItem(group, nil)
	authErr := fmt.Errorf("round failed: %w", &httpclient.Error{StatusCode: http.StatusUnauthorized})
	recordScoredFailure(group, 11, routeEpoch, authErr)
	encoded, err = json.Marshal(RouteStateOf(group))
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	var parsed struct {
		Scores map[int]int `json:"scores"`
	}
	if err := json.Unmarshal(encoded, &parsed); err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if len(parsed.Scores) != 1 || parsed.Scores[11] != 0 {
		t.Fatalf("0 分未被序列化保留: %s", encoded)
	}

	// 手动模式的未初始化状态同契约: 与 Cooldowns 一致给出空对象。
	manual := model.Group{ID: 5, Mode: model.GroupModeManual, ActiveItemID: 7}
	if state := RouteStateOf(manual); state.Scores == nil {
		t.Fatalf("手动模式未初始化状态的 Scores 为 nil")
	}
}
