package op

import (
	"context"
	"fmt"
	"reflect"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ChannelSyncSnapshot 是渠道同步在事务外读取的配置快照。
// 网络探测在写锁外进行, 探测完成后在写锁下重新读取最新配置并与快照比较,
// 若渠道被删除或关键配置变更则丢弃陈旧结果。
type ChannelSyncSnapshot struct {
	ChannelID   int
	Revision    string // 写锁外探测前的渠道版本令牌; apply 前重新读到的 revision 若不同, 说明配置被改, 丢弃陈旧结果。
	Config      model.ChannelConfig
	EnabledKeys []ChannelSyncKey
}

// ChannelSyncKey 是快照中的一条启用凭据。
type ChannelSyncKey struct {
	ID  int
	Key string
}

// KeyDiscovery 是单条凭据探测后的结果, 供 ApplyDiscovery 逐条应用。
type KeyDiscovery struct {
	KeyID   int
	Models  []model.ChannelFetchModel
	Partial bool // 单侧协议失败时为 true, 两侧成功为 false, 全失败由 Err 表达。
	Err     error
}

// SyncAdditions 是本次同步实际新增的模型与授权数量, 在 INSERT 处统计。
type SyncAdditions struct {
	AddedModels int
	AddedGrants int
}

// ChannelSyncReadSnapshot 读取渠道当前配置与启用凭据, 供事务外探测使用。
// 网络在写锁外进行: 读取此刻的配置与凭据, 探测完成后在写锁下重新读并比较。
// 仅从 DB 读取: 不混用缓存(可能 stale), DB 是写锁内重新验证的唯一可信来源。
func ChannelSyncReadSnapshot(channelID int) (ChannelSyncSnapshot, error) {
	var channel model.Channel
	if err := db.GetDB().Where("id = ?", channelID).First(&channel).Error; err != nil {
		return ChannelSyncSnapshot{}, fmt.Errorf("channel not found: %w", err)
	}
	var keys []model.ChannelKey
	if err := db.GetDB().Where("channel_id = ? AND enabled = ?", channelID, true).Find(&keys).Error; err != nil {
		return ChannelSyncSnapshot{}, fmt.Errorf("failed to load enabled keys: %w", err)
	}
	syncKeys := make([]ChannelSyncKey, 0, len(keys))
	for _, k := range keys {
		syncKeys = append(syncKeys, ChannelSyncKey{ID: k.ID, Key: k.Key})
	}
	return ChannelSyncSnapshot{
		ChannelID:   channelID,
		Revision:    channel.Revision,
		Config:      channel.ChannelConfig,
		EnabledKeys: syncKeys,
	}, nil
}

// ChannelSyncApplyDiscovery 在写锁内将探测发现的模型与授权增量应用到渠道。
// 只创建尚不存在的模型与授权: 已有模型不删, 已有授权不改协议。
// 每条凭据的发现结果只授权给该凭据, 不交叉授权其他凭据。
// 成功后在同一事务内触发规则分组补齐并返回 mutation, 供调用方发布 SSE。
// 返回的 SyncAdditions 在 INSERT 处统计, 确保计数准确。
func ChannelSyncApplyDiscovery(tx *gorm.DB, channelID int, discoveries []KeyDiscovery) (*ChannelMutation, SyncAdditions, error) {
	before, err := channelGroupMembersSnapshot(tx, channelID)
	if err != nil {
		return nil, SyncAdditions{}, err
	}
	additions := SyncAdditions{}
	for _, disc := range discoveries {
		if disc.Err != nil || len(disc.Models) == 0 {
			continue
		}
		key, err := resolveChannelKey(tx, channelID, disc.KeyID)
		if err != nil {
			return nil, SyncAdditions{}, fmt.Errorf("failed to resolve key %d: %w", disc.KeyID, err)
		}
		for _, m := range disc.Models {
			modelID, created, err := ensureChannelModel(tx, channelID, m.Name)
			if err != nil {
				return nil, SyncAdditions{}, fmt.Errorf("failed to ensure model %s: %w", m.Name, err)
			}
			if created {
				additions.AddedModels++
			}
			grantCreated, err := ensureChannelGrant(tx, modelID, key.ID, m.Protocols)
			if err != nil {
				return nil, SyncAdditions{}, fmt.Errorf("failed to ensure grant for %s: %w", m.Name, err)
			}
			if grantCreated {
				additions.AddedGrants++
			}
		}
	}
	// 同步成功后轮转渠道版本令牌: 进行中提交了 expected revision 的全量更新将因令牌过期而 CAS 失败,
	// 避免覆盖刚写入的模型/授权。新增了模型或授权即改变可编辑状态, 都轮转;
	// 空同步不改令牌, 避免无谓的并发更新失败。
	if additions.AddedModels > 0 || additions.AddedGrants > 0 {
		if err := tx.Model(&model.Channel{}).Where("id = ?", channelID).
			Update("revision", uuid.NewString()).Error; err != nil {
			return nil, SyncAdditions{}, fmt.Errorf("failed to rotate channel revision: %w", err)
		}
	}
	// 无新增授权不补齐分组: 全失败或空成功不触发已有匹配授权的补齐。
	if additions.AddedGrants == 0 {
		return nil, additions, nil
	}
	if err := supplementGroupsForChannel(tx, channelID); err != nil {
		return nil, SyncAdditions{}, fmt.Errorf("failed to supplement groups: %w", err)
	}
	after, err := channelGroupMembersSnapshot(tx, channelID)
	if err != nil {
		return nil, SyncAdditions{}, err
	}
	mutation, err := committedChannelMutationFromTx(tx, before, after)
	return mutation, additions, err
}

// ChannelSyncCommitAndRefresh 仿 ChannelUpdate: 事务已提交后刷新渠道子表缓存与分组缓存,
// 任一失败返回 PostCommitError 携带已提交 mutation 与 additions。
// 调用方必须在 groupGate 写锁内调用: cache/route/SSE 全序列在写锁内完成。
//
// 先发布已提交的 parent revision 到 channelCache, 再刷新子表与分组。
// 事务内已轮转 DB revision, 但 channelCache 仍持旧令牌: 若不发布,
// ChannelDetailGet 会返回过期值使后续 CAS 全量更新全部失败。
// 从 DB 读实际已提交的 revision, 保留缓存的实时统计; DB 读失败说明事务可能未提交,
// 不能伪造令牌, 返回 PostCommitError。
// 后续 child/group 刷新失败时已提交 revision 已发布, 令牌事实不丢。
func ChannelSyncCommitAndRefresh(ctx context.Context, channelID int, mutation *ChannelMutation) (*ChannelMutation, error) {
	// 先重载子缓存(keys/models/grants); 失败时不发布 parent revision 到缓存,
	// 但 DB 内的提交(含 revision 轮转与新增项)已经落库, ChannelDetailGet 仍可读到一致状态。
	if err := reloadChannelChildren(ctx, channelID); err != nil {
		return mutation, &PostCommitError{RefreshErr: err, Mutation: mutation}
	}
	if mutation != nil {
		if err := refreshGroupsAfterCommit(ctx); err != nil {
			return mutation, &PostCommitError{
				RefreshErr: fmt.Errorf("failed to refresh groups: %w", err),
				Mutation:   mutation,
			}
		}
	}
	// 子缓存重载成功后, 从 DB 读取已提交的 parent(含轮转后的 revision), 发布到 channelCache。
	// 保留缓存的实时统计(含尚未落库的累加), 仅替换 ChannelConfig 和 Revision。
	var committed model.Channel
	if err := db.GetDB().WithContext(ctx).Where("id = ?", channelID).First(&committed).Error; err != nil {
		return mutation, &PostCommitError{
			RefreshErr: fmt.Errorf("failed to read committed revision: %w", err),
			Mutation:   mutation,
		}
	}
	channelStatsNeedUpdateLock.Lock()
	cached := model.Channel{ID: channelID, ChannelConfig: committed.ChannelConfig, Revision: committed.Revision}
	if old, ok := channelCache.Get(channelID); ok {
		cached.StatsMetrics = old.StatsMetrics
	}
	channelCache.Set(channelID, cached)
	channelStatsNeedUpdateLock.Unlock()
	return mutation, nil
}

func resolveChannelKey(tx *gorm.DB, channelID, keyID int) (model.ChannelKey, error) {
	var key model.ChannelKey
	if err := tx.Where("id = ? AND channel_id = ?", keyID, channelID).First(&key).Error; err != nil {
		return model.ChannelKey{}, err
	}
	return key, nil
}

// ensureChannelModel 创建尚不存在的渠道模型, 已存在则返回其主键。
// 返回 created=true 表示本次新建。
func ensureChannelModel(tx *gorm.DB, channelID int, modelName string) (id int, created bool, err error) {
	var existing model.ChannelModel
	err = tx.Where("channel_id = ? AND name = ?", channelID, modelName).First(&existing).Error
	if err == nil {
		return existing.ID, false, nil
	}
	if err != gorm.ErrRecordNotFound {
		return 0, false, err
	}
	newModel := model.ChannelModel{ChannelID: channelID, Name: modelName}
	if err := tx.Create(&newModel).Error; err != nil {
		return 0, false, err
	}
	return newModel.ID, true, nil
}

// ensureChannelGrant 创建尚不存在的授权, 已存在则不修改协议。
// 返回 created=true 表示本次新建。
func ensureChannelGrant(tx *gorm.DB, modelID, keyID int, protocols model.Protocol) (bool, error) {
	var existing model.ChannelGrant
	err := tx.Where("channel_model_id = ? AND channel_key_id = ?", modelID, keyID).First(&existing).Error
	if err == nil {
		return false, nil
	}
	if err != gorm.ErrRecordNotFound {
		return false, err
	}
	grant := model.ChannelGrant{
		ChannelModelID: modelID,
		ChannelKeyID:   keyID,
		Protocols:      protocols,
	}
	if err := tx.Create(&grant).Error; err != nil {
		return false, err
	}
	return true, nil
}

// ChannelSyncConfigUnchanged 比较快照与当前配置, 判断探测结果是否仍然有效。
// 渠道删除、版本令牌变更、BaseURL 变更、凭据替换或禁用、AutoSyncModels 关闭均使结果陈旧, 应丢弃。
// 自动触发时必须验证 AutoSyncModels 仍为 true; 手动触发不强制(auto_sync=false 也可手动同步)。
func ChannelSyncConfigUnchanged(snapshot ChannelSyncSnapshot, current ChannelSyncSnapshot, currentKeys []model.ChannelKey, requireAutoSync bool) bool {
	// 版本令牌不同: 渠道被并发编辑/删除重建/导入覆盖, 探测结果基于旧配置, 丢弃。
	if snapshot.Revision != current.Revision {
		return false
	}
	cfg := current.Config
	if cfg.Enabled != snapshot.Config.Enabled ||
		cfg.BaseURL != snapshot.Config.BaseURL ||
		cfg.OpenAIResponsePath != snapshot.Config.OpenAIResponsePath ||
		cfg.AnthropicMessagePath != snapshot.Config.AnthropicMessagePath ||
		cfg.MatchRegex != snapshot.Config.MatchRegex ||
		!reflect.DeepEqual(snapshot.Config.CustomHeader, cfg.CustomHeader) ||
		cfg.ChannelProxy != snapshot.Config.ChannelProxy ||
		cfg.Proxy != snapshot.Config.Proxy {
		return false
	}
	if requireAutoSync && !cfg.AutoSyncModels {
		return false
	}
	if len(snapshot.EnabledKeys) != len(currentKeys) {
		return false
	}
	snapshotKeyMap := make(map[int]string, len(snapshot.EnabledKeys))
	for _, k := range snapshot.EnabledKeys {
		snapshotKeyMap[k.ID] = k.Key
	}
	for _, k := range currentKeys {
		v, ok := snapshotKeyMap[k.ID]
		if !ok || v != k.Key || !k.Enabled {
			return false
		}
	}
	return true
}
