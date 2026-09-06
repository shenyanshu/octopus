package op

import (
	"fmt"
	"reflect"
	"sort"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// channelCanonicalState 是渠道的可编辑状态快照, 用于导入前后比较以决定是否轮转 revision。
// 排除统计、revision、主键等不可编辑字段, 只保留用户可编辑的配置与子表内容。
// 所有集合字段经过稳定排序, 使比较结果与遍历顺序无关。
type channelCanonicalState struct {
	config model.ChannelConfig
	keys   []keyCanonical
	models []string
	grants []grantCanonical
}

type keyCanonical struct {
	name    string
	key     string
	enabled bool
}

type grantCanonical struct {
	modelName string
	keyName   string
	protocols model.Protocol
}

// snapshotChannelCanonical 从 DB 读取渠道的可编辑状态。
// 基于 DB 实际数据, 不依赖 channelCache; 主键不参与比较。
func snapshotChannelCanonical(tx *gorm.DB, channelID int) (channelCanonicalState, error) {
	var ch model.Channel
	if err := tx.Where("id = ?", channelID).First(&ch).Error; err != nil {
		return channelCanonicalState{}, err
	}

	var keys []model.ChannelKey
	if err := tx.Where("channel_id = ?", channelID).Find(&keys).Error; err != nil {
		return channelCanonicalState{}, err
	}

	var models []model.ChannelModel
	if err := tx.Where("channel_id = ?", channelID).Find(&models).Error; err != nil {
		return channelCanonicalState{}, err
	}

	modelIDs := make([]int, 0, len(models))
	modelNameByID := make(map[int]string, len(models))
	for _, m := range models {
		modelIDs = append(modelIDs, m.ID)
		modelNameByID[m.ID] = m.Name
	}

	var grants []model.ChannelGrant
	if len(modelIDs) > 0 {
		if err := tx.Where("channel_model_id IN ?", modelIDs).Find(&grants).Error; err != nil {
			return channelCanonicalState{}, err
		}
	}

	keyNameByID := make(map[int]string, len(keys))
	for _, k := range keys {
		keyNameByID[k.ID] = k.Name
	}

	state := channelCanonicalState{config: ch.ChannelConfig}

	ks := make([]keyCanonical, len(keys))
	for i, k := range keys {
		ks[i] = keyCanonical{name: k.Name, key: k.Key, enabled: k.Enabled}
	}
	sort.Slice(ks, func(i, j int) bool { return ks[i].name < ks[j].name })
	state.keys = ks

	ms := make([]string, len(models))
	for i, m := range models {
		ms[i] = m.Name
	}
	sort.Strings(ms)
	state.models = ms

	gs := make([]grantCanonical, len(grants))
	for i, g := range grants {
		gs[i] = grantCanonical{
			modelName: modelNameByID[g.ChannelModelID],
			keyName:   keyNameByID[g.ChannelKeyID],
			protocols: g.Protocols,
		}
	}
	sort.Slice(gs, func(i, j int) bool {
		if gs[i].modelName != gs[j].modelName {
			return gs[i].modelName < gs[j].modelName
		}
		return gs[i].keyName < gs[j].keyName
	})
	state.grants = gs

	return state, nil
}

// channelCanonicalEqual 比较两个可编辑状态快照是否完全一致。
// ChannelConfig 含 CustomHeader 切片, 用 reflect.DeepEqual 比较; 其余按值比较。
func channelCanonicalEqual(a, b channelCanonicalState) bool {
	if !reflect.DeepEqual(a.config, b.config) {
		return false
	}
	if len(a.keys) != len(b.keys) {
		return false
	}
	for i := range a.keys {
		if a.keys[i] != b.keys[i] {
			return false
		}
	}
	if len(a.models) != len(b.models) {
		return false
	}
	for i := range a.models {
		if a.models[i] != b.models[i] {
			return false
		}
	}
	if len(a.grants) != len(b.grants) {
		return false
	}
	for i := range a.grants {
		if a.grants[i] != b.grants[i] {
			return false
		}
	}
	return true
}

// rotateImportedChannelRevisions 在导入事务内, 对可编辑状态发生变化的已存在渠道轮转 revision。
// 新插入的渠道已由导入端分配新 UUID, 不在此处理; 只处理导入前已存在的渠道。
// 统计、revision、不可编辑 ID 不参与比较, 纯统计导入不触发轮转。
func rotateImportedChannelRevisions(tx *gorm.DB, existingIDs []int, beforeStates map[int]channelCanonicalState) error {
	for _, id := range existingIDs {
		after, err := snapshotChannelCanonical(tx, id)
		if err != nil {
			return fmt.Errorf("failed to snapshot channel %d after import: %w", id, err)
		}
		before, ok := beforeStates[id]
		if !ok {
			continue
		}
		if channelCanonicalEqual(before, after) {
			continue
		}
		if err := tx.Model(&model.Channel{}).Where("id = ?", id).
			Update("revision", uuid.NewString()).Error; err != nil {
			return fmt.Errorf("failed to rotate revision for changed channel %d: %w", id, err)
		}
	}
	return nil
}
