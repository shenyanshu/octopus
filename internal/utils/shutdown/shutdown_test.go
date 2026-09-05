package shutdown

import (
	"errors"
	"fmt"
	"testing"
)

// 测试替身日志器: 只需满足包内 logger 接口, 不做断言。
type silentLogger struct{}

func (silentLogger) Infof(string, ...interface{})  {}
func (silentLogger) Errorf(string, ...interface{}) {}
func (silentLogger) Warnf(string, ...interface{})  {}

// 中止哨兵必须挡住其后(即更早注册)的回调: 模拟评分收尾仍在途时, 统计落库与数据库关闭不得执行。
func TestRunCallbacksAbortsRemainingOnSentinel(t *testing.T) {
	Init(silentLogger{})
	defer Init(silentLogger{})

	var dbClosed, cacheSaved, drained bool
	Register(func() error { dbClosed = true; return nil })
	Register(func() error { cacheSaved = true; return nil })
	Register(func() error {
		drained = true
		// 与 cmd/start.go 同构的包裹方式, 验证 errors.Is 穿透。
		return fmt.Errorf("%w: %w", ErrAbortCallbacks, errors.New("writer still in flight"))
	})

	runCallbacks()

	if !drained {
		t.Fatalf("收尾回调未执行")
	}
	if cacheSaved || dbClosed {
		t.Fatalf("中止后剩余回调仍被执行: cache=%v db=%v", cacheSaved, dbClosed)
	}
}

// 普通错误不得中止流程: 后续回调照常执行并各自被记录。
func TestRunCallbacksContinuesOnNormalError(t *testing.T) {
	Init(silentLogger{})
	defer Init(silentLogger{})

	var order []string
	Register(func() error { order = append(order, "db"); return nil })
	Register(func() error { order = append(order, "cache"); return nil })
	Register(func() error { order = append(order, "drain"); return errors.New("boom") })

	runCallbacks()

	if fmt.Sprint(order) != fmt.Sprint([]string{"drain", "cache", "db"}) {
		t.Fatalf("回调执行顺序 = %v, 想要 drain,cache,db", order)
	}
}
