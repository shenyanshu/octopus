package channelsync

import (
	"context"
	"errors"
	"fmt"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/groupevents"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/modeldiscovery"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay"
	"github.com/charmbracelet/log"
	"gorm.io/gorm"
)

// Init 初始化协调器的根 context, 供所有同步 goroutine 继承取消信号。
func Init(ctx context.Context) {
	lifecycle.Lock()
	defer lifecycle.Unlock()
	rootCtx, rootCxl = context.WithCancel(ctx)
	stopped.Store(false)
}

// Stop 停止接受新同步并等待在途 worker 结束。
func Stop(ctx context.Context) error {
	lifecycle.Lock()
	stopped.Store(true)
	if rootCxl != nil {
		rootCxl()
	}
	lifecycle.Unlock()

	done := make(chan struct{})
	go func() {
		rootWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("channelsync Stop timeout: %w", ctx.Err())
	}
}

// StartSingle 启动单个渠道的同步。立即返回, 不等 HTTP。
// single 允许 auto_sync=false 但必须 channel.Enabled=true 且有 enabled key, 否则 skipped。
func StartSingle(channelID int) (started, busy, skipped bool, err error) {
	if stopped.Load() {
		return false, false, false, ErrStopped
	}
	snapshot, snapErr := op.ChannelSyncReadSnapshot(channelID)
	if snapErr != nil {
		if errors.Is(snapErr, gorm.ErrRecordNotFound) {
			return false, false, false, fmt.Errorf("%w: %v", ErrChannelNotFound, snapErr)
		}
		return false, false, false, snapErr
	}
	if !snapshot.Config.Enabled || len(snapshot.EnabledKeys) == 0 {
		return false, false, true, nil
	}
	claimResult := tryClaim(channelID, false)
	switch claimResult {
	case claimAccepted:
		// running 状态已在 tryClaim 内、go runWorker 之前设置,
		// 此处不再重复写, 防止快 worker 已完成后被覆盖为永久 running。
		return true, false, false, nil
	case claimBusy:
		return false, true, false, nil
	case claimStopped:
		return false, false, false, ErrStopped
	}
	return false, false, false, nil
}

// StartBatch 启动批量同步: 仅同步 Enabled && AutoSyncModels 的渠道。
func StartBatch() (model.ChannelSyncStartResult, error) {
	result := model.ChannelSyncStartResult{
		StartedIDs: []int{},
		BusyIDs:    []int{},
		SkippedIDs: []int{},
	}
	channels := op.ChannelStatsList()
	for _, ch := range channels {
		if !ch.Enabled {
			continue
		}
		snapshot, err := op.ChannelSyncReadSnapshot(ch.ChannelID)
		if err != nil {
			return result, fmt.Errorf("StartBatch: failed to read channel %d: %w", ch.ChannelID, err)
		}
		if !snapshot.Config.Enabled || !snapshot.Config.AutoSyncModels || len(snapshot.EnabledKeys) == 0 {
			result.SkippedIDs = append(result.SkippedIDs, ch.ChannelID)
			continue
		}
		claimResult := tryClaim(ch.ChannelID, true)
		switch claimResult {
		case claimAccepted:
			// running 状态已在 tryClaim 内设置, 此处不再重复写。
			result.StartedIDs = append(result.StartedIDs, ch.ChannelID)
		case claimBusy:
			result.BusyIDs = append(result.BusyIDs, ch.ChannelID)
		case claimStopped:
			// 协调器已停止: 返回 ErrStopped 而非静默跳过, handler 据此映射 503。
			return result, ErrStopped
		}
	}
	return result, nil
}

// GetStatus 返回全部渠道的最近同步状态, 不存在的渠道不出现。
// LastSyncAt 为 *string: 未同步过(running)为 nil, JSON 输出 null 而非 ""。
func GetStatus() []model.ChannelModelSyncStatus {
	statusesMu.Lock()
	defer statusesMu.Unlock()
	result := make([]model.ChannelModelSyncStatus, 0, len(statuses))
	for _, entry := range statuses {
		entry.mu.Lock()
		s := entry.status
		entry.mu.Unlock()
		if _, err := op.ChannelGet(s.ChannelID); err != nil {
			continue
		}
		result = append(result, s)
	}
	return result
}

// commitAndRefresh 是提交后缓存刷新的可测试 seam, 默认为 op.ChannelSyncCommitAndRefresh。
// 测试覆盖为返回传入 mutation + PostCommitError 以验证缓存未刷新时的编排路径。
var commitAndRefresh = op.ChannelSyncCommitAndRefresh

// ApplyChannelMutation 是渠道写操作 mutation 的统一编排。
func ApplyChannelMutation(mutation *op.ChannelMutation) {
	if mutation == nil {
		return
	}
	op.ApplyGroupMemberDeltas(mutation.GroupDeltas)
	for _, delta := range mutation.GroupDeltas {
		if delta.Removed {
			relay.PruneRouteMembers(delta.GroupID, delta.ItemIDs)
		}
	}
	publishMutationEvent(mutation)
}

func publishMutationEvent(mutation *op.ChannelMutation) {
	if mutation == nil {
		return
	}
	for _, delta := range mutation.GroupDeltas {
		group, err := op.GroupGet(delta.GroupID)
		if err != nil {
			log.Warnf("publishMutationEvent: skip group %d, group read failed after committed mutation: %v", delta.GroupID, err)
			continue
		}
		groupevents.Publish(groupevents.Event{
			Name: "changed",
			Data: groupevents.ChangedData{Group: group, Runtime: relay.RouteStateOf(group)},
		})
	}
}

type claimOutcome int

const (
	claimAccepted claimOutcome = iota
	claimBusy
	claimStopped
)

// tryClaim 在 lifecycle 锁内原子检查 stopped + claim + Add(1) + 设置 running 状态 + 启动 worker。
// running 状态在启动 worker 前设置, 防止快 worker 结束后覆盖 running 为终态。
func tryClaim(channelID int, isAuto bool) claimOutcome {
	lifecycle.Lock()
	defer lifecycle.Unlock()
	if stopped.Load() {
		return claimStopped
	}
	if _, exists := running[channelID]; exists {
		return claimBusy
	}
	running[channelID] = struct{}{}
	rootWG.Add(1)
	// Bug 1 修复: running 状态在 claim 临界区内、go runWorker 之前设置,
	// 防止 worker 先于 StartSingle 的 setStatusRunning 完成并覆盖为终态。
	setStatusRunning(channelID)
	go runWorker(channelID, isAuto)
	return claimAccepted
}

// runWorker 是单个渠道同步的 worker 生命周期: 等 slot → 运行 → 释放。
// isAuto 标记本次同步是自动触发(定时任务), 用于在写锁内重新验证 AutoSyncModels。
func runWorker(channelID int, isAuto bool) {
	defer rootWG.Done()
	defer func() {
		lifecycle.Lock()
		delete(running, channelID)
		lifecycle.Unlock()
	}()

	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-rootCtx.Done():
		setStatusDone(channelID, "skipped", op.SyncChanges{}, "coordinator stopped before start")
		return
	}

	ctx, cancel := context.WithTimeout(rootCtx, perChannelBudget)
	defer cancel()

	// Bug 2 修复: 取得 slot 后重新读取最新 DB, 验证渠道仍 eligible(enabled+keys)。
	// 自动触发时还需验证 AutoSyncModels 仍为 true。
	current, err := op.ChannelSyncReadSnapshot(channelID)
	if err != nil {
		setStatusDone(channelID, "failed", op.SyncChanges{}, "channel not found after slot")
		return
	}
	if !current.Config.Enabled || len(current.EnabledKeys) == 0 {
		setStatusDone(channelID, "skipped", op.SyncChanges{}, "channel disabled or no enabled keys")
		return
	}
	if isAuto && !current.Config.AutoSyncModels {
		setStatusDone(channelID, "skipped", op.SyncChanges{}, "auto sync disabled")
		return
	}

	runChannelSync(ctx, current, isAuto)
}

// runChannelSync 执行单个渠道的完整同步流程。
// isAuto 标记是否自动触发, 用于写锁内重新验证 AutoSyncModels。
func runChannelSync(ctx context.Context, snapshot op.ChannelSyncSnapshot, isAuto bool) {
	channelID := snapshot.ChannelID
	discoveries := discoverKeysSequential(ctx, snapshot)

	relay.GroupGateLock()
	defer relay.GroupGateUnlock()

	// Bug 3 修复: 写锁内完全以最新 DB snapshot 比对, 不混 cache(op.ChannelGet)。
	latest, err := op.ChannelSyncReadSnapshot(channelID)
	if err != nil {
		setStatusDone(channelID, "failed", op.SyncChanges{}, "failed to re-read channel config")
		return
	}
	// 网络前已验证 eligible, 这里验证配置未变(BaseURL/paths/regex/header/proxy/keys)。
	if !op.ChannelSyncConfigUnchanged(snapshot, latest, latestKeys(latest), isAuto) {
		setStatusDone(channelID, "skipped", op.SyncChanges{}, "channel config changed during sync")
		return
	}

	mutation, changes, applyErr := applyDiscovery(ctx, channelID, discoveries)
	if applyErr != nil {
		// Bug 6 修复: 日志用固定安全分类含 channelID, 不截断原始错误(可能含 model 名/SQL)。
		log.Warnf("channelsync: channel %d apply failed: %s", channelID, classifyError(applyErr))
		setStatusDone(channelID, "failed", op.SyncChanges{}, classifyError(applyErr))
		return
	}

	// commitAndRefresh 是提交后缓存刷新的窄 seam: 测试覆盖为返回 mutation + PostCommitError
	// 以验证 DB 已提交但缓存未刷新时的 cache/route/SSE 编排。生产默认为真实实现。
	mutation, commitErr := commitAndRefresh(ctx, channelID, mutation)
	if mutation != nil {
		ApplyChannelMutation(mutation)
	}
	if commitErr != nil {
		log.Warnf("channelsync: channel %d post-commit refresh failed: %s", channelID, classifyError(commitErr))
		setStatusDone(channelID, "failed", changes, "post-commit refresh failed")
		return
	}

	finalizeStatus(channelID, discoveries, changes)
}

// latestKeys 将 snapshot 的 EnabledKeys 转为 ChannelKey 列表供 config 比对。
func latestKeys(snap op.ChannelSyncSnapshot) []model.ChannelKey {
	keys := make([]model.ChannelKey, len(snap.EnabledKeys))
	for i, k := range snap.EnabledKeys {
		keys[i] = model.ChannelKey{ID: k.ID, ChannelKeyConfig: model.ChannelKeyConfig{Key: k.Key, Enabled: true}}
	}
	return keys
}

func discoverKeysSequential(ctx context.Context, snapshot op.ChannelSyncSnapshot) []op.KeyDiscovery {
	results := make([]op.KeyDiscovery, len(snapshot.EnabledKeys))
	for i, key := range snapshot.EnabledKeys {
		results[i] = discoverOneKey(ctx, snapshot, key)
	}
	return results
}

func discoverOneKey(ctx context.Context, snapshot op.ChannelSyncSnapshot, key op.ChannelSyncKey) op.KeyDiscovery {
	keyCtx, cancel := context.WithTimeout(ctx, perKeyTimeout)
	defer cancel()

	httpClient, err := buildHTTPClient(snapshot.Config)
	if err != nil {
		return op.KeyDiscovery{KeyID: key.ID, Err: fmt.Errorf("proxy init failed")}
	}
	defer httpClient.CloseIdleConnections()

	// 全局过滤只作用于手动"获取模型"路径(见 channel.go 的 fetch handler), 自动同步不参与:
	// 同步会删除"上游不再返回"的 sync_managed 授权与孤儿模型, 若在此应用全局正则,
	// 用户调整正则就会把被过滤的模型当作"上游不再返回"而误删。
	result, err := modeldiscovery.Discover(keyCtx, httpClient, snapshot.Config, key.Key, snapshot.Config.MatchRegex, "")
	if err != nil {
		return op.KeyDiscovery{KeyID: key.ID, Err: err}
	}
	return op.KeyDiscovery{KeyID: key.ID, Models: result.Models, Partial: result.Partial}
}

func applyDiscovery(ctx context.Context, channelID int, discoveries []op.KeyDiscovery) (*op.ChannelMutation, op.SyncChanges, error) {
	var mutation *op.ChannelMutation
	var changes op.SyncChanges
	err := db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		m, a, e := op.ChannelSyncApplyDiscovery(tx, channelID, discoveries)
		mutation = m
		changes = a
		return e
	})
	// Bug 5: 事务回滚时 mutation 必须为 nil, counts 必须为零, 不发布未提交事实。
	if err != nil {
		return nil, op.SyncChanges{}, err
	}
	return mutation, changes, nil
}

// finalizeStatus 根据探测结果设置最终状态。
// 成功指 d.Err==nil: 包括完全成功(两侧都成功)与部分成功(Partial=true, 单侧失败但另一侧有可用模型)。
// 失败指 d.Err!=nil(两侧协议都失败, 无可用模型)。
// 无成功 key → failed; 成功+任意失败 → partial; 全成功 → success。
func finalizeStatus(channelID int, discoveries []op.KeyDiscovery, changes op.SyncChanges) {
	var hasSuccess, hasFailure bool
	for _, d := range discoveries {
		switch {
		case d.Err != nil:
			hasFailure = true
		default:
			// d.Err==nil 即成功: 完全成功与 Partial(单侧失败但有可用模型)都算成功。
			hasSuccess = true
			if d.Partial {
				// Partial 表示单侧协议失败, 计入失败维度以使"成功+失败→partial"成立。
				hasFailure = true
			}
		}
	}
	switch {
	case !hasSuccess:
		setStatusDone(channelID, "failed", changes, "all keys failed")
	case hasFailure:
		setStatusDone(channelID, "partial", changes, "some keys failed")
	default:
		setStatusDone(channelID, "success", changes, "")
	}
}
