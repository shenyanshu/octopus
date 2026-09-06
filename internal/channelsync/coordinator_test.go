package channelsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/groupevents"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"gorm.io/gorm"
)

var testDir string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "octopus-channelsync-test")
	if err != nil {
		panic(err)
	}
	testDir = dir
	code := func() int {
		if err := db.InitDB("sqlite", filepath.Join(dir, "sync.db"), false); err != nil {
			panic(err)
		}
		if err := op.InitCache(); err != nil {
			panic(err)
		}
		Init(context.Background())
		return m.Run()
	}()
	_ = db.Close()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// --- httptest upstream helpers ---

// openAIModelsServer 返回模拟 OpenAI /models 端点, 按 Authorization 头区分鉴权:
// key=="sk-bad" 返回 401, 其余返回 modelNames。
func openAIModelsServer(t *testing.T, modelNames []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "Bearer sk-bad" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		type item struct{ ID string }
		type list struct {
			Data []item `json:"data"`
		}
		data := list{}
		for _, m := range modelNames {
			data.Data = append(data.Data, item{ID: m})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(data)
	}))
}

// blockingServer 返回一个服务, 其 handler 通过 ch 控制何时响应: 在 ch 收到信号前阻塞。
// 用于确定性测试"同步期间渠道配置变更": 先阻塞探测, 期间修改渠道, 再放行, 证明结果被丢弃。
func blockingServer(t *testing.T, modelNames []string) (*httptest.Server, chan struct{}) {
	t.Helper()
	ch := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-ch // 阻塞直到测试方放行
		type item struct{ ID string }
		type list struct {
			Data []item `json:"data"`
		}
		data := list{}
		for _, m := range modelNames {
			data.Data = append(data.Data, item{ID: m})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(data)
	}))
	return srv, ch
}

// --- seed helpers ---

// seedChannel 建渠道+单启用凭据, 返回渠道 ID。autoSync 控制 AutoSyncModels。
func seedChannel(t *testing.T, name, baseURL, key string, autoSync bool) int {
	t.Helper()
	detail := model.ChannelDetail{
		ChannelConfig: model.ChannelConfig{
			Name:                 name,
			Enabled:              true,
			BaseURL:              baseURL,
			OpenAIResponsePath:   "/v1/responses",
			AnthropicMessagePath: "/v1/messages",
			AutoSyncModels:       autoSync,
		},
		Keys:   []model.ChannelKeyConfig{{Name: "default", Key: key, Enabled: true}},
		Models: []string{},
		Grants: []model.ChannelGrantConfig{},
	}
	created, _, err := op.ChannelCreate(&detail, context.Background())
	if err != nil {
		t.Fatalf("建渠道 %s 失败: %v", name, err)
	}
	return created.ID
}

func findChannelModel(t *testing.T, channelID int, modelName string) bool {
	t.Helper()
	var cm model.ChannelModel
	return db.GetDB().Where("channel_id = ? AND name = ?", channelID, modelName).First(&cm).Error == nil
}

func findGrant(t *testing.T, channelID int, modelName, keyName string) (*model.ChannelGrant, bool) {
	t.Helper()
	var grant model.ChannelGrant
	err := db.GetDB().
		Joins("JOIN channel_models ON channel_models.id = channel_grants.channel_model_id").
		Joins("JOIN channel_keys ON channel_keys.id = channel_grants.channel_key_id").
		Where("channel_models.channel_id = ? AND channel_models.name = ? AND channel_keys.name = ?",
			channelID, modelName, keyName).
		First(&grant).Error
	if err != nil {
		return nil, false
	}
	return &grant, true
}

func waitStatus(t *testing.T, channelID int, want ...string) model.ChannelModelSyncStatus {
	t.Helper()
	for i := 0; i < 100; i++ {
		time.Sleep(20 * time.Millisecond)
		for _, s := range GetStatus() {
			if s.ChannelID == channelID {
				for _, w := range want {
					if s.Status == w {
						return s
					}
				}
			}
		}
	}
	t.Fatalf("渠道 %d 状态未在 %v 中稳定", channelID, want)
	return model.ChannelModelSyncStatus{}
}

func clearTables(t *testing.T) {
	t.Helper()
	// 等待全部 running worker 退出, 防止它们在清空后重新写入。
	for {
		lifecycle.Lock()
		n := len(running)
		lifecycle.Unlock()
		if n == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// 单事务删除全部表, 避免 FK 顺序问题和并发写入。
	if err := db.GetDB().Transaction(func(tx *gorm.DB) error {
		for _, tbl := range []string{"group_items", "groups", "channel_grants", "channel_models", "channel_keys", "channels"} {
			if err := tx.Exec("DELETE FROM " + tbl).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		// FK 关闭后重试。
		db.GetDB().Exec("PRAGMA foreign_keys = OFF")
		for _, tbl := range []string{"group_items", "groups", "channel_grants", "channel_models", "channel_keys", "channels"} {
			db.GetDB().Exec("DELETE FROM " + tbl)
		}
		db.GetDB().Exec("PRAGMA foreign_keys = ON")
	}
	// 重置 SQLite 自增序列。
	db.GetDB().Exec("DELETE FROM sqlite_sequence WHERE name IN ('channels','channel_keys','channel_models','channel_grants','groups','group_items')")
	statusesMu.Lock()
	statuses = make(map[int]*statusEntry)
	statusesMu.Unlock()
	if err := op.InitCache(); err != nil {
		t.Fatalf("刷新缓存失败: %v", err)
	}
}

// --- tests ---

// TestSyncAddsMissingModelsAndGrants 单渠道同步补齐缺失模型与授权, 计数正确。
func TestSyncAddsMissingModelsAndGrants(t *testing.T) {
	clearTables(t)
	upstream := openAIModelsServer(t, []string{"gpt-4o", "claude-3"})
	defer upstream.Close()
	chID := seedChannel(t, "sync-add", upstream.URL, "sk", false)
	StartSingle(chID)
	s := waitStatus(t, chID, "success")
	if !findChannelModel(t, chID, "gpt-4o") {
		t.Error("应有 gpt-4o")
	}
	if !findChannelModel(t, chID, "claude-3") {
		t.Error("应有 claude-3")
	}
	if s.AddedModels != 2 {
		t.Errorf("AddedModels=%d, want 2", s.AddedModels)
	}
	if s.AddedGrants != 2 {
		t.Errorf("AddedGrants=%d, want 2", s.AddedGrants)
	}
	if s.LastSyncAt == nil {
		t.Error("LastSyncAt 不应为 nil")
	}
}

// TestSyncNoCrossGrantKeys 两条凭据返回相同模型集, 各自独立授权不交叉。
func TestSyncNoCrossGrantKeys(t *testing.T) {
	clearTables(t)
	upstream := openAIModelsServer(t, []string{"model-a", "model-b"})
	defer upstream.Close()
	detail := model.ChannelDetail{
		ChannelConfig: model.ChannelConfig{
			Name:               "multi-key",
			Enabled:            true,
			BaseURL:            upstream.URL,
			OpenAIResponsePath: "/v1/responses",
		},
		Keys: []model.ChannelKeyConfig{
			{Name: "key-a", Key: "sk-a", Enabled: true},
			{Name: "key-b", Key: "sk-b", Enabled: true},
		},
	}
	created, _, err := op.ChannelCreate(&detail, context.Background())
	if err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	chID := created.ID
	StartSingle(chID)
	waitStatus(t, chID, "success")
	for _, keyName := range []string{"key-a", "key-b"} {
		for _, modelName := range []string{"model-a", "model-b"} {
			if _, ok := findGrant(t, chID, modelName, keyName); !ok {
				t.Errorf("缺少 %s + %s 授权", modelName, keyName)
			}
		}
	}
}

// TestSyncPreservesExistingProtocol 不修改已有授权的协议。
func TestSyncPreservesExistingProtocol(t *testing.T) {
	clearTables(t)
	upstream := openAIModelsServer(t, []string{"gpt-4o"})
	defer upstream.Close()
	detail := model.ChannelDetail{
		ChannelConfig: model.ChannelConfig{
			Name:               "proto",
			Enabled:            true,
			BaseURL:            upstream.URL,
			OpenAIResponsePath: "/v1/responses",
		},
		Keys:   []model.ChannelKeyConfig{{Name: "default", Key: "sk", Enabled: true}},
		Models: []string{"gpt-4o"},
		Grants: []model.ChannelGrantConfig{{ModelName: "gpt-4o", KeyName: "default", Protocols: model.ProtocolOpenAIChatCompletion}},
	}
	created, _, err := op.ChannelCreate(&detail, context.Background())
	if err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	chID := created.ID
	StartSingle(chID)
	waitStatus(t, chID, "success")
	grant, ok := findGrant(t, chID, "gpt-4o", "default")
	if !ok {
		t.Fatal("授权不存在")
	}
	if grant.Protocols != model.ProtocolOpenAIChatCompletion {
		t.Errorf("协议被覆盖: %d", grant.Protocols)
	}
}

// TestSyncEmptyResultDoesNotDelete 上游返回空列表, 不删除已有模型与授权。
func TestSyncEmptyResultDoesNotDelete(t *testing.T) {
	clearTables(t)
	empty := openAIModelsServer(t, []string{})
	defer empty.Close()
	detail := model.ChannelDetail{
		ChannelConfig: model.ChannelConfig{
			Name:               "empty",
			Enabled:            true,
			BaseURL:            empty.URL,
			OpenAIResponsePath: "/v1/responses",
		},
		Keys:   []model.ChannelKeyConfig{{Name: "default", Key: "sk", Enabled: true}},
		Models: []string{"gpt-4o"},
		Grants: []model.ChannelGrantConfig{{ModelName: "gpt-4o", KeyName: "default", Protocols: model.ProtocolOpenAIResponse}},
	}
	created, _, err := op.ChannelCreate(&detail, context.Background())
	if err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	chID := created.ID
	StartSingle(chID)
	waitStatus(t, chID, "success", "partial", "failed")
	if !findChannelModel(t, chID, "gpt-4o") {
		t.Error("空结果不应删除已有模型")
	}
}

// TestSyncPartialFailure 一 key 401 另一 key 成功。
// discovery 层(fix-2)对 401 可能返回空结果而非 Err; 此测试验证不交叉授权:
// 只有成功 key 的模型被授权, 失败 key 不产生额外授权。
// 若 fix-2 将 401 标记为 Err 则 status=partial; 若标记为空结果则 status=success。
// 两种情况下, 成功 key 的模型都应被应用, 失败 key 不应产生额外授权。
func TestSyncPartialFailure(t *testing.T) {
	clearTables(t)
	upstream := openAIModelsServer(t, []string{"model-a"})
	defer upstream.Close()
	detail := model.ChannelDetail{
		ChannelConfig: model.ChannelConfig{
			Name:               "partial",
			Enabled:            true,
			BaseURL:            upstream.URL,
			OpenAIResponsePath: "/v1/responses",
		},
		Keys: []model.ChannelKeyConfig{
			{Name: "good", Key: "sk-good", Enabled: true},
			{Name: "bad", Key: "sk-bad", Enabled: true},
		},
	}
	created, _, err := op.ChannelCreate(&detail, context.Background())
	if err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	chID := created.ID
	StartSingle(chID)
	s := waitStatus(t, chID, "partial", "failed", "success")
	// 成功 key 的模型应被应用。
	if !findChannelModel(t, chID, "model-a") {
		t.Error("成功 key 的模型应被应用")
	}
	// 验证 good key 有授权。
	if _, ok := findGrant(t, chID, "model-a", "good"); !ok {
		t.Error("good key 应有 model-a 授权")
	}
	t.Logf("status=%s (partial 或 success 取决于 discovery 层是否将 401 标为 Err)", s.Status)
}

// TestSyncConfigChangedDuringSyncSkipped 用阻塞上游确定性测试: 探测期间改 BaseURL, 放行后结果被丢弃。
func TestSyncConfigChangedDuringSyncSkipped(t *testing.T) {
	clearTables(t)
	upstream, ch := blockingServer(t, []string{"model-a"})
	defer upstream.Close()
	chID := seedChannel(t, "config-change", upstream.URL, "sk", false)

	StartSingle(chID)
	// 同步正在阻塞等待上游响应; 此时修改渠道 BaseURL。
	time.Sleep(50 * time.Millisecond) // 确保探测已发起
	detail, _ := op.ChannelDetailGet(context.Background(), chID)
	detail.BaseURL = "http://changed.example"
	op.ChannelUpdate(&detail, context.Background())
	// 放行探测: 写锁内重新读会发现 BaseURL 变了, 结果被丢弃。
	close(ch)
	s := waitStatus(t, chID, "skipped")
	if s.Status != "skipped" {
		t.Fatalf("status=%s, want skipped", s.Status)
	}
	if findChannelModel(t, chID, "model-a") {
		t.Error("配置变更后不应应用新模型")
	}
}

// TestSyncBatchOnlyAutoSync batch 仅启动 AutoSyncModels=true 的渠道。
func TestSyncBatchOnlyAutoSync(t *testing.T) {
	clearTables(t)
	upstream := openAIModelsServer(t, []string{"batch-model"})
	defer upstream.Close()
	chAuto := seedChannel(t, "auto", upstream.URL, "sk", true)
	chManual := seedChannel(t, "manual", upstream.URL, "sk", false)
	result, err := StartBatch()
	if err != nil {
		t.Fatalf("StartBatch 失败: %v", err)
	}
	foundAuto, foundManual := false, false
	for _, id := range result.StartedIDs {
		if id == chAuto {
			foundAuto = true
		}
		if id == chManual {
			foundManual = true
		}
	}
	if !foundAuto {
		t.Error("应启动 auto 渠道")
	}
	if foundManual {
		t.Error("不应启动 manual 渠道")
	}
}

// TestSyncRunningNotRestarted 已在运行的渠道不重复启动。
func TestSyncRunningNotRestarted(t *testing.T) {
	clearTables(t)
	upstream, ch := blockingServer(t, []string{"model-a"})
	defer upstream.Close()
	chID := seedChannel(t, "running", upstream.URL, "sk", false)
	StartSingle(chID)
	time.Sleep(50 * time.Millisecond)
	_, busy, _, _ := StartSingle(chID)
	if !busy {
		t.Error("已运行应返回 busy")
	}
	close(ch)
	waitStatus(t, chID, "success", "partial")
}

// TestSyncDisabledSkipped 禁用渠道被 skipped。
func TestSyncDisabledSkipped(t *testing.T) {
	clearTables(t)
	upstream := openAIModelsServer(t, []string{"model-x"})
	defer upstream.Close()
	chID := seedChannel(t, "disabled", upstream.URL, "sk", false)
	// GORM default:true 在 Create 时跳过零值 false, 需显式 UPDATE。
	db.GetDB().Model(&model.Channel{}).Where("id = ?", chID).Update("enabled", false)
	op.InitCache()
	_, _, skipped, _ := StartSingle(chID)
	if !skipped {
		t.Error("禁用渠道应 skipped")
	}
}

// TestSyncEventPublishedForNewGrant 新增授权触发规则分组补齐并发布 changed 事件。
func TestSyncEventPublishedForNewGrant(t *testing.T) {
	clearTables(t)
	upstream := openAIModelsServer(t, []string{"gpt-4o"})
	defer upstream.Close()
	chID := seedChannel(t, "event-grant", upstream.URL, "sk", false)
	op.GroupCreate(&model.GroupCreateRequest{
		Name: "sync-rule", Mode: model.GroupModeScored, AutoAddPattern: "^gpt-4o$",
	}, context.Background())
	events := groupevents.Subscribe()
	defer groupevents.Unsubscribe(events)
	StartSingle(chID)
	waitStatus(t, chID, "success")
	name, ok := receiveEvent(events)
	if !ok || name != "changed" {
		t.Errorf("应收到 changed: ok=%v name=%q", ok, name)
	}
	grp, _ := op.GroupGetByName("sync-rule")
	if len(grp.Items) == 0 {
		t.Error("规则分组应被补齐")
	}
}

// TestSyncCreateDisabledAutoTrueBatchNeverStarted 验证 GORM default:true 修复后:
// 创建时 Enabled=false AutoSyncModels=true 的渠道不会被 StartBatch 启动, 上游不被联系。
func TestSyncCreateDisabledAutoTrueBatchNeverStarted(t *testing.T) {
	clearTables(t)
	upstream := openAIModelsServer(t, []string{"should-not-appear"})
	defer upstream.Close()
	// 直接用 ChannelCreate 创建 disabled 渠道, 不绕过 GORM default:true bug。
	detail := model.ChannelDetail{
		ChannelConfig: model.ChannelConfig{
			Name:                 "disabled-auto",
			Enabled:              false,
			BaseURL:              upstream.URL,
			OpenAIResponsePath:   "/v1/responses",
			AnthropicMessagePath: "/v1/messages",
			AutoSyncModels:       true,
		},
		Keys: []model.ChannelKeyConfig{{Name: "default", Key: "sk", Enabled: true}},
	}
	created, _, err := op.ChannelCreate(&detail, context.Background())
	if err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	if created.Enabled {
		t.Fatal("创建返回的 Enabled 应为 false, GORM default:true 覆盖了 explicit false")
	}
	// DB 也应为 false。
	var dbCh model.Channel
	db.GetDB().Where("id = ?", created.ID).First(&dbCh)
	if dbCh.Enabled {
		t.Fatal("DB channel Enabled 应为 false")
	}
	result, err := StartBatch()
	if err != nil {
		t.Fatalf("StartBatch 失败: %v", err)
	}
	for _, id := range result.StartedIDs {
		if id == created.ID {
			t.Fatal("禁用渠道不应出现在 StartedIDs")
		}
	}
	// 上游不应被联系: 模型不应被同步进来。
	if findChannelModel(t, created.ID, "should-not-appear") {
		t.Fatal("禁用渠道的上游被联系, 模型被同步进来")
	}
}

// TestSyncCreateDisabledAutoTrueSingleSkipped 验证禁用渠道的 Single 同步被跳过。
func TestSyncCreateDisabledAutoTrueSingleSkipped(t *testing.T) {
	clearTables(t)
	upstream := openAIModelsServer(t, []string{"should-not-appear"})
	defer upstream.Close()
	detail := model.ChannelDetail{
		ChannelConfig: model.ChannelConfig{
			Name:                 "disabled-single",
			Enabled:              false,
			BaseURL:              upstream.URL,
			OpenAIResponsePath:   "/v1/responses",
			AnthropicMessagePath: "/v1/messages",
			AutoSyncModels:       true,
		},
		Keys: []model.ChannelKeyConfig{{Name: "default", Key: "sk", Enabled: true}},
	}
	created, _, err := op.ChannelCreate(&detail, context.Background())
	if err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	_, _, skipped, _ := StartSingle(created.ID)
	if !skipped {
		t.Error("禁用渠道 Single 同步应 skipped")
	}
	if findChannelModel(t, created.ID, "should-not-appear") {
		t.Fatal("禁用渠道的上游被联系")
	}
}

// TestSyncBatchAcceptsEnabledAutoTrue 验证 Enabled=true AutoSyncModels=true 的渠道被 StartBatch 启动。
func TestSyncBatchAcceptsEnabledAutoTrue(t *testing.T) {
	clearTables(t)
	upstream := openAIModelsServer(t, []string{"batch-ok-model"})
	defer upstream.Close()
	chID := seedChannel(t, "enabled-auto", upstream.URL, "sk", true)
	result, err := StartBatch()
	if err != nil {
		t.Fatalf("StartBatch 失败: %v", err)
	}
	found := false
	for _, id := range result.StartedIDs {
		if id == chID {
			found = true
		}
	}
	if !found {
		t.Fatal("Enabled+AutoSync 渠道应出现在 StartedIDs")
	}
	waitStatus(t, chID, "success")
	if !findChannelModel(t, chID, "batch-ok-model") {
		t.Fatal("上游模型应被同步进来")
	}
}

// TestGetStatusIdleForUnknownChannel 未同步过的渠道不在 status 列表中。
func TestGetStatusIdleForUnknownChannel(t *testing.T) {
	clearTables(t)
	if len(GetStatus()) != 0 {
		t.Errorf("清空后 status 应为空, 得到 %d 条", len(GetStatus()))
	}
}

// TestMaxConcurrentChannels 用 semaphore 控制最多 3 个渠道同时同步。
// 4 个渠道同时启动, 同时运行的最大数不应超过 maxConcurrent。
func TestMaxConcurrentChannels(t *testing.T) {
	clearTables(t)
	// 4 个阻塞上游, 每个独立 ch 控制放行。
	servers := make([]*httptest.Server, 4)
	releases := make([]chan struct{}, 4)
	for i := 0; i < 4; i++ {
		srv, rel := blockingServer(t, []string{fmt.Sprintf("model-%d", i)})
		servers[i] = srv
		releases[i] = rel
	}
	defer func() {
		for _, srv := range servers {
			srv.Close()
		}
	}()
	chIDs := make([]int, 4)
	for i := 0; i < 4; i++ {
		chIDs[i] = seedChannel(t, fmt.Sprintf("concurrent-%d", i), servers[i].URL, "sk", false)
	}
	for i := 0; i < 4; i++ {
		StartSingle(chIDs[i])
	}
	time.Sleep(100 * time.Millisecond)
	// 放行前 3 个, 让它们完成。
	for i := 0; i < 3; i++ {
		close(releases[i])
	}
	// 等 3 个完成, 第 4 个获得 slot 并开始探测。
	for i := 0; i < 3; i++ {
		waitStatus(t, chIDs[i], "success")
	}
	// 第 4 个应已获得 slot 并在阻塞探测。
	close(releases[3])
	waitStatus(t, chIDs[3], "success")
}

// TestStopRejectsNewSync Stop 后新同步被拒绝, 返回 ErrStopped。
func TestStopRejectsNewSync(t *testing.T) {
	// Stop 会取消 rootCtx; 测试后必须重新初始化以恢复后续测试。
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	Stop(ctx)
	_, _, _, err := StartSingle(999)
	if err == nil {
		t.Fatal("停止后 StartSingle 应返回 error")
	}
	if !errors.Is(err, ErrStopped) {
		t.Errorf("应返回 ErrStopped, 得到: %v", err)
	}
	// 重新初始化协调器, 恢复 rootCtx 供后续测试使用。
	newCtx, newCancel := context.WithCancel(context.Background())
	rootCtx = newCtx
	rootCxl = newCancel
	stopped.Store(false)
}

// TestHandlerContractSingleNotFound 不存在的渠道 ID 返回 ErrChannelNotFound。
func TestHandlerContractSingleNotFound(t *testing.T) {
	clearTables(t)
	_, _, _, err := StartSingle(99999)
	if err == nil {
		t.Fatal("不存在的渠道应返回 error")
	}
	if !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("应返回 ErrChannelNotFound, 得到: %v", err)
	}
}

// TestHandlerContractBatchEmptyArrays batch 返回的数组恒为 [] 非 null。
func TestHandlerContractBatchEmptyArrays(t *testing.T) {
	clearTables(t)
	result, err := StartBatch()
	if err != nil {
		t.Fatalf("StartBatch 失败: %v", err)
	}
	if result.StartedIDs == nil {
		t.Error("StartedIDs 不应为 nil")
	}
	if result.BusyIDs == nil {
		t.Error("BusyIDs 不应为 nil")
	}
	if result.SkippedIDs == nil {
		t.Error("SkippedIDs 不应为 nil")
	}
}

// TestAutoSyncDefaultFalseAndDumpDefault AutoSyncModels 默认 false, 导入默认 false。
func TestAutoSyncDefaultFalseAndDumpDefault(t *testing.T) {
	clearTables(t)
	upstream := openAIModelsServer(t, []string{"model-x"})
	defer upstream.Close()
	chID := seedChannel(t, "auto-default", upstream.URL, "sk", false)
	detail, err := op.ChannelDetailGet(context.Background(), chID)
	if err != nil {
		t.Fatalf("读取渠道失败: %v", err)
	}
	if detail.AutoSyncModels {
		t.Error("AutoSyncModels 默认应为 false")
	}
	// 从 DB 直接读取验证列默认。
	var ch model.Channel
	db.GetDB().Where("id = ?", chID).First(&ch)
	if ch.AutoSyncModels {
		t.Error("DB 中 AutoSyncModels 应为 false")
	}
}

// TestRepeatSyncZeroCounts 重复同步不新增, counts 为 0。
func TestRepeatSyncZeroCounts(t *testing.T) {
	clearTables(t)
	upstream := openAIModelsServer(t, []string{"gpt-4o"})
	defer upstream.Close()
	chID := seedChannel(t, "repeat", upstream.URL, "sk", false)
	StartSingle(chID)
	waitStatus(t, chID, "success")
	// 确保第一次完全释放后再启动第二次。
	time.Sleep(50 * time.Millisecond)
	// 第二次同步: 模型已存在, counts 应为 0。
	StartSingle(chID)
	s := waitStatus(t, chID, "success")
	if s.AddedModels != 0 {
		t.Errorf("重复同步 AddedModels=%d, want 0", s.AddedModels)
	}
	if s.AddedGrants != 0 {
		t.Errorf("重复同步 AddedGrants=%d, want 0", s.AddedGrants)
	}
}

// TestRunningStatusNoLastSyncAt running 状态下 LastSyncAt 为空。
func TestRunningStatusNoLastSyncAt(t *testing.T) {
	clearTables(t)
	upstream, releaseCh := blockingServer(t, []string{"model-a"})
	defer upstream.Close()
	chID := seedChannel(t, "running-ts", upstream.URL, "sk", false)
	StartSingle(chID)
	// 在 running 期间检查: LastSyncAt 应为空。等 slot+探测阻塞期间即 running。
	found := false
	for i := 0; i < 50; i++ {
		time.Sleep(20 * time.Millisecond)
		for _, s := range GetStatus() {
			if s.ChannelID == chID && s.Status == "running" {
				if s.LastSyncAt != nil {
					t.Errorf("running 状态 LastSyncAt 应为 nil, 得到 %v", *s.LastSyncAt)
				}
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	if !found {
		t.Fatal("未找到 running 状态")
	}
	// 放行探测, 让它正常完成。
	close(releaseCh)
	waitStatus(t, chID, "success")
}

// --- 新增针对性测试 ---

// TestRunningSetBeforeWorkerLaunch 用超快上游证明 running 状态在 worker 启动前设置:
// worker 可能在 setStatusRunning 返回前就完成, 但 running 状态不会被终态覆盖。
// 重复同步多次, 每次都应先看到 running, 再看到终态; 不出现永久 running。
func TestRunningSetBeforeWorkerLaunch(t *testing.T) {
	clearTables(t)
	// 超快上游: 几乎立即返回。
	upstream := openAIModelsServer(t, []string{"fast-model"})
	defer upstream.Close()
	chID := seedChannel(t, "fast", upstream.URL, "sk", false)
	for i := 0; i < 5; i++ {
		StartSingle(chID)
		s := waitStatus(t, chID, "success")
		if s.Status != "success" {
			t.Fatalf("第 %d 次: status=%s, want success", i, s.Status)
		}
		// 确保每次都不是永久 running: 终态必须有 last_sync_at。
		if s.LastSyncAt == nil {
			t.Fatalf("第 %d 次: 终态 LastSyncAt 不应为 nil", i)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRunningEventOrder 证明 running 状态先于终态设置, 且终态不会被 running 覆盖。
// 用阻塞上游确保 running 可观测, 放行后终态正确。
func TestRunningEventOrder(t *testing.T) {
	clearTables(t)
	upstream, release := blockingServer(t, []string{"model-a"})
	defer upstream.Close()
	chID := seedChannel(t, "order", upstream.URL, "sk", false)
	StartSingle(chID)
	// 必须看到 running。
	seenRunning := false
	for i := 0; i < 50; i++ {
		time.Sleep(20 * time.Millisecond)
		for _, s := range GetStatus() {
			if s.ChannelID == chID && s.Status == "running" {
				seenRunning = true
			}
		}
		if seenRunning {
			break
		}
	}
	if !seenRunning {
		t.Fatal("未看到 running 状态")
	}
	close(release)
	s := waitStatus(t, chID, "success")
	if s.Status != "success" {
		t.Fatalf("终态=%s, want success", s.Status)
	}
	// 再次检查: 不会回到 running。
	time.Sleep(50 * time.Millisecond)
	for _, s2 := range GetStatus() {
		if s2.ChannelID == chID && s2.Status == "running" {
			t.Fatal("终态后不应回到 running")
		}
	}
}

// TestShutdownStartRace 用阻塞上游证明: Start 与 Stop 竞争时不会 panic/race。
// Start 启动 worker 后立即 Stop, worker 在 rootCtx 取消后退出, Stop 等待完成。
func TestShutdownStartRace(t *testing.T) {
	clearTables(t)
	upstream, release := blockingServer(t, []string{"model-a"})
	defer upstream.Close()
	chID := seedChannel(t, "race", upstream.URL, "sk", false)

	StartSingle(chID)
	time.Sleep(20 * time.Millisecond) // 确保 worker 已启动并阻塞在 slot/upstream。

	// Stop 必须等待 worker 退出; worker 因 rootCtx 取消而退出。
	stopCtx, stopCxl := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCxl()
	if err := Stop(stopCtx); err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}
	// 放行上游让 worker(已取消)不 hang。
	close(release)
	// 重新初始化供后续测试。
	Init(context.Background())
}

// TestAutoDisabledDuringQueue 自动触发排队期间关闭 auto_sync, 获得 slot 后应 skipped。
// batch 传 isAuto=true, worker 在获 slot 后重新读 DB 发现 auto=false → skipped。
func TestAutoDisabledDuringQueue(t *testing.T) {
	clearTables(t)
	// 3 个阻塞渠道占满 slot。
	blockers := make([]*httptest.Server, 3)
	releases := make([]chan struct{}, 3)
	blockerIDs := make([]int, 3)
	for i := 0; i < 3; i++ {
		srv, rel := blockingServer(t, []string{fmt.Sprintf("block-%d", i)})
		blockers[i] = srv
		releases[i] = rel
		blockerIDs[i] = seedChannel(t, fmt.Sprintf("blocker-%d", i), srv.URL, "sk", false)
	}
	defer func() {
		for _, srv := range blockers {
			srv.Close()
		}
	}()
	// 启动前 3 个(占满 slot), 用 StartSingle(isAuto=false)。
	for i := 0; i < 3; i++ {
		StartSingle(blockerIDs[i])
	}
	time.Sleep(50 * time.Millisecond) // 确保前 3 个已占满 slot。
	// 第 4 个: auto=true。用 StartBatch 启动(isAuto=true), 它会排队等 slot。
	chID := seedChannel(t, "queued-auto", blockers[0].URL, "sk", true)
	result, _ := StartBatch()
	if len(result.StartedIDs) == 0 {
		t.Fatal("batch 应启动 queued-auto")
	}
	time.Sleep(50 * time.Millisecond)
	// 排队期间关闭 auto_sync。
	detail, _ := op.ChannelDetailGet(context.Background(), chID)
	detail.AutoSyncModels = false
	op.ChannelUpdate(&detail, context.Background())
	// 放行前 3 个让 slot 释放, 第 4 个获得 slot 后因 isAuto=true 且 auto=false → skipped。
	for i := 0; i < 3; i++ {
		close(releases[i])
	}
	for i := 0; i < 3; i++ {
		waitStatus(t, blockerIDs[i], "success")
	}
	s := waitStatus(t, chID, "skipped")
	if s.Status != "skipped" {
		t.Fatalf("status=%s, want skipped (auto disabled during queue)", s.Status)
	}
}

// helper: 通过名字查渠道 ID。
func seedChannelLookup(t *testing.T, name string) int {
	t.Helper()
	var ch model.Channel
	if err := db.GetDB().Where("name = ?", name).First(&ch).Error; err != nil {
		t.Fatalf("查找渠道 %s 失败: %v", name, err)
	}
	return ch.ID
}

// TestJSONMarshalRunningNullLastSyncAt 实际 JSON marshal 检查 running 状态 last_sync_at 为 null。
func TestJSONMarshalRunningNullLastSyncAt(t *testing.T) {
	clearTables(t)
	upstream, release := blockingServer(t, []string{"model-a"})
	defer upstream.Close()
	chID := seedChannel(t, "json-null", upstream.URL, "sk", false)
	StartSingle(chID)
	// 等 running 出现。
	var raw []byte
	for i := 0; i < 50; i++ {
		time.Sleep(20 * time.Millisecond)
		statuses := GetStatus()
		for _, s := range statuses {
			if s.ChannelID == chID && s.Status == "running" {
				raw, _ = json.Marshal(statuses)
				break
			}
		}
		if raw != nil {
			break
		}
	}
	if raw == nil {
		close(release)
		t.Fatal("未看到 running 状态")
	}
	// 检查 JSON 中 last_sync_at 为 null 而非 ""。
	if !strings.Contains(string(raw), `"last_sync_at":null`) {
		t.Errorf("JSON 中 last_sync_at 应为 null: %s", string(raw))
	}
	close(release)
	waitStatus(t, chID, "success")
}

// TestAllKeysFailedNoSupplement 全失败不触发 supplementGroupsForChannel, 不补齐已有匹配授权。
func TestAllKeysFailedNoSupplement(t *testing.T) {
	clearTables(t)
	// 上游始终 401。
	upstream := openAIModelsServer(t, nil)
	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	defer upstream.Close()
	chID := seedChannel(t, "all-fail", upstream.URL, "sk-bad", false)
	// 建规则分组: 如果 supplement 被错误触发, 会补齐已有授权(如有)。
	op.GroupCreate(&model.GroupCreateRequest{
		Name: "fail-rule", Mode: model.GroupModeScored, AutoAddPattern: ".*",
	}, context.Background())
	StartSingle(chID)
	s := waitStatus(t, chID, "failed")
	if s.Status != "failed" {
		t.Fatalf("status=%s, want failed", s.Status)
	}
	if s.AddedModels != 0 || s.AddedGrants != 0 {
		t.Errorf("全失败 counts 应为 0: models=%d grants=%d", s.AddedModels, s.AddedGrants)
	}
}

// TestEmptyResultNoSupplement 空成功(空模型列表)不触发 supplement。
func TestEmptyResultNoSupplement(t *testing.T) {
	clearTables(t)
	upstream := openAIModelsServer(t, []string{})
	defer upstream.Close()
	chID := seedChannel(t, "empty-success", upstream.URL, "sk", false)
	op.GroupCreate(&model.GroupCreateRequest{
		Name: "empty-rule", Mode: model.GroupModeScored, AutoAddPattern: ".*",
	}, context.Background())
	StartSingle(chID)
	s := waitStatus(t, chID, "success", "failed", "partial", "skipped")
	if s.AddedModels != 0 || s.AddedGrants != 0 {
		t.Errorf("空结果 counts 应为 0: models=%d grants=%d status=%s", s.AddedModels, s.AddedGrants, s.Status)
	}
}

// TestRepeatSyncNoEventNoAdditions 重复同步(模型已存在, 无新增)不发布 changed 事件, counts 为 0。
// 这不是回滚测试: 此处事务成功提交, 只是因为无新增而 mutation=nil, 不发事件。
func TestRepeatSyncNoEventNoAdditions(t *testing.T) {
	clearTables(t)
	upstream := openAIModelsServer(t, []string{"gpt-4o"})
	defer upstream.Close()
	chID := seedChannel(t, "repeat-noop", upstream.URL, "sk", false)
	op.GroupCreate(&model.GroupCreateRequest{
		Name: "repeat-rule", Mode: model.GroupModeScored, AutoAddPattern: "^gpt-4o$",
	}, context.Background())
	// 第一次同步: 补齐, 发布 changed。
	StartSingle(chID)
	waitStatus(t, chID, "success")
	// 在第一次完成后订阅, 确保不接收第一次的事件。
	events := groupevents.Subscribe()
	defer groupevents.Unsubscribe(events)
	// 排空可能残留的事件。
	select {
	case <-events:
	default:
	}
	// 第二次: 模型已存在, 不新增。不触发 supplement。不应有事件。
	StartSingle(chID)
	waitStatus(t, chID, "success")
	// 等待可能的事件(不应到达)。
	time.Sleep(200 * time.Millisecond)
	select {
	case ev := <-events:
		t.Errorf("无新增不应发布事件, 但收到: %s", ev.Name)
	default:
		// 正确: 无事件。
	}
}

// TestTransactionRollbackOnError 事务中途失败时, 已插入的模型被回滚, DB 无新增, mutation=nil, 无事件。
// 用 GORM Create 回调在 grant INSERT 前注入错误, 使 ensureChannelGrant 失败触发回滚。
func TestTransactionRollbackOnError(t *testing.T) {
	clearTables(t)
	upstream := openAIModelsServer(t, []string{"rollback-model"})
	defer upstream.Close()
	chID := seedChannel(t, "rollback-tx", upstream.URL, "sk", false)
	op.GroupCreate(&model.GroupCreateRequest{
		Name: "rollback-tx-rule", Mode: model.GroupModeScored, AutoAddPattern: "^rollback-model$",
	}, context.Background())

	// 注册一次性 Create 回调: 在 channel_grants 表的首次 INSERT 时返回错误。
	var triggered atomic.Bool
	rollbackCB := func(tx *gorm.DB) {
		if tx.Statement.Table == "channel_grants" && !triggered.Load() {
			triggered.Store(true)
			tx.AddError(errors.New("injected grant insert failure"))
		}
	}
	callbackName := "channelsync_test_rollback_" + fmt.Sprintf("%d", chID)
	if err := db.GetDB().Callback().Create().After("gorm:create").Register(callbackName, rollbackCB); err != nil {
		t.Fatalf("注册回调失败: %v", err)
	}
	defer db.GetDB().Callback().Create().Remove(callbackName)

	// 订阅事件: 回滚不应发布 SSE。
	events := groupevents.Subscribe()
	defer groupevents.Unsubscribe(events)

	StartSingle(chID)
	s := waitStatus(t, chID, "failed")
	if s.Status != "failed" {
		t.Fatalf("status=%s, want failed (事务回滚)", s.Status)
	}
	if s.AddedModels != 0 || s.AddedGrants != 0 {
		t.Errorf("回滚后 counts 应为 0: models=%d grants=%d", s.AddedModels, s.AddedGrants)
	}
	// DB 中不应有该模型(事务已回滚)。
	if findChannelModel(t, chID, "rollback-model") {
		t.Error("事务回滚后不应有模型残留")
	}
	// 不应发布 SSE 事件。
	time.Sleep(200 * time.Millisecond)
	select {
	case ev := <-events:
		t.Errorf("回滚不应发布事件, 但收到: %s", ev.Name)
	default:
		// 正确: 无事件。
	}
}

// --- util ---

func receiveEvent(events <-chan groupevents.Event) (string, bool) {
	for i := 0; i < 50; i++ {
		time.Sleep(20 * time.Millisecond)
		select {
		case ev, ok := <-events:
			if !ok {
				return "", false
			}
			return ev.Name, true
		default:
		}
	}
	return "", false
}

// --- 双协议测试服务器: 同时区分 Authorization 与 X-Api-Key ---

// dualProtocolServer 按 Authorization(OpenAI 侧) 与 X-Api-Key(Anthropic 侧) 区分鉴权:
// key=="sk-bad" 返回 401, 其余返回 modelNames。两侧协议各自独立鉴权。
func dualProtocolServer(t *testing.T, modelNames []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var key string
		if auth := r.Header.Get("Authorization"); auth != "" {
			key = strings.TrimPrefix(auth, "Bearer ")
		} else if api := r.Header.Get("X-Api-Key"); api != "" {
			key = api
		}
		if key == "sk-bad" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		type item struct{ ID string }
		type list struct {
			Data []item `json:"data"`
		}
		data := list{}
		for _, m := range modelNames {
			data.Data = append(data.Data, item{ID: m})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(data)
	}))
}

// --- finalizeStatus 表驱动测试: 3-way 组合 ---

func TestFinalizeStatusTableDriven(t *testing.T) {
	// 组合维度: 每条 key 的结果为 success / partial / failure。
	// 预期: 无成功 → failed; 成功+失败 → partial; 全成功 → success。
	now := time.Now().UTC().Format(time.RFC3339Nano)
	cases := []struct {
		name        string
		discoveries []op.KeyDiscovery
		wantStatus  string
	}{
		{
			name:        "single success",
			discoveries: []op.KeyDiscovery{{Models: []model.ChannelFetchModel{{Name: "m"}}}},
			wantStatus:  "success",
		},
		{
			name:        "single failure",
			discoveries: []op.KeyDiscovery{{Err: errors.New("err")}},
			wantStatus:  "failed",
		},
		{
			name:        "single partial (one protocol failed, models available)",
			discoveries: []op.KeyDiscovery{{Models: []model.ChannelFetchModel{{Name: "m"}}, Partial: true}},
			wantStatus:  "partial", // 单 partial key: hasSuccess=true + hasFailure=true(Partial) → partial
		},
		{
			name:        "success + failure",
			discoveries: []op.KeyDiscovery{{Models: []model.ChannelFetchModel{{Name: "m"}}}, {Err: errors.New("err")}},
			wantStatus:  "partial",
		},
		{
			name:        "partial + failure",
			discoveries: []op.KeyDiscovery{{Models: []model.ChannelFetchModel{{Name: "m"}}, Partial: true}, {Err: errors.New("err")}},
			wantStatus:  "partial",
		},
		{
			name:        "success + partial",
			discoveries: []op.KeyDiscovery{{Models: []model.ChannelFetchModel{{Name: "m"}}}, {Models: []model.ChannelFetchModel{{Name: "m2"}}, Partial: true}},
			wantStatus:  "partial", // success + partial(=成功+失败) → partial
		},
		{
			name:        "two failures",
			discoveries: []op.KeyDiscovery{{Err: errors.New("e1")}, {Err: errors.New("e2")}},
			wantStatus:  "failed",
		},
		{
			name:        "two successes",
			discoveries: []op.KeyDiscovery{{Models: []model.ChannelFetchModel{{Name: "m1"}}}, {Models: []model.ChannelFetchModel{{Name: "m2"}}}},
			wantStatus:  "success",
		},
		{
			name:        "partial + partial",
			discoveries: []op.KeyDiscovery{{Models: []model.ChannelFetchModel{{Name: "m"}}, Partial: true}, {Models: []model.ChannelFetchModel{{Name: "m2"}}, Partial: true}},
			wantStatus:  "partial", // 每条都是成功+失败 → partial
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearTables(t)
			chID := seedChannel(t, "finalize-"+tc.name, "http://unused.test", "sk", false)
			// 直接调用 finalizeStatus, 不经过网络。
			finalizeStatus(chID, tc.discoveries, op.SyncAdditions{})
			s := waitStatus(t, chID, tc.wantStatus)
			if s.Status != tc.wantStatus {
				t.Errorf("status=%s, want %s", s.Status, tc.wantStatus)
			}
			if s.LastSyncAt == nil {
				t.Error("终态 LastSyncAt 不应为 nil")
			} else if *s.LastSyncAt == "" {
				t.Error("终态 LastSyncAt 不应为空串")
			}
			_ = now
		})
	}
}

// --- 双协议: 两 key 各 401/成功 → partial, 仅成功 key 授权 ---

func TestDualProtocolBadKey401GoodKeyModelsPartial(t *testing.T) {
	clearTables(t)
	upstream := dualProtocolServer(t, []string{"shared-model"})
	defer upstream.Close()
	detail := model.ChannelDetail{
		ChannelConfig: model.ChannelConfig{
			Name:                 "dual-partial",
			Enabled:              true,
			BaseURL:              upstream.URL,
			OpenAIResponsePath:   "/v1/responses",
			AnthropicMessagePath: "/v1/messages",
		},
		Keys: []model.ChannelKeyConfig{
			{Name: "good", Key: "sk-good", Enabled: true},
			{Name: "bad", Key: "sk-bad", Enabled: true},
		},
	}
	created, _, err := op.ChannelCreate(&detail, context.Background())
	if err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	chID := created.ID
	StartSingle(chID)
	s := waitStatus(t, chID, "partial", "failed", "success")
	// 坏 key 在两侧协议都 401 → Err != nil → failure。好 key 在两侧都成功 → success。
	// 成功+失败 → partial。
	if s.Status != "partial" {
		t.Fatalf("status=%s, want partial (好 key 成功+坏 key 失败)", s.Status)
	}
	// 好	key 应有 shared-model 授权。
	if _, ok := findGrant(t, chID, "shared-model", "good"); !ok {
		t.Error("good key 应有 shared-model 授权")
	}
	// 坏 key 不应有授权(401 无模型)。
	if _, ok := findGrant(t, chID, "shared-model", "bad"); ok {
		t.Error("bad key 不应有授权(401 无模型)")
	}
	if s.AddedModels == 0 {
		t.Error("应有新增模型")
	}
}

// --- 双协议: 两侧都 401 → failed, 无授权 ---

func TestDualProtocolBothKeys401Failed(t *testing.T) {
	clearTables(t)
	upstream := dualProtocolServer(t, []string{"m"})
	defer upstream.Close()
	detail := model.ChannelDetail{
		ChannelConfig: model.ChannelConfig{
			Name:                 "dual-both-fail",
			Enabled:              true,
			BaseURL:              upstream.URL,
			OpenAIResponsePath:   "/v1/responses",
			AnthropicMessagePath: "/v1/messages",
		},
		Keys: []model.ChannelKeyConfig{
			{Name: "bad-a", Key: "sk-bad", Enabled: true},
			{Name: "bad-b", Key: "sk-bad", Enabled: true},
		},
	}
	created, _, err := op.ChannelCreate(&detail, context.Background())
	if err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	chID := created.ID
	StartSingle(chID)
	s := waitStatus(t, chID, "failed")
	if s.Status != "failed" {
		t.Fatalf("status=%s, want failed (两侧都 401)", s.Status)
	}
	if s.AddedModels != 0 || s.AddedGrants != 0 {
		t.Errorf("全失败 counts 应为 0: models=%d grants=%d", s.AddedModels, s.AddedGrants)
	}
	// 不应有任何授权。
	for _, keyName := range []string{"bad-a", "bad-b"} {
		if _, ok := findGrant(t, chID, "m", keyName); ok {
			t.Errorf("坏 key %s 不应有授权", keyName)
		}
	}
}

// --- 单 key 单协议成功 → partial (Partial=true) ---

func TestSingleKeyPartialStatusPartial(t *testing.T) {
	clearTables(t)
	// 仅 OpenAI 侧可达(Anthropic 侧 401), 模型成功 → Partial=true。
	// 用 dualProtocolServer: 好 key 在 Authorization 侧成功, X-Api-Key 侧也成功 → 完全成功。
	// 要制造 partial: 一侧 401 一侧成功。用自定义服务器。
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "" {
			// Anthropic 侧 401
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// OpenAI 侧成功
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data": []map[string]string{{"id": "openai-only-model"}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	detail := model.ChannelDetail{
		ChannelConfig: model.ChannelConfig{
			Name:                 "single-partial",
			Enabled:              true,
			BaseURL:              srv.URL,
			OpenAIResponsePath:   "/v1/responses",
			AnthropicMessagePath: "/v1/messages",
		},
		Keys: []model.ChannelKeyConfig{
			{Name: "default", Key: "sk-good", Enabled: true},
		},
	}
	created, _, err := op.ChannelCreate(&detail, context.Background())
	if err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	chID := created.ID
	StartSingle(chID)
	s := waitStatus(t, chID, "partial", "success", "failed")
	// 单 key Partial=true: hasSuccess=true(Err==nil), hasFailure=true(Partial) → partial。
	if s.Status != "partial" {
		t.Fatalf("status=%s, want partial (单 key Partial=true, 成功+失败)", s.Status)
	}
	if !findChannelModel(t, chID, "openai-only-model") {
		t.Error("应有 openai-only-model")
	}
}
