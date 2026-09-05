package task

import (
	"context"
	"time"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/price"
	"github.com/bestruirui/octopus/internal/relay"
	"github.com/charmbracelet/log"
)

const (
	TaskPriceUpdate = "price_update"
	TaskStatsSave   = "stats_save"
	TaskCleanLLM    = "clean_llm"
	TaskScoreFlush  = "score_flush"
)

func Init() {
	// 评分落库任务与任何设置无关, 必须先于全部可失败的设置解析注册:
	// 后续解析失败会提前返回, 排在后面的注册会被无关的配置问题连带停掉。
	// 通用调度器不等待上次执行结束, 落库入口自身用 TryLock 保证串行, 重叠触发只会被跳过。
	Register(TaskScoreFlush, relay.ScoreFlushInterval, false, func() {
		// 每次落库自带短于周期的预算, 数据库挂起时串行锁最多被占用不足一个周期。
		ctx, cancel := context.WithTimeout(context.Background(), relay.ScoreFlushTimeout)
		defer cancel()
		if err := relay.FlushScores(ctx); err != nil {
			log.Warnf("failed to flush group item scores: %v", err)
		}
	})

	priceUpdateIntervalHours, err := op.SettingGetInt(model.SettingKeyModelInfoUpdateInterval)
	if err != nil {
		log.Errorf("failed to get model info update interval: %v", err)
		return
	}
	priceUpdateInterval := time.Duration(priceUpdateIntervalHours) * time.Hour
	// 注册价格更新任务
	Register(string(model.SettingKeyModelInfoUpdateInterval), priceUpdateInterval, true, func() {
		if err := price.UpdateLLMPrice(context.Background()); err != nil {
			log.Warnf("failed to update price info: %v", err)
		}
	})

	// 注册统计保存任务
	statsSaveIntervalMinutes, err := op.SettingGetInt(model.SettingKeyStatsSaveInterval)
	if err != nil {
		log.Warnf("failed to get stats save interval: %v", err)
		return
	}
	statsSaveInterval := time.Duration(statsSaveIntervalMinutes) * time.Minute
	Register(TaskStatsSave, statsSaveInterval, false, op.StatsSaveDBTask)
}
