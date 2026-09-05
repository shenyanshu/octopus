package task

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
)

// TestMain 建立测试数据库与设置缓存: Init 的可失败路径依赖设置取值, 需要真实库支撑。
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "octopus-task-test")
	if err != nil {
		panic(err)
	}
	code := func() int {
		if err := db.InitDB("sqlite", filepath.Join(dir, "task.db"), false); err != nil {
			panic(err)
		}
		if err := op.InitCache(); err != nil {
			panic(err)
		}
		return m.Run()
	}()
	_ = db.Close()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// registeredTask 只读观察调度器注册表, 不为测试扩生产 API。
func registeredTask(name string) bool {
	tasksMu.RLock()
	defer tasksMu.RUnlock()
	_, exists := tasks[name]
	return exists
}

// resetTasksForTest 清空注册表, 使用例可重复调用 Init; RUN 未启动, 清空无并发风险。
func resetTasksForTest() {
	tasksMu.Lock()
	tasks = make(map[string]*taskEntry)
	tasksMu.Unlock()
}

// 无关设置的解析失败令 Init 提前返回, 评分落库任务仍必须已注册。
func TestScoreFlushRegisteredDespiteSettingParseFailure(t *testing.T) {
	cases := []struct {
		name     string
		breakKey model.SettingKey
	}{
		{"模型信息间隔解析失败", model.SettingKeyModelInfoUpdateInterval},
		{"统计保存间隔解析失败", model.SettingKeyStatsSaveInterval},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			original, err := op.SettingGetString(tc.breakKey)
			if err != nil {
				t.Fatalf("读取原始设置失败: %v", err)
			}
			// 写入非数字取值使 SettingGetInt 的解析确定性失败, 用后恢复以免渗入其他用例。
			if err := op.SettingSetString(tc.breakKey, "not-a-number"); err != nil {
				t.Fatalf("破坏设置取值失败: %v", err)
			}
			t.Cleanup(func() {
				if err := op.SettingSetString(tc.breakKey, original); err != nil {
					t.Fatalf("恢复设置失败: %v", err)
				}
			})

			resetTasksForTest()
			Init()

			if !registeredTask(TaskScoreFlush) {
				t.Fatalf("设置解析失败时评分落库任务未被注册")
			}
		})
	}
}
