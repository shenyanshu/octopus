package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/bestruirui/octopus/internal/channelsync"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/modeldiscovery"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay"
	"github.com/bestruirui/octopus/internal/rhttp"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/server/router"
	regexp2syntax "github.com/dlclark/regexp2/syntax"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func init() {
	router.NewGroupRouter("/api/v1/channel").
		Use(middleware.Auth()).
		Use(middleware.RequireJSON()).
		AddRoute(
			router.NewRoute("/detail/:id", http.MethodGet).
				Handle(getChannelDetail),
		).
		AddRoute(
			router.NewRoute("/stats", http.MethodGet).
				Handle(listChannelStats),
		).
		AddRoute(
			router.NewRoute("/grants", http.MethodGet).
				Handle(listChannelGrant),
		).
		AddRoute(
			router.NewRoute("/grants/preview", http.MethodPost).
				Handle(previewChannelGrants),
		).
		AddRoute(
			router.NewRoute("/create", http.MethodPost).
				Handle(createChannel),
		).
		AddRoute(
			router.NewRoute("/update", http.MethodPost).
				Handle(updateChannel),
		).
		AddRoute(
			router.NewRoute("/enable", http.MethodPost).
				Handle(enableChannel),
		).
		AddRoute(
			router.NewRoute("/delete/:id", http.MethodDelete).
				Handle(deleteChannel),
		).
		AddRoute(
			router.NewRoute("/fetch-model", http.MethodPost).
				Handle(fetchModel),
		).
		AddRoute(
			router.NewRoute("/sync-status", http.MethodGet).
				Handle(getSyncStatus),
		).
		AddRoute(
			router.NewRoute("/sync-models/:id", http.MethodPost).
				Handle(syncChannelModels),
		).
		AddRoute(
			router.NewRoute("/sync-models-all", http.MethodPost).
				Handle(syncAllChannelModels),
		).
		AddRoute(
			router.NewRoute("/auto-sync/enable-all", http.MethodPost).
				Handle(enableAllAutoSync),
		)
}

// getChannelDetail 返回单个渠道的完整配置, 供编辑表单打开时读取。
// 与列表分开: 整份配置带着路径, 代理与凭据明文, 只有正在编辑的那一个渠道用得上。
func getChannelDetail(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
		return
	}
	// 持读锁从 DB 组装一致的渠道详情: 与写锁互斥使编辑提交期间 GET 读不到半成品状态。
	// 读锁在组装完成后释放, 序列化在网络外进行。
	relay.GroupGateRLock()
	detail, err := op.ChannelDetailGet(c.Request.Context(), id)
	relay.GroupGateRUnlock()
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			resp.Error(c, http.StatusNotFound, "channel not found")
			return
		}
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, detail)
}

// listChannelStats 返回全部渠道及其模型的累计统计, 也是渠道列表页的数据来源。
// 不带整份配置: 统计每次转发都在变, 界面按更短的间隔刷新它, 而路径, 代理与凭据明文只在编辑时用得上。
func listChannelStats(c *gin.Context) {
	resp.Success(c, op.ChannelStatsList())
}

// listChannelGrant 返回全部渠道授权候选, 供分组页选取成员。
func listChannelGrant(c *gin.Context) {
	resp.Success(c, op.ChannelGrantCandidates())
}

func createChannel(c *gin.Context) {
	var req model.ChannelDetail
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	// 新建渠道可能被现有分组成员引用(授权引用渠道), 与 Forward 复核的读路径共享锁。
	relay.GroupGateLock()
	defer relay.GroupGateUnlock()
	channel, mutation, err := op.ChannelCreate(&req, c.Request.Context())
	processChannelMutation(mutation)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, channel)
}

func updateChannel(c *gin.Context) {
	var req model.ChannelDetail
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	if req.ID == 0 {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
		return
	}
	// 渠道变更与分组变更共享读写锁: 持写锁覆盖 DB→缓存→路由→SSE 的完整序列,
	// 使 Forward 选路后的复核读到最新已发布状态, 避免级联删除的旧缓存穿透。
	relay.GroupGateLock()
	defer relay.GroupGateUnlock()
	// 渠道全量替换会删除未列出的凭据、模型与授权, 经外键级联删除分组成员;
	// 启用自动补充规则的分组在同一事务内补入本渠道匹配授权, mutation 同时携带新增与删除两类事实。
	// 提交前失败 mutation 为 nil, 无从也无需校正; 提交后失败(缓存刷新失败)时携带提交事实,
	// 级联删除不可回滚, 必须先按事实校正路由并发布事件再报错, 否则已打开客户端会等到下次拉取才对齐。
	channel, mutation, err := op.ChannelUpdate(&req, c.Request.Context())
	processChannelMutation(mutation)
	if err != nil {
		if errors.Is(err, op.ErrRevisionRequired) {
			resp.Error(c, http.StatusBadRequest, err.Error())
			return
		}
		if errors.Is(err, op.ErrRevisionConflict) {
			resp.Error(c, http.StatusConflict, err.Error())
			return
		}
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	// 清理无引用的 auto 模型价格记录; manual 保留。
	if err := op.LLMCleanupGhosts(c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, channel)
}

func enableChannel(c *gin.Context) {
	var request struct {
		ID      int  `json:"id"`
		Enabled bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	// 渠道启停直接影响成员 Available, 与 Forward 复核的读路径共享锁使状态发布线性化。
	relay.GroupGateLock()
	defer relay.GroupGateUnlock()
	mutation, err := op.ChannelEnabled(request.ID, request.Enabled, c.Request.Context())
	processChannelMutation(mutation)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, nil)
}

func deleteChannel(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
		return
	}
	// 与 updateChannel 同一读写锁: 级联删除的缓存与路由校正不被 Forward 读路径穿透。
	relay.GroupGateLock()
	defer relay.GroupGateUnlock()
	// 删除渠道会经外键级联删除其授权与引用它的分组成员; mutation 语义与更新入口相同。
	mutation, err := op.ChannelDel(id, c.Request.Context())
	processChannelMutation(mutation)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	if err := op.LLMCleanupGhosts(c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, nil)
}

// processChannelMutation 委托 channelsync 执行渠道 mutation 的统一编排:
// reconcile(缓存+路由) → publish SSE changed 事件。
// handlers 与 channelsync 共用同一编排函数, 不复制不新事件框架。
func processChannelMutation(mutation *op.ChannelMutation) {
	channelsync.ApplyChannelMutation(mutation)
}

// fetchModel 按提交的渠道配置与凭据拉取上游模型列表, 并按过滤表达式筛选后返回。
// 委托 modeldiscovery.Discover 完成实际探测, 与后台自动同步共用同一函数。
func fetchModel(c *gin.Context) {
	var request model.ChannelFetchModelRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	ctx := c.Request.Context()
	target := request.Channel
	target.BaseURL = strings.TrimSpace(target.BaseURL)
	target.ChannelProxy = strings.TrimSpace(target.ChannelProxy)
	target.MatchRegex = strings.TrimSpace(target.MatchRegex)
	if target.BaseURL == "" {
		resp.Error(c, http.StatusBadRequest, "channel base url is required")
		return
	}

	httpClient, err := buildFetchHTTPClient(target)
	if err != nil {
		resp.Error(c, http.StatusBadGateway, err.Error())
		return
	}
	defer httpClient.CloseIdleConnections()

	// 全局过滤由设置页维护, 与渠道过滤同取 AND: 模型须同时通过两枚正则才保留, 留空的一侧不生效。
	// 设置缺失按不过滤处理: 启动初始化会补齐默认值, 缺行只可能出现在旧库尚未刷新的瞬间。
	globalFilter, _ := op.SettingGetString(model.SettingKeyModelFilter)
	result, err := modeldiscovery.Discover(ctx, httpClient, target, request.Key, target.MatchRegex, globalFilter)
	if err != nil {
		// 正则编译失败按 400 返回, 其余按 502 上游错误。
		if _, ok := err.(*regexp2syntax.Error); ok {
			resp.Error(c, http.StatusBadRequest, err.Error())
			return
		}
		resp.Error(c, http.StatusBadGateway, err.Error())
		return
	}
	resp.Success(c, result.Models)
}

// buildFetchHTTPClient 按渠道代理配置构建探测用 HTTP 客户端。
func buildFetchHTTPClient(target model.ChannelConfig) (*http.Client, error) {
	switch {
	case !target.Proxy:
		return rhttp.Direct()
	case target.ChannelProxy == "":
		return rhttp.Proxy()
	default:
		return rhttp.New(target.ChannelProxy)
	}
}

// getSyncStatus 返回全部渠道最近一次模型同步的进程内状态。
func getSyncStatus(c *gin.Context) {
	resp.Success(c, channelsync.GetStatus())
}

// syncChannelModels 启动单个渠道的模型同步。立即返回, 不等 HTTP。
// 不存在 → 404; 协调器已停止 → 503; 其余失败 → 500。
func syncChannelModels(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
		return
	}
	started, busy, skipped, startErr := channelsync.StartSingle(id)
	if startErr != nil {
		switch {
		case errors.Is(startErr, channelsync.ErrChannelNotFound):
			resp.Error(c, http.StatusNotFound, "channel not found")
		case errors.Is(startErr, channelsync.ErrStopped):
			resp.Error(c, http.StatusServiceUnavailable, "coordinator stopped")
		case errors.Is(startErr, gorm.ErrRecordNotFound):
			resp.Error(c, http.StatusNotFound, "channel not found")
		default:
			resp.Error(c, http.StatusInternalServerError, startErr.Error())
		}
		return
	}
	result := model.ChannelSyncStartResult{
		StartedIDs: []int{},
		BusyIDs:    []int{},
		SkippedIDs: []int{},
	}
	switch {
	case started:
		result.StartedIDs = append(result.StartedIDs, id)
	case busy:
		result.BusyIDs = append(result.BusyIDs, id)
	case skipped:
		result.SkippedIDs = append(result.SkippedIDs, id)
	}
	resp.Success(c, result)
}

// syncAllChannelModels 启动批量渠道模型同步, 仅同步 Enabled && AutoSyncModels 的渠道。
// 协调器已停止 → 503; 其余失败 → 500。
func syncAllChannelModels(c *gin.Context) {
	result, err := channelsync.StartBatch()
	if err != nil {
		if errors.Is(err, channelsync.ErrStopped) {
			resp.Error(c, http.StatusServiceUnavailable, "coordinator stopped")
			return
		}
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, result)
}

// enableAllAutoSync 批量开启全部渠道的自动同步。
// 持 relay.GroupGateLock across DB→cache 使变更线性化; 不启动同步, 不改 Enabled/keys/models/grants/stats。
// 请求 {} 无参数; 响应 data={updated_count:number}。
func enableAllAutoSync(c *gin.Context) {
	// 批量改渠道配置(auto_sync_models)与缓存共享读写锁, 持写锁覆盖 DB→cache 完整序列。
	relay.GroupGateLock()
	defer relay.GroupGateUnlock()
	count, err := op.ChannelEnableAllAutoSync(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, model.ChannelAutoSyncEnableAllResult{UpdatedCount: count})
}
