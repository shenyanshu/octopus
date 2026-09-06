package channelsync

// 本文件提供删除路径的真实编排测试, 全部经 StartSingle → runChannelSync → applyDiscovery 真实事务,
// 不直接调 ApplyDelta 或 mock 编排。
//
// Test A: 在 GORM After(gorm:update) 回调内(仅 channels revision UPDATE 后触发)以 tx.Session(NewDB:true)
//         读取未提交事务状态, 断言 A grant/item 已删 + active0 + B 已存在 + revision 已变化, 再 tx.AddError 注入,
//         保证失败确实发生在 DELETE 之后。verifiedAfterDelete 仅在全部前提检查通过后置 true;
//         回调内不调 t.Fatal(Goexit 会破坏 worker), 错误收集到 atomic string, 测试外统一 fatal。
// Test B: 覆盖 commitAndRefresh seam 返回 mutation + PostCommitError 以跳过缓存刷新, 断言 ApplyChannelMutation
//         仍将 cache/route/SSE 编排到一致状态, 收到恰一次 changed, status=failed 保留 removed 计数。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/groupevents"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay"
	"gorm.io/gorm"
)

// switchableModelsServer 返回一个可原子切换模型列表的上游 fixture。
// 切换 models 原子发布, handler 闭包每次读取最新值, 不动态改 http.Server.Handler。
// t.Cleanup 注册 Close, 保障资源释放。
func switchableModelsServer(t *testing.T, initial []string) (*httptest.Server, *atomic.Value) {
	t.Helper()
	var current atomic.Value
	current.Store(initial)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		models, _ := current.Load().([]string)
		type item struct{ ID string }
		type list struct {
			Data []item `json:"data"`
		}
		data := list{}
		for _, m := range models {
			data.Data = append(data.Data, item{ID: m})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(data)
	}))
	t.Cleanup(srv.Close)
	return srv, &current
}

// --- Test A: DELETE 后失败的真实回滚 ---

// TestDeleteThenFailRollback 在 GORM After(gorm:update) 回调(仅 channels revision UPDATE)内
// 断言 A grant/item 已删、active0、B 已存在、revision 已变化, 再 AddError 使事务回滚。
// 证明失败发生在 applySyncDeletions 之后, 而非 INSERT 阶段。
// verifiedAfterDelete 仅在全部前提检查通过后置 true; 回调内不调 t.Fatal(Goexit 破坏 worker),
// 错误收集到 atomic string, 测试外统一 fatal。
func TestDeleteThenFailRollback(t *testing.T) {
	clearTables(t)
	upstream, models := switchableModelsServer(t, []string{"model-a"})
	chID := seedChannel(t, "del-then-fail", upstream.URL, "sk", false)

	// 建规则分组匹配 model-a, 使首次同步补入 group item。
	grp, err := op.GroupCreate(&model.GroupCreateRequest{
		Name: "grp-dtf", Mode: model.GroupModeManual, AutoAddPattern: "^model-a$",
	}, context.Background())
	if err != nil {
		t.Fatalf("建分组失败: %v", err)
	}

	// 首次同步: 创建 model-a(sync_managed=true) + grant + group item。
	started, _, _, err := StartSingle(chID)
	if err != nil {
		t.Fatalf("StartSingle 失败: %v", err)
	}
	if !started {
		t.Fatal("应启动同步")
	}
	waitStatus(t, chID, "success")

	// 读取 model-a 的 grant ID 和 group item ID。
	var cm model.ChannelModel
	if err := db.GetDB().Where("channel_id = ? AND name = ?", chID, "model-a").First(&cm).Error; err != nil {
		t.Fatalf("读 model-a 失败: %v", err)
	}
	var cg model.ChannelGrant
	if err := db.GetDB().Where("channel_model_id = ?", cm.ID).First(&cg).Error; err != nil {
		t.Fatalf("读 grant 失败: %v", err)
	}
	grpLoaded, err := op.GroupGet(grp.ID)
	if err != nil {
		t.Fatalf("读分组失败: %v", err)
	}
	if len(grpLoaded.Items) == 0 {
		t.Fatal("规则分组应已补入 model-a 的成员")
	}
	activeItemID := grpLoaded.Items[0].ID
	if err := db.GetDB().Model(&model.Group{}).Where("id = ?", grp.ID).Update("active_item_id", activeItemID).Error; err != nil {
		t.Fatalf("设置 active_item_id 失败: %v", err)
	}

	// 读取旧 revision(必须合法非空)。
	var ch1 model.Channel
	if err := db.GetDB().Where("id = ?", chID).First(&ch1).Error; err != nil {
		t.Fatalf("读渠道失败: %v", err)
	}
	oldRevision := ch1.Revision
	if oldRevision == "" {
		t.Fatal("旧 revision 不应为空(前提不合法)")
	}

	// 注册 GORM After(gorm:update) 回调: 仅 channels revision UPDATE 后触发。
	var triggered atomic.Bool
	var verifiedAfterDelete atomic.Bool // 仅全部前提检查通过后置 true。
	var assertErr atomic.Value          // 回调内收集到的断言错误, 测试外统一 fatal; 回调内不调 t.Fatal。
	assertErr.Store("")
	cbName := "test_dtf_rollback_" + strconv.Itoa(chID)
	rollbackCB := func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.Table != "channels" || triggered.Load() {
			return
		}
		triggered.Store(true)

		// 用 NewDB:true 在事务连接上开净 session 读取未提交状态。
		readTx := tx.Session(&gorm.Session{NewDB: true})

		// 每个读检查后校验返回实例的 Error(链式调用返回新实例, 不读原始 readTx.Error)。
		var grantCount int64
		if err := readTx.Model(&model.ChannelGrant{}).Where("channel_model_id = ?", cm.ID).Count(&grantCount).Error; err != nil {
			assertErr.Store("read grant count: " + err.Error())
			tx.AddError(errors.New("assert read failed"))
			return
		}
		if grantCount != 0 {
			assertErr.Store("A grant should be deleted, got " + strconv.FormatInt(grantCount, 10))
			tx.AddError(errors.New("assert fail: A grant should be deleted"))
			return
		}

		// 断言 A model 已删。
		var modelCount int64
		if err := readTx.Model(&model.ChannelModel{}).Where("id = ?", cm.ID).Count(&modelCount).Error; err != nil {
			assertErr.Store("read model count: " + err.Error())
			tx.AddError(errors.New("assert read failed"))
			return
		}
		if modelCount != 0 {
			assertErr.Store("A model should be deleted, got " + strconv.FormatInt(modelCount, 10))
			tx.AddError(errors.New("assert fail: A model should be deleted"))
			return
		}

		// 断言 group active_item_id 已清零。
		var afterGrp model.Group
		if err := readTx.Where("id = ?", grp.ID).First(&afterGrp).Error; err != nil {
			assertErr.Store("read group: " + err.Error())
			tx.AddError(errors.New("assert read failed"))
			return
		}
		if afterGrp.ActiveItemID != 0 {
			assertErr.Store("active_item_id should be 0, got " + strconv.Itoa(afterGrp.ActiveItemID))
			tx.AddError(errors.New("assert fail: active_item_id should be 0"))
			return
		}

		// 断言 A group item 已删(FK 级联)。
		var itemCount int64
		if err := readTx.Model(&model.GroupItem{}).Where("group_id = ?", grp.ID).Count(&itemCount).Error; err != nil {
			assertErr.Store("read item count: " + err.Error())
			tx.AddError(errors.New("assert read failed"))
			return
		}
		if itemCount != 0 {
			assertErr.Store("A group item should be deleted, got " + strconv.FormatInt(itemCount, 10))
			tx.AddError(errors.New("assert fail: A group item should be deleted"))
			return
		}

		// 断言 B 已存在。
		var bCount int64
		if err := readTx.Model(&model.ChannelModel{}).Where("channel_id = ? AND name = ?", chID, "model-b").Count(&bCount).Error; err != nil {
			assertErr.Store("read B count: " + err.Error())
			tx.AddError(errors.New("assert read failed"))
			return
		}
		if bCount != 1 {
			assertErr.Store("model-b should exist, got " + strconv.FormatInt(bCount, 10))
			tx.AddError(errors.New("assert fail: model-b should exist"))
			return
		}

		// 断言 revision 已变化。
		var ch2 model.Channel
		if err := readTx.Where("id = ?", chID).First(&ch2).Error; err != nil {
			assertErr.Store("read revision: " + err.Error())
			tx.AddError(errors.New("assert read failed"))
			return
		}
		if ch2.Revision == oldRevision {
			assertErr.Store("revision should have changed")
			tx.AddError(errors.New("assert fail: revision should have changed"))
			return
		}

		// 全部前提检查通过, 置 verifiedAfterDelete=true, 再注入预期错误使事务回滚。
		verifiedAfterDelete.Store(true)
		tx.AddError(errors.New("injected after delete"))
	}
	if err := db.GetDB().Callback().Update().After("gorm:update").Register(cbName, rollbackCB); err != nil {
		t.Fatalf("注册回调失败: %v", err)
	}
	defer db.GetDB().Callback().Update().Remove(cbName)

	// 原子切换上游返回 model-b(不含 model-a), 不动态改 http.Server.Handler。
	models.Store([]string{"model-b"})

	// 订阅事件: 回滚不应发布 SSE。
	events := groupevents.Subscribe()
	defer groupevents.Unsubscribe(events)

	// 第二次同步: INSERT model-b → DELETE model-a → UPDATE revision → 回调注入错误 → 事务回滚。
	started2, _, _, err := StartSingle(chID)
	if err != nil {
		t.Fatalf("第二次 StartSingle 失败: %v", err)
	}
	if !started2 {
		t.Fatal("第二次应启动同步")
	}
	s := waitStatus(t, chID, "failed")

	// 核心验证标记: 仅全部前提检查通过后才 true; 这是证明失败确实在 DELETE 之后的关键。
	if !triggered.Load() {
		t.Fatal("GORM After(gorm:update) 回调未到达")
	}
	if !verifiedAfterDelete.Load() {
		// 回调到达但前提检查未通过: 报告具体失败原因。
		t.Fatalf("前提检查未通过(失败可能不在 DELETE 之后): %v", assertErr.Load())
	}
	if s.AddedModels != 0 || s.AddedGrants != 0 || s.RemovedModels != 0 || s.RemovedGrants != 0 {
		t.Errorf("回滚后 counts 应全为 0: added_m=%d added_g=%d removed_m=%d removed_g=%d",
			s.AddedModels, s.AddedGrants, s.RemovedModels, s.RemovedGrants)
	}

	// 断言 A model/grant/item/active/旧 revision 全恢复。
	if !findChannelModel(t, chID, "model-a") {
		t.Error("model-a 应恢复(事务回滚)")
	}
	var restoredGrant int64
	db.GetDB().Model(&model.ChannelGrant{}).Where("channel_model_id = ?", cm.ID).Count(&restoredGrant)
	if restoredGrant != 1 {
		t.Errorf("model-a 的 grant 应恢复(事务回滚), got %d", restoredGrant)
	}
	var restoredItem int64
	db.GetDB().Model(&model.GroupItem{}).Where("group_id = ?", grp.ID).Count(&restoredItem)
	if restoredItem != 1 {
		t.Errorf("group item 应恢复(事务回滚), got %d", restoredItem)
	}
	var restoredActive model.Group
	db.GetDB().Where("id = ?", grp.ID).First(&restoredActive)
	if restoredActive.ActiveItemID != activeItemID {
		t.Errorf("active_item_id 应恢复为 %d(事务回滚), got %d", activeItemID, restoredActive.ActiveItemID)
	}
	var ch3 model.Channel
	db.GetDB().Where("id = ?", chID).First(&ch3)
	if ch3.Revision != oldRevision {
		t.Errorf("revision 应恢复为 %s(事务回滚), got %s", oldRevision, ch3.Revision)
	}

	// 断言 B 不存在。
	if findChannelModel(t, chID, "model-b") {
		t.Error("model-b 不应存在(事务回滚)")
	}

	// 断言无 SSE 事件。
	time.Sleep(200 * time.Millisecond)
	select {
	case ev := <-events:
		t.Errorf("回滚不应发布事件, 但收到: %s", ev.Name)
	default:
	}
}

// --- Test B: PostCommit 失败的真实编排 ---

// TestPostCommitFailureOrchestration 覆盖 commitAndRefresh seam 返回 mutation + PostCommitError,
// 验证 ApplyChannelMutation 仍将 cache/route/SSE 编排到一致状态, 收到恰一次 changed, status=failed 保留 removed 计数。
// seam 内明确 mutation!=nil(错误从外部可见), 事件校验 groupID 避免错组。
func TestPostCommitFailureOrchestration(t *testing.T) {
	clearTables(t)
	upstream, models := switchableModelsServer(t, []string{"model-a"})
	chID := seedChannel(t, "pc-orch", upstream.URL, "sk", false)

	// 建规则分组匹配 model-a, 使首次同步补入 group item。
	grp, err := op.GroupCreate(&model.GroupCreateRequest{
		Name: "grp-pc", Mode: model.GroupModeManual, AutoAddPattern: "^model-a$",
	}, context.Background())
	if err != nil {
		t.Fatalf("建分组失败: %v", err)
	}

	// 首次同步: 创建 model-a(sync_managed=true) + grant + group item。
	started, _, _, err := StartSingle(chID)
	if err != nil {
		t.Fatalf("StartSingle 失败: %v", err)
	}
	if !started {
		t.Fatal("应启动同步")
	}
	waitStatus(t, chID, "success")

	// 读取 model-a 的 group item ID, 设置 active_item_id。
	grpLoaded, err := op.GroupGet(grp.ID)
	if err != nil {
		t.Fatalf("读分组失败: %v", err)
	}
	if len(grpLoaded.Items) == 0 {
		t.Fatal("规则分组应已补入 model-a 的成员")
	}
	aItemID := grpLoaded.Items[0].ID
	if err := db.GetDB().Model(&model.Group{}).Where("id = ?", grp.ID).Update("active_item_id", aItemID).Error; err != nil {
		t.Fatalf("设置 active_item_id 失败: %v", err)
	}
	// 刷新缓存使 active_item_id 对路由可见。
	if err := op.InitCache(); err != nil {
		t.Fatalf("刷新缓存失败: %v", err)
	}

	// 覆盖 commitAndRefresh seam: 返回传入 mutation + PostCommitError, 跳过缓存刷新。
	// mutation 非 nil(事务有删除+新增, mutation 携带 delta); 错误从外部可见。
	origCommit := commitAndRefresh
	commitAndRefresh = func(ctx context.Context, channelID int, mutation *op.ChannelMutation) (*op.ChannelMutation, error) {
		return mutation, &op.PostCommitError{
			RefreshErr: errors.New("injected refresh failure"),
			Mutation:   mutation,
		}
	}
	defer func() { commitAndRefresh = origCommit }()

	// 原子切换上游返回 model-b(不含 model-a)。
	models.Store([]string{"model-b"})

	// 订阅事件: 应收到恰一次 changed。
	events := groupevents.Subscribe()
	defer groupevents.Unsubscribe(events)

	// 第二次同步: 事务提交(删 A 加 B) → seam 返回 PostCommitError → ApplyChannelMutation 编排 cache/route/SSE。
	started2, _, _, err := StartSingle(chID)
	if err != nil {
		t.Fatalf("第二次 StartSingle 失败: %v", err)
	}
	if !started2 {
		t.Fatal("第二次应启动同步")
	}
	s := waitStatus(t, chID, "failed")
	if s.RemovedGrants != 1 || s.RemovedModels != 1 {
		t.Errorf("status=failed 应保留 removed 计数: removed_grants=%d removed_models=%d",
			s.RemovedGrants, s.RemovedModels)
	}
	if s.AddedModels != 1 || s.AddedGrants != 1 {
		t.Errorf("status=failed 应保留 added 计数: added_models=%d added_grants=%d",
			s.AddedModels, s.AddedGrants)
	}

	// DB 中 A 已删除。
	if findChannelModel(t, chID, "model-a") {
		t.Error("model-a 应已被删除(事务已提交)")
	}
	if !findChannelModel(t, chID, "model-b") {
		t.Error("model-b 应存在(事务已提交)")
	}

	// groupCache 中 A 消失且 active_item_id=0(delta 已应用)。
	grp2, err := op.GroupGet(grp.ID)
	if err != nil {
		t.Fatalf("读分组缓存失败: %v", err)
	}
	for _, item := range grp2.Items {
		if item.ID == aItemID {
			t.Error("groupCache 中 A 的 item 应已消失")
		}
	}
	if grp2.ActiveItemID != 0 {
		t.Errorf("groupCache active_item_id 应为 0, got %d", grp2.ActiveItemID)
	}

	// relay.RouteStateOf 不指 A(manual 模式 CurrentItemID = ActiveItemID = 0)。
	rs := relay.RouteStateOf(grp2)
	if rs.CurrentItemID != 0 {
		t.Errorf("RouteStateOf CurrentItemID 应为 0, got %d", rs.CurrentItemID)
	}

	// 收到恰一次 changed 事件, payload groupID 匹配且与 cache/route 一致。
	select {
	case ev, ok := <-events:
		if !ok || ev.Name != "changed" {
			t.Fatalf("应收到 changed 事件, got ok=%v name=%v", ok, ev)
		}
		cd, ok := ev.Data.(groupevents.ChangedData)
		if !ok {
			t.Fatalf("事件 Data 应为 ChangedData, got %T", ev.Data)
		}
		if cd.ID != grp.ID {
			t.Errorf("事件 payload groupID 应为 %d, got %d", grp.ID, cd.ID)
		}
		if cd.ActiveItemID != 0 {
			t.Errorf("事件 payload active_item_id 应为 0, got %d", cd.ActiveItemID)
		}
		if cd.Runtime.CurrentItemID != 0 {
			t.Errorf("事件 payload runtime.current_item_id 应为 0, got %d", cd.Runtime.CurrentItemID)
		}
	default:
		t.Fatal("应收到恰一次 changed 事件")
	}
	// 确认不再有第二个事件。
	time.Sleep(200 * time.Millisecond)
	select {
	case ev2 := <-events:
		t.Errorf("应只收到一次 changed, 但收到第二个: %s", ev2.Name)
	default:
	}
}

// --- JSON 合同测试: removed_* 零值输出 ---

// TestSyncStatusJSONContractRemovedZero 验证 status 无删除时 JSON 中 removed_* 恒为 0 而非 omitted。
func TestSyncStatusJSONContractRemovedZero(t *testing.T) {
	status := model.ChannelModelSyncStatus{
		Status:        "success",
		LastSyncAt:    nil,
		AddedModels:   2,
		AddedGrants:   2,
		RemovedModels: 0,
		RemovedGrants: 0,
	}
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("marshal 失败: %v", err)
	}
	rawStr := string(raw)
	if !strings.Contains(rawStr, `"removed_models":0`) {
		t.Errorf("JSON 应包含 removed_models:0, got %s", rawStr)
	}
	if !strings.Contains(rawStr, `"removed_grants":0`) {
		t.Errorf("JSON 应包含 removed_grants:0, got %s", rawStr)
	}
}
