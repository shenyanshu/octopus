package op

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/utils/cache"
	"github.com/charmbracelet/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

var (
	channelCache      = cache.New[int, model.Channel](16)      // 渠道配置的进程内副本。
	channelKeyCache   = cache.New[int, model.ChannelKey](16)   // 渠道凭据的进程内副本。
	channelModelCache = cache.New[int, model.ChannelModel](16) // 渠道模型的进程内副本。
	channelGrantCache = cache.New[int, model.ChannelGrant](16) // 渠道授权的进程内副本。
)

// 已定义的全部协议位, 用于校验提交的协议掩码。
const definedProtocols = model.ProtocolOpenAIChatCompletion | model.ProtocolOpenAIResponse | model.ProtocolAnthropicMessage

// ChannelDetailGet 返回指定渠道的完整配置, 供编辑表单读取。
// 直接从 DB 组装: revision 与子表状态来自同一提交快照, 不混用可能 stale 的 channelCache。
// 自身不加 groupGate 读锁: op 不依赖 relay, 调用方(handler)在持有读/写锁期间调用以保证一致性。
// 缺失渠道返回 gorm.ErrRecordNotFound, handler 据此返回 404。
func ChannelDetailGet(ctx context.Context, id int) (model.ChannelDetail, error) {
	conn := db.GetDB().WithContext(ctx)
	var channel model.Channel
	if err := conn.Where("id = ?", id).First(&channel).Error; err != nil {
		return model.ChannelDetail{}, err
	}
	var keys []model.ChannelKey
	if err := conn.Where("channel_id = ?", id).Find(&keys).Error; err != nil {
		return model.ChannelDetail{}, fmt.Errorf("failed to load channel keys: %w", err)
	}
	var models []model.ChannelModel
	if err := conn.Where("channel_id = ?", id).Find(&models).Error; err != nil {
		return model.ChannelDetail{}, fmt.Errorf("failed to load channel models: %w", err)
	}
	modelIDs := make([]int, 0, len(models))
	for _, m := range models {
		modelIDs = append(modelIDs, m.ID)
	}
	var grants []model.ChannelGrant
	if len(modelIDs) > 0 {
		if err := conn.Where("channel_model_id IN ?", modelIDs).Find(&grants).Error; err != nil {
			return model.ChannelDetail{}, fmt.Errorf("failed to load channel grants: %w", err)
		}
	}
	return channelDetailFromRows(channel, keys, models, grants), nil
}

// ChannelStatsList 返回全部渠道及其模型的累计统计, 自带名称与启停状态, 同时充当列表页的渠道列表。
// 不带整份配置: 路径, 代理与凭据明文只在编辑时用得上, 由 ChannelDetailGet 按主键单独给出。
func ChannelStatsList() []model.ChannelStats {
	modelsByChannel := make(map[int][]model.ChannelModelStats, channelCache.Len())
	for _, channelModel := range channelModelCache.GetAll() {
		modelsByChannel[channelModel.ChannelID] = append(modelsByChannel[channelModel.ChannelID], model.ChannelModelStats{
			ModelID:      channelModel.ID,
			ModelName:    channelModel.Name,
			StatsMetrics: channelModel.StatsMetrics,
		})
	}
	stats := make([]model.ChannelStats, 0, channelCache.Len())
	for _, channel := range channelCache.GetAll() {
		models := modelsByChannel[channel.ID]
		if models == nil {
			models = []model.ChannelModelStats{}
		}
		stats = append(stats, model.ChannelStats{
			ChannelID:    channel.ID,
			ChannelName:  channel.Name,
			Enabled:      channel.Enabled,
			Models:       models,
			StatsMetrics: channel.StatsMetrics,
		})
	}
	return stats
}

// GroupMembersDelta 描述一次已提交的渠道变更对单个分组成员集合的净影响。
type GroupMembersDelta struct {
	GroupID    int               // 成员集合发生变化的分组。
	ItemIDs    []int             // 提交后该分组仍存活的成员主键(含本次新增), 供路由状态按最新成员校正。
	AddedItems []model.GroupItem // 本次事务新增成员的完整行, 供缓存刷新失败时补发; 纯删除时为空。
	Removed    bool              // 是否有成员被删除; 纯新增不得前进路由代数, 免得误丢无关在途记账。
}

// ChannelMutation 汇集渠道写操作已提交的级联事实。
// 凭据/模型/授权的删除经外键级联直接删分组成员行, 事后无从查询"曾经挂载过",
// 因此受影响分组必须在事务内快照得出; 缓存刷新失败时这是唯一可信的校正依据。
// 自动补充规则经渠道写路径新增成员同样在此携带: 添加与删除共用同一份事实, 不另立平行框架。
type ChannelMutation struct {
	GroupDeltas []GroupMembersDelta // 成员集合实际变化的分组及其存活成员与新增行。
}

// PostCommitError 表示事务已提交、但提交后的缓存刷新失败: 库内级联不可回滚,
// 调用方必须先用 Mutation 携带的提交事实校正路由, 再把错误上报给界面。
type PostCommitError struct {
	RefreshErr error            // 提交后刷新失败的原因。
	Mutation   *ChannelMutation // 已提交的级联事实。
}

func (e *PostCommitError) Error() string {
	return fmt.Sprintf("channel change committed but cache refresh failed: %v", e.RefreshErr)
}

// Unwrap 使调用方仍可按原错误类型检视刷新失败原因。
func (e *PostCommitError) Unwrap() error { return e.RefreshErr }

// channelGroupMembersSnapshot 在事务内读取该渠道全部授权当前挂载的分组成员(组 ID → 成员 ID 集合)。
// 必须在删除级联发生前调用: 行被级联删除后便无从得知分组曾经挂载过哪些成员。
func channelGroupMembersSnapshot(tx *gorm.DB, channelID int) (map[int]map[int]struct{}, error) {
	type groupItemRef struct {
		GroupID int
		ID      int
	}
	var refs []groupItemRef
	if err := tx.Model(&model.GroupItem{}).
		Joins("JOIN channel_grants ON channel_grants.id = group_items.channel_grant_id").
		Joins("JOIN channel_models ON channel_models.id = channel_grants.channel_model_id").
		Where("channel_models.channel_id = ?", channelID).
		Select("group_items.group_id AS group_id, group_items.id AS id").
		Scan(&refs).Error; err != nil {
		return nil, fmt.Errorf("failed to snapshot group members: %w", err)
	}
	snapshot := make(map[int]map[int]struct{})
	for _, ref := range refs {
		if snapshot[ref.GroupID] == nil {
			snapshot[ref.GroupID] = make(map[int]struct{})
		}
		snapshot[ref.GroupID][ref.ID] = struct{}{}
	}
	return snapshot, nil
}

// committedChannelMutationFromTx 在事务内由"本渠道变更前后快照"得出提交事实。
// 受影响分组 = 快照中成员集合发生增删的分组; 其存活成员按分组全量重查(含其他渠道的成员),
// 供路由状态按完整最新成员集合修剪, 而不是只看本渠道的残留。
// 新增侧携带完整成员行: 刷新失败时缓存无从查到这些行, 提交事实是唯一来源。
// 无成员变化时返回 nil。
func committedChannelMutationFromTx(tx *gorm.DB, before, channelAfter map[int]map[int]struct{}) (*ChannelMutation, error) {
	type groupChange struct {
		removed bool
		added   []int
	}
	changes := make(map[int]*groupChange)
	touch := func(groupID int) *groupChange {
		if changes[groupID] == nil {
			changes[groupID] = &groupChange{}
		}
		return changes[groupID]
	}
	for groupID, beforeItems := range before {
		afterItems := channelAfter[groupID]
		for itemID := range beforeItems {
			if _, ok := afterItems[itemID]; !ok {
				touch(groupID).removed = true
				break
			}
		}
	}
	for groupID, afterItems := range channelAfter {
		for itemID := range afterItems {
			// before 缺失该分组时索引得 nil map, 单一判定即覆盖两种情况。
			if _, ok := before[groupID][itemID]; !ok {
				touch(groupID).added = append(touch(groupID).added, itemID)
			}
		}
	}
	if len(changes) == 0 {
		return nil, nil
	}
	affected := make([]int, 0, len(changes))
	for groupID, change := range changes {
		if !change.removed && len(change.added) == 0 {
			continue
		}
		affected = append(affected, groupID)
	}
	if len(affected) == 0 {
		return nil, nil
	}
	var survivors []model.GroupItem
	if err := tx.Where("group_id IN ?", affected).Find(&survivors).Error; err != nil {
		return nil, fmt.Errorf("failed to load surviving group members: %w", err)
	}
	addedIDs := make([]int, 0)
	for _, change := range changes {
		addedIDs = append(addedIDs, change.added...)
	}
	var addedRows []model.GroupItem
	if len(addedIDs) > 0 {
		if err := tx.Where("id IN ?", addedIDs).Find(&addedRows).Error; err != nil {
			return nil, fmt.Errorf("failed to load added group members: %w", err)
		}
	}
	addedByGroup := make(map[int][]model.GroupItem)
	for _, item := range addedRows {
		addedByGroup[item.GroupID] = append(addedByGroup[item.GroupID], item)
	}
	byGroup := make(map[int][]int, len(affected))
	for _, item := range survivors {
		byGroup[item.GroupID] = append(byGroup[item.GroupID], item.ID)
	}
	deltas := make([]GroupMembersDelta, 0, len(affected))
	for _, groupID := range affected {
		sort.Ints(byGroup[groupID])
		sort.Slice(addedByGroup[groupID], func(i, j int) bool {
			return addedByGroup[groupID][i].ID < addedByGroup[groupID][j].ID
		})
		deltas = append(deltas, GroupMembersDelta{
			GroupID:    groupID,
			ItemIDs:    byGroup[groupID],
			AddedItems: addedByGroup[groupID],
			Removed:    changes[groupID].removed,
		})
	}
	sort.Slice(deltas, func(i, j int) bool { return deltas[i].GroupID < deltas[j].GroupID })
	return &ChannelMutation{GroupDeltas: deltas}, nil
}

// refreshGroupsAfterCommit 是提交后的分组缓存刷新钩子; 声明为包内变量仅为测试注入提交后失败,
// 生产恒为 groupRefreshCache。提交后的失败一律包装成 PostCommitError 携带提交事实。
var refreshGroupsAfterCommit = groupRefreshCache

// ChannelCreate 创建渠道及其凭据, 模型与授权, 返回创建后的完整配置与提交事实。
// 三者在同一事务内落库: 授权按名称引用两侧, 待凭据与模型拿到主键后由 syncChannelGrants 解析,
// 由此建一个带授权的渠道只需一趟请求。
// 启用自动补充规则的分组在同一事务内补入本渠道匹配授权: 失败整体回滚, 不发布缓存与 SSE;
// 提交后刷新失败与 ChannelUpdate 同语义, 返回 *PostCommitError 携带提交事实。
func ChannelCreate(detail *model.ChannelDetail, ctx context.Context) (*model.ChannelDetail, *ChannelMutation, error) {
	if err := normalizeChannelDetail(detail); err != nil {
		return nil, nil, err
	}

	channel := model.Channel{ChannelConfig: detail.ChannelConfig, Revision: uuid.NewString()}
	var mutation *ChannelMutation
	if err := db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&channel).Error; err != nil {
			return fmt.Errorf("failed to create channel: %w", err)
		}
		// GORM 对 gorm:"default:true" 的 bool 字段在 Create 时把零值 false 覆盖为 true:
		// 显式禁用的渠道会被强制启用, StartBatch 误纳入。Create 后在同一事务内按提交值回写,
		// map 形式不触发默认值覆盖, 保证 explicit false 落库。
		if !detail.Enabled {
			if err := tx.Model(&model.Channel{}).Where("id = ?", channel.ID).
				Update("enabled", false).Error; err != nil {
				return fmt.Errorf("failed to set channel enabled=false: %w", err)
			}
			channel.Enabled = false
		}
		if err := syncChannelChildren(tx, channel.ID, detail); err != nil {
			return err
		}
		// 补齐必须在 tx 内: 此刻缓存还看不到新渠道的授权与启用状态。
		if err := supplementGroupsForChannel(tx, channel.ID); err != nil {
			return err
		}
		after, err := channelGroupMembersSnapshot(tx, channel.ID)
		if err != nil {
			return err
		}
		mutation, err = committedChannelMutationFromTx(tx, nil, after)
		return err
	}); err != nil {
		return nil, nil, err
	}

	channelCache.Set(channel.ID, channel)
	// 凭据, 模型与授权的主键都在事务内分配, 此刻只在库里; 重载子表缓存以让授权候选与转发都能查到。
	if err := reloadChannelChildren(ctx, channel.ID); err != nil {
		return nil, mutation, &PostCommitError{RefreshErr: err, Mutation: mutation}
	}
	// 新渠道可能给规则分组补了成员: 提交后刷新分组缓存, 失败时由调用方按事实校正。
	if mutation != nil {
		if err := refreshGroupsAfterCommit(ctx); err != nil {
			return nil, mutation, &PostCommitError{
				RefreshErr: fmt.Errorf("failed to refresh groups: %w", err),
				Mutation:   mutation,
			}
		}
	}
	// 返回的 detail 从 DB 组装: revision 与子表来自同一提交快照, 不混缓存。
	created, err := ChannelDetailGet(ctx, channel.ID)
	if err != nil {
		// 事务已提交, 读取失败不丢失提交事实: 返回 PostCommitError 携带 mutation。
		return nil, mutation, &PostCommitError{
			RefreshErr: fmt.Errorf("failed to read created channel detail: %w", err),
			Mutation:   mutation,
		}
	}
	return &created, mutation, nil
}

// ChannelUpdate 按提交的完整配置整体替换渠道及其凭据, 模型与授权, 返回刷新后的配置与提交事实。
// 提交即全量而非按字段比对增量: 渠道是人工编辑的十几个字段, 表单本就一次给出完整配置,
// 未列出的凭据与模型会被删除并级联删除其授权。
// 提交前失败返回 nil 事实(未产生任何库变更); 提交成功但缓存刷新失败返回 *PostCommitError,
// 其 Mutation 携带级联删除的分组影响, 调用方必须据此校正路由后再报错。
func ChannelUpdate(detail *model.ChannelDetail, ctx context.Context) (*model.ChannelDetail, *ChannelMutation, error) {
	// 全量更新必须携带 expected revision: 空串或缺失不允许绕过乐观锁, 直接 400。
	if detail.Revision == "" {
		return nil, nil, ErrRevisionRequired
	}
	if _, ok := channelCache.Get(detail.ID); !ok {
		return nil, nil, ErrRevisionConflict
	}
	if err := normalizeChannelDetail(detail); err != nil {
		return nil, nil, err
	}

	// 服务端生成新 revision, 写入 channels 行。expected 与库内不匹配时 RowsAffected=0。
	newRevision := uuid.NewString()
	var mutation *ChannelMutation
	if err := db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		before, err := channelGroupMembersSnapshot(tx, detail.ID)
		if err != nil {
			return err
		}
		// CAS 更新: WHERE id AND revision = expected。逐列点名而不整行覆盖:
		// 全量提交下 enabled 置假与被清空的可选字段都必须落库, 按零值跳过会写不进去;
		// 统计列由转发累加, 不在提交范围内, 整行覆盖会把它抹回提交时的旧值。
		result := tx.Model(&model.Channel{}).
			Where("id = ? AND revision = ?", detail.ID, detail.Revision).
			Select("name", "dialect", "enabled", "base_url",
				"openai_chat_completion_path", "openai_response_path", "anthropic_message_path",
				"proxy", "channel_proxy", "custom_header", "param_override", "match_regex", "auto_sync_models", "revision").
			Updates(&model.Channel{ChannelConfig: detail.ChannelConfig, Revision: newRevision})
		if result.Error != nil {
			return fmt.Errorf("failed to update channel: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			// expected revision 不匹配: 渠道被并发编辑、删除后重建或导入覆盖。
			return ErrRevisionConflict
		}
		if err := syncChannelChildren(tx, detail.ID, detail); err != nil {
			return err
		}
		if err := supplementGroupsForChannel(tx, detail.ID); err != nil {
			return err
		}
		after, err := channelGroupMembersSnapshot(tx, detail.ID)
		if err != nil {
			return err
		}
		mutation, err = committedChannelMutationFromTx(tx, before, after)
		return err
	}); err != nil {
		return nil, nil, err
	}

	channelStatsNeedUpdateLock.Lock()
	channel := model.Channel{ID: detail.ID, ChannelConfig: detail.ChannelConfig, Revision: newRevision}
	if cached, ok := channelCache.Get(detail.ID); ok {
		channel.StatsMetrics = cached.StatsMetrics
	}
	channelCache.Set(detail.ID, channel)
	channelStatsNeedUpdateLock.Unlock()

	// 凭据, 模型与授权的增删都会改变可选路由集合, 重载该渠道的三类缓存并刷新分组。
	// 两步都发生在提交之后: 失败只能暴露错误并携带提交事实, 不得吞掉已发生的级联删除。
	if err := reloadChannelChildren(ctx, detail.ID); err != nil {
		return nil, mutation, &PostCommitError{RefreshErr: err, Mutation: mutation}
	}
	if err := refreshGroupsAfterCommit(ctx); err != nil {
		return nil, mutation, &PostCommitError{
			RefreshErr: fmt.Errorf("failed to refresh groups: %w", err),
			Mutation:   mutation,
		}
	}
	// detail 的 revision 替换为新令牌, 返回给前端用于下一次更新的 expected。
	detail.Revision = newRevision
	// 返回的 detail 从 DB 组装: revision 与子表来自同一提交快照, 不混缓存。
	updated, err := ChannelDetailGet(ctx, detail.ID)
	if err != nil {
		// 事务已提交, 读取失败不丢失提交事实: 返回 PostCommitError 携带 mutation。
		return nil, mutation, &PostCommitError{
			RefreshErr: fmt.Errorf("failed to read updated channel detail: %w", err),
			Mutation:   mutation,
		}
	}
	return &updated, mutation, nil
}

// normalizeChannelConfig 补齐提交配置中的默认值并校验协议路径。
// 路径留空会与地址拼成错误的上游地址, 故一律回退到协议默认路径; Header 恒为数组, 免得落库后读出 null。
// 全部字段在此去空白: 落库后的配置被读侧无条件信任, 空白与空串都不会再传到转发链路。
func normalizeChannelConfig(config model.ChannelConfig) (model.ChannelConfig, error) {
	config.Name = strings.TrimSpace(config.Name)
	if config.Name == "" {
		return config, fmt.Errorf("channel name is required")
	}
	config.BaseURL = strings.TrimSpace(config.BaseURL)
	if config.BaseURL == "" {
		return config, fmt.Errorf("channel base url is required")
	}
	if config.Dialect == "" {
		config.Dialect = model.DialectGeneric
	}
	var err error
	if config.OpenAIChatCompletionPath, err = normalizedPath(config.OpenAIChatCompletionPath, "/v1/chat/completions"); err != nil {
		return config, err
	}
	if config.OpenAIResponsePath, err = normalizedPath(config.OpenAIResponsePath, "/v1/responses"); err != nil {
		return config, err
	}
	if config.AnthropicMessagePath, err = normalizedPath(config.AnthropicMessagePath, "/v1/messages"); err != nil {
		return config, err
	}
	if config.CustomHeader == nil {
		config.CustomHeader = []model.CustomHeader{}
	}

	config.ChannelProxy = strings.TrimSpace(config.ChannelProxy)
	config.ParamOverride = strings.TrimSpace(config.ParamOverride)
	config.MatchRegex = strings.TrimSpace(config.MatchRegex)
	return config, nil
}

// normalizeChannelDetail 规范化整份提交配置; 渠道自身的字段交由 normalizeChannelConfig 处理。
// 凭据, 模型与授权的名称在此去空白并校验非空: 名称是三者的匹配与引用依据, 集中在入口清理后,
// 下游三个同步函数拿到的即是干净数据, 无需各自再 trim 一遍。
func normalizeChannelDetail(detail *model.ChannelDetail) error {
	config, err := normalizeChannelConfig(detail.ChannelConfig)
	if err != nil {
		return err
	}
	detail.ChannelConfig = config

	for i := range detail.Keys {
		detail.Keys[i].Name = strings.TrimSpace(detail.Keys[i].Name)
		if detail.Keys[i].Name == "" {
			return fmt.Errorf("channel key name is required")
		}
		// 凭据两端的空白会被原样拼进认证 Header, 一并去掉。
		detail.Keys[i].Key = strings.TrimSpace(detail.Keys[i].Key)
	}
	for i := range detail.Models {
		detail.Models[i] = strings.TrimSpace(detail.Models[i])
		if detail.Models[i] == "" {
			return fmt.Errorf("channel model name is required")
		}
	}
	for i := range detail.Grants {
		detail.Grants[i].ModelName = strings.TrimSpace(detail.Grants[i].ModelName)
		detail.Grants[i].KeyName = strings.TrimSpace(detail.Grants[i].KeyName)
	}
	return nil
}

// syncChannelChildren 按提交的完整配置整体替换渠道下的凭据, 模型与授权。
// 凭据与模型必须先落库: 授权引用两者的主键, 新增的两者在同一事务内才拿得到。
func syncChannelChildren(tx *gorm.DB, channelID int, detail *model.ChannelDetail) error {
	if err := syncChannelKeys(tx, channelID, detail.Keys); err != nil {
		return err
	}
	if err := syncChannelModels(tx, channelID, detail.Models); err != nil {
		return err
	}
	return syncChannelGrants(tx, channelID, detail.Grants)
}

// ChannelEnabled 更新渠道启用状态, 并在启用路径补齐规则分组新增成员。
// 启用使本渠道授权重新可选: 规则分组在原事务内补入此刻匹配的授权; 禁用无新增。
// 仅在实际启停状态变化时轮转 revision: 无操作(no-op, 相同值)保持令牌不变。
// 提交事实语义与 ChannelUpdate 一致: 提交后刷新失败返回 *PostCommitError 携带提交事实。
func ChannelEnabled(id int, enabled bool, ctx context.Context) (*ChannelMutation, error) {
	channel, ok := channelCache.Get(id)
	if !ok {
		return nil, fmt.Errorf("channel not found")
	}
	// 仅在实际状态变化时轮转; no-op 保持令牌, 不让进行中的全量更新无谓失败。
	rotate := channel.Enabled != enabled
	var mutation *ChannelMutation
	var newRevision string
	if err := db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		before, err := channelGroupMembersSnapshot(tx, id)
		if err != nil {
			return err
		}
		if rotate {
			newRevision = uuid.NewString()
			if err := tx.Model(&model.Channel{}).Where("id = ?", id).
				Updates(map[string]any{"enabled": enabled, "revision": newRevision}).Error; err != nil {
				return fmt.Errorf("failed to update channel enabled: %w", err)
			}
		} else {
			if err := tx.Model(&model.Channel{}).Where("id = ?", id).
				Update("enabled", enabled).Error; err != nil {
				return fmt.Errorf("failed to update channel enabled: %w", err)
			}
		}
		// 补齐经 tx: 复核必须看到本轮更新后的 enabled, 缓存仍是旧值会漏掉刚启用的渠道。
		if err := supplementGroupsForChannel(tx, id); err != nil {
			return err
		}
		after, err := channelGroupMembersSnapshot(tx, id)
		if err != nil {
			return err
		}
		mutation, err = committedChannelMutationFromTx(tx, before, after)
		return err
	}); err != nil {
		return nil, err
	}

	// 发布实际当前 revision: 轮转了用新令牌, 没轮转保持原值。
	channel.Enabled = enabled
	if rotate {
		channel.Revision = newRevision
	}
	channelCache.Set(id, channel)
	if mutation != nil {
		if err := refreshGroupsAfterCommit(ctx); err != nil {
			return mutation, &PostCommitError{
				RefreshErr: fmt.Errorf("failed to refresh groups: %w", err),
				Mutation:   mutation,
			}
		}
	}
	return mutation, nil
}

// ChannelDel 删除渠道及其凭据, 模型与渠道授权, 关联分组项由数据库外键级联删除。
// 返回值语义与 ChannelUpdate 相同: 提交前失败无提交事实, 提交后刷新失败返回 *PostCommitError。
func ChannelDel(id int, ctx context.Context) (*ChannelMutation, error) {
	if _, ok := channelCache.Get(id); !ok {
		return nil, fmt.Errorf("channel not found")
	}
	grantIDs := channelGrantIDs(id)
	var mutation *ChannelMutation
	if err := db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		before, err := channelGroupMembersSnapshot(tx, id)
		if err != nil {
			return err
		}
		if len(grantIDs) > 0 {
			if err := clearActiveItems(tx, grantIDs); err != nil {
				return err
			}
		}
		if err := tx.Delete(&model.Channel{}, id).Error; err != nil {
			return fmt.Errorf("failed to delete channel: %w", err)
		}
		// 渠道已删, 其授权挂载的成员必然全失; 存活成员按受影响分组全量重查。
		mutation, err = committedChannelMutationFromTx(tx, before, nil)
		return err
	}); err != nil {
		return nil, err
	}

	channelStatsNeedUpdateLock.Lock()
	channelCache.Del(id)
	delete(channelStatsNeedUpdate, id)
	channelStatsNeedUpdateLock.Unlock()

	channelKeyStatsNeedUpdateLock.Lock()
	for _, channelKey := range channelKeyCache.GetAll() {
		if channelKey.ChannelID == id {
			channelKeyCache.Del(channelKey.ID)
			delete(channelKeyStatsNeedUpdate, channelKey.ID)
		}
	}
	channelKeyStatsNeedUpdateLock.Unlock()

	channelModelStatsNeedUpdateLock.Lock()
	for _, channelModel := range channelModelCache.GetAll() {
		if channelModel.ChannelID == id {
			channelModelCache.Del(channelModel.ID)
			delete(channelModelStatsNeedUpdate, channelModel.ID)
		}
	}
	channelModelStatsNeedUpdateLock.Unlock()

	channelGrantCache.Del(grantIDs...)
	if err := refreshGroupsAfterCommit(ctx); err != nil {
		return mutation, &PostCommitError{
			RefreshErr: fmt.Errorf("failed to refresh groups: %w", err),
			Mutation:   mutation,
		}
	}
	return mutation, nil
}

// ChannelGet 返回指定渠道的缓存副本, 供转发按地址, 路径与代理构造上游请求。
// 不补齐凭据, 模型与授权: 转发所需的授权由 ChannelGrantGet 按主键单独取, 那里已连带给出两侧。
func ChannelGet(id int) (model.Channel, error) {
	channel, ok := channelCache.Get(id)
	if !ok {
		return model.Channel{}, fmt.Errorf("channel not found")
	}
	return channel, nil
}

// ChannelGrantGet 返回可用于转发的渠道授权, 并补齐其模型与凭据。
// 凭据被停用, 以及模型, 凭据缺失时一律返回错误, 使调用方拿到的授权必然可直接转发, 无需再逐项检查。
// 授权本身没有停用状态: 不再授权就删掉该组合, 无需保留一行停用记录。
func ChannelGrantGet(id int) (model.ChannelGrant, error) {
	grant, ok := channelGrantCache.Get(id)
	if !ok {
		return model.ChannelGrant{}, fmt.Errorf("channel grant not found")
	}
	channelModel, ok := channelModelCache.Get(grant.ChannelModelID)
	if !ok {
		return model.ChannelGrant{}, fmt.Errorf("channel model %d not found", grant.ChannelModelID)
	}
	channelKey, ok := channelKeyCache.Get(grant.ChannelKeyID)
	if !ok {
		return model.ChannelGrant{}, fmt.Errorf("channel key %d not found", grant.ChannelKeyID)
	}
	if !channelKey.Enabled {
		return model.ChannelGrant{}, fmt.Errorf("channel key %d is disabled", channelKey.ID)
	}
	grant.ChannelModel = &channelModel
	grant.ChannelKey = &channelKey
	return grant, nil
}

// ChannelGrantCandidates 返回全部渠道授权及其展示字段, 供分组页选取成员。
// 可用性与 GroupList 补齐成员时同一口径: 渠道与凭据均启用即可用, 由此候选与已选成员不会各判一套。
// 两侧任一缺失的授权直接跳过: 它无法转发, 也无从展示名称。
func ChannelGrantCandidates() []model.ChannelGrantCandidate {
	candidates := make([]model.ChannelGrantCandidate, 0, channelGrantCache.Len())
	for _, grant := range channelGrantCache.GetAll() {
		channelModel, modelOK := channelModelCache.Get(grant.ChannelModelID)
		channelKey, keyOK := channelKeyCache.Get(grant.ChannelKeyID)
		if !modelOK || !keyOK {
			continue
		}
		channel, channelOK := channelCache.Get(channelModel.ChannelID)
		if !channelOK {
			continue
		}
		candidates = append(candidates, model.ChannelGrantCandidate{
			ID:          grant.ID,
			ChannelID:   channel.ID,
			ChannelName: channel.Name,
			ModelName:   channelModel.Name,
			KeyName:     channelKey.Name,
			Protocols:   grant.Protocols,
			Available:   channel.Enabled && channelKey.Enabled,
		})
	}
	// 按渠道, 模型, 凭据三级定序: 分组页按这三级组织候选且不提供排序开关, 顺序须由此处定稿。
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].ChannelID != candidates[j].ChannelID {
			return candidates[i].ChannelID < candidates[j].ChannelID
		}
		if candidates[i].ModelName != candidates[j].ModelName {
			return candidates[i].ModelName < candidates[j].ModelName
		}
		return candidates[i].KeyName < candidates[j].KeyName
	})
	return candidates
}

// channelRefreshCache 从数据库刷新渠道, 凭据, 模型与渠道授权缓存。
func channelRefreshCache(ctx context.Context) error {
	conn := db.GetDB().WithContext(ctx)
	channels := []model.Channel{}
	if err := conn.Find(&channels).Error; err != nil {
		log.Warnf("failed to get channels: %v", err)
		return err
	}
	channelKeys := []model.ChannelKey{}
	if err := conn.Find(&channelKeys).Error; err != nil {
		return err
	}
	channelModels := []model.ChannelModel{}
	if err := conn.Find(&channelModels).Error; err != nil {
		return err
	}
	channelGrants := []model.ChannelGrant{}
	if err := conn.Find(&channelGrants).Error; err != nil {
		return err
	}

	channelCache.Clear()
	channelKeyCache.Clear()
	channelModelCache.Clear()
	channelGrantCache.Clear()
	for _, channel := range channels {
		channelCache.Set(channel.ID, channel)
	}
	for _, channelKey := range channelKeys {
		channelKeyCache.Set(channelKey.ID, channelKey)
	}
	for _, channelModel := range channelModels {
		channelModelCache.Set(channelModel.ID, channelModel)
	}
	for _, grant := range channelGrants {
		channelGrantCache.Set(grant.ID, grant)
	}
	return nil
}

// reloadChannelChildren 重新加载单个渠道的凭据, 模型与渠道授权缓存。
// 存活目标保留缓存中尚未落库的统计, 避免刷新丢失本轮累加。
// 声明为包内变量仅为测试注入提交后子缓存刷新失败, 不改变生产行为。
var reloadChannelChildren = func(ctx context.Context, channelID int) error {
	conn := db.GetDB().WithContext(ctx)
	channelKeys := []model.ChannelKey{}
	if err := conn.Where("channel_id = ?", channelID).Find(&channelKeys).Error; err != nil {
		return fmt.Errorf("failed to load channel keys: %w", err)
	}
	channelModels := []model.ChannelModel{}
	if err := conn.Where("channel_id = ?", channelID).Find(&channelModels).Error; err != nil {
		return fmt.Errorf("failed to load channel models: %w", err)
	}
	modelIDs := make([]int, 0, len(channelModels))
	for _, channelModel := range channelModels {
		modelIDs = append(modelIDs, channelModel.ID)
	}
	grants := []model.ChannelGrant{}
	if len(modelIDs) > 0 {
		if err := conn.Where("channel_model_id IN ?", modelIDs).Find(&grants).Error; err != nil {
			return fmt.Errorf("failed to load channel grants: %w", err)
		}
	}

	// 先按库内行覆盖再清理消失的行: 配置以库内为准, 统计以缓存为准。
	// 存活行的缓存值含尚未落库的累加, 比库内的行更新, 不能被库内的统计覆盖。
	channelKeyStatsNeedUpdateLock.Lock()
	liveKeys := make(map[int]struct{}, len(channelKeys))
	for _, channelKey := range channelKeys {
		liveKeys[channelKey.ID] = struct{}{}
		if cached, ok := channelKeyCache.Get(channelKey.ID); ok {
			channelKey.StatsMetrics = cached.StatsMetrics
		}
		channelKeyCache.Set(channelKey.ID, channelKey)
	}
	for _, cached := range channelKeyCache.GetAll() {
		if _, live := liveKeys[cached.ID]; cached.ChannelID == channelID && !live {
			channelKeyCache.Del(cached.ID)
		}
	}
	channelKeyStatsNeedUpdateLock.Unlock()

	channelModelStatsNeedUpdateLock.Lock()
	liveModels := make(map[int]struct{}, len(channelModels))
	for _, channelModel := range channelModels {
		liveModels[channelModel.ID] = struct{}{}
		if cached, ok := channelModelCache.Get(channelModel.ID); ok {
			channelModel.StatsMetrics = cached.StatsMetrics
		}
		channelModelCache.Set(channelModel.ID, channelModel)
	}
	for _, cached := range channelModelCache.GetAll() {
		if _, live := liveModels[cached.ID]; cached.ChannelID == channelID && !live {
			channelModelCache.Del(cached.ID)
		}
	}
	channelModelStatsNeedUpdateLock.Unlock()

	// 授权不带统计, 直接按库内行整体替换。
	for _, grantID := range channelGrantIDs(channelID) {
		channelGrantCache.Del(grantID)
	}
	for _, grant := range grants {
		channelGrantCache.Set(grant.ID, grant)
	}
	return nil
}

// channelDetailFromRows 从同一提交快照的 DB 行组装 ChannelDetail。
// 不含主键与统计: 编辑表单只需名称与配置, 主键与统计分别由 DB 分配与转发累加。
// 集合字段恒为非空数组: 读取侧承诺不为 null。
func channelDetailFromRows(channel model.Channel, keys []model.ChannelKey, models []model.ChannelModel, grants []model.ChannelGrant) model.ChannelDetail {
	detail := model.ChannelDetail{ID: channel.ID, Revision: channel.Revision, ChannelConfig: channel.ChannelConfig}

	detail.Keys = make([]model.ChannelKeyConfig, 0, len(keys))
	keyNameByID := make(map[int]string, len(keys))
	for _, k := range keys {
		detail.Keys = append(detail.Keys, k.ChannelKeyConfig)
		keyNameByID[k.ID] = k.Name
	}
	sort.Slice(detail.Keys, func(i, j int) bool { return detail.Keys[i].Name < detail.Keys[j].Name })

	detail.Models = make([]string, 0, len(models))
	modelNameByID := make(map[int]string, len(models))
	for _, m := range models {
		detail.Models = append(detail.Models, m.Name)
		modelNameByID[m.ID] = m.Name
	}
	sort.Strings(detail.Models)

	// 授权按模型主键归属本渠道, 两侧主键在此翻译成名称。
	grantsOut := make([]model.ChannelGrantConfig, 0, len(grants))
	for _, g := range grants {
		modelName, ok := modelNameByID[g.ChannelModelID]
		if !ok {
			continue
		}
		keyName, ok := keyNameByID[g.ChannelKeyID]
		if !ok {
			continue
		}
		grantsOut = append(grantsOut, model.ChannelGrantConfig{ModelName: modelName, KeyName: keyName, Protocols: g.Protocols})
	}
	sort.Slice(grantsOut, func(i, j int) bool {
		if grantsOut[i].ModelName != grantsOut[j].ModelName {
			return grantsOut[i].ModelName < grantsOut[j].ModelName
		}
		return grantsOut[i].KeyName < grantsOut[j].KeyName
	})
	detail.Grants = grantsOut
	return detail
}

// channelGrantIDs 返回指定渠道下全部渠道授权的主键。
func channelGrantIDs(channelID int) []int {
	grantIDs := make([]int, 0)
	for _, grant := range channelGrantCache.GetAll() {
		if channelModel, ok := channelModelCache.Get(grant.ChannelModelID); ok && channelModel.ChannelID == channelID {
			grantIDs = append(grantIDs, grant.ID)
		}
	}
	return grantIDs
}

// syncChannelKeys 按提交的凭据集合新增, 更新与删除渠道凭据。
// 凭据在渠道内按名称唯一, 名称作为匹配依据; 删除凭据会级联删除其渠道授权。
func syncChannelKeys(tx *gorm.DB, channelID int, requested []model.ChannelKeyConfig) error {
	var existing []model.ChannelKey
	if err := tx.Where("channel_id = ?", channelID).Find(&existing).Error; err != nil {
		return fmt.Errorf("failed to load channel keys: %w", err)
	}
	existingByName := make(map[string]model.ChannelKey, len(existing))
	for _, channelKey := range existing {
		existingByName[channelKey.Name] = channelKey
	}
	for _, requestedKey := range requested {
		if current, ok := existingByName[requestedKey.Name]; ok {
			if current.Key != requestedKey.Key || current.Enabled != requestedKey.Enabled {
				if err := tx.Model(&model.ChannelKey{}).Where("id = ?", current.ID).
					Updates(map[string]any{"key": requestedKey.Key, "enabled": requestedKey.Enabled}).Error; err != nil {
					return fmt.Errorf("failed to update channel key: %w", err)
				}
			}
			delete(existingByName, requestedKey.Name)
			continue
		}
		newKey := model.ChannelKey{ChannelID: channelID, ChannelKeyConfig: requestedKey}
		if err := tx.Create(&newKey).Error; err != nil {
			return fmt.Errorf("failed to create channel key: %w", err)
		}
		// 与 ChannelCreate 同理: gorm:"default:true" 在 Create 时把 false 覆盖为 true。
		// 同一事务内按提交值回写, 保证 explicit false 落库。
		if !requestedKey.Enabled {
			if err := tx.Model(&model.ChannelKey{}).Where("id = ?", newKey.ID).
				Update("enabled", false).Error; err != nil {
				return fmt.Errorf("failed to set channel key enabled=false: %w", err)
			}
			newKey.Enabled = false
		}
	}
	deletedKeyIDs := make([]int, 0, len(existingByName))
	for _, channelKey := range existingByName {
		deletedKeyIDs = append(deletedKeyIDs, channelKey.ID)
	}
	if len(deletedKeyIDs) == 0 {
		return nil
	}
	if err := clearActiveItems(tx, grantsOf(tx, "channel_key_id", deletedKeyIDs)); err != nil {
		return err
	}
	if err := tx.Delete(&model.ChannelKey{}, deletedKeyIDs).Error; err != nil {
		return fmt.Errorf("failed to delete channel keys: %w", err)
	}
	return nil
}

// syncChannelModels 按提交的模型名称集合新增与删除渠道模型。
// 模型在渠道内按名称唯一, 除名称外无可更新字段, 故提交侧直接给名称; 删除模型会级联删除其渠道授权。
func syncChannelModels(tx *gorm.DB, channelID int, requested []string) error {
	var existing []model.ChannelModel
	if err := tx.Where("channel_id = ?", channelID).Find(&existing).Error; err != nil {
		return fmt.Errorf("failed to load channel models: %w", err)
	}
	existingByName := make(map[string]model.ChannelModel, len(existing))
	for _, channelModel := range existing {
		existingByName[channelModel.Name] = channelModel
	}
	for _, requestedModel := range requested {
		if _, ok := existingByName[requestedModel]; ok {
			delete(existingByName, requestedModel)
			continue
		}
		if err := tx.Create(&model.ChannelModel{ChannelID: channelID, Name: requestedModel}).Error; err != nil {
			return fmt.Errorf("failed to create channel model: %w", err)
		}
	}
	deletedModelIDs := make([]int, 0, len(existingByName))
	for _, channelModel := range existingByName {
		deletedModelIDs = append(deletedModelIDs, channelModel.ID)
	}
	if len(deletedModelIDs) == 0 {
		return nil
	}
	if err := clearActiveItems(tx, grantsOf(tx, "channel_model_id", deletedModelIDs)); err != nil {
		return err
	}
	if err := tx.Delete(&model.ChannelModel{}, deletedModelIDs).Error; err != nil {
		return fmt.Errorf("failed to delete channel models: %w", err)
	}
	return nil
}

// syncChannelGrants 按提交的授权集合新增, 更新与删除渠道授权。
// 授权以 (模型, 凭据) 组合唯一, 该组合作为匹配依据; 提交方按名称引用, 名称在此解析为本渠道的主键。
// 凭据与模型已在本事务内先行同步, 故新增的两者在此都能查到, 一次提交即可完成建模型与授权。
func syncChannelGrants(tx *gorm.DB, channelID int, requested []model.ChannelGrantConfig) error {
	var channelModels []model.ChannelModel
	if err := tx.Where("channel_id = ?", channelID).Find(&channelModels).Error; err != nil {
		return fmt.Errorf("failed to load channel models: %w", err)
	}
	modelIDByName := make(map[string]int, len(channelModels))
	modelIDList := make([]int, 0, len(channelModels))
	for _, channelModel := range channelModels {
		modelIDByName[channelModel.Name] = channelModel.ID
		modelIDList = append(modelIDList, channelModel.ID)
	}
	var channelKeys []model.ChannelKey
	if err := tx.Where("channel_id = ?", channelID).Find(&channelKeys).Error; err != nil {
		return fmt.Errorf("failed to load channel keys: %w", err)
	}
	keyIDByName := make(map[string]int, len(channelKeys))
	for _, channelKey := range channelKeys {
		keyIDByName[channelKey.Name] = channelKey.ID
	}

	var existing []model.ChannelGrant
	if len(modelIDList) > 0 {
		if err := tx.Where("channel_model_id IN ?", modelIDList).Find(&existing).Error; err != nil {
			return fmt.Errorf("failed to load channel grants: %w", err)
		}
	}
	type grantKey struct {
		modelID int // 渠道模型主键。
		keyID   int // 渠道凭据主键。
	}
	existingByKey := make(map[grantKey]model.ChannelGrant, len(existing))
	for _, grant := range existing {
		existingByKey[grantKey{grant.ChannelModelID, grant.ChannelKeyID}] = grant
	}

	for _, requestedGrant := range requested {
		modelID, ok := modelIDByName[requestedGrant.ModelName]
		if !ok {
			return fmt.Errorf("channel model %q does not belong to channel %d", requestedGrant.ModelName, channelID)
		}
		keyID, ok := keyIDByName[requestedGrant.KeyName]
		if !ok {
			return fmt.Errorf("channel key %q does not belong to channel %d", requestedGrant.KeyName, channelID)
		}
		if requestedGrant.Protocols == 0 || requestedGrant.Protocols&^definedProtocols != 0 {
			return fmt.Errorf("channel grant protocols %d is empty or contains undefined bits", requestedGrant.Protocols)
		}
		key := grantKey{modelID, keyID}
		if current, ok := existingByKey[key]; ok {
			if current.Protocols != requestedGrant.Protocols {
				if err := tx.Model(&model.ChannelGrant{}).Where("id = ?", current.ID).
					Update("protocols", requestedGrant.Protocols).Error; err != nil {
					return fmt.Errorf("failed to update channel grant: %w", err)
				}
			}
			delete(existingByKey, key)
			continue
		}
		newGrant := model.ChannelGrant{
			ChannelModelID: modelID,
			ChannelKeyID:   keyID,
			Protocols:      requestedGrant.Protocols,
		}
		if err := tx.Create(&newGrant).Error; err != nil {
			return fmt.Errorf("failed to create channel grant: %w", err)
		}
	}

	deletedGrantIDs := make([]int, 0, len(existingByKey))
	for _, grant := range existingByKey {
		deletedGrantIDs = append(deletedGrantIDs, grant.ID)
	}
	if len(deletedGrantIDs) == 0 {
		return nil
	}
	if err := clearActiveItems(tx, deletedGrantIDs); err != nil {
		return err
	}
	if err := tx.Delete(&model.ChannelGrant{}, deletedGrantIDs).Error; err != nil {
		return fmt.Errorf("failed to delete channel grants: %w", err)
	}
	return nil
}

// normalizedPath 去掉协议路径两端空白, 留空时回退到默认路径, 并校验前导斜杠。
// 缺少前导斜杠会与地址拼成错误的上游地址, 在写入前拒绝。
func normalizedPath(value, fallback string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	if !strings.HasPrefix(value, "/") {
		return value, fmt.Errorf("channel path %q must start with /", value)
	}
	return value, nil
}

// clearActiveItems 清理引用待删除授权的分组当前项。
// grants 既可以是授权主键切片, 也可以是筛选授权主键的子查询, 由调用方按删除的是授权, 模型还是凭据给出。
func clearActiveItems(tx *gorm.DB, grants any) error {
	itemIDs := tx.Model(&model.GroupItem{}).Select("id").Where("channel_grant_id IN (?)", grants)
	if err := tx.Model(&model.Group{}).
		Where("active_item_id IN (?)", itemIDs).
		Update("active_item_id", 0).Error; err != nil {
		return fmt.Errorf("failed to clear active items: %w", err)
	}
	return nil
}

// grantsOf 返回筛选指定列命中某组主键的授权主键子查询, 供 clearActiveItems 级联定位。
func grantsOf(tx *gorm.DB, column string, ids []int) *gorm.DB {
	return tx.Model(&model.ChannelGrant{}).Select("id").Where(column+" IN ?", ids)
}
