package relay

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/bestruirui/octopus/internal/model"
)

// 本文件是路由状态生命周期操作的聚焦测试: 切模保分与成员删除的代数失效。
// 生命周期语义(保什么、丢什么)在这里单点证明, 处理器层只补接线证据。

// assertTransientReset 断言分组的瞬态路由已清零。
func assertTransientReset(t *testing.T, groupID int) {
	t.Helper()
	routeMu.Lock()
	defer routeMu.Unlock()
	route := routes[groupID]
	if route == nil {
		t.Fatalf("分组 %d 的路由状态不存在", groupID)
	}
	if route.CurrentItemID != 0 || route.ProbeItemID != 0 || route.AffinityUntil != 0 || route.affinityArmed {
		t.Fatalf("瞬态路由未清零: %+v", route)
	}
	if len(route.Cooldowns) != 0 {
		t.Fatalf("冷却未清零: %v", route.Cooldowns)
	}
}

// epochOf 断言辅助: 返回分组当前路由代数。
func epochOf(groupID int) uint64 {
	routeMu.Lock()
	defer routeMu.Unlock()
	if route := routes[groupID]; route != nil {
		return route.epoch
	}
	return 0
}

// 切模只清瞬态: 未落库的最新分原样保留, 旧代数迟到结果被拒, 切回后按保留分选路。
func TestModeSwitchRebuildKeepsUnflushedScores(t *testing.T) {
	resetScorePersistenceForTest()
	group := scoredGroupOf(11, 22)

	_, epoch := pickScoredItem(group, nil)                    // 11 现任。
	recordScoredFailure(group, 11, epoch, errors.New("boom")) // 99 → 97, 未落库。
	recordScoredSuccess(group, 22, epoch)                     // 22 → 100, 未落库。

	RebuildRouteState(group.ID)
	assertTransientReset(t, group.ID)
	if epochOf(group.ID) == epoch {
		t.Fatalf("切模后代数未前进")
	}
	// 评分与模式无关, 必须原样保留(含未落库值)。
	if got := scoreOfItem(group.ID, 11); got != 97 {
		t.Fatalf("切模后成员 11 分数 = %d, 想要保留的 97", got)
	}
	if got := scoreOfItem(group.ID, 22); got != 100 {
		t.Fatalf("切模后成员 22 分数 = %d, 想要保留的 100", got)
	}

	// 切模前在途请求的迟到结果携带旧代数: 不得写回重建后的状态。
	recordScoredSuccess(group, 11, epoch)
	recordScoredFailure(group, 11, epoch, errors.New("late boom"))
	if got := scoreOfItem(group.ID, 11); got != 97 {
		t.Fatalf("旧代数迟到结果改写了保留分数: %d", got)
	}

	// 切回评分模式后的首次选路必须选择保留分最高的 22。
	if item, _ := pickScoredItem(group, nil); item.ID != 22 {
		t.Fatalf("切回后首次选路 = %d, 想要保留分最高的 22", item.ID)
	}

	// 未落库的 dirty 不因切模丢失: 落库一次写入保留的最新值。
	written := make(chan map[int]int, 1)
	withWriteScores(t, func(_ context.Context, scores map[int]int) error {
		written <- maps.Clone(scores)
		return nil
	})
	if err := FlushScores(context.Background()); err != nil {
		t.Fatalf("落库失败: %v", err)
	}
	got := <-written
	if got[11] != 97 || got[22] != 100 {
		t.Fatalf("落库值 = %v, 想要切模期间保留的 {11:97, 22:100}", got)
	}
}

// 切到故障转移后保留的评分不得影响其选路: 故障转移仍按配置顺序。
func TestModeSwitchAwayKeepsFailoverRoutingUnchanged(t *testing.T) {
	resetScorePersistenceForTest()
	group := scoredGroupOf(11, 22)

	_, epoch := pickScoredItem(group, nil)
	recordScoredSuccess(group, 22, epoch) // 22 满分, 若评分渗入将改变选路。
	RebuildRouteState(group.ID)

	failover := group
	failover.Mode = model.GroupModeFailover
	// CurrentItemID 已清零, 故障转移必须按优先级取配置顺序首位 11, 与 22 的满分无关。
	if item, _ := pickGroupItem(failover, nil); item.ID != 11 {
		t.Fatalf("切到故障转移后选路 = %d, 想要配置顺序首位 11", item.ID)
	}
}

// 分组删除仍是彻底丢弃: 评分与路由状态一并消失, 不存在任何保留。
func TestGroupDeleteStillDiscardsRouteState(t *testing.T) {
	resetScorePersistenceForTest()
	group := scoredGroupOf(11)

	_, epoch := pickScoredItem(group, nil)
	recordScoredSuccess(group, 11, epoch)

	ResetRouteState(group.ID)
	routeMu.Lock()
	_, exists := routes[group.ID]
	routeMu.Unlock()
	if exists {
		t.Fatalf("分组删除后路由状态仍残留")
	}
}

// 成员删除: 保留成员评分原样保留, 被删成员的评分清理且旧代数迟到结果既写不回也弄脏不了。
func TestMemberRemovalPrunesScoresAndInvalidatesEpoch(t *testing.T) {
	resetScorePersistenceForTest()
	group := scoredGroupOf(11, 22)

	_, epoch := pickScoredItem(group, nil)
	recordScoredFailure(group, 11, epoch, errors.New("boom")) // 11 → 97, dirty。
	recordScoredSuccess(group, 22, epoch)                     // 22 → 100, dirty。

	// 整体替换语义: 新集合只剩 22。
	PruneRouteMembers(group.ID, []int{22})
	assertTransientReset(t, group.ID)
	if got := scoreOfItem(group.ID, 22); got != 100 {
		t.Fatalf("保留成员 22 分数 = %d, 想要 100", got)
	}
	if epochOf(group.ID) == epoch {
		t.Fatalf("成员删除后代数未前进")
	}

	// 在途请求的迟到结果携带旧代数: 不得把已删成员 11 写回评分表。
	recordScoredSuccess(group, 11, epoch)
	recordScoredFailure(group, 11, epoch, errors.New("late boom"))
	routeMu.Lock()
	_, reinserted := routes[group.ID].Scores[11]
	routeMu.Unlock()
	if reinserted {
		t.Fatalf("旧代数迟到结果把已删成员写回了评分表")
	}

	// 落库只写保留成员: 11 的残留 dirty 标记在快照对账时被丢弃, 不得发起对已删行的写。
	written := make(chan map[int]int, 1)
	withWriteScores(t, func(_ context.Context, scores map[int]int) error {
		written <- maps.Clone(scores)
		return nil
	})
	if err := FlushScores(context.Background()); err != nil {
		t.Fatalf("落库失败: %v", err)
	}
	got := <-written
	if len(got) != 1 || got[22] != 100 {
		t.Fatalf("落库值 = %v, 想要仅含保留成员的 {22:100}", got)
	}
}

// PruneRouteMembers 对不存在的路由是无操作: 手动分组等从未建路的分组不受影响。
func TestMemberRemovalWithoutRouteIsNoop(t *testing.T) {
	resetScorePersistenceForTest()
	PruneRouteMembers(999, []int{1})
	routeMu.Lock()
	_, exists := routes[999]
	routeMu.Unlock()
	if exists {
		t.Fatalf("无路由分组被 PruneRouteMembers 创建了状态")
	}
}
