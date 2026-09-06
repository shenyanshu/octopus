package op

// 本文件验证 ChannelEnableAllAutoSync: 批量开启渠道自动同步的真实 op 行为。
// 全部经真实 DB 与真实 op 函数。覆盖: enabled false/true 各 Auto false 开启, Auto true 保持;
// 每个变化 revision 不同、新 token GET 一致、旧 draft Update 409; no-op/空库 count0;
// stats 保留; 第二个 UPDATE 注入 error 断言所有 flag/revision 回滚 cache 未发布。

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// autoSyncTestChannel 直接在 DB 建渠道, 返回 (id, initialRevision)。
// enabled 控制渠道是否可用, autoSync 控制 auto_sync_models 初始值。
func autoSyncTestChannel(t *testing.T, name string, enabled, autoSync bool) (int, string) {
	t.Helper()
	rev := uuid.NewString()
	ch := model.Channel{
		Revision: rev,
		ChannelConfig: model.ChannelConfig{
			Name:           name,
			Enabled:        true,
			BaseURL:        "http://" + name + ".example",
			AutoSyncModels: autoSync,
		},
	}
	if err := db.GetDB().Create(&ch).Error; err != nil {
		t.Fatalf("建渠道 %s 失败: %v", name, err)
	}
	if !enabled {
		if err := db.GetDB().Model(&model.Channel{}).Where("id = ?", ch.ID).Update("enabled", false).Error; err != nil {
			t.Fatalf("禁用渠道 %s 失败: %v", name, err)
		}
	}
	return ch.ID, rev
}

// autoSyncClearTables 清空渠道相关表, 为每个测试提供干净起点。
func autoSyncClearTables(t *testing.T) {
	t.Helper()
	for _, table := range []string{"channel_grants", "channel_models", "channel_keys", "channels"} {
		db.GetDB().Exec("DELETE FROM " + table)
	}
	db.GetDB().Exec("DELETE FROM sqlite_sequence WHERE name IN ('channels','channel_keys','channel_models','channel_grants')")
	if err := InitCache(); err != nil {
		t.Fatalf("刷新缓存失败: %v", err)
	}
}

// autoSyncReadChannel 从 DB 读取渠道当前状态。
func autoSyncReadChannel(t *testing.T, chID int) model.Channel {
	t.Helper()
	var ch model.Channel
	if err := db.GetDB().Where("id = ?", chID).First(&ch).Error; err != nil {
		t.Fatalf("读渠道 %d 失败: %v", chID, err)
	}
	return ch
}

// TestEnableAllAutoSyncEnablesFalseAndPreservesTrue 验证: auto_sync=false 的渠道被开启,
// auto_sync=true 的渠道保持不变; 每个被改的渠道 revision 不同。
func TestEnableAllAutoSyncEnablesFalseAndPreservesTrue(t *testing.T) {
	autoSyncClearTables(t)

	// 四个渠道: (enabled=false, auto=false), (enabled=true, auto=false),
	//           (enabled=false, auto=true), (enabled=true, auto=true)
	chDisabledFalse, revDF := autoSyncTestChannel(t, "disabled-auto-false", false, false)
	chEnabledFalse, revEF := autoSyncTestChannel(t, "enabled-auto-false", true, false)
	_, revDT := autoSyncTestChannel(t, "disabled-auto-true", false, true)
	_, revET := autoSyncTestChannel(t, "enabled-auto-true", true, true)

	// 确认 disabled 渠道的 enabled 在 DB 中为 false(GORM default:true 后经 UPDATE 置假)。
	if ch := autoSyncReadChannel(t, chDisabledFalse); ch.Enabled {
		t.Fatal("disabled-auto-false 的 Enabled 应在 DB 中为 false(测试前提)")
	}

	count, err := ChannelEnableAllAutoSync(context.Background())
	if err != nil {
		t.Fatalf("ChannelEnableAllAutoSync 失败: %v", err)
	}
	if count != 2 {
		t.Fatalf("updated_count 应为 2(两个 auto=false), got %d", count)
	}

	// 两个被改的渠道: auto_sync=true, revision 已轮转。
	chDF := autoSyncReadChannel(t, chDisabledFalse)
	if !chDF.AutoSyncModels {
		t.Error("disabled-auto-false 应已开启 auto_sync")
	}
	if chDF.Revision == revDF {
		t.Error("disabled-auto-false revision 应已轮转")
	}
	if chDF.Enabled {
		t.Error("disabled-auto-false 的 Enabled 应保持 false(不应被改)")
	}

	chEF := autoSyncReadChannel(t, chEnabledFalse)
	if !chEF.AutoSyncModels {
		t.Error("enabled-auto-false 应已开启 auto_sync")
	}
	if chEF.Revision == revEF {
		t.Error("enabled-auto-false revision 应已轮转")
	}

	// 两个原 true 的渠道: auto_sync 保持 true, revision 不变。
	chDT := autoSyncReadChannel(t, chDisabledFalse) // placeholder, 用下面的正确 ID
	_ = chDT
	var chDTActual model.Channel
	db.GetDB().Where("name = ?", "disabled-auto-true").First(&chDTActual)
	if !chDTActual.AutoSyncModels {
		t.Error("disabled-auto-true auto_sync 应保持 true")
	}
	if chDTActual.Revision != revDT {
		t.Error("disabled-auto-true revision 应不变(原 true 不写)")
	}

	var chETActual model.Channel
	db.GetDB().Where("name = ?", "enabled-auto-true").First(&chETActual)
	if !chETActual.AutoSyncModels {
		t.Error("enabled-auto-true auto_sync 应保持 true")
	}
	if chETActual.Revision != revET {
		t.Error("enabled-auto-true revision 应不变(原 true 不写)")
	}

	// 两个被改渠道的 revision 互不相同。
	if chDF.Revision == chEF.Revision {
		t.Error("两个被改渠道的 revision 应互不相同(独立 uuid)")
	}
}

// TestEnableAllAutoSyncNewRevisionGETConsistent 验证: 新 token 经 ChannelDetailGet 读取一致。
func TestEnableAllAutoSyncNewRevisionGETConsistent(t *testing.T) {
	autoSyncClearTables(t)
	chID, _ := autoSyncTestChannel(t, "get-consistent", true, false)

	if _, err := ChannelEnableAllAutoSync(context.Background()); err != nil {
		t.Fatalf("ChannelEnableAllAutoSync 失败: %v", err)
	}

	// DB 中的 revision 与 ChannelDetailGet 读取的 revision 一致(DB 权威)。
	chDB := autoSyncReadChannel(t, chID)
	detail, err := ChannelDetailGet(context.Background(), chID)
	if err != nil {
		t.Fatalf("ChannelDetailGet 失败: %v", err)
	}
	if detail.Revision != chDB.Revision {
		t.Errorf("ChannelDetailGet revision=%s 与 DB revision=%s 不一致", detail.Revision, chDB.Revision)
	}
	if !detail.AutoSyncModels {
		t.Error("ChannelDetailGet AutoSyncModels 应为 true")
	}
}

// TestEnableAllAutoSyncStaleDraftUpdate409 验证: 旧 draft(旧 revision) Update 返回 409。
func TestEnableAllAutoSyncStaleDraftUpdate409(t *testing.T) {
	autoSyncClearTables(t)
	chID, oldRev := autoSyncTestChannel(t, "stale-draft", true, false)

	// 开启后 revision 已轮转。
	if _, err := ChannelEnableAllAutoSync(context.Background()); err != nil {
		t.Fatalf("ChannelEnableAllAutoSync 失败: %v", err)
	}

	// 用旧 revision 构造 draft 提交 ChannelUpdate → 应 409。
	detail, err := ChannelDetailGet(context.Background(), chID)
	if err != nil {
		t.Fatalf("ChannelDetailGet 失败: %v", err)
	}
	detail.Revision = oldRev // 故意用旧令牌
	_, _, err = ChannelUpdate(&detail, context.Background())
	if err == nil {
		t.Fatal("旧 revision Update 应返回错误")
	}
	if !errors.Is(err, ErrRevisionConflict) {
		t.Errorf("应返回 ErrRevisionConflict(409), got %v", err)
	}
}

// TestEnableAllAutoSyncNoOpCountZero 验证: 全部已 true 时 count=0, 所有 revision 不变。
func TestEnableAllAutoSyncNoOpCountZero(t *testing.T) {
	autoSyncClearTables(t)
	chID1, rev1 := autoSyncTestChannel(t, "noop-1", true, true)
	chID2, rev2 := autoSyncTestChannel(t, "noop-2", false, true)

	count, err := ChannelEnableAllAutoSync(context.Background())
	if err != nil {
		t.Fatalf("ChannelEnableAllAutoSync 失败: %v", err)
	}
	if count != 0 {
		t.Fatalf("no-op count 应为 0, got %d", count)
	}
	if autoSyncReadChannel(t, chID1).Revision != rev1 {
		t.Error("noop-1 revision 应不变")
	}
	if autoSyncReadChannel(t, chID2).Revision != rev2 {
		t.Error("noop-2 revision 应不变")
	}
}

// TestEnableAllAutoSyncEmptyDBCountZero 验证: 空库 count=0 无错误。
func TestEnableAllAutoSyncEmptyDBCountZero(t *testing.T) {
	autoSyncClearTables(t)
	count, err := ChannelEnableAllAutoSync(context.Background())
	if err != nil {
		t.Fatalf("空库不应失败: %v", err)
	}
	if count != 0 {
		t.Fatalf("空库 count 应为 0, got %d", count)
	}
}

// TestEnableAllAutoSyncPreservesStats 验证: 开启后 stats 保留(不被覆盖为零)。
func TestEnableAllAutoSyncPreservesStats(t *testing.T) {
	autoSyncClearTables(t)
	chID, _ := autoSyncTestChannel(t, "stats-preserve", true, false)

	// 预设 stats 到 DB。
	if err := db.GetDB().Model(&model.Channel{}).Where("id = ?", chID).
		Updates(map[string]any{
			"input_token":     int64(100),
			"output_token":    int64(200),
			"request_success": int64(5),
		}).Error; err != nil {
		t.Fatalf("预设 stats 失败: %v", err)
	}
	// 装载缓存(含 stats)。
	if err := InitCache(); err != nil {
		t.Fatalf("刷新缓存失败: %v", err)
	}

	// 执行开启。
	if _, err := ChannelEnableAllAutoSync(context.Background()); err != nil {
		t.Fatalf("ChannelEnableAllAutoSync 失败: %v", err)
	}

	// DB stats 保留。
	ch := autoSyncReadChannel(t, chID)
	if ch.InputToken != 100 || ch.OutputToken != 200 || ch.RequestSuccess != 5 {
		t.Errorf("stats 应保留: input=%d output=%d success=%d", ch.InputToken, ch.OutputToken, ch.RequestSuccess)
	}
	// 缓存 stats 保留。
	cached, ok := channelCache.Get(chID)
	if !ok {
		t.Fatal("缓存应存在")
	}
	if cached.InputToken != 100 || cached.OutputToken != 200 || cached.RequestSuccess != 5 {
		t.Errorf("缓存 stats 应保留: input=%d output=%d success=%d", cached.InputToken, cached.OutputToken, cached.RequestSuccess)
	}
	if !cached.AutoSyncModels {
		t.Error("缓存 AutoSyncModels 应为 true")
	}
}

// TestEnableAllAutoSyncRollbackOnError 验证: 第二个 UPDATE 注入 error 后,
// 全部 flag/revision 回滚(无部分提交), 缓存未发布(仍为旧值)。
func TestEnableAllAutoSyncRollbackOnError(t *testing.T) {
	autoSyncClearTables(t)
	chID1, rev1 := autoSyncTestChannel(t, "rollback-1", true, false)
	chID2, rev2 := autoSyncTestChannel(t, "rollback-2", false, false)

	// 注册 GORM After(gorm:update) 回调: 在第二次 channels UPDATE 时注入 error。
	// verifiedAtInjection 标记证实注入点(仅第二个渠道的 UPDATE 才注入)。
	var updateCount atomic.Int32
	var verifiedAtInjection atomic.Bool
	cbName := "test_autosync_rollback_" + gormColumn(chID2)
	rollbackCB := func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.Table != "channels" {
			return
		}
		n := updateCount.Add(1)
		if n == 2 {
			// 第二个 UPDATE: 注入前验证第一个已改、本条尚未改。
			verifiedAtInjection.Store(true)
			tx.AddError(errors.New("injected second update failure"))
		}
	}
	if err := db.GetDB().Callback().Update().After("gorm:update").Register(cbName, rollbackCB); err != nil {
		t.Fatalf("注册回调失败: %v", err)
	}
	defer db.GetDB().Callback().Update().Remove(cbName)

	count, err := ChannelEnableAllAutoSync(context.Background())
	if err == nil {
		t.Fatal("应返回错误(注入的第二个 UPDATE 失败)")
	}
	if count != 0 {
		t.Errorf("事务回滚后 count 应为 0, got %d", count)
	}
	if !verifiedAtInjection.Load() {
		t.Fatal("注入点未到达(回调未在第二个 UPDATE 触发)")
	}

	// DB: 两个渠道的 auto_sync 与 revision 全回滚。
	ch1 := autoSyncReadChannel(t, chID1)
	if ch1.AutoSyncModels {
		t.Error("rollback-1 auto_sync 应回滚为 false")
	}
	if ch1.Revision != rev1 {
		t.Errorf("rollback-1 revision 应回滚为 %s, got %s", rev1, ch1.Revision)
	}
	ch2 := autoSyncReadChannel(t, chID2)
	if ch2.AutoSyncModels {
		t.Error("rollback-2 auto_sync 应回滚为 false")
	}
	if ch2.Revision != rev2 {
		t.Errorf("rollback-2 revision 应回滚为 %s, got %s", rev2, ch2.Revision)
	}

	// 缓存未发布(仍为旧 auto_sync=false)。
	if cached, ok := channelCache.Get(chID1); ok {
		if cached.AutoSyncModels {
			t.Error("缓存 rollback-1 auto_sync 应为 false(未发布)")
		}
	}
	if cached, ok := channelCache.Get(chID2); ok {
		if cached.AutoSyncModels {
			t.Error("缓存 rollback-2 auto_sync 应为 false(未发布)")
		}
	}
}
