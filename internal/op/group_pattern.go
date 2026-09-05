package op

import (
	"fmt"
	"regexp"
	"sort"

	"github.com/bestruirui/octopus/internal/model"
	"gorm.io/gorm"
)

// 本文件实现自动补充规则的事务内取数与补齐。
// 全部查询走 tx 而非缓存: 渠道写路径在提交前补齐, 缓存此刻看不到本事务内的最新授权与启用状态;
// 分组写路径同样经 tx, 使补齐与主变更同生共死。

// grantMatchRow 是一次规则匹配查到的授权候选行, 字段与 ChannelGrantCandidates 的定序口径一致。
type grantMatchRow struct {
	GrantID   int
	ChannelID int
	ModelName string
	KeyName   string
}

// matchingGrantRows 在事务内按规则查询当前匹配且渠道与凭据均启用的授权。
// 正则匹配在 Go 侧完成: 标准库 regexp 的语义与数据库方言无关, 不落回 SQL 的 LIKE/正则。
// 排序与 ChannelGrantCandidates 同口径(渠道, 模型, 凭据, 必要时授权主键兜底):
// 补入顺序由此定稿, 分组页不提供排序开关, 顺序漂移会被用户当成成员变动。
func matchingGrantRows(tx *gorm.DB, re *regexp.Regexp, channelID int) ([]grantMatchRow, error) {
	query := tx.Table("channel_grants").
		Joins("JOIN channel_models ON channel_models.id = channel_grants.channel_model_id").
		Joins("JOIN channel_keys ON channel_keys.id = channel_grants.channel_key_id").
		Joins("JOIN channels ON channels.id = channel_models.channel_id").
		Where("channels.enabled = ? AND channel_keys.enabled = ?", true, true)
	if channelID != 0 {
		query = query.Where("channel_models.channel_id = ?", channelID)
	}
	var rows []grantMatchRow
	if err := query.
		Select("channel_grants.id AS grant_id, channel_models.channel_id AS channel_id, channel_models.name AS model_name, channel_keys.name AS key_name").
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("failed to match grants by pattern: %w", err)
	}
	matched := make([]grantMatchRow, 0, len(rows))
	for _, row := range rows {
		if re.MatchString(row.ModelName) {
			matched = append(matched, row)
		}
	}
	sort.Slice(matched, func(i, j int) bool {
		if matched[i].ChannelID != matched[j].ChannelID {
			return matched[i].ChannelID < matched[j].ChannelID
		}
		if matched[i].ModelName != matched[j].ModelName {
			return matched[i].ModelName < matched[j].ModelName
		}
		if matched[i].KeyName != matched[j].KeyName {
			return matched[i].KeyName < matched[j].KeyName
		}
		return matched[i].GrantID < matched[j].GrantID
	})
	return matched, nil
}

// appendAutoMembers 把规则当前匹配的可用授权追加进分组, 只追加不删除不重排。
// 新成员 Enabled 显式置 true(GORM 布尔零值陷阱), Score 落库默认 99,
// Priority 接在现有最大优先级之后, 既有成员的任何字段一概不动。
// 按授权主键去重: 已挂载(含本事务前序步骤刚建)的授权不重复补入。
func appendAutoMembers(tx *gorm.DB, groupID int, pattern string, restrictChannelID int) error {
	re, err := model.CompileGroupPattern(pattern)
	if err != nil {
		return err
	}
	if re == nil {
		return nil
	}
	rows, err := matchingGrantRows(tx, re, restrictChannelID)
	if err != nil {
		return err
	}
	var existing []model.GroupItem
	if err := tx.Where("group_id = ?", groupID).Find(&existing).Error; err != nil {
		return fmt.Errorf("failed to load group items for auto add: %w", err)
	}
	present := make(map[int]struct{}, len(existing))
	maxPriority := 0
	for _, item := range existing {
		present[item.ChannelGrantID] = struct{}{}
		if item.Priority > maxPriority {
			maxPriority = item.Priority
		}
	}
	for _, row := range rows {
		if _, ok := present[row.GrantID]; ok {
			continue
		}
		item := model.GroupItem{GroupID: groupID, ChannelGrantID: row.GrantID, Priority: maxPriority + 1, Enabled: true}
		if err := tx.Create(&item).Error; err != nil {
			return fmt.Errorf("failed to create auto added group item: %w", err)
		}
		maxPriority++
		present[row.GrantID] = struct{}{}
	}
	return nil
}

// mergeAutoItems 把规则当前匹配的缺失授权并入提交的成员列表, 供整体替换前合并。
// 必须在 syncGroupItems 之前合并: 先 sync 后补会把用户从草稿移除的仍匹配项删后重建, 丢主键/评分/禁用态;
// 并入 requested 后, 原有匹配项走 existingByGrant 匹配保留身份。
// 草稿顺序优先在前, 匹配缺失项按稳定顺序追加在后: 用户的手工排列意图不被规则改写。
func mergeAutoItems(tx *gorm.DB, requested []model.GroupItemInput, pattern string) ([]model.GroupItemInput, error) {
	re, err := model.CompileGroupPattern(pattern)
	if err != nil {
		return nil, err
	}
	if re == nil {
		return requested, nil
	}
	rows, err := matchingGrantRows(tx, re, 0)
	if err != nil {
		return nil, err
	}
	requestedGrants := make(map[int]struct{}, len(requested))
	for _, item := range requested {
		requestedGrants[item.ChannelGrantID] = struct{}{}
	}
	merged := make([]model.GroupItemInput, len(requested), len(requested)+len(rows))
	copy(merged, requested)
	for _, row := range rows {
		if _, ok := requestedGrants[row.GrantID]; ok {
			continue
		}
		merged = append(merged, model.GroupItemInput{ChannelGrantID: row.GrantID})
	}
	return merged, nil
}

// supplementGroupsForChannel 在渠道事务内, 对所有启用自动补充规则的分组补入本渠道当前可用且匹配的授权。
// 只看本渠道: 其他渠道无需因本次渠道操作重新补齐, 它们各自的写路径自行负责。
// 渠道被禁用时 tx 内 enabled 过滤天然查不到匹配, 禁用路径自然无新增。
func supplementGroupsForChannel(tx *gorm.DB, channelID int) error {
	var groups []model.Group
	if err := tx.Where("auto_add_pattern <> ''").Find(&groups).Error; err != nil {
		return fmt.Errorf("failed to load pattern groups for channel supplement: %w", err)
	}
	for _, group := range groups {
		if err := appendAutoMembers(tx, group.ID, group.AutoAddPattern, channelID); err != nil {
			return fmt.Errorf("failed to supplement group %d: %w", group.ID, err)
		}
	}
	return nil
}
