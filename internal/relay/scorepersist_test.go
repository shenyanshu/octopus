package relay

import (
	"context"
	"errors"
	"maps"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
)

// 本文件是评分持久化核心的聚焦测试: 启动恢复, dirty 标记, 串行落库与失败恢复。
// 真实数据库用例复用 forward 测试的 TestMain 库; 阻塞与失败用例通过落库 seam 注入。

// resetScorePersistenceForTest 清空路由与 dirty 状态, 复位冻结闸门并重投落库令牌, 保证用例互不渗透。
func resetScorePersistenceForTest() {
	resetRoutesForTest()
	scoreDirtyMu.Lock()
	dirtyScores = make(map[int]struct{})
	scoreDirtyMu.Unlock()
	scoreFrozen.Store(false)
	for {
		select {
		case <-flushOwnership:
			continue
		default:
		}
		break
	}
	// 非阻塞投放: 若终写协程的迟归还恰好抢先入槽, 保持一枚令牌现状即可, 复位绝不阻塞。
	select {
	case flushOwnership <- struct{}{}:
	default:
	}
}

// withWriteScores 注入落库 seam, 用例结束时按后进先出恢复。
func withWriteScores(t *testing.T, fn func(context.Context, map[int]int) error) {
	t.Helper()
	previous := writeScores
	writeScores = fn
	t.Cleanup(func() { writeScores = previous })
}

// dirtyScoreCount 断言辅助: 返回当前待落库成员数。
func dirtyScoreCount() int {
	scoreDirtyMu.Lock()
	defer scoreDirtyMu.Unlock()
	return len(dirtyScores)
}

// assertItemScore 断言成员持久分数列的当前库值。
func assertItemScore(t *testing.T, itemID, want int) {
	t.Helper()
	var item model.GroupItem
	if err := db.GetDB().First(&item, itemID).Error; err != nil {
		t.Fatalf("读成员 %d 失败: %v", itemID, err)
	}
	if item.Score != want {
		t.Fatalf("成员 %d 持久分数 = %d, 想要 %d", itemID, item.Score, want)
	}
}

// 持久分数装载: 0 是鉴权归零的合法分数不得当作缺失, 缺省分成员不产生条目以维持 runtime.scores 契约。
func TestRestoreGroupScoresFromPersistedValues(t *testing.T) {
	resetScorePersistenceForTest()
	group := scoredGroupOf(11, 22, 33, 44)
	group.Items[0].Score = 0
	group.Items[1].Score = 97
	group.Items[2].Score = 100
	group.Items[3].Score = 99

	restoreGroupScores(group)

	if got := scoreOfItem(group.ID, 11); got != 0 {
		t.Fatalf("持久 0 分被当作缺失, 恢复 = %d, 想要 0", got)
	}
	if got := scoreOfItem(group.ID, 22); got != 97 {
		t.Fatalf("成员 22 恢复 = %d, 想要 97", got)
	}
	if got := scoreOfItem(group.ID, 33); got != 100 {
		t.Fatalf("成员 33 恢复 = %d, 想要 100", got)
	}
	if got := scoreOfItem(group.ID, 44); got != scoreInitial {
		t.Fatalf("缺省分成员恢复 = %d, 想要按未记录语义的初始分", got)
	}
	routeMu.Lock()
	entries := len(routes[group.ID].Scores)
	routeMu.Unlock()
	if entries != 3 {
		t.Fatalf("评分表条目 = %d, 想要只含偏离初始分的 3 条", entries)
	}

	// 恢复后的首次选路必须选择持久分最高的 33, 而不是配置顺序首位 11。
	if item, _ := pickScoredItem(group, nil); item.ID != 33 {
		t.Fatalf("恢复后首次选路 = %d, 想要持久分最高的 33", item.ID)
	}
}

// 启动恢复走真实缓存与库值: 只为评分分组建状态, 恢复结果直接影响首次选路。
func TestRestoreScoresFromDatabaseCache(t *testing.T) {
	fixture := seedScoredGroup(t, model.GroupModeScored, model.ProtocolOpenAIChatCompletion,
		newUpstreamStub(t, passthroughGood), newUpstreamStub(t, passthroughGood))
	// 把第二成员写成低分, 模拟重启前的历史健康度。
	if err := db.GetDB().Model(&model.GroupItem{}).
		Where("id = ?", fixture.members[1].itemID).Update("score", 40).Error; err != nil {
		t.Fatalf("写入持久分数失败: %v", err)
	}
	refreshGroupCache(t)
	resetRoutesForTest()

	RestoreScores()

	if got := scoreOfItem(fixture.id, fixture.members[1].itemID); got != 40 {
		t.Fatalf("恢复后的持久分数 = %d, 想要 40", got)
	}
	// 首次选路必须避开恢复出的低分成员。
	group := mustGroupOf(t, fixture.id)
	if item, _ := pickScoredItem(group, nil); item.ID != fixture.members[0].itemID {
		t.Fatalf("恢复后首次选路 = %d, 想要初始分的配置顺序首位", item.ID)
	}
}

// 非评分分组的恢复装的是休眠评分: 状态存在但不影响故障转移选路。
func TestRestoreScoresBuildsDormantRouteInNonScoredModes(t *testing.T) {
	fixture := seedScoredGroup(t, model.GroupModeFailover, model.ProtocolOpenAIChatCompletion,
		newUpstreamStub(t, passthroughGood), newUpstreamStub(t, passthroughGood))
	// 持久 0 分若渗入选路将排到最后, 休眠条目不得影响故障转移的配置顺序。
	if err := db.GetDB().Model(&model.GroupItem{}).
		Where("id = ?", fixture.members[0].itemID).Update("score", 0).Error; err != nil {
		t.Fatalf("写入持久分数失败: %v", err)
	}
	refreshGroupCache(t)
	resetRoutesForTest()

	RestoreScores()

	if got := scoreOfItem(fixture.id, fixture.members[0].itemID); got != 0 {
		t.Fatalf("休眠恢复分数 = %d, 想要 0", got)
	}
	group := mustGroupOf(t, fixture.id)
	// 故障转移仍按配置顺序取首位, 休眠的 0 分不参与任何决策。
	if item := pickGroupItem(group); item.ID != fixture.members[0].itemID {
		t.Fatalf("休眠分数影响了故障转移选路: %d", item.ID)
	}
}

// 全场景: 手动模式下的持久分数经"重启"(清空进程内状态)恢复为休眠评分,
// 切换回评分模式后原样生效并主导选路; 瞬态一律不恢复。
func TestDormantScoreRestoredThroughModeRoundTrip(t *testing.T) {
	fixture := seedScoredGroup(t, model.GroupModeManual, model.ProtocolOpenAIChatCompletion,
		newUpstreamStub(t, passthroughGood), newUpstreamStub(t, passthroughGood))
	// 重启前的历史健康度: 成员 0 是 97, 成员 1 是 100。
	if err := db.GetDB().Model(&model.GroupItem{}).Where("id = ?", fixture.members[0].itemID).Update("score", 97).Error; err != nil {
		t.Fatalf("写入持久分数失败: %v", err)
	}
	if err := db.GetDB().Model(&model.GroupItem{}).Where("id = ?", fixture.members[1].itemID).Update("score", 100).Error; err != nil {
		t.Fatalf("写入持久分数失败: %v", err)
	}
	refreshGroupCache(t)
	resetRoutesForTest()

	// 进程重启: 启动恢复把手动分组的持久分数装成休眠评分。
	RestoreScores()
	if got := scoreOfItem(fixture.id, fixture.members[0].itemID); got != 97 {
		t.Fatalf("休眠恢复分数 = %d, 想要 97", got)
	}
	if got := scoreOfItem(fixture.id, fixture.members[1].itemID); got != 100 {
		t.Fatalf("休眠恢复分数 = %d, 想要 100", got)
	}

	// 切换到评分模式(重建瞬态, 保留评分): 恢复的分数立即主导首次选路。
	RebuildRouteState(fixture.id)
	scoredGroup := mustGroupOf(t, fixture.id)
	scoredGroup.Mode = model.GroupModeScored
	if item, _ := pickScoredItem(scoredGroup, nil); item.ID != fixture.members[1].itemID {
		t.Fatalf("休眠分数主导的首次选路 = %d, 想要 100 分的成员 1", item.ID)
	}
	routeMu.Lock()
	current := routes[fixture.id].CurrentItemID
	routeMu.Unlock()
	if current != fixture.members[1].itemID {
		t.Fatalf("现任成员 = %d, 想要 100 分的成员 1", current)
	}
}

// 分数真实变化才标 dirty, 落库写最终值; 已满分的成功不产生新 dirty。
func TestScoreChangeFlushesToDatabase(t *testing.T) {
	resetScorePersistenceForTest()
	fixture := seedScoredGroup(t, model.GroupModeScored, model.ProtocolOpenAIChatCompletion,
		newUpstreamStub(t, passthroughGood), newUpstreamStub(t, passthroughGood))
	group := mustGroupOf(t, fixture.id)
	itemID := fixture.members[0].itemID
	plainErr := errors.New("boom")

	_, epoch := pickScoredItem(group, nil)
	recordScoredFailure(group, itemID, epoch, plainErr) // 99 → 97
	if dirtyScoreCount() != 1 {
		t.Fatalf("分数变化后 dirty = %d, 想要 1", dirtyScoreCount())
	}

	if err := FlushScores(context.Background()); err != nil {
		t.Fatalf("落库失败: %v", err)
	}
	if dirtyScoreCount() != 0 {
		t.Fatalf("成功落库后 dirty = %d, 想要 0", dirtyScoreCount())
	}
	assertItemScore(t, itemID, 97)

	recordScoredSuccess(group, itemID, epoch) // 97 → 100
	if err := FlushScores(context.Background()); err != nil {
		t.Fatalf("落库失败: %v", err)
	}
	assertItemScore(t, itemID, 100)

	// 已满分的成功不构成变化: 不得反复标记 dirty 刷爆落库。
	recordScoredSuccess(group, itemID, epoch)
	if dirtyScoreCount() != 0 {
		t.Fatalf("已满分的成功仍标记了 dirty")
	}
}

// 落库 I/O 阻塞期间的新变化进入新 dirty; 旧快照写完后下一次落库写最新值; 两次落库不可并发。
func TestFlushBlockedIOKeepsNewChangesDirty(t *testing.T) {
	resetScorePersistenceForTest()
	group := scoredGroupOf(11)
	const itemID = 11

	block := make(chan struct{})
	firstWritten := make(chan map[int]int, 1)
	var writeCalls atomic.Int32
	withWriteScores(t, func(_ context.Context, scores map[int]int) error {
		writeCalls.Add(1)
		firstWritten <- maps.Clone(scores)
		<-block
		return nil
	})

	_, epoch := pickScoredItem(group, nil)
	recordScoredFailure(group, itemID, epoch, errors.New("boom")) // 99 → 97

	done := make(chan error, 1)
	go func() { done <- FlushScores(context.Background()) }()
	if written := <-firstWritten; written[itemID] != 97 {
		t.Fatalf("首次落库快照 = %v, 想要 97", written)
	}

	// I/O 阻塞期间分数再变 97 → 95: 必须进入新 dirty, 而不是被旧快照吞掉。
	recordScoredFailure(group, itemID, epoch, errors.New("boom again"))
	if dirtyScoreCount() != 1 {
		t.Fatalf("I/O 期间的新变化未进入新 dirty, dirty = %d", dirtyScoreCount())
	}

	// 重叠的落库调用必须立即跳过: 不排队、不并发写。
	overlapReturned := make(chan struct{})
	go func() { FlushScores(context.Background()); close(overlapReturned) }()
	select {
	case <-overlapReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("重叠的 FlushScores 未被跳过, 出现排队或并发")
	}
	if writeCalls.Load() != 1 {
		t.Fatalf("落库出现并发: 写调用 = %d, 想要仍为 1", writeCalls.Load())
	}

	close(block)
	if err := <-done; err != nil {
		t.Fatalf("首次落库失败: %v", err)
	}
	if dirtyScoreCount() != 1 {
		t.Fatalf("旧落库结束后 dirty = %d, 想要仍含待写的 95", dirtyScoreCount())
	}

	// 下一次落库必须写最新值 95, 而非旧快照 97。
	nextWritten := make(chan map[int]int, 1)
	withWriteScores(t, func(_ context.Context, scores map[int]int) error {
		nextWritten <- maps.Clone(scores)
		return nil
	})
	if err := FlushScores(context.Background()); err != nil {
		t.Fatalf("第二次落库失败: %v", err)
	}
	if written := <-nextWritten; written[itemID] != 95 {
		t.Fatalf("第二次落库 = %v, 想要最新的 95", written)
	}
	if dirtyScoreCount() != 0 {
		t.Fatalf("第二次落库后 dirty = %d, 想要 0", dirtyScoreCount())
	}
}

// 落库失败不影响内存选路, 下一次落库按最新值重写, 不复用失败快照。
func TestFlushFailureKeepsDirtyAndRewritesLatest(t *testing.T) {
	resetScorePersistenceForTest()
	group := scoredGroupOf(11, 22)
	withWriteScores(t, func(_ context.Context, _ map[int]int) error { return errors.New("db locked") })

	_, epoch := pickScoredItem(group, nil)
	recordScoredFailure(group, 11, epoch, errors.New("boom")) // 99 → 97

	if err := FlushScores(context.Background()); err == nil {
		t.Fatalf("落库失败被吞掉, 想要返回错误")
	}
	if dirtyScoreCount() != 1 {
		t.Fatalf("失败后 dirty = %d, 想要重新标记为 1", dirtyScoreCount())
	}

	// 内存选路不受落库失败影响: 22 记满分后必须接过路由。
	recordScoredSuccess(group, 22, epoch)
	if item, _ := pickScoredItem(group, nil); item.ID != 22 {
		t.Fatalf("落库失败影响了内存选路, 选路 = %d, 想要 22", item.ID)
	}

	// 再扣一分 97 → 95 后换成功 seam: 下一次落库必须写最新值 95, 而非失败的 97 快照。
	written := make(chan map[int]int, 1)
	withWriteScores(t, func(_ context.Context, scores map[int]int) error {
		written <- maps.Clone(scores)
		return nil
	})
	recordScoredFailure(group, 11, epoch, errors.New("boom again"))
	if err := FlushScores(context.Background()); err != nil {
		t.Fatalf("第二次落库失败: %v", err)
	}
	if got := <-written; got[11] != 95 {
		t.Fatalf("失败后的落库值 = %v, 想要重新读取的最新值 95", got)
	}
	if dirtyScoreCount() != 0 {
		t.Fatalf("第二次落库后 dirty = %d, 想要 0", dirtyScoreCount())
	}
}

// 迟到落库碰上已删除的成员行: UPDATE 影响 0 行, 不报错也不复活该行;
// 路由状态清理后残留的 dirty 标记不再发起任何写。
func TestFlushDoesNotResurrectDeletedItem(t *testing.T) {
	resetScorePersistenceForTest()
	fixture := seedScoredGroup(t, model.GroupModeScored, model.ProtocolOpenAIChatCompletion,
		newUpstreamStub(t, passthroughGood), newUpstreamStub(t, passthroughGood))
	group := mustGroupOf(t, fixture.id)
	itemID := fixture.members[0].itemID

	_, epoch := pickScoredItem(group, nil)
	recordScoredFailure(group, itemID, epoch, errors.New("boom")) // 99 → 97, dirty

	// 成员行被直接删除而路由清理尚未发生: dirty 仍在, 落库必须静默落空。
	if err := db.GetDB().Delete(&model.GroupItem{}, itemID).Error; err != nil {
		t.Fatalf("删除成员行失败: %v", err)
	}
	if err := FlushScores(context.Background()); err != nil {
		t.Fatalf("对已删行落库报错: %v", err)
	}
	var count int64
	if err := db.GetDB().Model(&model.GroupItem{}).Where("id = ?", itemID).Count(&count).Error; err != nil {
		t.Fatalf("统计成员行失败: %v", err)
	}
	if count != 0 {
		t.Fatalf("已删成员被落库复活")
	}

	// 路由状态已被重置(成员删除或切模触发), 手动塞回 dirty 模拟迟到标记: 不得发起写。
	withWriteScores(t, func(_ context.Context, _ map[int]int) error {
		t.Fatal("无可写成员时不应调用落库")
		return nil
	})
	resetRoutesForTest()
	scoreDirtyMu.Lock()
	dirtyScores[itemID] = struct{}{}
	scoreDirtyMu.Unlock()
	if err := FlushScores(context.Background()); err != nil {
		t.Fatalf("空快照落库报错: %v", err)
	}
}

// 落库的取消与超时预算必须到达写 seam: 预算耗尽按失败返回并保住 dirty,
// 内存权威不受影响, 重试按最新值写入而非复用超时前的快照。
func TestFlushDeadlineReachesWriteAndKeepsDirty(t *testing.T) {
	resetScorePersistenceForTest()
	group := scoredGroupOf(11)
	const itemID = 11

	seamEntered := make(chan struct{}, 1)
	withWriteScores(t, func(ctx context.Context, scores map[int]int) error {
		seamEntered <- struct{}{}
		<-ctx.Done() // 模拟一次挂起直到预算耗尽的数据库调用
		return ctx.Err()
	})

	_, epoch := pickScoredItem(group, nil)
	recordScoredFailure(group, itemID, epoch, errors.New("boom")) // 99 → 97

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- FlushScores(ctx) }()
	<-seamEntered
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("预算耗尽未按失败返回: %v", err)
	}
	if dirtyScoreCount() != 1 {
		t.Fatalf("预算耗尽后 dirty = %d, 想要重新标记为 1", dirtyScoreCount())
	}
	if got := scoreOfItem(group.ID, itemID); got != 97 {
		t.Fatalf("预算耗尽影响了内存权威: 分数 = %d, 想要 97", got)
	}

	// 重试必须重新读取最新值 95, 而非超时前的 97 快照。
	written := make(chan map[int]int, 1)
	withWriteScores(t, func(_ context.Context, scores map[int]int) error {
		written <- maps.Clone(scores)
		return nil
	})
	recordScoredFailure(group, itemID, epoch, errors.New("boom again")) // 97 → 95
	if err := FlushScores(context.Background()); err != nil {
		t.Fatalf("重试落库失败: %v", err)
	}
	if got := <-written; got[itemID] != 95 {
		t.Fatalf("重试落库值 = %v, 想要最新的 95", got)
	}
	if dirtyScoreCount() != 0 {
		t.Fatalf("重试落库后 dirty = %d, 想要 0", dirtyScoreCount())
	}
}

// 停机收尾必须等待在途周期落库完成而非跳过, 之后把 I/O 期间的新变化按最新值写库。
func TestDrainWaitsForInFlightFlushThenWritesNewerValue(t *testing.T) {
	resetScorePersistenceForTest()
	group := scoredGroupOf(11)
	const itemID = 11

	var writeCalls atomic.Int32
	release := make(chan struct{})
	firstWritten := make(chan map[int]int, 1)
	secondWritten := make(chan map[int]int, 1)
	withWriteScores(t, func(_ context.Context, scores map[int]int) error {
		if writeCalls.Add(1) == 1 {
			firstWritten <- maps.Clone(scores)
			<-release // 模拟在途周期落库的慢 I/O。
			return nil
		}
		secondWritten <- maps.Clone(scores)
		return nil
	})

	_, epoch := pickScoredItem(group, nil)
	recordScoredFailure(group, itemID, epoch, errors.New("boom")) // 99 → 97

	periodicDone := make(chan error, 1)
	go func() { periodicDone <- FlushScores(context.Background()) }()
	if written := <-firstWritten; written[itemID] != 97 {
		t.Fatalf("在途周期落库快照 = %v, 想要 97", written)
	}

	// 在途 I/O 期间、收尾冻结前的新变化: 旧落库写 97, 收尾必须补写最新的 95。
	// (冻结建立之后的记账会被有意忽略, 该行为由 TestFreezeBlocksPostDrainScoring 单独证明。)
	recordScoredFailure(group, itemID, epoch, errors.New("boom again")) // 97 → 95。

	// 周期落库仍在途时启动收尾: 它必须等待令牌, 不得跳过造成分差丢失。
	drainDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		drainDone <- DrainScores(ctx)
	}()
	select {
	case err := <-drainDone:
		t.Fatalf("收尾未等待在途落库即返回: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	if err := <-periodicDone; err != nil {
		t.Fatalf("在途周期落库失败: %v", err)
	}
	if err := <-drainDone; err != nil {
		t.Fatalf("收尾失败: %v", err)
	}
	if written := <-secondWritten; written[itemID] != 95 {
		t.Fatalf("收尾落库值 = %v, 想要最新的 95", written)
	}
	if dirtyScoreCount() != 0 {
		t.Fatalf("收尾后 dirty = %d, 想要 0", dirtyScoreCount())
	}
}

// 收尾建立的冻结使此后的成败记账既不改分也不弄脏; 周期落库也被令牌核对挡住。
func TestFreezeBlocksPostDrainScoring(t *testing.T) {
	resetScorePersistenceForTest()
	group := scoredGroupOf(11)
	const itemID = 11

	written := make(chan map[int]int, 1)
	withWriteScores(t, func(_ context.Context, scores map[int]int) error {
		written <- maps.Clone(scores)
		return nil
	})
	_, epoch := pickScoredItem(group, nil)
	recordScoredFailure(group, itemID, epoch, errors.New("boom")) // 99 → 97。
	if err := DrainScores(context.Background()); err != nil {
		t.Fatalf("收尾失败: %v", err)
	}
	if got := <-written; got[itemID] != 97 {
		t.Fatalf("收尾落库值 = %v, 想要 97", got)
	}

	// 冻结后的记账: 分数与 dirty 都不得变化, 也不得发起任何写。
	recordScoredFailure(group, itemID, epoch, errors.New("after freeze")) // 若未冻结将是 95。
	recordScoredSuccess(group, itemID, epoch)
	if got := scoreOfItem(group.ID, itemID); got != 97 {
		t.Fatalf("冻结后的记账改了分: %d, 想要保持 97", got)
	}
	withWriteScores(t, func(context.Context, map[int]int) error {
		t.Fatal("冻结后不得再发起任何落库写")
		return nil
	})
	if err := FlushScores(context.Background()); err != nil {
		t.Fatalf("周期落库应无害返回: %v", err)
	}
	if dirtyScoreCount() != 0 {
		t.Fatalf("冻结后的记账弄脏了落库集合: %d", dirtyScoreCount())
	}
}

// 在途落库持有令牌且无视取消时, 收尾必须由自身预算有界返回, 不得轮询或悬挂。
func TestDrainDeadlineReturnsWhileWriterHoldsOwnership(t *testing.T) {
	resetScorePersistenceForTest()
	group := scoredGroupOf(11)
	const itemID = 11

	writerEntered := make(chan struct{}, 1)
	release := make(chan struct{})
	withWriteScores(t, func(ctx context.Context, _ map[int]int) error {
		writerEntered <- struct{}{}
		<-release // 在途落库的库调用挂起且无视自身取消, 令牌被其长期持有。
		return nil
	})
	_, epoch := pickScoredItem(group, nil)
	recordScoredFailure(group, itemID, epoch, errors.New("boom")) // 99 → 97。

	writerDone := make(chan error, 1)
	go func() { writerDone <- FlushScores(context.Background()) }()
	<-writerEntered

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := DrainScores(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("令牌被无视取消的写入持有时, 收尾未按预算失败返回: %v", err)
	}
	// 收尾返回时写入仍被挂起: 令牌未被偷走, 写入结束后才归还。
	select {
	case <-writerDone:
		t.Fatalf("在途写入被收尾打断")
	default:
	}
	close(release)
	if err := <-writerDone; err != nil {
		t.Fatalf("在途写入失败: %v", err)
	}
	// 写入正常返回即完成了令牌的延迟归还; 此后再无 dirty, 不再有可写内容。
}

// 收尾自身的预算耗尽必须有界退出并保住 dirty。
func TestDrainDeadlineExitsBoundedly(t *testing.T) {
	resetScorePersistenceForTest()
	group := scoredGroupOf(11)
	const itemID = 11

	withWriteScores(t, func(ctx context.Context, _ map[int]int) error {
		<-ctx.Done() // 模拟挂起直到预算耗尽的数据库调用。
		return ctx.Err()
	})
	_, epoch := pickScoredItem(group, nil)
	recordScoredFailure(group, itemID, epoch, errors.New("boom")) // 99 → 97。

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := DrainScores(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("收尾预算耗尽未按失败返回: %v", err)
	}
	// 终写协程与收尾共用同一预算, 可能尚未跑完: 等令牌归还即等它真正结束, 断言才是确定性的。
	if err := BeginScoreImportBarrier(context.Background()); err != nil {
		t.Fatalf("终写未归还令牌: %v", err)
	}
	if dirtyScoreCount() != 1 {
		t.Fatalf("收尾失败后 dirty = %d, 想要保留 1", dirtyScoreCount())
	}
	EndScoreImportBarrier(nil)
}

// 终写无视上下文挂起时: 收尾按自身预算返回 DrainInFlightError 并暴露预算耗尽原因,
// 令牌留在终写协程内直至真正完成 —— 期间任何路径都写不了库。
func TestDrainFinalWriteHangReturnsInFlightError(t *testing.T) {
	resetScorePersistenceForTest()
	group := scoredGroupOf(11)
	const itemID = 11

	writerEntered := make(chan struct{})
	writerReturned := make(chan struct{})
	release := make(chan struct{})
	withWriteScores(t, func(ctx context.Context, _ map[int]int) error {
		writerEntered <- struct{}{}
		<-release // 模拟无视取消而挂起的数据库驱动。
		close(writerReturned)
		return nil
	})

	_, epoch := pickScoredItem(group, nil)
	recordScoredFailure(group, itemID, epoch, errors.New("boom")) // 99 → 97, dirty。

	drainDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		drainDone <- DrainScores(ctx)
	}()
	<-writerEntered // 终写已真正进入写出口后, 收尾的预算才开始有意义。

	err := <-drainDone
	var inFlight *DrainInFlightError
	if !errors.As(err, &inFlight) {
		t.Fatalf("终写挂起时收尾未返回 DrainInFlightError: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("未暴露预算耗尽原因: %v", err)
	}
	select {
	case <-writerReturned:
		t.Fatalf("终写被收尾打断")
	default:
	}

	// 释放后终写完成并归还令牌: 以导入隔离立即成功作为令牌已归还的确定性证明。
	close(release)
	<-writerReturned
	if err := BeginScoreImportBarrier(context.Background()); err != nil {
		t.Fatalf("终写完成后令牌未归还: %v", err)
	}
	EndScoreImportBarrier(nil)
}

// 导入隔离的进入必须等待在途落库真正完成; 等不到时按预算失败且不偷令牌。
func TestImportBarrierBeginWaitsForInFlightWriter(t *testing.T) {
	resetScorePersistenceForTest()
	group := scoredGroupOf(11)
	const itemID = 11

	writerEntered := make(chan struct{})
	writerReturned := make(chan struct{})
	release := make(chan struct{})
	withWriteScores(t, func(_ context.Context, _ map[int]int) error {
		writerEntered <- struct{}{}
		<-release
		close(writerReturned)
		return nil
	})

	_, epoch := pickScoredItem(group, nil)
	recordScoredFailure(group, itemID, epoch, errors.New("boom")) // 97, dirty。

	writerDone := make(chan error, 1)
	go func() { writerDone <- FlushScores(context.Background()) }()
	<-writerEntered

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := BeginScoreImportBarrier(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("在途写持有令牌时隔离未按预算失败: %v", err)
	}
	select {
	case <-writerReturned:
		t.Fatalf("隔离抢走了在途写的令牌")
	default:
	}

	close(release)
	if err := <-writerDone; err != nil {
		t.Fatalf("在途写失败: %v", err)
	}

	// 令牌空闲后隔离立即成功, End 归还令牌可再次进入。
	if err := BeginScoreImportBarrier(context.Background()); err != nil {
		t.Fatalf("隔离进入失败: %v", err)
	}
	EndScoreImportBarrier(nil)
	if err := BeginScoreImportBarrier(context.Background()); err != nil {
		t.Fatalf("End 未归还令牌: %v", err)
	}
	EndScoreImportBarrier(nil)
}

// 逻辑导入复用已删成员主键: 隔离必须作废陈旧 dirty 与受影响路由, 旧快照不得落到新身份上。
func TestImportBarrierProtectsReusedIdentityInDatabase(t *testing.T) {
	resetScorePersistenceForTest()
	fixture := seedScoredGroup(t, model.GroupModeScored, model.ProtocolOpenAIChatCompletion,
		newUpstreamStub(t, passthroughGood), newUpstreamStub(t, passthroughGood))
	group := mustGroupOf(t, fixture.id)
	itemID := fixture.members[0].itemID

	_, epoch := pickScoredItem(group, nil)
	recordScoredFailure(group, itemID, epoch, errors.New("boom")) // 99 → 97, 未落库。

	// 模拟导入: 删除成员行后以同一主键重建, 新身份取库默认 99。
	var old model.GroupItem
	if err := db.GetDB().First(&old, itemID).Error; err != nil {
		t.Fatalf("读旧成员行失败: %v", err)
	}
	if err := db.GetDB().Delete(&model.GroupItem{}, itemID).Error; err != nil {
		t.Fatalf("删旧成员行失败: %v", err)
	}
	recreated := old
	recreated.Score = 0
	if err := db.GetDB().Create(&recreated).Error; err != nil {
		t.Fatalf("复用主键重建成员失败: %v", err)
	}

	// 隔离-导入-释放: 陈旧 dirty 全部作废, 受影响分组路由整体重置。
	if err := BeginScoreImportBarrier(context.Background()); err != nil {
		t.Fatalf("隔离进入失败: %v", err)
	}
	EndScoreImportBarrier([]int{fixture.id})

	// 隔离释放后旧快照无处可落: 周期落库跑空, 复用主键仍是默认 99。
	if err := FlushScores(context.Background()); err != nil {
		t.Fatalf("落库失败: %v", err)
	}
	var row model.GroupItem
	if err := db.GetDB().First(&row, itemID).Error; err != nil {
		t.Fatalf("读复用成员行失败: %v", err)
	}
	if row.Score != 99 {
		t.Fatalf("复用主键被旧快照污染: %d, 想要 99", row.Score)
	}
	routeMu.Lock()
	gone := routes[fixture.id] == nil
	routeMu.Unlock()
	if !gone {
		t.Fatalf("受影响分组的路由状态未被重置")
	}
}
