package channelsync

// 本文件用受控 httptest 上游按 Authorization vs X-Api-Key 记录每协议命中,
// 验证 sync status 分类、502 partial/failed 保留旧 managed 授权与模型、
// 空结果不删自动项。全部纯 Go httptest, 无 Python/exec/绝对路径/固定端口/t.Skip。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
)

// upstreamRound 配置一轮同步的上游响应。
type upstreamRound struct {
	openAIStatus    int
	openAIModels    []string
	anthropicStatus int
	anthropicModels []string
}

// controllableUpstream 是可重配置的双协议 httptest 上游。
// 按 Authorization(OpenAI 侧)与 X-Api-Key(Anthropic 侧)区分协议,
// 记录每侧命中数供本轮断言。
type controllableUpstream struct {
	srv           *httptest.Server
	cfg           atomic.Pointer[upstreamRound]
	openAIHits    atomic.Int64
	anthropicHits atomic.Int64
}

// newControllableUpstream 创建受控上游, t.Cleanup 确保关闭。
func newControllableUpstream(t *testing.T) *controllableUpstream {
	t.Helper()
	u := &controllableUpstream{}
	u.cfg.Store(&upstreamRound{
		openAIStatus:    http.StatusOK,
		anthropicStatus: http.StatusOK,
	})
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := u.cfg.Load()
		if r.Header.Get("Authorization") != "" {
			u.openAIHits.Add(1)
			writeModelsResponse(w, cfg.openAIStatus, cfg.openAIModels)
			return
		}
		if r.Header.Get("X-Api-Key") != "" {
			u.anthropicHits.Add(1)
			writeModelsResponse(w, cfg.anthropicStatus, cfg.anthropicModels)
			return
		}
		// 未携带已知鉴权头: 返回 401 防止误判协议归属。
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

// setRound 重配置上游响应供下一轮探测。
func (u *controllableUpstream) setRound(r upstreamRound) {
	u.cfg.Store(&r)
}

// writeModelsResponse 按状态码写模型列表; 非 2xx 不写 body, 空列表输出 {"data":[]}。
func writeModelsResponse(w http.ResponseWriter, status int, models []string) {
	if status != http.StatusOK {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	type item struct{ ID string }
	type list struct {
		Data []item `json:"data"`
	}
	data := list{Data: make([]item, 0, len(models))}
	for _, m := range models {
		data.Data = append(data.Data, item{ID: m})
	}
	json.NewEncoder(w).Encode(data)
}

// findManagedModel 读取渠道模型并断言 sync_managed=true。
func findManagedModel(t *testing.T, channelID int, name string) model.ChannelModel {
	t.Helper()
	var cm model.ChannelModel
	if err := db.GetDB().Where("channel_id = ? AND name = ?", channelID, name).First(&cm).Error; err != nil {
		t.Fatalf("模型 %s 不存在: %v", name, err)
	}
	if !cm.SyncManaged {
		t.Fatalf("模型 %s sync_managed 应为 true", name)
	}
	return cm
}

// findManagedGrant 读取授权并断言 sync_managed=true。
func findManagedGrant(t *testing.T, channelID int, modelName, keyName string) model.ChannelGrant {
	t.Helper()
	var grant model.ChannelGrant
	err := db.GetDB().
		Joins("JOIN channel_models ON channel_models.id = channel_grants.channel_model_id").
		Joins("JOIN channel_keys ON channel_keys.id = channel_grants.channel_key_id").
		Where("channel_models.channel_id = ? AND channel_models.name = ? AND channel_keys.name = ?",
			channelID, modelName, keyName).
		First(&grant).Error
	if err != nil {
		t.Fatalf("授权 %s+%s 不存在: %v", modelName, keyName, err)
	}
	if !grant.SyncManaged {
		t.Fatalf("授权 %s+%s sync_managed 应为 true", modelName, keyName)
	}
	return grant
}

// findGroupItemForModel 查找规则分组中引用指定模型授权的成员, 返回 (item ID, grant ID)。
func findGroupItemForModel(t *testing.T, groupName, modelName string) (int, int) {
	t.Helper()
	grp, err := op.GroupGetByName(groupName)
	if err != nil {
		t.Fatalf("分组 %s 不存在: %v", groupName, err)
	}
	for _, item := range grp.Items {
		if item.ModelName == modelName {
			return item.ID, item.ChannelGrantID
		}
	}
	t.Fatalf("分组 %s 中未找到模型 %s 的成员", groupName, modelName)
	return 0, 0
}

// waitStatusNewRound 等待渠道达到指定终态且 LastSyncAt 与上一轮不同,
// 防止误拿上一轮相同终态。
func waitStatusNewRound(t *testing.T, channelID int, prevLastSyncAt string, want ...string) model.ChannelModelSyncStatus {
	t.Helper()
	for i := 0; i < 200; i++ {
		time.Sleep(20 * time.Millisecond)
		for _, s := range GetStatus() {
			if s.ChannelID != channelID || s.LastSyncAt == nil || *s.LastSyncAt == prevLastSyncAt {
				continue
			}
			for _, w := range want {
				if s.Status == w {
					return s
				}
			}
		}
	}
	t.Fatalf("渠道 %d 新轮次终态未在 %v 中稳定", channelID, want)
	return model.ChannelModelSyncStatus{}
}

// waitWorkerIdle 等待渠道 worker 从 running 集合移除,
// 确保下一轮 StartSingle 不被 busy 拒绝。
func waitWorkerIdle(t *testing.T, channelID int) {
	t.Helper()
	for i := 0; i < 100; i++ {
		lifecycle.Lock()
		_, exists := running[channelID]
		lifecycle.Unlock()
		if !exists {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("渠道 %d worker 未退出", channelID)
}

// mustStartSingle 启动单渠道同步, 失败即 fatal。
func mustStartSingle(t *testing.T, round string, chID int) {
	t.Helper()
	started, _, _, err := StartSingle(chID)
	if err != nil {
		t.Fatalf("%s StartSingle 错误: %v", round, err)
	}
	if !started {
		t.Fatalf("%s StartSingle 未启动", round)
	}
}

// assertHitDelta 断言上游命中增量。
func assertHitDelta(t *testing.T, round, proto string, got, want int64) {
	t.Helper()
	if got != want {
		t.Errorf("%s %s 命中 %d, want %d", round, proto, got, want)
	}
}

// TestVerifyManagedPreservationAcrossRounds 用受控 httptest 上游验证四轮同步的 managed 保留语义。
// 每轮按 Authorization vs X-Api-Key 记录协议命中, 读取 DB 显式断言 sync_managed=true 与 ID 不变。
// 第1轮: 两侧都返回 model-A → success, managed model/grant/group item 建立。
// 第2轮: OpenAI 返回 model-B, Anthropic 502 → partial, B 新增, A 各项 ID 不变。
// 第3轮: 两侧都 502 → failed, A/B 自动项均保留。
// 第4轮: 两侧都 200 空 → success, A/B 自动项不被空结果删除。
func TestVerifyManagedPreservationAcrossRounds(t *testing.T) {
	clearTables(t)
	upstream := newControllableUpstream(t)
	chID := seedChannel(t, "verify-managed", upstream.srv.URL, "sk-verify", true)

	// 规则分组匹配 model-A, 同步后自动补入。
	if _, err := op.GroupCreate(&model.GroupCreateRequest{
		Name: "verify-rule", Mode: model.GroupModeScored, AutoAddPattern: "^model-A$",
	}, context.Background()); err != nil {
		t.Fatalf("建分组失败: %v", err)
	}

	var prevLastSyncAt string
	var modelAID, grantAID, groupItemAID int

	// --- 第 1 轮: 两侧协议都返回 model-A → success ---
	upstream.setRound(upstreamRound{
		openAIStatus:    http.StatusOK,
		openAIModels:    []string{"model-A"},
		anthropicStatus: http.StatusOK,
		anthropicModels: []string{"model-A"},
	})
	openAIPrev := upstream.openAIHits.Load()
	anthropicPrev := upstream.anthropicHits.Load()
	mustStartSingle(t, "第1轮", chID)
	s1 := waitStatusNewRound(t, chID, prevLastSyncAt, "success")
	if s1.Status != "success" {
		t.Fatalf("第1轮 status=%s, want success", s1.Status)
	}
	prevLastSyncAt = *s1.LastSyncAt
	assertHitDelta(t, "第1轮", "OpenAI", upstream.openAIHits.Load()-openAIPrev, 1)
	assertHitDelta(t, "第1轮", "Anthropic", upstream.anthropicHits.Load()-anthropicPrev, 1)
	// 读取 DB: model-A managed=true, grant managed=true。
	ma := findManagedModel(t, chID, "model-A")
	modelAID = ma.ID
	ga := findManagedGrant(t, chID, "model-A", "default")
	grantAID = ga.ID
	// 规则分组应补入 model-A 的授权。
	gid, ggid := findGroupItemForModel(t, "verify-rule", "model-A")
	groupItemAID = gid
	if ggid != grantAID {
		t.Fatalf("第1轮 group item 引用 grant %d, 但 DB grant ID 为 %d", ggid, grantAID)
	}

	// --- 第 2 轮: OpenAI 返回 model-B, Anthropic 502 → partial ---
	waitWorkerIdle(t, chID)
	upstream.setRound(upstreamRound{
		openAIStatus:    http.StatusOK,
		openAIModels:    []string{"model-B"},
		anthropicStatus: http.StatusBadGateway,
	})
	openAIPrev = upstream.openAIHits.Load()
	anthropicPrev = upstream.anthropicHits.Load()
	mustStartSingle(t, "第2轮", chID)
	s2 := waitStatusNewRound(t, chID, prevLastSyncAt, "partial")
	if s2.Status != "partial" {
		t.Fatalf("第2轮 status=%s, want partial", s2.Status)
	}
	prevLastSyncAt = *s2.LastSyncAt
	assertHitDelta(t, "第2轮", "OpenAI", upstream.openAIHits.Load()-openAIPrev, 1)
	assertHitDelta(t, "第2轮", "Anthropic", upstream.anthropicHits.Load()-anthropicPrev, 1)
	// model-B 新增且 managed=true。
	findManagedModel(t, chID, "model-B")
	findManagedGrant(t, chID, "model-B", "default")
	// model-A 仍在, ID 不变, managed=true。
	ma2 := findManagedModel(t, chID, "model-A")
	if ma2.ID != modelAID {
		t.Errorf("第2轮 model-A ID=%d, 第1轮=%d (应不变)", ma2.ID, modelAID)
	}
	// grant-A 仍在, ID 不变, managed=true。
	ga2 := findManagedGrant(t, chID, "model-A", "default")
	if ga2.ID != grantAID {
		t.Errorf("第2轮 grant-A ID=%d, 第1轮=%d (应不变)", ga2.ID, grantAID)
	}
	// 分组成员仍在, item ID 与引用的 grant ID 均不变。
	gid2, ggid2 := findGroupItemForModel(t, "verify-rule", "model-A")
	if gid2 != groupItemAID {
		t.Errorf("第2轮 group item ID=%d, 第1轮=%d (应不变)", gid2, groupItemAID)
	}
	if ggid2 != grantAID {
		t.Errorf("第2轮 group item grant=%d, 应为 %d", ggid2, grantAID)
	}

	// --- 第 3 轮: 两侧都 502 → failed, 保留 A/B ---
	waitWorkerIdle(t, chID)
	upstream.setRound(upstreamRound{
		openAIStatus:    http.StatusBadGateway,
		anthropicStatus: http.StatusBadGateway,
	})
	openAIPrev = upstream.openAIHits.Load()
	anthropicPrev = upstream.anthropicHits.Load()
	mustStartSingle(t, "第3轮", chID)
	s3 := waitStatusNewRound(t, chID, prevLastSyncAt, "failed")
	if s3.Status != "failed" {
		t.Fatalf("第3轮 status=%s, want failed", s3.Status)
	}
	prevLastSyncAt = *s3.LastSyncAt
	assertHitDelta(t, "第3轮", "OpenAI", upstream.openAIHits.Load()-openAIPrev, 1)
	assertHitDelta(t, "第3轮", "Anthropic", upstream.anthropicHits.Load()-anthropicPrev, 1)
	// A 和 B 都仍在, managed=true。
	findManagedModel(t, chID, "model-A")
	findManagedModel(t, chID, "model-B")
	findManagedGrant(t, chID, "model-A", "default")
	findManagedGrant(t, chID, "model-B", "default")

	// --- 第 4 轮: 两侧都 200 空 → success, 不删自动项 ---
	waitWorkerIdle(t, chID)
	upstream.setRound(upstreamRound{
		openAIStatus:    http.StatusOK,
		openAIModels:    []string{},
		anthropicStatus: http.StatusOK,
		anthropicModels: []string{},
	})
	openAIPrev = upstream.openAIHits.Load()
	anthropicPrev = upstream.anthropicHits.Load()
	mustStartSingle(t, "第4轮", chID)
	s4 := waitStatusNewRound(t, chID, prevLastSyncAt, "success")
	if s4.Status != "success" {
		t.Fatalf("第4轮 status=%s, want success", s4.Status)
	}
	assertHitDelta(t, "第4轮", "OpenAI", upstream.openAIHits.Load()-openAIPrev, 1)
	assertHitDelta(t, "第4轮", "Anthropic", upstream.anthropicHits.Load()-anthropicPrev, 1)
	// A 和 B 都仍在, 不被空结果删除。
	findManagedModel(t, chID, "model-A")
	findManagedModel(t, chID, "model-B")
	findManagedGrant(t, chID, "model-A", "default")
	findManagedGrant(t, chID, "model-B", "default")
}
