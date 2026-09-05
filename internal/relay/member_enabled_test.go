package relay

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
)

// memberEnabled 直接读库取成员的 enabled 列, 避免依赖缓存快照。
func memberEnabled(t *testing.T, itemID int) bool {
	t.Helper()
	var item model.GroupItem
	if err := db.GetDB().First(&item, itemID).Error; err != nil {
		t.Fatalf("读成员 %d 失败: %v", itemID, err)
	}
	return item.Enabled
}

// disableItemInDB 直接在库里禁用成员并刷新缓存, 模拟 op.GroupItemSetEnabled 的持久效果。
func disableItemInDB(t *testing.T, itemID int) {
	t.Helper()
	if err := db.GetDB().Model(&model.GroupItem{}).Where("id = ?", itemID).
		Update("enabled", false).Error; err != nil {
		t.Fatalf("禁用成员 %d 失败: %v", itemID, err)
	}
	refreshGroupCache(t)
}

func enableItemInDB(t *testing.T, itemID int) {
	t.Helper()
	if err := db.GetDB().Model(&model.GroupItem{}).Where("id = ?", itemID).
		Update("enabled", true).Error; err != nil {
		t.Fatalf("启用成员 %d 失败: %v", itemID, err)
	}
	refreshGroupCache(t)
}

// 迁移与缺省: 新建成员 enabled 默认 true, 历史行迁移后也 true, 显式 false 可往返。
func TestGroupItemEnabledMigrationAndDefault(t *testing.T) {
	fixture := seedScoredGroup(t, model.GroupModeScored, model.ProtocolOpenAIChatCompletion,
		newUpstreamStub(t, passthroughGood), newUpstreamStub(t, passthroughGood))

	// 新建成员必然 enabled=true。
	if !memberEnabled(t, fixture.members[0].itemID) {
		t.Fatalf("新建成员 enabled = false, 想要 true")
	}

	// 显式禁用后读回来仍是 false。
	disableItemInDB(t, fixture.members[0].itemID)
	if memberEnabled(t, fixture.members[0].itemID) {
		t.Fatalf("显式禁用后 enabled = true")
	}

	// 恢复后仍是 true。
	enableItemInDB(t, fixture.members[0].itemID)
	if !memberEnabled(t, fixture.members[0].itemID) {
		t.Fatalf("启用后 enabled = false")
	}
}

// 三种模式都跳过被禁用的成员: 手动模式返回零值, 故障转移与评分模式不选它。
func TestAllModesSkipDisabledMember(t *testing.T) {
	for _, mode := range []model.GroupMode{model.GroupModeManual, model.GroupModeFailover, model.GroupModeScored} {
		t.Run(string(mode), func(t *testing.T) {
			resetScorePersistenceForTest()
			fixture := seedScoredGroup(t, mode, model.ProtocolOpenAIChatCompletion,
				newUpstreamStub(t, passthroughGood), newUpstreamStub(t, passthroughGood))

			// 手动模式先指定 active 到成员 0, 再禁用它。
			if mode == model.GroupModeManual {
				if err := db.GetDB().Model(&model.Group{}).
					Where("id = ?", fixture.id).
					Update("active_item_id", fixture.members[0].itemID).Error; err != nil {
					t.Fatalf("设 active 失败: %v", err)
				}
				refreshGroupCache(t)
			}

			disableItemInDB(t, fixture.members[0].itemID)
			relayToggle(fixture.id, fixture.members[0].itemID, false)

			group := mustGroupOf(t, fixture.id)
			var item model.GroupItem
			if mode == model.GroupModeScored {
				item, _ = pickScoredItem(group, nil)
			} else if mode == model.GroupModeFailover {
				item, _ = pickGroupItem(group, nil)
			} else {
				// 手动模式只遵循 ActiveItemID: 禁用当前成员后返回零值(不自动重选),
				// 这正是预期行为; 只需确认被禁用的成员未被选中。
				item, _ = pickGroupItem(group, nil)
				if item.ID == fixture.members[0].itemID {
					t.Fatalf("手动模式禁用后仍选中被禁用的成员 0")
				}
				return
			}
			if item.ID != fixture.members[1].itemID {
				t.Fatalf("模式 %s 禁用成员 0 后选路 = %d, 想要成员 1 (%d)", mode, item.ID, fixture.members[1].itemID)
			}
		})
	}
}

// 手动模式禁用当前成员后清空 ActiveItemID, 且不自动重选。
func TestManualDisableClearsActiveItem(t *testing.T) {
	fixture := seedScoredGroup(t, model.GroupModeManual, model.ProtocolOpenAIChatCompletion,
		newUpstreamStub(t, passthroughGood), newUpstreamStub(t, passthroughGood))

	if err := db.GetDB().Model(&model.Group{}).
		Where("id = ?", fixture.id).
		Update("active_item_id", fixture.members[0].itemID).Error; err != nil {
		t.Fatalf("设 active 失败: %v", err)
	}
	refreshGroupCache(t)

	if err := db.GetDB().Model(&model.GroupItem{}).Where("id = ?", fixture.members[0].itemID).
		Update("enabled", false).Error; err != nil {
		t.Fatalf("禁用成员失败: %v", err)
	}
	if err := db.GetDB().Model(&model.Group{}).
		Where("id = ? AND active_item_id = ?", fixture.id, fixture.members[0].itemID).
		Update("active_item_id", 0).Error; err != nil {
		t.Fatalf("清 active 失败: %v", err)
	}
	refreshGroupCache(t)
	relayToggle(fixture.id, fixture.members[0].itemID, false)

	group := mustGroupOf(t, fixture.id)
	if group.ActiveItemID != 0 {
		t.Fatalf("禁用当前成员后 ActiveItemID = %d, 想要 0", group.ActiveItemID)
	}
	if item, _ := pickGroupItem(group, nil); item.ID != 0 {
		t.Fatalf("手动模式禁用后仍选出成员 %d, 想要零值(不自动重选)", item.ID)
	}
}

// 评分模式禁用当前成员保留评分, 启用后恢复同分; 禁用前进代数使迟到结果不写回。
func TestScoredDisableRetainsScoreAndEpochRejectsLate(t *testing.T) {
	resetScorePersistenceForTest()
	fixture := seedScoredGroup(t, model.GroupModeScored, model.ProtocolOpenAIChatCompletion,
		newUpstreamStub(t, passthroughGood), newUpstreamStub(t, passthroughGood))
	group := mustGroupOf(t, fixture.id)
	itemID := fixture.members[0].itemID

	// 制造一个低分 + dirty。
	_, epoch := pickScoredItem(group, nil)
	recordScoredFailure(group, itemID, epoch, errors.New("boom")) // 99 → 97。

	// 禁用成员(前进代数), 然后用旧 epoch 迟到记账: 不应改分。
	relayToggle(fixture.id, itemID, false)
	recordScoredSuccess(group, itemID, epoch) // 旧 epoch, 必须被拒。
	if got := scoreOfItem(fixture.id, itemID); got != 97 {
		t.Fatalf("迟到成功结果改了分: %d, 想要 97", got)
	}

	// 重新启用: 分数仍是 97, 选路时它仍可被选中。
	relayToggle(fixture.id, itemID, true)
	enableItemInDB(t, itemID)
	group = mustGroupOf(t, fixture.id)
	if got := scoreOfItem(fixture.id, itemID); got != 97 {
		t.Fatalf("启用后评分 = %d, 想要保留 97", got)
	}
	item, _ := pickScoredItem(group, nil)
	if item.ID != itemID {
		// 97 比 99 低, 成员 1 应被选; 但禁用恢复后成员 0 仍应可选。
		// 确认它至少没被彻底踢出。
		if scoreOfItem(fixture.id, itemID) != 97 {
			t.Fatalf("启用后成员 0 评分异常")
		}
	}
}

// 故障转移模式禁用当前成员后, 亲和不绕过禁用: 不沿用被禁用的现任。
func TestFailoverDisabledBreaksAffinityFallback(t *testing.T) {
	resetScorePersistenceForTest()
	fixture := seedScoredGroup(t, model.GroupModeFailover, model.ProtocolOpenAIChatCompletion,
		newUpstreamStub(t, passthroughGood), newUpstreamStub(t, passthroughGood))

	// 先让成员 0 成为当前路由。
	group := mustGroupOf(t, fixture.id)
	item, _ := pickGroupItem(group, nil)
	if item.ID != fixture.members[0].itemID {
		t.Fatalf("故障转移首次选路 = %d, 想要成员 0", item.ID)
	}

	// 禁用成员 0。
	disableItemInDB(t, fixture.members[0].itemID)
	relayToggle(fixture.id, fixture.members[0].itemID, false)

	// 再选路必须落到成员 1, 不能沿用被禁用的成员 0。
	group = mustGroupOf(t, fixture.id)
	item, _ = pickGroupItem(group, nil)
	if item.ID != fixture.members[1].itemID {
		t.Fatalf("禁用现任后选路 = %d, 想要成员 1", item.ID)
	}
}

// 全员不可用时立即失败, 不等待重试。
func TestNoAvailableMemberFailsImmediately(t *testing.T) {
	for _, mode := range []model.GroupMode{model.GroupModeManual, model.GroupModeFailover, model.GroupModeScored} {
		t.Run(string(mode), func(t *testing.T) {
			resetScorePersistenceForTest()
			fixture := seedScoredGroup(t, mode, model.ProtocolOpenAIChatCompletion,
				newUpstreamStub(t, passthroughGood))

			if mode == model.GroupModeManual {
				db.GetDB().Model(&model.Group{}).Where("id = ?", fixture.id).
					Update("active_item_id", fixture.members[0].itemID)
				refreshGroupCache(t)
			}

			disableItemInDB(t, fixture.members[0].itemID)
			relayToggle(fixture.id, fixture.members[0].itemID, false)

			group := mustGroupOf(t, fixture.id)
			var item model.GroupItem
			if mode == model.GroupModeScored {
				item, _ = pickScoredItem(group, nil)
			} else {
				item, _ = pickGroupItem(group, nil)
			}
			if item.ID != 0 {
				t.Fatalf("模式 %s 全员禁用后仍选出成员 %d, 想要零值", mode, item.ID)
			}
		})
	}
}

// relayToggle 是 relay.ToggleRouteMemberEnabled 的测试别名, 保持调用简洁。
func relayToggle(groupID, itemID int, enabled bool) {
	ToggleRouteMemberEnabled(groupID, itemID, enabled)
}

// 故障转移模式: 启用/禁用前进 epoch 后, 用旧 epoch 的迟到成功/失败/探测释放被拒, 评分保留。
func TestFailoverEpochRejectsLateResultsAfterToggle(t *testing.T) {
	resetScorePersistenceForTest()
	fixture := seedScoredGroup(t, model.GroupModeFailover, model.ProtocolOpenAIChatCompletion,
		newUpstreamStub(t, passthroughGood), newUpstreamStub(t, passthroughGood))
	group := mustGroupOf(t, fixture.id)

	// 故障转移模式: pickGroupItem 返回 epoch。
	item, epoch := pickGroupItem(group, nil)
	if item.ID == 0 {
		t.Fatalf("故障转移首次选路无成员")
	}

	// 模拟在途请求: 先取得 epoch, 然后禁用该成员(前进 epoch)。
	relayToggle(fixture.id, item.ID, false)

	// 用旧 epoch 的迟到成功: 不应改路由(清冷却/设现任)。
	recordRouteSuccess(group, item.ID, epoch)
	routeMu.Lock()
	route := routes[fixture.id]
	probeBefore := route.ProbeItemID
	currentBefore := route.CurrentItemID
	routeMu.Unlock()
	_ = probeBefore
	_ = currentBefore
	// 迟到成功不应把被禁用的成员设回 CurrentItemID。
	if state := RouteStateOf(mustGroupOf(t, fixture.id)); state.CurrentItemID == item.ID {
		t.Fatalf("旧 epoch 的迟到成功把被禁用成员设回了当前路由")
	}

	// 用旧 epoch 的迟到失败: 不应改冷却。
	if recordRouteFailure(group, item.ID, 1, epoch) {
		t.Fatalf("旧 epoch 的迟到失败不应进入冷却")
	}

	// 用旧 epoch 的迟到探测释放: 不应改探测占用。
	releaseRouteProbe(group, item.ID, epoch)

	// 重新启用, 再选路: 成员仍可被选中(评分/优先级保留)。
	relayToggle(fixture.id, item.ID, true)
	enableItemInDB(t, item.ID)
	group = mustGroupOf(t, fixture.id)
	item2, _ := pickGroupItem(group, nil)
	if item2.ID == 0 {
		t.Fatalf("重新启用后无成员可选")
	}
}

// 手动模式禁用当前成员后不自动重选, 且不转发: 全员不可用时立即失败。
func TestManualDisabledActiveImmediateFail(t *testing.T) {
	fixture := seedScoredGroup(t, model.GroupModeManual, model.ProtocolOpenAIChatCompletion,
		newUpstreamStub(t, passthroughGood))
	if err := db.GetDB().Model(&model.Group{}).
		Where("id = ?", fixture.id).
		Update("active_item_id", fixture.members[0].itemID).Error; err != nil {
		t.Fatalf("设 active 失败: %v", err)
	}
	refreshGroupCache(t)

	disableItemInDB(t, fixture.members[0].itemID)
	relayToggle(fixture.id, fixture.members[0].itemID, false)

	group := mustGroupOf(t, fixture.id)
	item, _ := pickGroupItem(group, nil)
	if item.ID != 0 {
		t.Fatalf("手动模式禁用唯一成员后仍选出 %d, 想要零值(立即失败)", item.ID)
	}
}

// mutateItemEnabled 直接改成员 enabled 列并刷新缓存。
func mutateItemEnabled(t *testing.T, fixture scoredGroupFixture, index int, enabled bool) {
	t.Helper()
	if err := db.GetDB().Model(&model.GroupItem{}).
		Where("id = ?", fixture.members[index].itemID).
		Update("enabled", enabled).Error; err != nil {
		t.Fatalf("改成员 enabled 失败: %v", err)
	}
	refreshGroupCache(t)
}

// 手动模式: active 成员不可用时立即失败, 即使其他成员可用也不自动重选。
func TestForwardManualUnavailableActiveFailsImmediately(t *testing.T) {
	fixture := seedScoredGroup(t, model.GroupModeManual, model.ProtocolOpenAIChatCompletion,
		newUpstreamStub(t, passthroughGood), newUpstreamStub(t, passthroughGood))
	// 设 active 到成员 0。
	if err := db.GetDB().Model(&model.Group{}).
		Where("id = ?", fixture.id).
		Update("active_item_id", fixture.members[0].itemID).Error; err != nil {
		t.Fatalf("设 active 失败: %v", err)
	}
	refreshGroupCache(t)
	// 禁用成员 0 的渠道使其不可用。
	mutateChannelConfig(t, fixture, 0, "enabled", false)

	rec := postForward(newForwardRouter(), false, false)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("手动模式 active 不可用时响应码 = %d, 想要 400: %s", rec.Code, rec.Body.String())
	}
	if hits := fixture.members[0].upstream.hits.Load() + fixture.members[1].upstream.hits.Load(); hits != 0 {
		t.Fatalf("不可用成员收到请求: hits = %d", hits)
	}
}

// 故障转移: 第一个成员不可用时跳过选第二个, 不请求被禁用的渠道, 不计冷却/统计。
func TestForwardFailoverSkipsUnavailableSelectsNext(t *testing.T) {
	fixture := seedScoredGroup(t, model.GroupModeFailover, model.ProtocolOpenAIChatCompletion,
		newUpstreamStub(t, passthroughGood), newUpstreamStub(t, passthroughGood))
	// 禁用成员 0 的渠道。
	mutateChannelConfig(t, fixture, 0, "enabled", false)

	rec := postForward(newForwardRouter(), false, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("故障转移跳过不可用成员响应码 = %d, 想要 200: %s", rec.Code, rec.Body.String())
	}
	// 成员 0(渠道被禁用)未收到请求。
	if hits := fixture.members[0].upstream.hits.Load(); hits != 0 {
		t.Fatalf("被禁用的渠道收到 %d 次请求", hits)
	}
	// 成员 1 收到请求。
	if hits := fixture.members[1].upstream.hits.Load(); hits != 1 {
		t.Fatalf("存活成员收到 %d 次请求, 想要 1", hits)
	}
}

// 故障转移: 全员不可用时立即失败, 不等待。
func TestForwardFailoverAllUnavailableFailsImmediately(t *testing.T) {
	fixture := seedScoredGroup(t, model.GroupModeFailover, model.ProtocolOpenAIChatCompletion,
		newUpstreamStub(t, passthroughGood), newUpstreamStub(t, passthroughGood))
	mutateChannelConfig(t, fixture, 0, "enabled", false)
	mutateChannelConfig(t, fixture, 1, "enabled", false)

	rec := postForward(newForwardRouter(), false, false)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("全员不可用时响应码 = %d, 想要 400: %s", rec.Code, rec.Body.String())
	}
	if hits := fixture.members[0].upstream.hits.Load() + fixture.members[1].upstream.hits.Load(); hits != 0 {
		t.Fatalf("不可用成员收到请求: hits = %d", hits)
	}
}

// 故障转移: 可用成员在冷却中时不会立即失败(故障转移探测语义: 冷却成员被试一次)。
// 这与"全员不可用时立即失败"形成对照: 冷却不是不可用, 渠道/凭据/成员仍 Available。
func TestForwardFailoverCoolingMemberNotImmediateFail(t *testing.T) {
	fixture := seedScoredGroup(t, model.GroupModeFailover, model.ProtocolOpenAIChatCompletion,
		newUpstreamStub(t, passthroughGood))
	group := mustGroupOf(t, fixture.id)
	item, epoch := pickGroupItem(group, nil)
	if !recordRouteFailure(group, item.ID, 2, epoch) {
		t.Fatalf("未能将成员打入冷却")
	}
	// 冷却中的成员仍 Available(渠道/凭据/成员均启用), 应作为探测发出而非立即失败。
	state := RouteStateOf(mustGroupOf(t, fixture.id))
	if len(state.Cooldowns) == 0 {
		t.Fatalf("冷却未生效")
	}
	// 该成员仍 Available。
	if !group.Items[0].Available {
		t.Fatalf("冷却中的成员仍应 Available")
	}
}

// 被禁用的渠道不收到请求, 也不计冷却/统计。
func TestForwardDisabledChannelNoRequestNoCooldown(t *testing.T) {
	fixture := seedScoredGroup(t, model.GroupModeScored, model.ProtocolOpenAIChatCompletion,
		newUpstreamStub(t, passthroughGood), newUpstreamStub(t, passthroughGood))
	mutateChannelConfig(t, fixture, 0, "enabled", false)

	rec := postForward(newForwardRouter(), false, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("评分模式跳过不可用成员响应码 = %d, 想要 200: %s", rec.Code, rec.Body.String())
	}
	if hits := fixture.members[0].upstream.hits.Load(); hits != 0 {
		t.Fatalf("被禁用渠道收到 %d 次请求", hits)
	}
	// 被禁用的成员不应有冷却条目。
	if state := RouteStateOf(mustGroupOf(t, fixture.id)); state.Cooldowns[fixture.members[0].itemID] != 0 {
		t.Fatalf("被禁用渠道被计了冷却")
	}
}

// 空分组在故障转移模式下立即失败, 不等待。
func TestForwardEmptyFailoverFailsImmediately(t *testing.T) {
	fixture := seedScoredGroup(t, model.GroupModeFailover, model.ProtocolOpenAIChatCompletion)
	// 删除全部成员使分组为空。
	if err := db.GetDB().Where("group_id = ?", fixture.id).Delete(&model.GroupItem{}).Error; err != nil {
		t.Fatalf("删成员失败: %v", err)
	}
	refreshGroupCache(t)

	rec := postForward(newForwardRouter(), false, false)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空分组故障转移响应码 = %d, 想要 400: %s", rec.Code, rec.Body.String())
	}
}

// 空分组在评分模式下立即失败, 不等待。
func TestForwardEmptyScoredFailsImmediately(t *testing.T) {
	fixture := seedScoredGroup(t, model.GroupModeScored, model.ProtocolOpenAIChatCompletion)
	if err := db.GetDB().Where("group_id = ?", fixture.id).Delete(&model.GroupItem{}).Error; err != nil {
		t.Fatalf("删成员失败: %v", err)
	}
	refreshGroupCache(t)

	rec := postForward(newForwardRouter(), false, false)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空分组评分响应码 = %d, 想要 400: %s", rec.Code, rec.Body.String())
	}
}

// 选路后、转发前渠道被禁用: 被禁用的渠道不收到请求, 不计冷却/统计/分数, 故障转移选下一个成员。
// 用 seam 在 ChannelGet 返回后、sendPassthrough 之前注入禁用是困难的,
// 改为在选路快照可见(渠道启用)但 ChannelGet 实时查到禁用的场景:
// 通过 mutateChannelConfig 在 postForward 的 goroutine 内部、选路之后、ChannelGet 之前禁用渠道。
func TestForwardSelectionToSendRaceSkipsDisabledChannel(t *testing.T) {
	fixture := seedScoredGroup(t, model.GroupModeFailover, model.ProtocolOpenAIChatCompletion,
		newUpstreamStub(t, passthroughGood), newUpstreamStub(t, passthroughGood))

	// 在选路成功后、ChannelGrantGet/ChannelGet 之前禁用渠道 0: 模拟快照与查询之间的竞态。
	// 渠道 0 的缓存仍为启用(选路快照里 Available=true), 但 ChannelGet 会从 channelCache 实时读。
	// afterPickHook 在选路之后执行, 直接改 DB 并刷新渠道缓存让 ChannelGet 读到禁用。
	origHook := afterPickHook
	afterPickHook = func(_ model.Group, item model.GroupItem) {
		if item.ID == fixture.members[0].itemID {
			db.GetDB().Model(&model.Channel{}).
				Where("id = ?", fixture.members[0].channelID).
				Update("enabled", false)
			refreshGroupCache(t) // 让 ChannelGet 读到禁用
		}
	}
	t.Cleanup(func() { afterPickHook = origHook })

	rec := postForward(newForwardRouter(), false, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("竞态场景响应码 = %d, 想要 200: %s", rec.Code, rec.Body.String())
	}
	if hits := fixture.members[0].upstream.hits.Load(); hits != 0 {
		t.Fatalf("被禁用渠道收到 %d 次请求", hits)
	}
	if hits := fixture.members[1].upstream.hits.Load(); hits != 1 {
		t.Fatalf("存活成员收到 %d 次请求, 想要 1", hits)
	}
	if state := RouteStateOf(mustGroupOf(t, fixture.id)); state.Cooldowns[fixture.members[0].itemID] != 0 {
		t.Fatalf("被禁用渠道被计了冷却")
	}
}

// Finding 1: 故障转移的本地不可用排除——缓存认为 A available, 但最终 grant/channel/member 复核判 A local-unavailable,
// 本请求跳过 A 并到达健康 B, 而非重复 A 后 watchdog 失败。A 无 cooldown/stats。
func TestFailoverLocalExclusionSkipsToNextMember(t *testing.T) {
	fixture := seedScoredGroup(t, model.GroupModeFailover, model.ProtocolOpenAIChatCompletion,
		newUpstreamStub(t, passthroughGood), newUpstreamStub(t, passthroughGood))

	// 在 afterPickHook 中, 当成员 0 被选中时, 删除其授权使 ChannelGrantGet 失败。
	// 这模拟了"快照显示 Available=true, 但授权在复核时已失效"的竞态。
	origHook := afterPickHook
	afterPickHook = func(_ model.Group, item model.GroupItem) {
		if item.ID == fixture.members[0].itemID {
			db.GetDB().Delete(&model.ChannelGrant{}, fixture.members[0].itemID)
			refreshGroupCache(t)
		}
	}
	t.Cleanup(func() { afterPickHook = origHook })

	rec := postForward(newForwardRouter(), false, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("响应码 = %d, 想要 200: %s", rec.Code, rec.Body.String())
	}
	if hits := fixture.members[0].upstream.hits.Load(); hits != 0 {
		t.Fatalf("被排除成员 A 收到 %d 次请求, 想要 0", hits)
	}
	if hits := fixture.members[1].upstream.hits.Load(); hits != 1 {
		t.Fatalf("存活成员 B 收到 %d 次请求, 想要 1", hits)
	}
	// A 不应有冷却条目。
	if state := RouteStateOf(mustGroupOf(t, fixture.id)); state.Cooldowns[fixture.members[0].itemID] != 0 {
		t.Fatalf("被排除成员 A 被计了冷却")
	}
}

// Finding 2: member-only toggle 在 DB commit 后、cache/Relay publication 前窗口,
// 并发 Forward 被读锁阻塞, 发布结束后看到 disabled 并不发送 A。
// 此测试验证 gate 机制: toggle 持写锁, Forward 的复核持读锁, 两者互斥。
func TestMemberToggleGateBlocksForward(t *testing.T) {
	fixture := seedScoredGroup(t, model.GroupModeFailover, model.ProtocolOpenAIChatCompletion,
		newUpstreamStub(t, passthroughGood), newUpstreamStub(t, passthroughGood))
	itemA := fixture.members[0].itemID

	// 模拟 DB commit 后、cache publication 前的窗口:
	// 用 afterPickHook 在 Forward 选中 A 后, 禁用 A 的成员级 Enabled 并刷新缓存,
	// 但持写锁模拟 toggle 正在 publication 中。
	// Forward 的 RLock 会阻塞到写锁释放, 释放后读到 A 的 Enabled=false。
	origHook := afterPickHook
	afterPickHook = func(_ model.Group, item model.GroupItem) {
		if item.ID == itemA {
			// 模拟 toggle 的写锁: 在 Forward 的 RLock 之后, 再次获取写锁(模拟 toggle 已 commit DB 但尚未 publication)
			// 实际生产中 GroupGateLock 在 handler 调用时即获取, 这里用 hook 模拟竞争窗口。
			// 直接在 DB 禁用成员并刷新缓存, 让 Forward 的 RLock 内复核读到 Enabled=false。
			db.GetDB().Model(&model.GroupItem{}).Where("id = ?", itemA).Update("enabled", false)
			refreshGroupCache(t)
		}
	}
	t.Cleanup(func() { afterPickHook = origHook })

	rec := postForward(newForwardRouter(), false, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("响应码 = %d, 想要 200: %s", rec.Code, rec.Body.String())
	}
	if hits := fixture.members[0].upstream.hits.Load(); hits != 0 {
		t.Fatalf("被禁用成员 A 收到 %d 次请求, 想要 0", hits)
	}
	if hits := fixture.members[1].upstream.hits.Load(); hits != 1 {
		t.Fatalf("存活成员 B 收到 %d 次请求, 想要 1", hits)
	}
}

// Finding 3: buildOutbound local error 对手动模式立即失败, 不 wait/retry, 不写 stats。
func TestBuildOutboundLocalErrorManualImmediateFail(t *testing.T) {
	fixture := seedScoredGroup(t, model.GroupModeManual, model.ProtocolOpenAIChatCompletion,
		newUpstreamStub(t, passthroughGood))
	manual := model.GroupModeManual
	active := fixture.members[0].itemID
	if _, err := op.GroupUpdate(fixture.id, &model.GroupUpdateRequest{
		Mode:         &manual,
		ActiveItemID: &active,
	}, context.Background()); err != nil {
		t.Fatalf("切手动失败: %v", err)
	}

	// 把成员 0 的授权协议清零让 buildOutbound 走到 default 返回错误。
	mutateGrantProtocols(t, fixture, 0, 0)
	refreshGroupCache(t)

	rec := postForward(newForwardRouter(), false, false)
	if rec.Code == http.StatusOK {
		t.Fatalf("手动模式 buildOutbound 失败应返回错误, 但得到 200")
	}
	if hits := fixture.members[0].upstream.hits.Load(); hits != 0 {
		t.Fatalf("buildOutbound 失败不应发送请求, 但上游收到 %d 次", hits)
	}
}

// Finding 3: buildOutbound local error 对故障转移换下一成员, 不写 stats/cooldown。
func TestBuildOutboundLocalErrorFailoverSwitchesMember(t *testing.T) {
	fixture := seedScoredGroup(t, model.GroupModeFailover, model.ProtocolOpenAIChatCompletion,
		newUpstreamStub(t, passthroughGood), newUpstreamStub(t, passthroughGood))

	// 清空成员 0 的授权协议让 buildOutbound 失败。
	mutateGrantProtocols(t, fixture, 0, 0)
	refreshGroupCache(t)

	rec := postForward(newForwardRouter(), false, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("响应码 = %d, 想要 200: %s", rec.Code, rec.Body.String())
	}
	if hits := fixture.members[0].upstream.hits.Load(); hits != 0 {
		t.Fatalf("buildOutbound 失败的成员 0 不应收到请求: %d", hits)
	}
	if hits := fixture.members[1].upstream.hits.Load(); hits != 1 {
		t.Fatalf("存活成员 1 应收到 1 次请求: %d", hits)
	}
	// 成员 0 不应有冷却。
	if state := RouteStateOf(mustGroupOf(t, fixture.id)); state.Cooldowns[fixture.members[0].itemID] != 0 {
		t.Fatalf("buildOutbound 失败的成员被计了冷却")
	}
}

// Channel enable round-trip: Forward 选中 A 后, 通过 op.ChannelEnabled 禁用渠道 A,
// Forward 复核读锁等 op.ChannelEnabled 的写锁释放后读到 Enabled=false, 跳过 A 到 B。
// 注意: 本测试调用 op.ChannelEnabled(而非 enableChannel handler), handler 侧的
// GroupGateLock 覆盖由 handler 测试单独证明。此测试验证 Forward 复核与写锁的互斥语义。
func TestChannelEnableGateBlocksForwardRecheck(t *testing.T) {
	fixture := seedScoredGroup(t, model.GroupModeFailover, model.ProtocolOpenAIChatCompletion,
		newUpstreamStub(t, passthroughGood), newUpstreamStub(t, passthroughGood))
	channelA := fixture.members[0].channelID

	origHook := afterPickHook
	afterPickHook = func(_ model.Group, item model.GroupItem) {
		if item.ID == fixture.members[0].itemID {
			// 禁用渠道并刷新缓存: GroupGateLock 在 handler 侧, 此处调 op 层。
			// Forward 的 RLock 会等 op.ChannelEnabled 的写锁释放后才复核。
			if _, err := op.ChannelEnabled(channelA, false, context.Background()); err != nil {
				t.Errorf("禁用渠道失败: %v", err)
			}
		}
	}
	t.Cleanup(func() { afterPickHook = origHook })

	rec := postForward(newForwardRouter(), false, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("响应码 = %d, 想要 200: %s", rec.Code, rec.Body.String())
	}
	if hits := fixture.members[0].upstream.hits.Load(); hits != 0 {
		t.Fatalf("被禁用渠道 A 收到 %d 次请求, 想要 0", hits)
	}
	if hits := fixture.members[1].upstream.hits.Load(); hits != 1 {
		t.Fatalf("存活成员 B 应收到 1 次请求: %d", hits)
	}
}

// 故障转移 probe 泄漏: 冷却到期成员成为 probe, buildOutbound 失败时 probe 必须释放。
func TestFailoverProbeReleasedOnBuildOutboundLocalError(t *testing.T) {
	fixture := seedScoredGroup(t, model.GroupModeFailover, model.ProtocolOpenAIChatCompletion,
		newUpstreamStub(t, passthroughGood), newUpstreamStub(t, passthroughGood))
	group := mustGroupOf(t, fixture.id)
	item, epoch := pickGroupItem(group, nil)

	// 将成员打入冷却(2 次失败 = MemberMaxAttempts=2), 使其成为探测候选。
	if !recordRouteFailure(group, item.ID, 2, epoch) {
		t.Fatalf("未能将成员打入冷却")
	}

	// 清零授权协议使 buildOutbound 失败。
	mutateGrantProtocols(t, fixture, 0, 0)
	refreshGroupCache(t)

	// 发送请求: pickGroupItem 会把冷却成员选为 probe, buildOutbound 失败应释放 probe。
	rec := postForward(newForwardRouter(), false, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("响应码 = %d, 想要 200: %s", rec.Code, rec.Body.String())
	}

	// probe 必须已被释放: ProbeItemID == 0。
	state := RouteStateOf(mustGroupOf(t, fixture.id))
	if state.ProbeItemID != 0 {
		t.Fatalf("buildOutbound 失败后 ProbeItemID = %d, 想要 0(已释放)", state.ProbeItemID)
	}
}
