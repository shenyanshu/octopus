package relay

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
)

// ScoreFlushInterval 是评分落库周期: 分数只实时存于内存, 每周期把发生变化的成员批量写库。
// 周期越长重启后可能丢失的分差越大, 落库频率与数据库压力则越低, 5 秒是两者的折中。
const ScoreFlushInterval = 5 * time.Second

// ScoreFlushTimeout 是单次落库的数据库调用预算, 必须短于落库周期:
// 挂起的库调用最多占用不足一个周期, 串行锁随之释放, 下一周期仍能带着最新分差重试;
// 对单实例的小批量 UPDATE, 3 秒足够宽裕。
const ScoreFlushTimeout = 3 * time.Second

// ScoreDrainTimeout 是停机收尾落库的预算: 覆盖等待在途周期落库(其自身至多 ScoreFlushTimeout)
// 加最终一次落库, 仍有界保证停机不悬挂。
const ScoreDrainTimeout = 5 * time.Second

var (
	// scoreDirtyMu 只保护 dirtyScores; 记账路径持有 routeMu 时调用 markScoreDirty,
	// 锁序恒为 routeMu → scoreDirtyMu, 落库路径从不嵌套持有两把锁, 无死锁面。
	scoreDirtyMu sync.Mutex
	dirtyScores  = make(map[int]struct{}) // 分数已变化待落库的成员 ID 集合; 只增删标记, 持续失败时规模有界于成员总数。
	// flushOwnership 是落库的唯一令牌(容量 1): 周期落库非阻塞抢令牌, 抢不到即跳过;
	// 停机收尾用 select 连同自身预算一起等待令牌, 在途落库无视取消时由预算有界返回。
	flushOwnership = make(chan struct{}, 1)
	// scoreFrozen 是评分写入冻结闸门: 停机收尾在 routeMu 临界区内建立,
	// 记账路径在同一临界区内核对, 冻结之后的成败记账既不改分也不弄脏。
	scoreFrozen atomic.Bool
)

// init 投递唯一落库令牌; 测试通过 resetScorePersistenceForTest 复位。
func init() { flushOwnership <- struct{}{} }

// writeScores 是评分落库的唯一出口; 声明为包内函数变量仅为测试注入阻塞与失败, 生产恒为 op 批量更新。
var writeScores = op.GroupItemUpdateScores

// markScoreDirty 标记成员分数已变化待落库; 只做内存集合操作, 供持有 routeMu 的记账路径调用,
// 保证转发热路径上绝不出现数据库 I/O。
func markScoreDirty(itemID int) {
	scoreDirtyMu.Lock()
	dirtyScores[itemID] = struct{}{}
	scoreDirtyMu.Unlock()
}

// RestoreScores 在进程启动、尚未承接请求时把分组成员的持久化分数装载回路由状态。
// 恢复不区分模式: 非评分分组装的是休眠评分, 供日后切换/重建回评分模式时沿用;
// 故障转移与手动的选路不读评分, 休眠条目不影响它们的任何决策。
// 只装载偏离初始分的成员: 缺省条目与未记录语义同为初始分, runtime.scores 的既有契约不变。
// 现任成员与路由代数不恢复, 由重启后的首次选路重建; 本函数只应在启动接线中调用一次。
func RestoreScores() {
	for _, group := range op.GroupList() {
		restoreGroupScores(group)
	}
}

// restoreGroupScores 把单个分组的持久化分数装入路由状态; 分数以实时状态为权威, 装载后不再回写。
// 持久 0 分是鉴权归零的合法历史, 不得当作缺失处理成初始分。
func restoreGroupScores(group model.Group) {
	routeMu.Lock()
	defer routeMu.Unlock()

	route := groupRouteLocked(group)
	for _, item := range group.Items {
		if item.Score != scoreInitial {
			route.Scores[item.ID] = item.Score
		}
	}
}

// FlushScores 把发生变化的评分批量落库, 由定时任务周期调用。
// ctx 承载本次落库的取消与超时预算, 由调用方给出, 周期路径用 ScoreFlushTimeout;
// 预算耗尽按失败处理, dirty 保留待下一轮重写。
// 非阻塞抢令牌, 抢不到即跳过: 上一次仍在途说明数据库正慢, 排队只会堆积协程,
// 跳过后 dirty 仍在, 由下一周期兜底; 由此任意时刻至多一个落库在途, 旧快照不可能并发覆盖新值。
func FlushScores(ctx context.Context) error {
	select {
	case <-flushOwnership:
	default:
		return nil
	}
	defer func() { flushOwnership <- struct{}{} }()
	// 拿到令牌后核对冻结闸门: 收尾已开始的进程里, 周期落库不得再发起任何写;
	// 放在令牌内核对使"先读到旧闸门再抢令牌"的窗口不可能漏过收尾之后的写。
	if scoreFrozen.Load() {
		return nil
	}
	return flushLocked(ctx)
}

// DrainInFlightError 表示收尾预算耗尽而落库仍在途: 数据库可能仍被在途写访问,
// 停机流程不得在其后关闭数据库, 进程退出时由系统回收残留资源。
type DrainInFlightError struct {
	Cause error
}

func (e *DrainInFlightError) Error() string {
	return fmt.Sprintf("score drain still in flight: %v", e.Cause)
}

func (e *DrainInFlightError) Unwrap() error { return e.Cause }

// DrainScores 停机收尾落库: 冻结评分写入, 等待在途落库交还令牌(而非跳过),
// 再把剩余 dirty 按最新值一次写库。必须在数据库关闭前、在途请求排空后调用;
// 进程即将退出, 失败仅上报, 不做重试循环。ctx 给出有限预算, 超时按失败返回, 停机不悬挂。
func DrainScores(ctx context.Context) error {
	// 冻结必须与记账核对同处 routeMu 临界区: 冻结建立前开始的记账必然已改完分并标好 dirty,
	// 由本次终写收走; 冻结建立后开始的记账必然看到冻结而放弃, 终写结果即为最终值。
	routeMu.Lock()
	scoreFrozen.Store(true)
	routeMu.Unlock()

	// 有界等待唯一令牌: 在途落库无视取消时, 由本 ctx 预算兜底返回, 绝不轮询、绝不另起协程。
	select {
	case <-flushOwnership:
	case <-ctx.Done():
		return &DrainInFlightError{Cause: ctx.Err()}
	}

	// 终写由单个有界协程承载并经通道回报: 驱动若无视上下文而挂起, 收尾仍按自身预算有界返回;
	// 令牌停留在写协程内直至真正完成 —— 期间任何路径都写不了库, 停机据 DrainInFlightError 放弃数据库关闭。
	type writeOutcome struct{ err error }
	outcome := make(chan writeOutcome, 1)
	go func() {
		defer func() { flushOwnership <- struct{}{} }()
		outcome <- writeOutcome{flushLocked(ctx)}
	}()

	select {
	case result := <-outcome:
		return result.err
	case <-ctx.Done():
		return &DrainInFlightError{Cause: ctx.Err()}
	}
}

// BeginScoreImportBarrier 进入逻辑导入的身份隔离: 等待在途落库真正完成后持有落库令牌,
// 导入期间任何周期/收尾落库都无法启动, 防止旧分数快照写到导入复用的成员主键上。
// 预算耗尽返回错误, 调用方必须放弃导入且不得调用 EndScoreImportBarrier。
func BeginScoreImportBarrier(ctx context.Context) error {
	select {
	case <-flushOwnership:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// EndScoreImportBarrier 结束身份隔离: 作废全部待写分数(成员身份已整体改写, 陈旧快照一律不可信),
// 丢弃受影响分组的路由状态(评分与瞬态一并重来), 最后归还落库令牌。
// 必须与成功的 BeginScoreImportBarrier 配对(通常 defer), 任何导入结果下都要执行。
func EndScoreImportBarrier(affectedGroupIDs []int) {
	// 令牌仍在手, 全程不会有任何落库启动, 清理顺序无并发窗口。
	scoreDirtyMu.Lock()
	dirtyScores = make(map[int]struct{})
	scoreDirtyMu.Unlock()
	for _, groupID := range affectedGroupIDs {
		ResetRouteState(groupID)
	}
	flushOwnership <- struct{}{}
}

// flushLocked 执行一次完整的落库流程, 调用方必须持有落库令牌。
func flushLocked(ctx context.Context) error {
	// 先交换 dirty 集合: 落库 I/O 期间的新变化进入新集合, 由下一周期落库。
	scoreDirtyMu.Lock()
	pending := dirtyScores
	dirtyScores = make(map[int]struct{})
	scoreDirtyMu.Unlock()
	if len(pending) == 0 {
		return nil
	}

	// 锁内只取最新分数快照, 数据库 I/O 全部在锁外; 每次落库都重新读实时值,
	// 上一次失败的快照绝不复用。待写成员已不在任何路由状态(成员被删或状态被重置)时丢弃:
	// 对已删行写库毫无意义, 也不允许把成员复活。
	routeMu.Lock()
	snapshot := make(map[int]int, len(pending))
	for _, route := range routes {
		for itemID := range pending {
			if score, ok := route.Scores[itemID]; ok {
				snapshot[itemID] = score
			}
		}
	}
	routeMu.Unlock()
	if len(snapshot) == 0 {
		return nil
	}

	if err := writeScores(ctx, snapshot); err != nil {
		// 失败只重新标记 dirty, 下一周期按最新分数重写; dirty 始终有界于成员数。
		scoreDirtyMu.Lock()
		for itemID := range snapshot {
			dirtyScores[itemID] = struct{}{}
		}
		scoreDirtyMu.Unlock()
		return err
	}
	return nil
}
