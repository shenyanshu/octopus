package handlers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay"
	"github.com/gin-gonic/gin"
	"github.com/looplj/axonhub/llm"
)

// 评分分组成员集合变化的运行时清理测试: 删除成员的更新必须在响应生成前同步清理 runtime 并让旧 epoch 失效,
// 纯重排则不得误清评分。状态通过真实转发请求建立, 不直接改包内私有状态。

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	dir, err := os.MkdirTemp("", "octopus-handlers-test")
	if err != nil {
		panic(err)
	}
	code := func() int {
		if err := db.InitDB("sqlite", filepath.Join(dir, "handlers.db"), false); err != nil {
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

// seedTwoMemberScoredGroup 建立带两个成员的评分分组并发起一次成功转发, 让 A 成为满分现任。
func seedTwoMemberScoredGroup(t *testing.T) (groupID int, grantA, grantB int) {
	t.Helper()
	dbConn := db.GetDB()
	for _, table := range []string{"groups", "group_items", "channel_grants", "channel_models", "channel_keys", "channels"} {
		if err := dbConn.Exec("DELETE FROM " + table).Error; err != nil {
			t.Fatalf("清理表 %s 失败: %v", table, err)
		}
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	t.Cleanup(upstream.Close)

	grants := make([]int, 0, 2)
	for i := 0; i < 2; i++ {
		channel := model.Channel{ChannelConfig: model.ChannelConfig{
			Name: "ch-" + strconv.Itoa(i), Enabled: true, BaseURL: upstream.URL,
			OpenAIChatCompletionPath: "/chat", AnthropicMessagePath: "/msg",
		}}
		if err := dbConn.Create(&channel).Error; err != nil {
			t.Fatalf("建渠道失败: %v", err)
		}
		key := model.ChannelKey{ChannelID: channel.ID, ChannelKeyConfig: model.ChannelKeyConfig{Name: "k", Key: "sk-test", Enabled: true}}
		if err := dbConn.Create(&key).Error; err != nil {
			t.Fatalf("建凭据失败: %v", err)
		}
		channelModel := model.ChannelModel{ChannelID: channel.ID, Name: "up-model"}
		if err := dbConn.Create(&channelModel).Error; err != nil {
			t.Fatalf("建模型失败: %v", err)
		}
		grant := model.ChannelGrant{ChannelModelID: channelModel.ID, ChannelKeyID: key.ID, Protocols: model.ProtocolOpenAIChatCompletion}
		if err := dbConn.Create(&grant).Error; err != nil {
			t.Fatalf("建授权失败: %v", err)
		}
		grants = append(grants, grant.ID)
	}
	if _, err := op.GroupCreate(&model.GroupCreateRequest{
		Name: "runtime-cleanup", Mode: model.GroupModeScored,
		Items: []model.GroupItemInput{{ChannelGrantID: grants[0]}, {ChannelGrantID: grants[1]}},
	}, context.Background()); err != nil {
		t.Fatalf("建分组失败: %v", err)
	}
	if err := op.InitCache(); err != nil {
		t.Fatalf("刷新缓存失败: %v", err)
	}

	router := gin.New()
	router.POST("/v1/chat/completions", relay.Forward(llm.APIFormatOpenAIChatCompletion))
	body := []byte(`{"model":"runtime-cleanup","messages":[{"role":"user","content":"hi"}]}`)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("预热请求失败: %d %s", rec.Code, rec.Body.String())
	}

	group, err := op.GroupGetByName("runtime-cleanup")
	if err != nil {
		t.Fatalf("读分组失败: %v", err)
	}
	state := relay.RouteStateOf(group)
	if state.CurrentItemID == 0 || len(state.Scores) != 1 {
		t.Fatalf("预热后状态不符合预期: %+v", state)
	}
	return group.ID, grants[0], grants[1]
}

// callUpdateGroup 以给定请求体调用更新接口。
func callUpdateGroup(t *testing.T, groupID int, body string) {
	t.Helper()
	rec := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(rec)
	context.Request = httptest.NewRequest(http.MethodPut, "/", bytes.NewReader([]byte(body)))
	context.Request.Header.Set("Content-Type", "application/json")
	context.Params = gin.Params{{Key: "id", Value: strconv.Itoa(groupID)}}
	updateGroup(context)
	if rec.Code != http.StatusOK {
		t.Fatalf("更新分组失败: %d %s", rec.Code, rec.Body.String())
	}
}

func TestUpdateGroupResetsScoredRuntimeOnMemberRemoval(t *testing.T) {
	groupID, grantA, grantB := seedTwoMemberScoredGroup(t)

	// 删除成员 A: 更新响应生成前 runtime 必须已清理, 旧请求的迟到结果无法再写回新状态。
	callUpdateGroup(t, groupID, `{"name":"runtime-cleanup","mode":"scored","items":[{"channel_grant_id":`+strconv.Itoa(grantB)+`}]}`)

	group, err := op.GroupGetByName("runtime-cleanup")
	if err != nil {
		t.Fatalf("读分组失败: %v", err)
	}
	state := relay.RouteStateOf(group)
	if state.CurrentItemID != 0 || len(state.Scores) != 0 {
		t.Fatalf("删除成员后 runtime 未清理: %+v", state)
	}

	// 控制组: 不删成员的重排不得清掉评分。
	// 重建状态后重排 A、B 顺序。
	router := gin.New()
	router.POST("/v1/chat/completions", relay.Forward(llm.APIFormatOpenAIChatCompletion))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"runtime-cleanup","messages":[{"role":"user","content":"hi"}]}`))))
	if rec.Code != http.StatusOK {
		t.Fatalf("重建状态请求失败: %d %s", rec.Code, rec.Body.String())
	}
	callUpdateGroup(t, groupID, `{"name":"runtime-cleanup","mode":"scored","items":[{"channel_grant_id":`+strconv.Itoa(grantB)+`},{"channel_grant_id":`+strconv.Itoa(grantA)+`}]}`)
	group, err = op.GroupGetByName("runtime-cleanup")
	if err != nil {
		t.Fatalf("读分组失败: %v", err)
	}
	state = relay.RouteStateOf(group)
	if len(state.Scores) == 0 {
		t.Fatalf("纯重排清掉了评分: %+v", state)
	}
}
