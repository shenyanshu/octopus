package op

import (
	"context"
	"fmt"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 渠道拆分为渠道, 凭据, 模型与渠道授权后导出结构变化, 版本随之递增;
// 版本 5 起 api_keys.supported_models 由逗号分隔字符串改为 JSON 数组。
const dbDumpVersion = 5

// DBExportAll 导出完整数据库内容，包括所有统计数据。
func DBExportAll(ctx context.Context) (*model.DBDump, error) {
	conn := db.GetDB().WithContext(ctx)

	d := &model.DBDump{
		Version:    dbDumpVersion,
		ExportedAt: time.Now().UTC(),
	}

	if err := conn.Find(&d.Channels).Error; err != nil {
		return nil, fmt.Errorf("export channels: %w", err)
	}
	if err := conn.Find(&d.Groups).Error; err != nil {
		return nil, fmt.Errorf("export groups: %w", err)
	}
	if err := conn.Find(&d.ChannelKeys).Error; err != nil {
		return nil, fmt.Errorf("export channel_keys: %w", err)
	}
	if err := conn.Find(&d.ChannelModels).Error; err != nil {
		return nil, fmt.Errorf("export channel_models: %w", err)
	}
	if err := conn.Find(&d.ChannelGrants).Error; err != nil {
		return nil, fmt.Errorf("export channel_grants: %w", err)
	}
	if err := conn.Find(&d.GroupItems).Error; err != nil {
		return nil, fmt.Errorf("export group_items: %w", err)
	}
	if err := conn.Find(&d.LLMInfos).Error; err != nil {
		return nil, fmt.Errorf("export llm_infos: %w", err)
	}
	if err := conn.Find(&d.APIKeys).Error; err != nil {
		return nil, fmt.Errorf("export api_keys: %w", err)
	}
	if err := conn.Find(&d.Settings).Error; err != nil {
		return nil, fmt.Errorf("export settings: %w", err)
	}

	if err := conn.Find(&d.StatsTotal).Error; err != nil {
		return nil, fmt.Errorf("export stats_total: %w", err)
	}
	if err := conn.Find(&d.StatsDaily).Error; err != nil {
		return nil, fmt.Errorf("export stats_daily: %w", err)
	}
	if err := conn.Find(&d.StatsHourly).Error; err != nil {
		return nil, fmt.Errorf("export stats_hourly: %w", err)
	}
	if err := conn.Find(&d.StatsAPIKey).Error; err != nil {
		return nil, fmt.Errorf("export stats_api_key: %w", err)
	}

	return d, nil
}

func DBImportIncremental(ctx context.Context, dump *model.DBDump) (*model.DBImportResult, error) {
	if dump == nil {
		return nil, fmt.Errorf("empty dump")
	}

	if dump.Version != 0 && dump.Version != dbDumpVersion {
		return nil, fmt.Errorf("unsupported dump version: %d", dump.Version)
	}

	// 导入也是写入边界, 渠道配置须与接口提交走同一套规范化: 手改过的备份文件同样可能带空白,
	// 空串或缺省字段, 不在此收敛则读侧要为每个字段各自兜底。
	for i := range dump.Channels {
		config, err := normalizeChannelConfig(dump.Channels[i].ChannelConfig)
		if err != nil {
			return nil, fmt.Errorf("import channel %d: %w", dump.Channels[i].ID, err)
		}
		dump.Channels[i].ChannelConfig = config
		// 为新插入的渠道分配新版本令牌: createDoNothing 对已存在渠道跳过插入,
		// 故此令牌只写入新渠道; 已存在渠道的 revision 在事务内按可编辑状态比较决定是否轮转。
		dump.Channels[i].Revision = uuid.NewString()
	}

	// 自动补充规则与接口同口径校验: 落库的 pattern 恒为可编译, 后续运行期补齐无需再防非法输入。
	// 导入本身不自动补齐成员, 也不改变评分落库隔离: 校验只挡非法 pattern 不落库。
	for i := range dump.Groups {
		if _, err := model.CompileGroupPattern(dump.Groups[i].AutoAddPattern); err != nil {
			return nil, fmt.Errorf("import group %d: %w", dump.Groups[i].ID, err)
		}
	}

	conn := db.GetDB().WithContext(ctx)
	res := &model.DBImportResult{RowsAffected: map[string]int64{}}
	err := conn.Transaction(func(tx *gorm.DB) error {
		// 导入前已存在的渠道: 记录可编辑状态快照, 导入后比较, 变化的轮转 revision。
		// 新插入的渠道已有新 UUID, 不参与比较; 纯统计导入不改变可编辑状态, 不轮转。
		var existingIDs []int
		if err := tx.Model(&model.Channel{}).Pluck("id", &existingIDs).Error; err != nil {
			return fmt.Errorf("import: failed to read existing channels: %w", err)
		}
		beforeStates := make(map[int]channelCanonicalState, len(existingIDs))
		for _, id := range existingIDs {
			state, err := snapshotChannelCanonical(tx, id)
			if err != nil {
				return fmt.Errorf("import: failed to snapshot channel %d before: %w", id, err)
			}
			beforeStates[id] = state
		}

		// base tables
		if n, err := createDoNothing(tx, dump.Channels); err != nil {
			return fmt.Errorf("import channels: %w", err)
		} else {
			res.RowsAffected["channels"] = n
		}
		// 渠道按主键冲突跳过, 统计需单独覆盖; 凭据与模型走整行覆盖, 统计随行一并导入。
		for _, channel := range dump.Channels {
			if err := tx.Model(&model.Channel{}).
				Where("id = ?", channel.ID).
				Select("input_token", "output_token", "input_cost", "output_cost", "wait_time", "request_success", "request_failed").
				Updates(&channel).Error; err != nil {
				return fmt.Errorf("import channel stats: %w", err)
			}
		}
		if n, err := createDoNothing(tx, dump.Groups); err != nil {
			return fmt.Errorf("import groups: %w", err)
		} else {
			res.RowsAffected["groups"] = n
		}
		if n, err := createUpsertAll(tx, dump.ChannelKeys, []clause.Column{{Name: "id"}}); err != nil {
			return fmt.Errorf("import channel_keys: %w", err)
		} else {
			res.RowsAffected["channel_keys"] = n
		}
		if n, err := createUpsertAll(tx, dump.ChannelModels, []clause.Column{{Name: "id"}}); err != nil {
			return fmt.Errorf("import channel_models: %w", err)
		} else {
			res.RowsAffected["channel_models"] = n
		}
		if n, err := createUpsertAll(tx, dump.ChannelGrants, []clause.Column{{Name: "id"}}); err != nil {
			return fmt.Errorf("import channel_grants: %w", err)
		} else {
			res.RowsAffected["channel_grants"] = n
		}
		// 导入前读出已存在的 GroupItem ID, 用于区分本次真正插入的新行与被跳过的旧行。
		existingItemIDs := make(map[int]bool, 0)
		if len(dump.GroupItems) > 0 {
			ids := make([]int, 0, len(dump.GroupItems))
			for _, item := range dump.GroupItems {
				ids = append(ids, item.ID)
			}
			var found []int
			if err := tx.Model(&model.GroupItem{}).Where("id IN ?", ids).Pluck("id", &found).Error; err != nil {
				return fmt.Errorf("import group_items check existing: %w", err)
			}
			for _, id := range found {
				existingItemIDs[id] = true
			}
		}
		// createDoNothing 的 CreateInBatches 会把 GORM default:true 的列回写为 true,
		// 故在调用前快照需要补写 false 的成员 ID, 之后用快照而非被回写的切片。
		disabledNewItemIDs := make([]int, 0)
		for _, item := range dump.GroupItems {
			if !item.Enabled && !existingItemIDs[item.ID] {
				disabledNewItemIDs = append(disabledNewItemIDs, item.ID)
			}
		}
		if n, err := createDoNothing(tx, dump.GroupItems); err != nil {
			return fmt.Errorf("import group_items: %w", err)
		} else {
			res.RowsAffected["group_items"] = n
		}
		// GORM 的 default:true 使 Create 把布尔零值(含显式 false)写为 true;
		// 只对本批真正插入的新行(不在 existingItemIDs 中)补写 false,
		// 已存在的行被 ON CONFLICT DO NOTHING 跳过, 既有值不被覆盖。
		for _, id := range disabledNewItemIDs {
			if err := tx.Model(&model.GroupItem{}).Where("id = ?", id).
				Update("enabled", false).Error; err != nil {
				return fmt.Errorf("import group_items enabled: %w", err)
			}
		}
		if n, err := createUpsertAll(tx, dump.LLMInfos, []clause.Column{{Name: "name"}}); err != nil {
			return fmt.Errorf("import llm_infos: %w", err)
		} else {
			res.RowsAffected["llm_infos"] = n
		}
		if n, err := createDoNothing(tx, dump.APIKeys); err != nil {
			return fmt.Errorf("import api_keys: %w", err)
		} else {
			res.RowsAffected["api_keys"] = n
		}
		if n, err := createUpsertSettings(tx, dump.Settings); err != nil {
			return fmt.Errorf("import settings: %w", err)
		} else {
			res.RowsAffected["settings"] = n
		}

		if n, err := createUpsertAll(tx, dump.StatsTotal, []clause.Column{{Name: "id"}}); err != nil {
			return fmt.Errorf("import stats_total: %w", err)
		} else {
			res.RowsAffected["stats_total"] = n
		}
		if n, err := createUpsertAll(tx, dump.StatsDaily, []clause.Column{{Name: "date"}}); err != nil {
			return fmt.Errorf("import stats_daily: %w", err)
		} else {
			res.RowsAffected["stats_daily"] = n
		}
		if n, err := createUpsertAll(tx, dump.StatsHourly, []clause.Column{{Name: "hour"}}); err != nil {
			return fmt.Errorf("import stats_hourly: %w", err)
		} else {
			res.RowsAffected["stats_hourly"] = n
		}
		if n, err := createUpsertAll(tx, dump.StatsAPIKey, []clause.Column{{Name: "api_key_id"}}); err != nil {
			return fmt.Errorf("import stats_api_key: %w", err)
		} else {
			res.RowsAffected["stats_api_key"] = n
		}

		// 导入后比较已存在渠道的可编辑状态: 变化的轮转 revision, 未变的不动。
		if err := rotateImportedChannelRevisions(tx, existingIDs, beforeStates); err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// batchSize 控制每次 INSERT 的最大行数。
// 单行字段数较多（如 stats_hourly 含 9 个字段），若一次插入过多行会超过数据库绑定参数上限（SQLite/PostgreSQL 为 65535），按行数分批写入可规避该限制。
const batchSize = 2000

func createDoNothing[T any](tx *gorm.DB, rows []T) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	result := tx.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(&rows, batchSize)
	return result.RowsAffected, result.Error
}

func createUpsertAll[T any](tx *gorm.DB, rows []T, columns []clause.Column) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	result := tx.Clauses(clause.OnConflict{
		Columns:   columns,
		UpdateAll: true,
	}).CreateInBatches(&rows, batchSize)
	return result.RowsAffected, result.Error
}

func createUpsertSettings(tx *gorm.DB, rows []model.Setting) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	result := tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value"}),
	}).Create(&rows)
	return result.RowsAffected, result.Error
}
