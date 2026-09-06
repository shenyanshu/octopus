package channelsync

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/rhttp"
)

// errConfigChanged 表示同步期间渠道配置已变更, 探测结果应丢弃。
var errConfigChanged = errors.New("channelsync: config changed during sync")

// buildHTTPClient 按渠道代理配置构建 HTTP 客户端。
func buildHTTPClient(config model.ChannelConfig) (*http.Client, error) {
	switch {
	case !config.Proxy:
		return rhttp.Direct()
	case config.ChannelProxy == "":
		return rhttp.Proxy()
	default:
		return rhttp.New(config.ChannelProxy)
	}
}

// setStatusRunning 设置渠道为 running 状态, last_sync_at 为空(未完成)。
func setStatusRunning(channelID int) {
	setStatus(channelID, model.ChannelModelSyncStatus{
		ChannelID: channelID,
		Status:    "running",
	})
}

// setStatusDone 设置已完成状态(非 running), last_sync_at 用 RFC3339Nano 以区分两次快速 sync。
func setStatusDone(channelID int, status string, additions op.SyncAdditions, errMsg string) {
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	setStatus(channelID, model.ChannelModelSyncStatus{
		ChannelID:   channelID,
		Status:      status,
		LastSyncAt:  &ts,
		AddedModels: additions.AddedModels,
		AddedGrants: additions.AddedGrants,
		Error:       errMsg,
	})
}

// setStatus 设置渠道同步状态。
func setStatus(channelID int, s model.ChannelModelSyncStatus) {
	statusesMu.Lock()
	entry, exists := statuses[channelID]
	if !exists {
		entry = &statusEntry{}
		statuses[channelID] = entry
	}
	statusesMu.Unlock()
	entry.mu.Lock()
	entry.status = s
	entry.mu.Unlock()
}

// classifyError 将内部错误映射为有限安全分类, 不输出 token / url / header / 上游原文。
// 日志同样不打印敏感字段: 调用方传入的 err 仅用于分类判定, 不直接写入 status 或日志。
func classifyError(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case isContextError(err):
		return "timeout or cancelled"
	case isChannelDeleted(err):
		return "channel deleted during sync"
	case isConfigChanged(err):
		return "channel config changed during sync"
	default:
		return "sync failed"
	}
}

func isContextError(err error) bool {
	return err == context.DeadlineExceeded || err == context.Canceled
}

func isChannelDeleted(err error) bool {
	return err == ErrChannelNotFound
}

func isConfigChanged(err error) bool {
	return err == errConfigChanged
}
