package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/bestruirui/octopus/internal/channelsync"
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

// forwardOK 沿真实转发路径发一条请求, 建立或推进路由状态。
func forwardOK(t *testing.T, modelName string) {
	t.Helper()
	router := gin.New()
	router.POST("/v1/chat/completions", relay.Forward(llm.APIFormatOpenAIChatCompletion))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"`+modelName+`","messages":[{"role":"user","content":"hi"}]}`))))
	if rec.Code != http.StatusOK {
		t.Fatalf("转发失败: %d %s", rec.Code, rec.Body.String())
	}
}

// deadBaseURL 返回一个必然拒绝连接的地址, 用于给指定成员制造真实失败。
func deadBaseURL(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("探测端口失败: %v", err)
	}
	baseURL := "http://" + listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("关闭探测端口失败: %v", err)
	}
	return baseURL
}

// seedScoredPair 建立双成员评分分组(共享一个健康上游), 返回分组 ID、两渠道 ID 与两授权 ID。
func seedScoredPair(t *testing.T, name string) (int, int, int, int, int) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)

	dbConn := db.GetDB()
	channelIDs := make([]int, 0, 2)
	grants := make([]int, 0, 2)
	for i := 0; i < 2; i++ {
		channel := model.Channel{ChannelConfig: model.ChannelConfig{
			Name: name + "-ch-" + strconv.Itoa(i), Enabled: true, BaseURL: upstream.URL,
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
		channelIDs = append(channelIDs, channel.ID)
		grants = append(grants, grant.ID)
	}
	if _, err := op.GroupCreate(&model.GroupCreateRequest{
		Name: name, Mode: model.GroupModeScored,
		Items: []model.GroupItemInput{{ChannelGrantID: grants[0]}, {ChannelGrantID: grants[1]}},
	}, context.Background()); err != nil {
		t.Fatalf("建分组失败: %v", err)
	}
	if err := op.InitCache(); err != nil {
		t.Fatalf("刷新缓存失败: %v", err)
	}
	group, err := op.GroupGetByName(name)
	if err != nil {
		t.Fatalf("读分组失败: %v", err)
	}
	return group.ID, channelIDs[0], channelIDs[1], grants[0], grants[1]
}

// itemIDOfGrant 在分组成员里找授权对应的成员主键。
func itemIDOfGrant(t *testing.T, name string, grantID int) int {
	t.Helper()
	group, err := op.GroupGetByName(name)
	if err != nil {
		t.Fatalf("读分组失败: %v", err)
	}
	for _, item := range group.Items {
		if item.ChannelGrantID == grantID {
			return item.ID
		}
	}
	t.Fatalf("分组 %s 里找不到授权 %d 的成员", name, grantID)
	return 0
}

// 模式往返经真实接口: 未落库的最新分在切换中保留, 瞬态清零, 切回后首次选路按保留分进行。
func TestUpdateGroupModeSwitchRoundTripRetainsScores(t *testing.T) {
	const name = "mode-roundtrip"
	groupID, channelA, _, grantA, grantB := seedScoredPair(t, name)

	// 预热: A 现任 100 分。
	forwardOK(t, name)
	// 制造一次 A 的真实失败: A=98, B 补位成功=100。
	dbConn := db.GetDB()
	var channel model.Channel
	if err := dbConn.First(&channel, channelA).Error; err != nil {
		t.Fatalf("读渠道失败: %v", err)
	}
	originalURL := channel.BaseURL
	if err := dbConn.Model(&model.Channel{}).Where("id = ?", channelA).Update("base_url", deadBaseURL(t)).Error; err != nil {
		t.Fatalf("改渠道地址失败: %v", err)
	}
	if err := op.InitCache(); err != nil {
		t.Fatalf("刷新缓存失败: %v", err)
	}
	forwardOK(t, name)
	// 恢复 A 的地址, 后续请求两者皆健康。
	if err := dbConn.Model(&model.Channel{}).Where("id = ?", channelA).Update("base_url", originalURL).Error; err != nil {
		t.Fatalf("恢复渠道地址失败: %v", err)
	}
	if err := op.InitCache(); err != nil {
		t.Fatalf("刷新缓存失败: %v", err)
	}
	itemA := itemIDOfGrant(t, name, grantA)
	itemB := itemIDOfGrant(t, name, grantB)

	// 切到手动: 手动模式的 runtime 由配置推导, 保留的评分不可见。
	callUpdateGroup(t, groupID, `{"name":"`+name+`","mode":"manual","active_item":`+strconv.Itoa(grantB)+`}`)
	manual, err := op.GroupGetByName(name)
	if err != nil {
		t.Fatalf("读分组失败: %v", err)
	}
	if state := relay.RouteStateOf(manual); len(state.Scores) != 0 {
		t.Fatalf("手动模式透出了保留评分: %+v", state)
	}

	// 切回评分: 瞬态清零, A/B 的最新分(98/100)原样恢复。
	callUpdateGroup(t, groupID, `{"name":"`+name+`","mode":"scored"}`)
	scoredGroup, err := op.GroupGetByName(name)
	if err != nil {
		t.Fatalf("读分组失败: %v", err)
	}
	state := relay.RouteStateOf(scoredGroup)
	if state.CurrentItemID != 0 {
		t.Fatalf("切回后瞬态未清零: %+v", state)
	}
	if state.Scores[itemA] != 98 || state.Scores[itemB] != 100 {
		t.Fatalf("切回后保留分不符合预期: %+v", state)
	}

	// 切回后的首次选路必须落在保留分最高的 B 上。
	forwardOK(t, name)
	scoredGroup, err = op.GroupGetByName(name)
	if err != nil {
		t.Fatalf("读分组失败: %v", err)
	}
	if state := relay.RouteStateOf(scoredGroup); state.CurrentItemID != itemB {
		t.Fatalf("首次选路 = %d, 想要保留分最高的 %d: %+v", state.CurrentItemID, itemB, state)
	}
}

// callDeleteChannel 以给定渠道 ID 调用删除接口。
func callDeleteChannel(t *testing.T, channelID int) {
	t.Helper()
	rec := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(rec)
	ginContext.Request = httptest.NewRequest(http.MethodDelete, "/", nil)
	ginContext.Params = gin.Params{{Key: "id", Value: strconv.Itoa(channelID)}}
	deleteChannel(ginContext)
	if rec.Code != http.StatusOK {
		t.Fatalf("删除渠道失败: %d %s", rec.Code, rec.Body.String())
	}
}

// assertCascadeReconciled 断言级联校正后的运行时形态: 目标分组被修剪, 旁观分组原样保留。
func assertCascadeReconciled(t *testing.T, targetName, bystanderName string, itemA, itemC int) {
	t.Helper()
	targetGroup, err := op.GroupGetByName(targetName)
	if err != nil {
		t.Fatalf("读分组失败: %v", err)
	}
	targetState := relay.RouteStateOf(targetGroup)
	if targetState.CurrentItemID != 0 || len(targetState.Scores) != 0 {
		t.Fatalf("级联删除后目标分组 runtime 未按最新成员校正: %+v", targetState)
	}
	if targetState.Scores[itemA] != 0 {
		t.Fatalf("已删成员的评分残留: %+v", targetState)
	}

	bystanderGroup, err := op.GroupGetByName(bystanderName)
	if err != nil {
		t.Fatalf("读分组失败: %v", err)
	}
	bystanderState := relay.RouteStateOf(bystanderGroup)
	if bystanderState.CurrentItemID != itemC || bystanderState.Scores[itemC] != 100 {
		t.Fatalf("无关分组被级联通知波及: %+v", bystanderState)
	}
}

// 渠道删除经外键级联删除分组成员: 受影响分组的评分按最新成员校正,
// 无关分组(哪怕同源路由)不受波及, 未受影响的现任与评分原样保留。
func TestChannelDeletionCascadePrunesRouteState(t *testing.T) {
	const target = "cascade-target"
	const bystander = "cascade-bystander"
	_, channelA, _, grantA, _ := seedScoredPair(t, target)
	_, _, _, grantC, _ := seedScoredPair(t, bystander)

	// 双分组各自预热: 目标分组 A=100 现任, 旁观分组 C=100 现任。
	forwardOK(t, target)
	forwardOK(t, bystander)
	itemA := itemIDOfGrant(t, target, grantA)
	itemC := itemIDOfGrant(t, bystander, grantC)

	// 删除 A 所在渠道: 授权与分组成员被级联删除。
	callDeleteChannel(t, channelA)
	assertCascadeReconciled(t, target, bystander, itemA, itemC)
}

// 提交后刷新失败的处理器分支: op 以 PostCommitError 携带提交事实,
// 处理器必须先按事实校正路由再报错; 事实的库内真伪由 op 层测试单独证明。
func TestReconcileChannelMutationOnPostCommitFailure(t *testing.T) {
	// 与级联用例错开分组名: 两个用例共享同一测试库, 避免唯一约束互相干扰。
	const target = "postcommit-target"
	const bystander = "postcommit-bystander"
	_, _, _, grantA, grantB := seedScoredPair(t, target)
	_, _, _, grantC, _ := seedScoredPair(t, bystander)

	forwardOK(t, target)
	forwardOK(t, bystander)
	itemA := itemIDOfGrant(t, target, grantA)
	itemB := itemIDOfGrant(t, target, grantB)
	itemC := itemIDOfGrant(t, bystander, grantC)
	targetGroup, err := op.GroupGetByName(target)
	if err != nil {
		t.Fatalf("读分组失败: %v", err)
	}

	// 模拟 op 在提交后刷新失败时交出的提交事实: 目标分组仅剩成员 B, 确有删除故 Removed 为真。
	channelsync.ApplyChannelMutation(&op.ChannelMutation{
		GroupDeltas: []op.GroupMembersDelta{{GroupID: targetGroup.ID, ItemIDs: []int{itemB}, Removed: true}},
	})
	assertCascadeReconciled(t, target, bystander, itemA, itemC)
}

// 提交后刷新失败时, 缓存与路由必须按同一提交事实校正:
// 陈旧的分组缓存不得再向选路提供已删成员, 下一次评分转发只能落在存活成员上。
func TestPostCommitCacheReconcileKeepsSelectionOffDeletedMember(t *testing.T) {
	const target = "postcommit-cache"
	_, _, _, grantA, grantB := seedScoredPair(t, target)
	forwardOK(t, target) // A=100 现任。
	itemA := itemIDOfGrant(t, target, grantA)
	itemB := itemIDOfGrant(t, target, grantB)
	targetGroup, err := op.GroupGetByName(target)
	if err != nil {
		t.Fatalf("读分组失败: %v", err)
	}

	// 模拟已提交级联 + 刷新失败: 成员行已从库中消失, 而分组缓存保持陈旧。
	if err := db.GetDB().Delete(&model.GroupItem{}, itemA).Error; err != nil {
		t.Fatalf("删除成员行失败: %v", err)
	}

	channelsync.ApplyChannelMutation(&op.ChannelMutation{
		GroupDeltas: []op.GroupMembersDelta{{GroupID: targetGroup.ID, ItemIDs: []int{itemB}, Removed: true}},
	})

	prunedGroup, err := op.GroupGetByName(target)
	if err != nil {
		t.Fatalf("读分组失败: %v", err)
	}
	if len(prunedGroup.Items) != 1 || prunedGroup.Items[0].ID != itemB {
		t.Fatalf("陈旧缓存未按提交事实校正: %+v", prunedGroup.Items)
	}

	// 校正后的首次评分选路只能落在存活成员上。
	forwardOK(t, target)
	if state := relay.RouteStateOf(prunedGroup); state.CurrentItemID != itemB {
		t.Fatalf("校正后选路仍落向已删成员: %+v", state)
	}
}

// TestImportBarrierResetsGroupPresentOnlyInGroupItems 证明 Oracle 修复的精确缝隙:
// 载荷不显式导入已存在的评分分组, 但其成员以复用已删主键的方式出现在 GroupItems;
// 导入隔离必须把该分组纳入受影响集合, 作废陈旧 dirty 与路由状态, 旧分数不得落在新身份上。
//
// 本用例是生产序编排回归: 直接调用 op.DBImportIncremental 与 op.InitCache,
// 但严格复刻 importDB 的调用序(Begin → import → InitCache → defer End)与令牌不泄漏保证。
// 未走真实 Gin 处理器, 因其 JSON 校验/LLM 信息归一化与本缝隙无关且需不成比例的载荷装配。
func TestImportBarrierResetsGroupPresentOnlyInGroupItems(t *testing.T) {
	const name = "import-barrier-ghost"
	groupID, channelA, _, grantA, grantB := seedScoredPair(t, name)

	// 成员 A 设为必然拒绝连接, 转发会真实计失败: 99 → 97, 并标 dirty。
	if err := db.GetDB().Model(&model.Channel{}).
		Where("id = ?", channelA).Update("base_url", deadBaseURL(t)).Error; err != nil {
		t.Fatalf("改 A 渠道为死地址失败: %v", err)
	}
	forwardOK(t, name) // 落到 A 即失败, 落到 B 会成功并满分 —— 任一都让分组出现非缺省分。
	itemA := itemIDOfGrant(t, name, grantA)
	_ = itemIDOfGrant(t, name, grantB) // 成员 B 仅作存活参照, 断言不直接用。

	// 删除成员 A 的行, 保留分组与授权外键合法, 为导入留出可复用主键。
	if err := db.GetDB().Delete(&model.GroupItem{}, itemA).Error; err != nil {
		t.Fatalf("预删成员失败: %v", err)
	}

	// 载荷完全不含该分组, 但以同主键 + 同授权 + 同分组的 GroupItem 形式复用 A 的身份。
	dump := &model.DBDump{Version: 5}
	dump.GroupItems = append(dump.GroupItems, model.GroupItem{
		ID: itemA, GroupID: groupID, ChannelGrantID: grantA, Priority: 1,
	})

	// 旧快照此时正挂在陈旧 dirty 上, 且路由状态仍记着它。导入隔离必须等在途写、
	// 持令牌期间作废 dirty 并重置该分组路由; 复用主键重建后落库默认 99, 旧值不得渗透。
	affected := importAffectedGroupIDs(dump)
	if len(affected) != 1 || affected[0] != groupID {
		t.Fatalf("受影响分组 = %v, 想要 [%d]", affected, groupID)
	}

	// 生产序: Begin 持令牌 → import → InitCache(令牌仍在手) → defer End。
	// 任何步骤失败都走同一 defer, 令牌绝不在测试中途泄漏。
	if err := relay.BeginScoreImportBarrier(context.Background()); err != nil {
		t.Fatalf("隔离进入失败: %v", err)
	}
	barrierHeld := true
	defer func() {
		if barrierHeld {
			relay.EndScoreImportBarrier(affected)
		}
	}()

	if _, err := op.DBImportIncremental(context.Background(), dump); err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if err := op.InitCache(); err != nil {
		t.Fatalf("缓存重建失败: %v", err)
	}
	// 令牌仍持有: 与 importDB 一致, End 排在 InitCache 之后, 由 defer 执行。
	relay.EndScoreImportBarrier(affected)
	barrierHeld = false

	// 复用主键重建的行仍是默认 99: 旧快照未落上去。
	var row model.GroupItem
	if err := db.GetDB().First(&row, itemA).Error; err != nil {
		t.Fatalf("读复用成员行失败: %v", err)
	}
	if row.Score != 99 {
		t.Fatalf("复用主键被旧快照污染: %d, 想要 99", row.Score)
	}

	// 受影响分组的路由状态已被重置: 重建后不带陈旧分数;
	// 复用主键重建的成员 A 与原存活的成员 B 都在, 但都取默认 99, 旧快照不渗透。
	group, err := op.GroupGetByName(name)
	if err != nil {
		t.Fatalf("读分组失败: %v", err)
	}
	if len(group.Items) != 2 {
		t.Fatalf("导入后分组缓存与库不一致: %+v", group.Items)
	}
	if state := relay.RouteStateOf(group); len(state.Scores) != 0 {
		t.Fatalf("导入隔离后路由仍带陈旧分数: %+v", state.Scores)
	}
}

// callToggleItemEnabled 而后断言: 通过真实 gin.CreateTestContext 直接执行 toggleGroupItemEnabled handler。
func callToggleItemEnabled(t *testing.T, groupID, itemID int, enabled bool) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"group_id": groupID,
		"item_id":  itemID,
		"enabled":  enabled,
	})
	rec := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(rec)
	ginContext.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	ginContext.Request.Header.Set("Content-Type", "application/json")
	toggleGroupItemEnabled(ginContext)
	return rec
}

// TestToggleGroupItemEnabledHandlerSuccess 通过真实 toggleGroupItemEnabled handler 证明:
// 响应结构正确, DB 落 enabled=false, 缓存 Available=false, 手动模式 ActiveItemID 清零, 路由不指向被禁用成员。
func TestToggleGroupItemEnabledHandlerSuccess(t *testing.T) {
	const name = "toggle-handler"
	groupID, _, _, grantA, _ := seedScoredPair(t, name)

	// 切到手动模式并设 active 到成员 A。
	manual := model.GroupModeManual
	if _, err := op.GroupUpdate(groupID, &model.GroupUpdateRequest{
		Mode:         &manual,
		ActiveItemID: &[]int{0}[0],
	}, context.Background()); err != nil {
		t.Fatalf("切模式失败: %v", err)
	}
	itemA := itemIDOfGrant(t, name, grantA)
	activeA := itemA
	if _, err := op.GroupUpdate(groupID, &model.GroupUpdateRequest{
		ActiveItemID: &activeA,
	}, context.Background()); err != nil {
		t.Fatalf("设 active 失败: %v", err)
	}

	// 通过真实 handler 禁用成员 A。
	rec := callToggleItemEnabled(t, groupID, itemA, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("handler 响应码 = %d, 想要 200: %s", rec.Code, rec.Body.String())
	}

	// 响应结构: groupResponse 含 runtime。
	var resp struct {
		Code int `json:"code"`
		Data struct {
			ID      int               `json:"id"`
			Items   []model.GroupItem `json:"items"`
			Runtime relay.RouteState  `json:"runtime"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v: %s", err, rec.Body.String())
	}
	if resp.Code != http.StatusOK {
		t.Fatalf("响应 code = %d, 想要 200: %s", resp.Code, rec.Body.String())
	}
	if resp.Data.ID != groupID {
		t.Fatalf("响应分组 ID = %d, 想要 %d", resp.Data.ID, groupID)
	}
	// 响应中被禁用成员的 enabled=false 且 available=false。
	for _, item := range resp.Data.Items {
		if item.ID == itemA {
			if item.Enabled {
				t.Fatalf("响应 item.enabled = true, 想要 false")
			}
			if item.Available {
				t.Fatalf("响应 item.available = true, 想要 false")
			}
		}
	}
	// Runtime 存在且非零值(分组 ID 匹配)。
	if resp.Data.Runtime.GroupID != groupID {
		t.Fatalf("响应 runtime.GroupID = %d, 想要 %d", resp.Data.Runtime.GroupID, groupID)
	}

	// DB: enabled=false。
	var row model.GroupItem
	db.GetDB().First(&row, itemA)
	if row.Enabled {
		t.Fatalf("DB enabled = true, 想要 false")
	}

	// 手动模式 ActiveItemID 已清零: handler 的 op 层在禁用时清 ActiveItemID。
	loaded, err := op.GroupGetByName(name)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ActiveItemID != 0 {
		t.Fatalf("ActiveItemID = %d, 想要 0(禁用当前成员后清空)", loaded.ActiveItemID)
	}

	// 缓存: 成员仍在, Available=false。
	found := false
	for _, item := range loaded.Items {
		if item.ID == itemA {
			found = true
			if item.Available {
				t.Fatalf("缓存 Available = true, 想要 false")
			}
		}
	}
	if !found {
		t.Fatalf("被禁用成员从缓存消失")
	}
}

// TestToggleGroupItemEnabledHandlerForeignItemRejected 通过真实 handler 证明:
// 不属于该分组的 item_id 返回错误, 不改路由/缓存。
func TestToggleGroupItemEnabledHandlerForeignItemRejected(t *testing.T) {
	const nameA = "toggle-foreign-a"
	const nameB = "toggle-foreign-b"
	_, _, _, grantA, _ := seedScoredPair(t, nameA)
	groupB, _, _, grantB, _ := seedScoredPair(t, nameB)
	itemA := itemIDOfGrant(t, nameA, grantA)
	itemB := itemIDOfGrant(t, nameB, grantB)

	// 通过真实 handler 尝试用 A 的 itemID 在 B 上禁用。
	rec := callToggleItemEnabled(t, groupB, itemA, false)
	if rec.Code == http.StatusOK {
		t.Fatalf("跨分组成员被误接受: %d %s", rec.Code, rec.Body.String())
	}

	// B 的成员未被改。
	var row model.GroupItem
	db.GetDB().First(&row, itemB)
	if !row.Enabled {
		t.Fatalf("B 成员被误禁用")
	}
}

// TestToggleGroupItemEnabledHandlerMissingItemRejected 不存在的成员 ID 返回错误, 不改路由。
func TestToggleGroupItemEnabledHandlerMissingItemRejected(t *testing.T) {
	const name = "toggle-missing"
	groupID, _, _, _, _ := seedScoredPair(t, name)

	rec := callToggleItemEnabled(t, groupID, 99999, false)
	if rec.Code == http.StatusOK {
		t.Fatalf("不存在的成员被误接受: %d %s", rec.Code, rec.Body.String())
	}
}

// TestEnableChannelHandlerProductionRoundTrip 通过真实 enableChannel handler 禁用渠道,
// 验证后续 Forward 不发送到被禁用渠道的成员, 改选存活成员。
// 这是生产 handler round-trip 测试(非竞态互斥证明): enableChannel handler 持 GroupGateLock
// 完成 DB→cache 发布后, 后续 Forward 的复核读到 Enabled=false。
func TestEnableChannelHandlerProductionRoundTrip(t *testing.T) {
	const name = "enable-handler-roundtrip"
	_, channelA, _, _, _ := seedScoredPair(t, name)

	// 转发一次让分组路由建立。
	forwardOK(t, name)

	// 通过真实 enableChannel handler 禁用渠道 A。
	ginContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginContext.Request = httptest.NewRequest(http.MethodPost, "/api/v1/channel/enabled",
		bytes.NewBufferString(`{"id":`+strconv.Itoa(channelA)+`,"enabled":false}`))
	ginContext.Request.Header.Set("Content-Type", "application/json")
	enableChannel(ginContext)

	// 刷新缓存使 Available 反映渠道禁用。
	if err := op.InitCache(); err != nil {
		t.Fatalf("刷新缓存失败: %v", err)
	}

	// 后续转发: 应跳过被禁用渠道 A 的成员, 改选渠道 B 的成员。
	forwardOK(t, name)

	// 验证渠道 A 的成员 Available=false: Forward 复核跳过它。
	group, err := op.GroupGetByName(name)
	if err != nil {
		t.Fatalf("读分组失败: %v", err)
	}
	for _, item := range group.Items {
		if item.ChannelID == channelA && item.Available {
			t.Fatalf("被禁用渠道 A 的成员仍 Available=true")
		}
	}
}
