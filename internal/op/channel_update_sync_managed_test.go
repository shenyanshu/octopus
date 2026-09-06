package op

// 本文件验证 ChannelUpdate 编辑来源语义: sync 创建的 model/grant(sync_managed=true) 保留,
// 手动新增的 model/grant(sync_managed=false) 不被收敛为 true。协议不被收敛复用既有测试。

import (
	"context"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
)

// TestChannelUpdatePreservesSyncManaged 验证: sync 创建的 model/grant(sync_managed=true) 经 ChannelUpdate 保留,
// 手动新增的 model/grant 默认 sync_managed=false。
func TestChannelUpdatePreservesSyncManaged(t *testing.T) {
	clearAllForSyncDelete(t)
	chID, _ := seedChannel(t, "edit-src", true, nil, []keySpec{{"k1", true}})

	// 同步创建 model-a(sync_managed=true) + grant(sync_managed=true)。
	disc := []KeyDiscovery{{
		KeyID:  getKeyID(t, chID, "k1"),
		Models: []model.ChannelFetchModel{{Name: "model-a", Protocols: model.ProtocolOpenAIChatCompletion}},
	}}
	if _, _, err := syncDeleteApply(t, chID, disc); err != nil {
		t.Fatalf("同步失败: %v", err)
	}

	// 刷新缓存使 ChannelUpdate 可读到渠道。
	if err := InitCache(); err != nil {
		t.Fatalf("刷新缓存失败: %v", err)
	}

	// 读取当前 revision 供 ChannelUpdate 乐观锁。
	detail, err := ChannelDetailGet(context.Background(), chID)
	if err != nil {
		t.Fatalf("读取渠道失败: %v", err)
	}

	// ChannelUpdate: 保留 model-a, 新增手动 model-b + grant。
	detail.Models = []string{"model-a", "model-b"}
	detail.Grants = []model.ChannelGrantConfig{
		{ModelName: "model-a", KeyName: "k1", Protocols: model.ProtocolOpenAIChatCompletion},
		{ModelName: "model-b", KeyName: "k1", Protocols: model.ProtocolOpenAIChatCompletion},
	}
	_, _, err = ChannelUpdate(&detail, context.Background())
	if err != nil {
		t.Fatalf("ChannelUpdate 失败: %v", err)
	}

	// model-a 的 sync_managed 应保留 true。
	var cmA model.ChannelModel
	db.GetDB().Where("channel_id = ? AND name = ?", chID, "model-a").First(&cmA)
	if !cmA.SyncManaged {
		t.Error("model-a(sync 创建) 的 sync_managed 应保留 true")
	}

	// model-a 的 grant 的 sync_managed 应保留 true。
	var cgA model.ChannelGrant
	db.GetDB().Where("channel_model_id = ? AND channel_key_id = ?", cmA.ID, getKeyID(t, chID, "k1")).First(&cgA)
	if !cgA.SyncManaged {
		t.Error("model-a 的 grant(sync 创建) 的 sync_managed 应保留 true")
	}

	// model-b(手动新增) 的 sync_managed 应为 false。
	var cmB model.ChannelModel
	db.GetDB().Where("channel_id = ? AND name = ?", chID, "model-b").First(&cmB)
	if cmB.SyncManaged {
		t.Error("model-b(手动新增) 的 sync_managed 应为 false")
	}

	// model-b 的 grant 的 sync_managed 应为 false。
	var cgB model.ChannelGrant
	db.GetDB().Where("channel_model_id = ? AND channel_key_id = ?", cmB.ID, getKeyID(t, chID, "k1")).First(&cgB)
	if cgB.SyncManaged {
		t.Error("model-b 的 grant(手动新增) 的 sync_managed 应为 false")
	}
}

// TestChannelUpdateProtocolNotConverged 验证: 已有授权的协议不被 ChannelUpdate 改变(除非提交方明确改协议)。
// 复用既有协议保留语义: syncChannelGrants 仅在 protocols 不同时 UPDATE。
func TestChannelUpdateProtocolNotConverged(t *testing.T) {
	clearAllForSyncDelete(t)
	chID, _ := seedChannel(t, "edit-proto", true, nil, []keySpec{{"k1", true}})

	// 同步创建 model-a with OpenAIChatCompletion。
	disc := []KeyDiscovery{{
		KeyID:  getKeyID(t, chID, "k1"),
		Models: []model.ChannelFetchModel{{Name: "model-a", Protocols: model.ProtocolOpenAIChatCompletion}},
	}}
	if _, _, err := syncDeleteApply(t, chID, disc); err != nil {
		t.Fatalf("同步失败: %v", err)
	}

	if err := InitCache(); err != nil {
		t.Fatalf("刷新缓存失败: %v", err)
	}

	detail, _ := ChannelDetailGet(context.Background(), chID)

	// ChannelUpdate 提交相同协议: 不应触发 UPDATE。
	detail.Models = []string{"model-a"}
	detail.Grants = []model.ChannelGrantConfig{
		{ModelName: "model-a", KeyName: "k1", Protocols: model.ProtocolOpenAIChatCompletion},
	}
	if _, _, err := ChannelUpdate(&detail, context.Background()); err != nil {
		t.Fatalf("ChannelUpdate 失败: %v", err)
	}

	// 验证协议未变。
	var cm model.ChannelModel
	db.GetDB().Where("channel_id = ? AND name = ?", chID, "model-a").First(&cm)
	var cg model.ChannelGrant
	db.GetDB().Where("channel_model_id = ?", cm.ID).First(&cg)
	if cg.Protocols != model.ProtocolOpenAIChatCompletion {
		t.Errorf("协议不应被改变, got %d", cg.Protocols)
	}
}
