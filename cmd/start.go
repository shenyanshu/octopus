package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/bestruirui/octopus/internal/conf"
	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay"
	"github.com/bestruirui/octopus/internal/server"
	"github.com/bestruirui/octopus/internal/task"
	"github.com/bestruirui/octopus/internal/utils/shutdown"
	"github.com/charmbracelet/log"
	"github.com/spf13/cobra"
)

var cfgFile string

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start " + conf.APP_NAME,
	PreRun: func(cmd *cobra.Command, args []string) {
		conf.PrintBanner()
		conf.Load(cfgFile)
		if level, err := log.ParseLevel(conf.AppConfig.Log.Level); err == nil {
			log.SetLevel(level)
		}
	},
	Run: func(cmd *cobra.Command, args []string) {
		shutdown.Init(log.Default())
		if err := db.InitDB(conf.AppConfig.Database.Type, conf.AppConfig.Database.Path, conf.IsDebug()); err != nil {
			log.Errorf("database init error: %v", err)
			return
		}
		shutdown.Register(db.Close)

		if err := op.InitCache(); err != nil {
			log.Errorf("cache init error: %v", err)
			return
		}
		shutdown.Register(op.SaveCache)

		// 分组成员的持久分数须在承接请求前装回路由状态: 恢复只此一次, 现任成员与代数由首次选路重建。
		relay.RestoreScores()

		if err := op.UserInit(); err != nil {
			log.Errorf("user init error: %v", err)
			return
		}

		// 停机回调按注册逆序执行: 评分收尾排在连接排空之后、统计落库与数据库关闭之前,
		// 先等在途请求的成败记账结束, 再把剩余 dirty 按最新值一次写库; 失败由停机框架记录。
		shutdown.Register(func() error {
			ctx, cancel := context.WithTimeout(context.Background(), relay.ScoreDrainTimeout)
			defer cancel()
			err := relay.DrainScores(ctx)
			var inFlight *relay.DrainInFlightError
			if errors.As(err, &inFlight) {
				// 终写仍在途: 数据库不能关闭, 中止剩余停机回调, 进程退出时由系统回收残留资源。
				log.Warnf("score drain still in flight, skipping db close; resources will be reclaimed by process exit")
				return fmt.Errorf("%w: %w", shutdown.ErrAbortCallbacks, err)
			}
			return err
		})

		if err := server.Start(); err != nil {
			log.Errorf("server start error: %v", err)
			return
		}
		shutdown.Register(server.Close)

		task.Init()
		go task.RUN()
		shutdown.Listen()
	},
}

func init() {
	startCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is ./data/config.json)")
	rootCmd.AddCommand(startCmd)
}
