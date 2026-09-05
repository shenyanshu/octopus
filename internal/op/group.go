package op

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/utils/cache"
	"gorm.io/gorm"
)

var (
	groupCache     = cache.New[int, model.Group](16) // 按主键保存完整分组配置。
	groupNameIndex = cache.New[string, int](16)      // 客户端模型名对应的分组主键。
)

// GroupList 返回缓存中的全部分组, 成员已补齐界面展示所需的名称与可用性, 按名称定序。
// 不含实时路由状态: 路由状态由 Relay 持有, 而 Relay 依赖本包, 故由处理器在返回前补齐。
// 定序是为 API Key 面板的模型选择器: 那里没有排序开关, 而缓存遍历顺序随机;
// 分组页自带升降序开关, 会按开关重排, 不依赖此顺序。
func GroupList() []model.Group {
	groups := make([]model.Group, 0, groupCache.Len())
	for _, group := range groupCache.GetAll() {
		groups = append(groups, groupSnapshot(group))
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
	return groups
}

// GroupGet 返回指定分组的读取副本, 成员已补齐界面展示所需的名称与可用性。
// 不含实时路由状态: 与 GroupList 同理, 由处理器在返回前补齐。
func GroupGet(id int) (model.Group, error) {
	group, ok := groupCache.Get(id)
	if !ok {
		return model.Group{}, fmt.Errorf("group not found")
	}
	return groupSnapshot(group), nil
}

// GroupListModel 返回缓存中的全部分组模型名, 按名称定序。
// 两个消费方都不提供排序开关: /v1/models 由第三方客户端直接展示, API Key 面板按返回顺序列出可用模型,
// 而缓存遍历顺序随机, 故顺序须由此处定稿。
func GroupListModel() []string {
	models := make([]string, 0, groupCache.Len())
	for _, group := range groupCache.GetAll() {
		models = append(models, group.Name)
	}
	sort.Strings(models)
	return models
}

// GroupGetByName 返回客户端模型名称对应的分组配置, 供转发选路使用。
// 成员的可用性由 groupSnapshot 与读取接口同口径补齐: 评分模式选路需要跳过不可用成员,
// 名称等其余展示字段对转发无害; 授权详情由 ChannelGrantGet 按主键单独取,
// 那里会连带校验凭据启用与两侧存在, 使拿到的授权必然可直接转发。
func GroupGetByName(name string) (model.Group, error) {
	groupID, ok := groupNameIndex.Get(name)
	if !ok {
		return model.Group{}, fmt.Errorf("group not found")
	}
	group, ok := groupCache.Get(groupID)
	if !ok {
		return model.Group{}, fmt.Errorf("group not found")
	}
	return groupSnapshot(group), nil
}

// ApplyGroupMemberDeltas 把渠道提交的成员变更事实套用到分组缓存。
// 提交后刷新失败时缓存已陈旧: 不校正的话, 删除侧会让下一次请求选到已删成员, 添加侧会让新成员永久漏在缓存外。
// 只动受影响分组; 删除侧保留存活成员条目, 添加侧并入携带的新成员行, 事实来自事务内已提交读, 此处不再查库。
func ApplyGroupMemberDeltas(deltas []GroupMembersDelta) {
	for _, delta := range deltas {
		group, ok := groupCache.Get(delta.GroupID)
		if !ok {
			continue
		}
		survivors := make([]model.GroupItem, 0, len(delta.ItemIDs)+len(delta.AddedItems))
		for _, item := range group.Items {
			for _, id := range delta.ItemIDs {
				if item.ID == id {
					survivors = append(survivors, item)
					break
				}
			}
		}
		// 添加侧按主键去重后并入: 成功路径的整表刷新不会走到这里, 此处补的是刷新失败漏掉的新行。
		ids := make(map[int]struct{}, len(survivors))
		for _, item := range survivors {
			ids[item.ID] = struct{}{}
		}
		for _, item := range delta.AddedItems {
			if _, ok := ids[item.ID]; !ok {
				survivors = append(survivors, item)
			}
		}
		sortGroupItems(survivors)
		group.Items = survivors
		groupCache.Set(group.ID, group)
	}
}

// GroupItemUpdateScores 批量落库分组成员的评分分数, 供 Relay 的后台落库调用。
// 只做 UPDATE 不做插入: 待写成员的行已被删除时本批影响 0 行, 已删成员不得经由落库复活;
// 整批放进一个事务, 部分失败整体回滚, 由调用方重新标记后按最新分数重试。
// ctx 让事务内每条语句继承调用方的取消与超时: 挂起的库调用不得无限占用落库串行锁。
func GroupItemUpdateScores(ctx context.Context, scores map[int]int) error {
	if len(scores) == 0 {
		return nil
	}
	return db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for itemID, score := range scores {
			if err := tx.Model(&model.GroupItem{}).Where("id = ?", itemID).Update("score", score).Error; err != nil {
				return fmt.Errorf("failed to update group item %d score: %w", itemID, err)
			}
		}
		return nil
	})
}

// GroupCreate 创建分组及其成员并刷新缓存, 返回创建后的分组。
// 成员的提交顺序即优先级顺序; 规则在同一事务内补齐当前匹配的可用授权,
// 补齐失败与创建失败一样整体回滚, 缓存与 SSE 一概不发布。
func GroupCreate(req *model.GroupCreateRequest, ctx context.Context) (*model.Group, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, fmt.Errorf("group name is required")
	}
	// 规则合法性在入口校验: 落库的 pattern 恒为可编译, 后续事务内补齐无需再防非法输入。
	if _, err := model.CompileGroupPattern(req.AutoAddPattern); err != nil {
		return nil, err
	}
	group := model.Group{
		Name:           name,
		Mode:           req.Mode,
		AutoAddPattern: req.AutoAddPattern,
		RelayConfig:    req.RelayConfig,
		Items:          make([]model.GroupItem, len(req.Items)),
	}
	if group.Mode == "" {
		group.Mode = model.GroupModeManual
	}
	model.NormalizeGroupRelayConfig(&group.RelayConfig)
	for i, item := range req.Items {
		group.Items[i] = model.GroupItem{ChannelGrantID: item.ChannelGrantID, Priority: i + 1, Enabled: true}
	}
	if err := db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&group).Error; err != nil {
			return fmt.Errorf("failed to create group: %w", err)
		}
		if err := appendAutoMembers(tx, group.ID, group.AutoAddPattern, 0); err != nil {
			return err
		}
		// 补齐发生在同一事务内, 重载后返回与缓存发布的才是含自动成员的完整形态。
		if err := tx.Preload("Items").First(&group, group.ID).Error; err != nil {
			return fmt.Errorf("failed to reload created group: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	sortGroupItems(group.Items)
	groupCache.Set(group.ID, group)
	groupNameIndex.Set(group.Name, group.ID)
	snapshot := groupSnapshot(group)
	return &snapshot, nil
}

// GroupUpdate 更新分组配置, 成员和当前成员，并返回刷新后的分组。
func GroupUpdate(id int, req *model.GroupUpdateRequest, ctx context.Context) (*model.Group, error) {
	oldGroup, ok := groupCache.Get(id)
	if !ok {
		return nil, fmt.Errorf("group not found")
	}
	oldName := oldGroup.Name

	var selectFields []string
	updates := model.Group{ID: id}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			return nil, fmt.Errorf("group name is required")
		}
		selectFields = append(selectFields, "name")
		updates.Name = name
	}
	if req.Mode != nil {
		selectFields = append(selectFields, "mode")
		updates.Mode = *req.Mode
	}
	// 生效规则: 未提交该字段时沿用现值; 提交空串即清除规则。
	// 逐列点名写入使空串能落库: 按零值跳过会把"清除"静默变成"不变"。
	effectivePattern := oldGroup.AutoAddPattern
	if req.AutoAddPattern != nil {
		if _, err := model.CompileGroupPattern(*req.AutoAddPattern); err != nil {
			return nil, err
		}
		effectivePattern = *req.AutoAddPattern
		selectFields = append(selectFields, "auto_add_pattern")
		updates.AutoAddPattern = *req.AutoAddPattern
	}
	if req.RelayConfig != nil {
		config := *req.RelayConfig
		model.NormalizeGroupRelayConfig(&config)
		selectFields = append(selectFields, "relay_config")
		updates.RelayConfig = config
	}

	var group model.Group
	err := db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if len(selectFields) > 0 {
			if err := tx.Model(&model.Group{}).Where("id = ?", id).Select(selectFields).Updates(&updates).Error; err != nil {
				return fmt.Errorf("failed to update group: %w", err)
			}
		}
		if req.Items != nil {
			// 先把规则匹配项并入草稿再整体替换: 直接 sync 会把用户从草稿移除的仍匹配项删后重建,
			// 丢主键/评分/禁用态; 并入后这些项走 existingByGrant 保留身份。规则只补缺不改排列。
			merged, err := mergeAutoItems(tx, *req.Items, effectivePattern)
			if err != nil {
				return err
			}
			if err := syncGroupItems(tx, id, merged); err != nil {
				return err
			}
		} else {
			// 未提交成员列表(如仅改规则或配置): 不重排既有列表, 只按规则追加缺失匹配。
			if err := appendAutoMembers(tx, id, effectivePattern, 0); err != nil {
				return err
			}
		}
		if err := tx.Preload("Items").First(&group, id).Error; err != nil {
			return fmt.Errorf("failed to load updated group: %w", err)
		}
		// 当前成员在成员集合定稿后才写入: syncGroupItems 会清空指向已删除成员的当前成员,
		// 先写会被它覆盖; 归属校验同样只对最终集合成立。
		if req.ActiveItemID != nil {
			if *req.ActiveItemID != 0 {
				// 指定的成员必须在最终成员集合中且处于启用状态: 禁用成员不可作为手动模式的当前成员。
				var target model.GroupItem
				found := false
				for _, item := range group.Items {
					if item.ID == *req.ActiveItemID {
						target = item
						found = true
						break
					}
				}
				if !found {
					return fmt.Errorf("group item not found")
				}
				if !target.Enabled {
					return fmt.Errorf("group item is disabled")
				}
			}
			if err := tx.Model(&model.Group{}).Where("id = ?", id).Update("active_item_id", *req.ActiveItemID).Error; err != nil {
				return fmt.Errorf("failed to update active item: %w", err)
			}
			group.ActiveItemID = *req.ActiveItemID
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	sortGroupItems(group.Items)
	groupCache.Set(group.ID, group)
	groupNameIndex.Set(group.Name, group.ID)
	if oldName != group.Name {
		groupNameIndex.Del(oldName)
	}
	snapshot := groupSnapshot(group)
	return &snapshot, nil
}

// syncGroupItems 按提交的成员集合新增, 重排与删除分组成员。
// 成员在分组内按渠道授权唯一, 该授权作为匹配依据, 由此已有成员保留其主键:
// 主键被分组的当前成员和 Relay 的路由状态引用, 换主键会让人工选择与冷却记录失效。
// 优先级一律按提交顺序重写, 前端只需提交当前排列, 无需自行算出哪些成员的顺序发生了变化。
func syncGroupItems(tx *gorm.DB, groupID int, requested []model.GroupItemInput) error {
	var existing []model.GroupItem
	if err := tx.Where("group_id = ?", groupID).Find(&existing).Error; err != nil {
		return fmt.Errorf("failed to load group items: %w", err)
	}
	existingByGrant := make(map[int]model.GroupItem, len(existing))
	for _, item := range existing {
		existingByGrant[item.ChannelGrantID] = item
	}

	for priority, requestedItem := range requested {
		current, ok := existingByGrant[requestedItem.ChannelGrantID]
		if !ok {
			newItem := model.GroupItem{GroupID: groupID, ChannelGrantID: requestedItem.ChannelGrantID, Priority: priority + 1, Enabled: true}
			if err := tx.Create(&newItem).Error; err != nil {
				return fmt.Errorf("failed to create group item: %w", err)
			}
			continue
		}
		if current.Priority != priority+1 {
			if err := tx.Model(&model.GroupItem{}).Where("id = ?", current.ID).
				Update("priority", priority+1).Error; err != nil {
				return fmt.Errorf("failed to update group item: %w", err)
			}
		}
		delete(existingByGrant, requestedItem.ChannelGrantID)
	}

	deletedIDs := make([]int, 0, len(existingByGrant))
	for _, item := range existingByGrant {
		deletedIDs = append(deletedIDs, item.ID)
	}
	if len(deletedIDs) == 0 {
		return nil
	}
	// 被删掉的成员可能正是当前人工指定的成员, 需一并清空, 否则分组会指向一个已不存在的成员。
	if err := tx.Model(&model.Group{}).
		Where("id = ? AND active_item_id IN ?", groupID, deletedIDs).
		Update("active_item_id", 0).Error; err != nil {
		return fmt.Errorf("failed to clear active item: %w", err)
	}
	if err := tx.Delete(&model.GroupItem{}, deletedIDs).Error; err != nil {
		return fmt.Errorf("failed to delete group items: %w", err)
	}
	return nil
}

// GroupDel 删除分组及其成员，成员删除不会影响被其他分组引用的渠道授权。
func GroupDel(id int, ctx context.Context) error {
	group, ok := groupCache.Get(id)
	if !ok {
		return fmt.Errorf("group not found")
	}
	if err := db.GetDB().WithContext(ctx).Delete(&model.Group{}, id).Error; err != nil {
		return fmt.Errorf("failed to delete group: %w", err)
	}
	groupCache.Del(id)
	groupNameIndex.Del(group.Name)
	return nil
}

// groupRefreshCache 从数据库刷新完整分组缓存和名称索引。
// 缓存只存库内行, 成员的名称与可用性在读取时由 groupSnapshot 现算: 它们随渠道与凭据变化,
// 存进缓存就得在每次渠道改动后跟着刷新一遍。
func groupRefreshCache(ctx context.Context) error {
	groups := []model.Group{}
	if err := db.GetDB().WithContext(ctx).
		Preload("Items").
		Find(&groups).Error; err != nil {
		return err
	}
	groupCache.Clear()
	groupNameIndex.Clear()
	for _, group := range groups {
		sortGroupItems(group.Items)
		groupCache.Set(group.ID, group)
		groupNameIndex.Set(group.Name, group.ID)
	}
	return nil
}

// sortGroupItems 按优先级和主键生成稳定的成员顺序。
func sortGroupItems(items []model.GroupItem) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].Priority != items[j].Priority {
			return items[i].Priority < items[j].Priority
		}
		return items[i].ID < items[j].ID
	})
}

// groupSnapshot 为成员补齐授权两侧的名称, 所属渠道与可用性。
// 可用性在此一次定稿: 渠道与凭据均启用且模型, 凭据均存在时可转发, 否则仍列出该成员但标记不可用,
// 由此界面无需再按渠道列表回查, 也不会出现前后端各判一套的分歧。
func groupSnapshot(group model.Group) model.Group {
	// 成员恒为数组: 读取侧承诺该字段不为 null, 空分组也要给出空数组。
	group.Items = append(make([]model.GroupItem, 0, len(group.Items)), group.Items...)
	for i := range group.Items {
		grant, ok := channelGrantCache.Get(group.Items[i].ChannelGrantID)
		if !ok {
			continue
		}
		group.Items[i].Protocols = grant.Protocols
		channelModel, modelOK := channelModelCache.Get(grant.ChannelModelID)
		channelKey, keyOK := channelKeyCache.Get(grant.ChannelKeyID)
		if !modelOK || !keyOK {
			continue
		}
		group.Items[i].ChannelID = channelModel.ChannelID
		group.Items[i].ModelName = channelModel.Name
		group.Items[i].KeyName = channelKey.Name
		channel, channelOK := channelCache.Get(channelModel.ChannelID)
		if !channelOK {
			continue
		}
		group.Items[i].ChannelName = channel.Name
		group.Items[i].Available = channel.Enabled && channelKey.Enabled && group.Items[i].Enabled
	}
	return group
}

// GroupItemSetEnabled 切换分组成员的启用状态。
// 成员仍在分组与缓存中, 主键、优先级与评分一律保留; 禁用只影响选路。
// 禁用当前人工指定的成员时清空 ActiveItemID(不自动重选); 手动模式恢复后也不自动选中。
// 返回刷新后的分组快照, 供调用方发布事件与校正路由。
func GroupItemSetEnabled(ctx context.Context, groupID, itemID int, enabled bool) (model.Group, error) {
	group, ok := groupCache.Get(groupID)
	if !ok {
		return model.Group{}, fmt.Errorf("group not found")
	}
	belongs := false
	for _, item := range group.Items {
		if item.ID == itemID {
			belongs = true
			break
		}
	}
	if !belongs {
		return model.Group{}, fmt.Errorf("group item not found")
	}

	err := db.GetDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// WHERE 同时约束 group_id 与 id: 防止跨分组的同 ID 成员被误改, 且精确匹配一行。
		result := tx.Model(&model.GroupItem{}).
			Where("id = ? AND group_id = ?", itemID, groupID).
			Update("enabled", enabled)
		if result.Error != nil {
			return fmt.Errorf("failed to update group item enabled: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("group item not found")
		}
		// 禁用当前人工指定的成员时清空 ActiveItemID: 手动模式不自动重选,
		// 留给前端在恢复后再显式指定; 其余模式忽略该字段。
		if !enabled {
			if err := tx.Model(&model.Group{}).
				Where("id = ? AND active_item_id = ?", groupID, itemID).
				Update("active_item_id", 0).Error; err != nil {
				return fmt.Errorf("failed to clear active item: %w", err)
			}
		}
		if err := tx.Preload("Items").First(&group, groupID).Error; err != nil {
			return fmt.Errorf("failed to reload group: %w", err)
		}
		return nil
	})
	if err != nil {
		return model.Group{}, err
	}

	sortGroupItems(group.Items)
	groupCache.Set(group.ID, group)
	groupNameIndex.Set(group.Name, group.ID)
	return groupSnapshot(group), nil
}
