package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/bestruirui/octopus/internal/conf"
	_ "github.com/bestruirui/octopus/internal/server/handlers"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/server/router"
	"github.com/bestruirui/octopus/static"
	"github.com/charmbracelet/log"
	"github.com/gin-gonic/gin"
)

var httpSrv http.Server

func Start() error {
	if conf.IsDebug() {
		gin.SetMode(gin.DebugMode)
	} else {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.New()
	r.Use(gin.CustomRecovery(func(c *gin.Context, _ any) {
		resp.Error(c, http.StatusInternalServerError, resp.ErrInternalServer)
		c.Abort()
	}))

	if conf.IsDebug() {
		r.Use(middleware.Logger())
	}
	r.Use(middleware.Cors())
	r.Use(middleware.StaticEmbed("/", static.StaticFS))

	if err := router.RegisterAll(r); err != nil {
		return err
	}

	httpSrv.Addr = fmt.Sprintf("%s:%d", conf.AppConfig.Server.Host, conf.AppConfig.Server.Port)
	httpSrv.Handler = r
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Errorf("http server listen and serve error: %v", err)
		}
	}()
	return nil
}

// serverShutdownTimeout 是停机时等待在途请求结束的时间预算。
// SSE 长连接在预算内不会自行结束, 预算耗尽后回退强制关闭全部连接, 保证停机总有界。
const serverShutdownTimeout = 5 * time.Second

// Close 停止接收新连接并等待在途请求结束, 超时后强制关闭剩余连接。
// 用 Shutdown 而非直接 Close: 后续的评分收尾依赖在途请求的成败记账已经发生,
// 直接 Close 会立刻掐断连接, 让记账与最终落库产生竞态;
// 超过预算仍无法结束的请求(如超长流式响应)强制关闭, 其最终记账丢失, 与既有停机语义一致。
func Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), serverShutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		return httpSrv.Close()
	}
	return nil
}
