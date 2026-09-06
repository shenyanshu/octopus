package op

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// seedRevChannel 建一条带 revision 的测试渠道, 返回 ID。
// op 包有两个 seedChannel 定义(group_pattern_test 与 channelsync 包), 签名不同;
// 这里用闭名封装统一的创建路径, 避免依赖具体某个 seedChannel 的参数顺序。
func seedRevChannel(t *testing.T, name string) int {
	t.Helper()
	id, _ := seedChannel(t, name, true, nil, []keySpec{{"k", true}})
	reloadAllCache(t)
	return id
}

// TestChannelCreateAssignsRevision 验证 ChannelCreate 为新渠道分配非空 revision。
func TestChannelCreateAssignsRevision(t *testing.T) {
	clearAll(t)
	detail := model.ChannelDetail{
		ChannelConfig: model.ChannelConfig{
			Name:    "rev-create",
			Enabled: true,
			BaseURL: "http://rev-create.example",
			Dialect: model.DialectGeneric,
		},
		Keys:   []model.ChannelKeyConfig{{Name: "k", Key: "sk", Enabled: true}},
		Models: []string{},
		Grants: []model.ChannelGrantConfig{},
	}
	created, _, err := ChannelCreate(&detail, context.Background())
	if err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	if created.Revision == "" {
		t.Fatal("新建渠道 revision 为空, 应由服务端生成 UUID")
	}
	reloadAllCache(t)
	ch, ok := channelCache.Get(created.ID)
	if !ok {
		t.Fatal("渠道缓存缺失")
	}
	if ch.Revision != created.Revision {
		t.Fatalf("缓存 revision %q != 返回 %q", ch.Revision, created.Revision)
	}
}

// TestChannelUpdateRequiresRevision 验证空串 expected revision 返回 ErrRevisionRequired。
func TestChannelUpdateRequiresRevision(t *testing.T) {
	clearAll(t)
	chID := seedRevChannel(t, "rev-req")
	detail, _ := ChannelDetailGet(context.Background(), chID)
	detail.Revision = ""
	_, _, err := ChannelUpdate(&detail, context.Background())
	if !errors.Is(err, ErrRevisionRequired) {
		t.Fatalf("空 revision 应返回 ErrRevisionRequired, got %v", err)
	}
}

// TestChannelUpdateWrongRevisionReturnsConflict 验证过期 revision 返回 ErrRevisionConflict。
func TestChannelUpdateWrongRevisionReturnsConflict(t *testing.T) {
	clearAll(t)
	chID := seedRevChannel(t, "rev-wrong")
	detail, _ := ChannelDetailGet(context.Background(), chID)
	detail.Revision = uuid.NewString()
	_, _, err := ChannelUpdate(&detail, context.Background())
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("过期 revision 应返回 ErrRevisionConflict, got %v", err)
	}
}

// TestChannelUpdateRotatesRevision 验证成功更新后返回新 revision, 旧令牌失效。
func TestChannelUpdateRotatesRevision(t *testing.T) {
	clearAll(t)
	chID := seedRevChannel(t, "rev-rotate")
	detail, _ := ChannelDetailGet(context.Background(), chID)
	oldRev := detail.Revision
	if oldRev == "" {
		t.Fatal("初始 revision 为空")
	}
	updated, _, err := ChannelUpdate(&detail, context.Background())
	if err != nil {
		t.Fatalf("更新渠道失败: %v", err)
	}
	if updated.Revision == "" {
		t.Fatal("更新后 revision 为空")
	}
	if updated.Revision == oldRev {
		t.Fatal("更新后 revision 未轮转, 仍为旧值")
	}
	detail2, _ := ChannelDetailGet(context.Background(), chID)
	detail2.Revision = oldRev
	_, _, err = ChannelUpdate(&detail2, context.Background())
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("旧 revision 应返回 ErrRevisionConflict, got %v", err)
	}
}

// TestChannelEnabledRotatesRevision 验证启停操作轮转 revision。
func TestChannelEnabledRotatesRevision(t *testing.T) {
	clearAll(t)
	chID := seedRevChannel(t, "rev-enable")
	detail, _ := ChannelDetailGet(context.Background(), chID)
	oldRev := detail.Revision
	_, err := ChannelEnabled(chID, false, context.Background())
	if err != nil {
		t.Fatalf("禁用渠道失败: %v", err)
	}
	detail2, _ := ChannelDetailGet(context.Background(), chID)
	if detail2.Revision == oldRev {
		t.Fatal("启停后 revision 未轮转")
	}
}

// TestChannelDelMissingReturnsNotFound 验证删除不存在的渠道返回 "channel not found"(HEAD 语义),
// 而非 ErrRevisionConflict: 409 仅保留给全量更新的 expected token 冲突。
func TestChannelDelMissingReturnsNotFound(t *testing.T) {
	clearAll(t)
	_, err := ChannelDel(99999, context.Background())
	if err == nil {
		t.Fatal("删除不存在渠道应返回错误")
	}
	if errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("删除不存在渠道不应返回 ErrRevisionConflict, 应为 channel not found")
	}
}

// TestConcurrentChannelUpdateOneWins 验证并发更新同一渠道: 恰有一个成功, 其余 CAS 失败。
func TestConcurrentChannelUpdateOneWins(t *testing.T) {
	clearAll(t)
	chID := seedRevChannel(t, "rev-concurrent")
	detail, _ := ChannelDetailGet(context.Background(), chID)

	const N = 5
	var wg sync.WaitGroup
	wg.Add(N)
	var mu sync.Mutex
	successCount := 0
	conflictCount := 0
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			// 深拷贝 detail: normalizeChannelDetail 会原地修改 Keys/Models/Grants 切片元素,
			// 浅拷贝共享底层数组会在并发下触发 data race。
			d := detail
			d.Keys = make([]model.ChannelKeyConfig, len(detail.Keys))
			copy(d.Keys, detail.Keys)
			d.Models = make([]string, len(detail.Models))
			copy(d.Models, detail.Models)
			d.Grants = make([]model.ChannelGrantConfig, len(detail.Grants))
			copy(d.Grants, detail.Grants)
			_, _, err := ChannelUpdate(&d, context.Background())
			mu.Lock()
			if err == nil {
				successCount++
			} else if errors.Is(err, ErrRevisionConflict) {
				conflictCount++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if successCount != 1 {
		t.Fatalf("并发更新应恰有 1 个成功, got %d", successCount)
	}
}

// TestChannelSyncApplyRotatesRevision 验证同步成功新增授权后轮转 DB revision。
func TestChannelSyncApplyRotatesRevision(t *testing.T) {
	clearAll(t)
	chID := seedRevChannel(t, "rev-sync")
	detail, _ := ChannelDetailGet(context.Background(), chID)
	beforeRev := detail.Revision

	// ChannelKeyConfig 没有 ID 字段, 从 DB 查 channel_keys 主键。
	var key model.ChannelKey
	if err := db.GetDB().Where("channel_id = ?", chID).First(&key).Error; err != nil {
		t.Fatalf("读凭据失败: %v", err)
	}
	discoveries := []KeyDiscovery{{
		KeyID: key.ID,
		Models: []model.ChannelFetchModel{{
			Name:      "new-sync-model",
			Protocols: model.ProtocolOpenAIChatCompletion,
		}},
	}}
	_, additions, err := ChannelSyncApplyDiscovery(db.GetDB(), chID, discoveries)
	if err != nil {
		t.Fatalf("应用发现失败: %v", err)
	}
	if additions.AddedGrants == 0 {
		t.Fatal("应新增至少一条授权")
	}
	var dbChannel model.Channel
	if err := db.GetDB().Where("id = ?", chID).First(&dbChannel).Error; err != nil {
		t.Fatalf("读 DB 渠道失败: %v", err)
	}
	if dbChannel.Revision == beforeRev {
		t.Fatal("同步成功后 DB revision 未轮转")
	}
}

// TestChannelSyncApplyNoAdditionsNoRotate 验证空同步(无新增授权)不轮转 revision。
func TestChannelSyncApplyNoAdditionsNoRotate(t *testing.T) {
	clearAll(t)
	chID := seedRevChannel(t, "rev-sync-empty")
	detail, _ := ChannelDetailGet(context.Background(), chID)
	beforeRev := detail.Revision

	var key model.ChannelKey
	if err := db.GetDB().Where("channel_id = ?", chID).First(&key).Error; err != nil {
		t.Fatalf("读凭据失败: %v", err)
	}
	discoveries := []KeyDiscovery{{
		KeyID: key.ID,
		Err:   errors.New("upstream unreachable"),
	}}
	_, additions, err := ChannelSyncApplyDiscovery(db.GetDB(), chID, discoveries)
	if err != nil {
		t.Fatalf("空发现不应报错: %v", err)
	}
	if additions.AddedGrants != 0 {
		t.Fatalf("空发现不应新增授权, got %d", additions.AddedGrants)
	}
	var dbChannel model.Channel
	if err := db.GetDB().Where("id = ?", chID).First(&dbChannel).Error; err != nil {
		t.Fatalf("读 DB 渠道失败: %v", err)
	}
	if dbChannel.Revision != beforeRev {
		t.Fatal("空同步不应轮转 revision")
	}
}

// TestExportRevisionNotInJSON 验证导出的序列化 JSON 不含 revision 字段。
// Channel.Revision 标记 json:"-", 结构体字段虽有值, JSON 序列化时自动剔除。
func TestExportRevisionNotInJSON(t *testing.T) {
	clearAll(t)
	seedRevChannel(t, "rev-export")
	dump, err := DBExportAll(context.Background())
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	// 序列化整个 dump 为 JSON, 确认 channels 中不含 "revision" 键。
	raw, err := json.Marshal(dump)
	if err != nil {
		t.Fatalf("序列化 dump 失败: %v", err)
	}
	if strings.Contains(string(raw), `"revision"`) {
		t.Fatal("导出 JSON 不应包含 revision 字段")
	}
}

// TestImportAssignsNewRevision 验证导入端为渠道分配新 revision。
func TestImportAssignsNewRevision(t *testing.T) {
	clearAll(t)
	seedRevChannel(t, "rev-import")
	dump, err := DBExportAll(context.Background())
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	clearAll(t)
	_, err = DBImportIncremental(context.Background(), dump)
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	var channels []model.Channel
	if err := db.GetDB().Find(&channels).Error; err != nil {
		t.Fatalf("读 DB 渠道失败: %v", err)
	}
	for i, ch := range channels {
		if ch.Revision == "" {
			t.Fatalf("导入后渠道 %d revision 应为非空 UUID", i)
		}
	}
}

// TestChannelSyncConfigUnchangedRevisionMismatch 验证 revision 不匹配时配置变更检测返回 false。
func TestChannelSyncConfigUnchangedRevisionMismatch(t *testing.T) {
	snap := ChannelSyncSnapshot{Revision: "aaa", Config: model.ChannelConfig{BaseURL: "http://a.example"}}
	curr := ChannelSyncSnapshot{Revision: "bbb", Config: model.ChannelConfig{BaseURL: "http://a.example"}}
	if ChannelSyncConfigUnchanged(snap, curr, nil, false) {
		t.Fatal("revision 不同应判定为配置已变")
	}
}

// TestChannelSyncConfigUnchangedRevisionMatch 验证 revision 相同且配置相同返回 true。
func TestChannelSyncConfigUnchangedRevisionMatch(t *testing.T) {
	snap := ChannelSyncSnapshot{Revision: "aaa", Config: model.ChannelConfig{BaseURL: "http://a.example"}}
	curr := ChannelSyncSnapshot{Revision: "aaa", Config: model.ChannelConfig{BaseURL: "http://a.example"}}
	if !ChannelSyncConfigUnchanged(snap, curr, nil, false) {
		t.Fatal("revision 与配置均相同应判定为未变")
	}
}

var _ = gorm.ErrRecordNotFound

// TestChannelEnabledNoOpRetainsRevision 验证 no-op 启停(相同值)不轮转 revision。
func TestChannelEnabledNoOpRetainsRevision(t *testing.T) {
	clearAll(t)
	chID := seedRevChannel(t, "rev-noop")
	detail, _ := ChannelDetailGet(context.Background(), chID)
	beforeRev := detail.Revision
	// 渠道已启用, 再次启用为 no-op。
	_, err := ChannelEnabled(chID, true, context.Background())
	if err != nil {
		t.Fatalf("no-op 启停失败: %v", err)
	}
	detail2, _ := ChannelDetailGet(context.Background(), chID)
	if detail2.Revision != beforeRev {
		t.Fatalf("no-op 启停不应轮转 revision: %q -> %q", beforeRev, detail2.Revision)
	}
}

// TestChannelEnabledActualChangeRotatesRevision 验证实际状态变化轮转 revision。
func TestChannelEnabledActualChangeRotatesRevision(t *testing.T) {
	clearAll(t)
	chID := seedRevChannel(t, "rev-toggle")
	detail, _ := ChannelDetailGet(context.Background(), chID)
	beforeRev := detail.Revision
	_, err := ChannelEnabled(chID, false, context.Background())
	if err != nil {
		t.Fatalf("禁用渠道失败: %v", err)
	}
	detail2, _ := ChannelDetailGet(context.Background(), chID)
	if detail2.Revision == beforeRev {
		t.Fatal("实际状态变化应轮转 revision")
	}
}

// TestChannelEnabledMissingReturnsNotFound 验证启停不存在渠道返回 "channel not found" 而非 ErrRevisionConflict。
func TestChannelEnabledMissingReturnsNotFound(t *testing.T) {
	clearAll(t)
	_, err := ChannelEnabled(99999, true, context.Background())
	if err == nil {
		t.Fatal("启停不存在渠道应返回错误")
	}
	if errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("启停不存在渠道不应返回 ErrRevisionConflict")
	}
}

// TestSyncCommitPublishesRevisionToCache 验证 ChannelSyncCommitAndRefresh 发布已提交 revision 到 channelCache。
// 模拟: apply 轮转 DB revision → commit 发布到缓存 → ChannelDetailGet 返回新令牌。
func TestSyncCommitPublishesRevisionToCache(t *testing.T) {
	clearAll(t)
	chID := seedRevChannel(t, "rev-sync-cache")
	detail, _ := ChannelDetailGet(context.Background(), chID)
	oldRev := detail.Revision

	var key model.ChannelKey
	if err := db.GetDB().Where("channel_id = ?", chID).First(&key).Error; err != nil {
		t.Fatalf("读凭据失败: %v", err)
	}
	discoveries := []KeyDiscovery{{
		KeyID: key.ID,
		Models: []model.ChannelFetchModel{{
			Name:      "sync-cache-model",
			Protocols: model.ProtocolOpenAIChatCompletion,
		}},
	}}
	// apply 在事务内轮转 DB revision。
	mutation, additions, err := ChannelSyncApplyDiscovery(db.GetDB(), chID, discoveries)
	if err != nil {
		t.Fatalf("apply 失败: %v", err)
	}
	if additions.AddedGrants == 0 {
		t.Fatal("应新增授权")
	}
	// commit 发布已提交 revision 到缓存。
	_, err = ChannelSyncCommitAndRefresh(context.Background(), chID, mutation)
	if err != nil {
		t.Fatalf("commit 失败: %v", err)
	}
	// ChannelDetailGet 应返回新 revision(与 DB 一致, 与旧值不同)。
	detail2, _ := ChannelDetailGet(context.Background(), chID)
	if detail2.Revision == oldRev {
		t.Fatal("commit 后 ChannelDetailGet 仍返回旧 revision")
	}
	// 验证 DB revision 与 detail 一致。
	var dbChannel model.Channel
	if err := db.GetDB().Where("id = ?", chID).First(&dbChannel).Error; err != nil {
		t.Fatalf("读 DB 渠道失败: %v", err)
	}
	if detail2.Revision != dbChannel.Revision {
		t.Fatalf("detail revision %q != DB revision %q", detail2.Revision, dbChannel.Revision)
	}
}

// TestSyncCommitPreservesRevisionOnChildRefreshFailure 验证 child 缓存刷新失败时:
// commit 返回 PostCommitError, 但 DB 内的 revision 轮转和新增模型/授权已落库,
// ChannelDetailGet 从 DB 读到新 revision 和新增项; 基于该详情做 ChannelUpdate 不丢失新增。
// 用包内变量 reloadChannelChildren 注入一次性失败, 不破坏表结构。
func TestSyncCommitPreservesRevisionOnChildRefreshFailure(t *testing.T) {
	clearAll(t)
	chID := seedRevChannel(t, "rev-sync-fail")
	detail, _ := ChannelDetailGet(context.Background(), chID)
	oldRev := detail.Revision

	var key model.ChannelKey
	if err := db.GetDB().Where("channel_id = ?", chID).First(&key).Error; err != nil {
		t.Fatalf("读凭据失败: %v", err)
	}
	discoveries := []KeyDiscovery{{
		KeyID: key.ID,
		Models: []model.ChannelFetchModel{{
			Name:      "sync-fail-model",
			Protocols: model.ProtocolOpenAIChatCompletion,
		}},
	}}
	mutation, _, err := ChannelSyncApplyDiscovery(db.GetDB(), chID, discoveries)
	if err != nil {
		t.Fatalf("apply 失败: %v", err)
	}

	// 注入一次性 reloadChannelChildren 失败, 测试后恢复。
	origReload := reloadChannelChildren
	reloadChannelChildren = func(ctx context.Context, id int) error {
		return fmt.Errorf("injected child reload failure")
	}
	defer func() { reloadChannelChildren = origReload }()

	_, commitErr := ChannelSyncCommitAndRefresh(context.Background(), chID, mutation)
	if commitErr == nil {
		t.Fatal("child reload 失败应返回 PostCommitError")
	}
	var pce *PostCommitError
	if !errors.As(commitErr, &pce) {
		t.Fatalf("应返回 *PostCommitError, got %T", commitErr)
	}
	// mutation 可能为 nil(无分组模式匹配时不产生 GroupDeltas), PostCommitError 本身即提交事实。

	// 恢复 reload 后, ChannelDetailGet 从 DB 读到新 revision 和新增模型。
	reloadChannelChildren = origReload
	detail2, err := ChannelDetailGet(context.Background(), chID)
	if err != nil {
		t.Fatalf("child 失败后 ChannelDetailGet 应可读: %v", err)
	}
	if detail2.Revision == oldRev {
		t.Fatal("child refresh 失败后 DB 仍持旧 revision")
	}
	found := false
	for _, m := range detail2.Models {
		if m == "sync-fail-model" {
			found = true
		}
	}
	if !found {
		t.Fatal("child refresh 失败后 DB detail 缺少新增模型 sync-fail-model")
	}

	// 基于该 fetched detail 做 ChannelUpdate: 新增的模型和授权不应被删除。
	detail2.BaseURL = "http://post-fail-update.example"
	updated, _, updateErr := ChannelUpdate(&detail2, context.Background())
	if updateErr != nil {
		t.Fatalf("基于 fetched detail 更新失败: %v", updateErr)
	}
	// 验证更新后模型仍在。
	var dbModels []model.ChannelModel
	if err := db.GetDB().Where("channel_id = ?", chID).Find(&dbModels).Error; err != nil {
		t.Fatalf("读 DB 模型失败: %v", err)
	}
	stillExists := false
	for _, m := range dbModels {
		if m.Name == "sync-fail-model" {
			stillExists = true
		}
	}
	if !stillExists {
		t.Fatal("ChannelUpdate 后新增模型被删除")
	}
	_ = updated // updated 详情不再断言具体字段, 模型存活已验证
}

// TestSyncCommitRevisionRejectsOldToken 验证同步后旧 revision 被拒绝, 新 revision 可更新。
func TestSyncCommitRevisionRejectsOldToken(t *testing.T) {
	clearAll(t)
	chID := seedRevChannel(t, "rev-sync-reject")
	detail, _ := ChannelDetailGet(context.Background(), chID)
	oldRev := detail.Revision

	var key model.ChannelKey
	if err := db.GetDB().Where("channel_id = ?", chID).First(&key).Error; err != nil {
		t.Fatalf("读凭据失败: %v", err)
	}
	discoveries := []KeyDiscovery{{
		KeyID: key.ID,
		Models: []model.ChannelFetchModel{{
			Name:      "sync-reject-model",
			Protocols: model.ProtocolOpenAIChatCompletion,
		}},
	}}
	mutation, _, err := ChannelSyncApplyDiscovery(db.GetDB(), chID, discoveries)
	if err != nil {
		t.Fatalf("apply 失败: %v", err)
	}
	_, err = ChannelSyncCommitAndRefresh(context.Background(), chID, mutation)
	if err != nil {
		t.Fatalf("commit 失败: %v", err)
	}
	// 同步后新增的模型仍在 DB。
	var cm model.ChannelModel
	if err := db.GetDB().Where("channel_id = ? AND name = ?", chID, "sync-reject-model").First(&cm).Error; err != nil {
		t.Fatal("同步新增的模型应在 DB 中存在")
	}
	// 旧 revision 更新应失败(CAS)。
	detail2, _ := ChannelDetailGet(context.Background(), chID)
	detail2.Revision = oldRev
	_, _, err = ChannelUpdate(&detail2, context.Background())
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("旧 revision 应被拒绝, got %v", err)
	}
	// 同步新增的模型仍存在(旧 token 更新未删除它)。
	if err := db.GetDB().Where("channel_id = ? AND name = ?", chID, "sync-reject-model").First(&cm).Error; err != nil {
		t.Fatal("旧 token 更新失败后, 同步新增的模型不应被删除")
	}
	// 新 revision 更新应成功。
	detail3, _ := ChannelDetailGet(context.Background(), chID)
	_, _, err = ChannelUpdate(&detail3, context.Background())
	if err != nil {
		t.Fatalf("新 revision 更新应成功, got %v", err)
	}
}

// TestChildFailureRollbackPreservesConfigAndRevision 验证 ChannelUpdate 事务回滚不改变配置与 revision。
func TestChildFailureRollbackPreservesConfigAndRevision(t *testing.T) {
	clearAll(t)
	chID := seedRevChannel(t, "rev-rollback")
	detail, _ := ChannelDetailGet(context.Background(), chID)
	beforeRev := detail.Revision
	beforeBaseURL := detail.BaseURL

	// 构造一个会使 syncChannelGrants 失败的 detail: grant 引用不存在的 key name。
	detail.Grants = append(detail.Grants, model.ChannelGrantConfig{
		ModelName: "m",
		KeyName:   "nonexistent-key",
		Protocols: model.ProtocolOpenAIChatCompletion,
	})
	_, _, err := ChannelUpdate(&detail, context.Background())
	if err == nil {
		t.Fatal("引用不存在的 key name 应失败")
	}
	// 回滚后配置与 revision 不变。
	detail2, _ := ChannelDetailGet(context.Background(), chID)
	if detail2.Revision != beforeRev {
		t.Fatalf("事务回滚后 revision 应不变: %q -> %q", beforeRev, detail2.Revision)
	}
	if detail2.BaseURL != beforeBaseURL {
		t.Fatalf("事务回滚后配置应不变: %q -> %q", beforeBaseURL, detail2.BaseURL)
	}
}

// TestImportRotatesExistingChannelOnKeyChange 验证导入改变了已存在渠道的凭据内容时轮转 revision。
func TestImportRotatesExistingChannelOnKeyChange(t *testing.T) {
	clearAll(t)
	chID := seedRevChannel(t, "rev-import-key")
	detail, _ := ChannelDetailGet(context.Background(), chID)
	beforeRev := detail.Revision

	dump, err := DBExportAll(context.Background())
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	// 修改导出中该渠道的 key secret: 导入 upsert 会覆盖。
	for i := range dump.ChannelKeys {
		if dump.ChannelKeys[i].ChannelID == chID {
			dump.ChannelKeys[i].Key = "changed-secret"
		}
	}
	_, err = DBImportIncremental(context.Background(), dump)
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	var dbChannel model.Channel
	if err := db.GetDB().Where("id = ?", chID).First(&dbChannel).Error; err != nil {
		t.Fatalf("读 DB 渠道失败: %v", err)
	}
	if dbChannel.Revision == beforeRev {
		t.Fatal("key 内容变化后 revision 应轮转")
	}
}

// TestImportNoRotateIdenticalChannel 验证相同内容的导入不轮转 revision。
func TestImportNoRotateIdenticalChannel(t *testing.T) {
	clearAll(t)
	chID := seedRevChannel(t, "rev-import-identical")
	detail, _ := ChannelDetailGet(context.Background(), chID)
	beforeRev := detail.Revision

	dump, err := DBExportAll(context.Background())
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	// 不修改任何内容, 直接导入。
	_, err = DBImportIncremental(context.Background(), dump)
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	var dbChannel model.Channel
	if err := db.GetDB().Where("id = ?", chID).First(&dbChannel).Error; err != nil {
		t.Fatalf("读 DB 渠道失败: %v", err)
	}
	if dbChannel.Revision != beforeRev {
		t.Fatalf("相同内容导入不应轮转 revision: %q -> %q", beforeRev, dbChannel.Revision)
	}
}

// TestImportNoRotateStatOnly 验证仅统计变更的导入不轮转 revision。
func TestImportNoRotateStatOnly(t *testing.T) {
	clearAll(t)
	chID := seedRevChannel(t, "rev-import-stat")
	detail, _ := ChannelDetailGet(context.Background(), chID)
	beforeRev := detail.Revision

	dump, err := DBExportAll(context.Background())
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	// 仅修改统计, 不改可编辑状态。
	for i := range dump.Channels {
		if dump.Channels[i].ID == chID {
			dump.Channels[i].InputToken = 99999
			dump.Channels[i].OutputToken = 88888
		}
	}
	_, err = DBImportIncremental(context.Background(), dump)
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	var dbChannel model.Channel
	if err := db.GetDB().Where("id = ?", chID).First(&dbChannel).Error; err != nil {
		t.Fatalf("读 DB 渠道失败: %v", err)
	}
	if dbChannel.Revision != beforeRev {
		t.Fatalf("纯统计导入不应轮转 revision: %q -> %q", beforeRev, dbChannel.Revision)
	}
}

// TestImportDeleteReimportFreshTokenRejectsOld 验证删除后重新导入同 ID 得到新令牌, 旧令牌无效。
func TestImportDeleteReimportFreshTokenRejectsOld(t *testing.T) {
	clearAll(t)
	chID := seedRevChannel(t, "rev-reimport")
	detail, _ := ChannelDetailGet(context.Background(), chID)
	oldRev := detail.Revision

	dump, err := DBExportAll(context.Background())
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	// 删除渠道。
	_, err = ChannelDel(chID, context.Background())
	if err != nil {
		t.Fatalf("删除渠道失败: %v", err)
	}
	// 重新导入同 ID: createDoNothing 插入新行(渠道已删除), 分配新 UUID。
	_, err = DBImportIncremental(context.Background(), dump)
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	// 导入后应能用新 revision 更新。
	reloadAllCache(t)
	detail2, _ := ChannelDetailGet(context.Background(), chID)
	if detail2.Revision == oldRev {
		t.Fatal("重新导入后 revision 应为新 UUID, 非旧值")
	}
	// 旧令牌更新应失败。
	detail2.Revision = oldRev
	_, _, err = ChannelUpdate(&detail2, context.Background())
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("旧 revision 应被拒绝, got %v", err)
	}
}

// TestImportRotatesExistingChannelOnModelChange 验证导入改变了已存在渠道的模型名称时轮转 revision。
func TestImportRotatesExistingChannelOnModelChange(t *testing.T) {
	clearAll(t)
	chID, _ := seedChannel(t, "rev-import-model", true, []string{"m"}, []keySpec{{"k", true}})
	reloadAllCache(t)
	detail, _ := ChannelDetailGet(context.Background(), chID)
	beforeRev := detail.Revision

	dump, err := DBExportAll(context.Background())
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	// 修改导出中该渠道的模型名: 导入 upsert 按 ID 覆盖, 模型名会变。
	for i := range dump.ChannelModels {
		if dump.ChannelModels[i].ChannelID == chID && dump.ChannelModels[i].Name == "m" {
			dump.ChannelModels[i].Name = "changed-m"
		}
	}
	_, err = DBImportIncremental(context.Background(), dump)
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	var dbChannel model.Channel
	if err := db.GetDB().Where("id = ?", chID).First(&dbChannel).Error; err != nil {
		t.Fatalf("读 DB 渠道失败: %v", err)
	}
	if dbChannel.Revision == beforeRev {
		t.Fatal("模型名变化后 revision 应轮转")
	}
}

// TestImportCrossOwnerKeyPKTransferRotatesBoth 验证:
// 导入把已有 key 的 channel_id 从 A 改到 B(PK 不变, 归属迁移),
// 两个渠道的可编辑状态都变化, 两者 revision 都轮转。
func TestImportCrossOwnerKeyPKTransferRotatesBoth(t *testing.T) {
	clearAll(t)
	chA, _ := seedChannel(t, "owner-a", true, nil, []keySpec{{"ka", true}})
	chB, _ := seedChannel(t, "owner-b", true, nil, []keySpec{{"kb", true}})
	reloadAllCache(t)
	detailA, _ := ChannelDetailGet(context.Background(), chA)
	detailB, _ := ChannelDetailGet(context.Background(), chB)
	revA := detailA.Revision
	revB := detailB.Revision

	dump, err := DBExportAll(context.Background())
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	// 把 A 的 key 的 channel_id 改为 B: upsert 按 PK 覆盖, key 从 A 迁移到 B。
	for i := range dump.ChannelKeys {
		if dump.ChannelKeys[i].ChannelID == chA {
			dump.ChannelKeys[i].ChannelID = chB
		}
	}
	if _, err := DBImportIncremental(context.Background(), dump); err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	var dbA, dbB model.Channel
	db.GetDB().Where("id = ?", chA).First(&dbA)
	db.GetDB().Where("id = ?", chB).First(&dbB)
	if dbA.Revision == revA {
		t.Fatal("A 丢失 key 后 revision 应轮转")
	}
	if dbB.Revision == revB {
		t.Fatal("B 获得 key 后 revision 应轮转")
	}
}

// TestImportCrossOwnerModelPKTransferRotatesBoth 验证:
// 导入把已有 model 的 channel_id 从 A 改到 B, 两者 revision 都轮转。
func TestImportCrossOwnerModelPKTransferRotatesBoth(t *testing.T) {
	clearAll(t)
	chA, _ := seedChannel(t, "model-owner-a", true, []string{"shared"}, []keySpec{{"ka", true}})
	chB, _ := seedChannel(t, "model-owner-b", true, nil, []keySpec{{"kb", true}})
	reloadAllCache(t)
	detailA, _ := ChannelDetailGet(context.Background(), chA)
	detailB, _ := ChannelDetailGet(context.Background(), chB)
	revA := detailA.Revision
	revB := detailB.Revision

	dump, err := DBExportAll(context.Background())
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	for i := range dump.ChannelModels {
		if dump.ChannelModels[i].ChannelID == chA {
			dump.ChannelModels[i].ChannelID = chB
		}
	}
	// grant 的 channel_model_id 仍指向原 model PK, model 现在归属 B。
	// A 丢失 model+grant, B 获得 model(grant 仍指向同一 model PK, 属于 B)。
	if _, err := DBImportIncremental(context.Background(), dump); err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	var dbA, dbB model.Channel
	db.GetDB().Where("id = ?", chA).First(&dbA)
	db.GetDB().Where("id = ?", chB).First(&dbB)
	if dbA.Revision == revA {
		t.Fatal("A 丢失 model 后 revision 应轮转")
	}
	if dbB.Revision == revB {
		t.Fatal("B 获得 model 后 revision 应轮转")
	}
}

// TestImportGrantProtocolChangeRotatesOwner 验证:
// 导入修改已有 grant 的 protocols 位, 所属渠道的可编辑状态变化, revision 轮转。
func TestImportGrantProtocolChangeRotatesOwner(t *testing.T) {
	clearAll(t)
	chID, _ := seedChannel(t, "grant-proto", true, []string{"m"}, []keySpec{{"k", true}})
	reloadAllCache(t)
	detail, _ := ChannelDetailGet(context.Background(), chID)
	beforeRev := detail.Revision

	dump, err := DBExportAll(context.Background())
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	// 修改 grant 的 protocols: 原为 OpenAIChatCompletion(2), 改为 OpenAIResponse(4)。
	for i := range dump.ChannelGrants {
		dump.ChannelGrants[i].Protocols = model.ProtocolOpenAIResponse
	}
	if _, err := DBImportIncremental(context.Background(), dump); err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	var dbChannel model.Channel
	db.GetDB().Where("id = ?", chID).First(&dbChannel)
	if dbChannel.Revision == beforeRev {
		t.Fatal("grant protocols 变化后 revision 应轮转")
	}
}

// TestChannelCreateExplicitDisabledStaysDisabled 验证 GORM default:true 不覆盖 explicit false:
// 创建时 Enabled=false 的渠道在 DB、返回 detail 和缓存中都保持 false。
func TestChannelCreateExplicitDisabledStaysDisabled(t *testing.T) {
	clearAll(t)
	detail := model.ChannelDetail{
		ChannelConfig: model.ChannelConfig{
			Name:           "explicit-disabled",
			Enabled:        false,
			BaseURL:        "http://disabled.example",
			AutoSyncModels: true,
		},
		Keys:   []model.ChannelKeyConfig{{Name: "k", Key: "sk", Enabled: false}},
		Models: []string{"m"},
		Grants: []model.ChannelGrantConfig{{ModelName: "m", KeyName: "k", Protocols: model.ProtocolOpenAIChatCompletion}},
	}
	created, _, err := ChannelCreate(&detail, context.Background())
	if err != nil {
		t.Fatalf("创建渠道失败: %v", err)
	}
	// 返回的 detail 应保持 false。
	if created.Enabled {
		t.Fatal("返回 detail Enabled 应为 false, GORM default:true 覆盖了 explicit false")
	}
	for _, k := range created.Keys {
		if k.Enabled {
			t.Fatal("返回 detail key Enabled 应为 false")
		}
	}
	// DB 应保持 false。
	var dbCh model.Channel
	if err := db.GetDB().Where("id = ?", created.ID).First(&dbCh).Error; err != nil {
		t.Fatalf("读 DB 渠道失败: %v", err)
	}
	if dbCh.Enabled {
		t.Fatal("DB channel Enabled 应为 false")
	}
	var dbKeys []model.ChannelKey
	if err := db.GetDB().Where("channel_id = ?", created.ID).Find(&dbKeys).Error; err != nil {
		t.Fatalf("读 DB 凭据失败: %v", err)
	}
	for _, k := range dbKeys {
		if k.Enabled {
			t.Fatalf("DB key %s Enabled 应为 false", k.Name)
		}
	}
}

// TestChannelCreateExplicitEnabledStaysEnabled 验证 Enabled=true 仍正确落库。
func TestChannelCreateExplicitEnabledStaysEnabled(t *testing.T) {
	clearAll(t)
	detail := model.ChannelDetail{
		ChannelConfig: model.ChannelConfig{
			Name:           "explicit-enabled",
			Enabled:        true,
			BaseURL:        "http://enabled.example",
			AutoSyncModels: false,
		},
		Keys: []model.ChannelKeyConfig{{Name: "k", Key: "sk", Enabled: true}},
	}
	created, _, err := ChannelCreate(&detail, context.Background())
	if err != nil {
		t.Fatalf("创建渠道失败: %v", err)
	}
	if !created.Enabled {
		t.Fatal("返回 detail Enabled 应为 true")
	}
	var dbCh model.Channel
	db.GetDB().Where("id = ?", created.ID).First(&dbCh)
	if !dbCh.Enabled {
		t.Fatal("DB channel Enabled 应为 true")
	}
}
