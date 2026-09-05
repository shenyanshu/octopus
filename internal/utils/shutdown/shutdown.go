package shutdown

import (
	"errors"
	"os"
	"os/signal"
	"syscall"
)

// ErrAbortCallbacks 作为回调错误的包裹标记: 底层资源仍可能被在途任务使用,
// 剩余回调(如统计落库与数据库关闭)不得继续执行。进程退出时由系统回收残留资源。
var ErrAbortCallbacks = errors.New("shutdown: abort remaining callbacks")

type logger interface {
	Infof(template string, args ...interface{})
	Errorf(template string, args ...interface{})
	Warnf(template string, args ...interface{})
}

var ilog logger
var funcs []func() error

func Init(log logger) {
	ilog = log
	funcs = make([]func() error, 0)
}

func Register(fn func() error) {
	funcs = append(funcs, fn)
}

func Listen() {
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	ilog.Infof("Program started, press Ctrl+C to exit")
	sig := <-quit
	ilog.Warnf("Received exit signal: %v", sig)
	runCallbacks()
	os.Exit(0)
}

// runCallbacks 逆序执行停机回调; 返回 ErrAbortCallbacks 包裹错误的回调会中止剩余回调。
func runCallbacks() {
	if len(funcs) == 0 {
		return
	}
	for i := len(funcs) - 1; i >= 0; i-- {
		if err := funcs[i](); err != nil {
			if errors.Is(err, ErrAbortCallbacks) {
				ilog.Errorf("Shutdown callbacks aborted, remaining callbacks skipped; resources will be reclaimed by process exit: %v", err)
				return
			}
			ilog.Errorf("Closing functions execution failed: %v", err)
		}
	}
	ilog.Infof("Shutdown completed successfully")
}

func Shutdown() {
	runCallbacks()
}
