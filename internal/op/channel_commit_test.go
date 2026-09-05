package op

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
)

// 本文件证明渠道写操作的提交边界: 级联已提交但缓存刷新失败时,
// 必须以类型化错误携带提交事实(受影响分组与存活成员), 而不是让级联影响随错误一起丢失。

// commitSeedFixture 汇聚用例所需的主键: 主渠道两个授权、旁渠道一个授权、分组挂三个成员。
type commitSeedFixture struct {
	channelID    int
	grantA       int // 主渠道待删授权。
	grantB       int // 主渠道保留授权。
	grantX       int // 旁渠道授权, 验证存活成员含其他渠道。
	groupID      int
	itemA, itemB int
	itemX        int
}

// seedCommitChannel 建立主渠道(两授权)+旁渠道(一授权), 分组挂满三个成员, 并装载渠道缓存。
func seedCommitChannel(t *testing.T) commitSeedFixture {
	t.Helper()
	dbConn := db.GetDB()
	// 用例共享一个库: 先按子表到父表的顺序清场, 避免渠道名唯一约束与上一用例的残留互相干扰。
	for _, table := range []string{"groups", "group_items", "channel_grants", "channel_models", "channel_keys", "channels"} {
		if err := dbConn.Exec("DELETE FROM " + table).Error; err != nil {
			t.Fatalf("清理表 %s 失败: %v", table, err)
		}
	}
	seed := commitSeedFixture{}

	master := model.Channel{ChannelConfig: model.ChannelConfig{Name: "commit-master", Enabled: true, BaseURL: "http://master.example"}}
	if err := dbConn.Create(&master).Error; err != nil {
		t.Fatalf("建主渠道失败: %v", err)
	}
	seed.channelID = master.ID
	grants := make([]int, 0, 2)
	for i := 0; i < 2; i++ {
		key := model.ChannelKey{ChannelID: master.ID, ChannelKeyConfig: model.ChannelKeyConfig{Name: fmt.Sprintf("k%d", i), Key: "sk", Enabled: true}}
		if err := dbConn.Create(&key).Error; err != nil {
			t.Fatalf("建凭据失败: %v", err)
		}
		channelModel := model.ChannelModel{ChannelID: master.ID, Name: fmt.Sprintf("m%d", i)}
		if err := dbConn.Create(&channelModel).Error; err != nil {
			t.Fatalf("建模型失败: %v", err)
		}
		grant := model.ChannelGrant{ChannelModelID: channelModel.ID, ChannelKeyID: key.ID, Protocols: model.ProtocolOpenAIChatCompletion}
		if err := dbConn.Create(&grant).Error; err != nil {
			t.Fatalf("建授权失败: %v", err)
		}
		grants = append(grants, grant.ID)
	}
	seed.grantA, seed.grantB = grants[0], grants[1]

	side := model.Channel{ChannelConfig: model.ChannelConfig{Name: "commit-side", Enabled: true, BaseURL: "http://side.example"}}
	if err := dbConn.Create(&side).Error; err != nil {
		t.Fatalf("建旁渠道失败: %v", err)
	}
	sideKey := model.ChannelKey{ChannelID: side.ID, ChannelKeyConfig: model.ChannelKeyConfig{Name: "kx", Key: "sk", Enabled: true}}
	if err := dbConn.Create(&sideKey).Error; err != nil {
		t.Fatalf("建旁凭据失败: %v", err)
	}
	sideModel := model.ChannelModel{ChannelID: side.ID, Name: "mx"}
	if err := dbConn.Create(&sideModel).Error; err != nil {
		t.Fatalf("建旁模型失败: %v", err)
	}
	sideGrant := model.ChannelGrant{ChannelModelID: sideModel.ID, ChannelKeyID: sideKey.ID, Protocols: model.ProtocolOpenAIChatCompletion}
	if err := dbConn.Create(&sideGrant).Error; err != nil {
		t.Fatalf("建旁授权失败: %v", err)
	}
	seed.grantX = sideGrant.ID

	group := model.Group{Name: "commit-group", Mode: model.GroupModeScored}
	if err := dbConn.Create(&group).Error; err != nil {
		t.Fatalf("建分组失败: %v", err)
	}
	seed.groupID = group.ID
	for i, grantID := range []int{seed.grantA, seed.grantB, seed.grantX} {
		item := model.GroupItem{GroupID: group.ID, ChannelGrantID: grantID, Priority: i + 1}
		if err := dbConn.Create(&item).Error; err != nil {
			t.Fatalf("建成员失败: %v", err)
		}
		switch grantID {
		case seed.grantA:
			seed.itemA = item.ID
		case seed.grantB:
			seed.itemB = item.ID
		default:
			seed.itemX = item.ID
		}
	}
	if err := channelRefreshCache(context.Background()); err != nil {
		t.Fatalf("刷新渠道缓存失败: %v", err)
	}
	return seed
}

// withRefreshFailure 注入提交后刷新失败; 用例结束恢复生产实现。
func withRefreshFailure(t *testing.T) {
	t.Helper()
	previous := refreshGroupsAfterCommit
	refreshGroupsAfterCommit = func(context.Context) error { return errors.New("refresh boom") }
	t.Cleanup(func() { refreshGroupsAfterCommit = previous })
}

// assertCommittedCascade 断言级联确已提交: 授权 A 与其成员行消失, 指定存活成员仍在, 分组行仍在。
func assertCommittedCascade(t *testing.T, seed commitSeedFixture, wantSurvivors []int) {
	t.Helper()
	dbConn := db.GetDB()
	var grantCount, itemCount int64
	dbConn.Model(&model.ChannelGrant{}).Where("id = ?", seed.grantA).Count(&grantCount)
	if grantCount != 0 {
		t.Fatalf("授权 A 未被级联删除")
	}
	dbConn.Model(&model.GroupItem{}).Where("id = ?", seed.itemA).Count(&itemCount)
	if itemCount != 0 {
		t.Fatalf("成员 A 未被级联删除")
	}
	dbConn.Model(&model.GroupItem{}).Where("id IN ?", wantSurvivors).Count(&itemCount)
	if itemCount != int64(len(wantSurvivors)) {
		t.Fatalf("存活成员数 = %d, 想要 %d", itemCount, len(wantSurvivors))
	}
	var groupCount int64
	dbConn.Model(&model.Group{}).Where("id = ?", seed.groupID).Count(&groupCount)
	if groupCount != 1 {
		t.Fatalf("分组行被误删")
	}
}

// assertExactDeltas 断言提交事实只含受影响分组, 存活成员含其他渠道且有序。
func assertExactDeltas(t *testing.T, seed commitSeedFixture, mutation *ChannelMutation, want []int) {
	t.Helper()
	if mutation == nil || len(mutation.GroupDeltas) != 1 {
		t.Fatalf("提交事实 = %+v, 想要恰一个受影响分组", mutation)
	}
	delta := mutation.GroupDeltas[0]
	if delta.GroupID != seed.groupID {
		t.Fatalf("受影响分组 = %d, 想要 %d", delta.GroupID, seed.groupID)
	}
	if fmt.Sprint(delta.ItemIDs) != fmt.Sprint(want) {
		t.Fatalf("存活成员 = %v, 想要 %v", delta.ItemIDs, want)
	}
}

// 提交后刷新失败: 级联已不可回滚, 必须以 PostCommitError 携带精确提交事实。
// 删整个渠道会带走其全部授权: 成员 A、B 都消失, 只剩旁渠道的成员 X。
func TestChannelDelPostCommitRefreshFailure(t *testing.T) {
	seed := seedCommitChannel(t)
	withRefreshFailure(t)

	mutation, err := ChannelDel(seed.channelID, context.Background())
	if err == nil {
		t.Fatalf("提交后刷新失败未返回错误")
	}
	var post *PostCommitError
	if !errors.As(err, &post) {
		t.Fatalf("错误不是 PostCommitError: %T", err)
	}
	if errors.Unwrap(err) == nil {
		t.Fatalf("PostCommitError 未暴露刷新失败原因")
	}
	assertCommittedCascade(t, seed, []int{seed.itemX})
	assertExactDeltas(t, seed, mutation, []int{seed.itemX})
}

// 更新入口同样以提交事实区分: 提交后刷新失败不吞级联影响。
// 只移除授权 A 的组合: 成员 A 消失, 同渠道保留授权的成员 B 与旁渠道成员 X 存活。
func TestChannelUpdatePostCommitRefreshFailure(t *testing.T) {
	seed := seedCommitChannel(t)
	withRefreshFailure(t)

	// 从缓存副本构造编辑表单, 只去掉授权 A 的组合: 授权 A 及其成员被级联删除。
	channel, ok := channelCache.Get(seed.channelID)
	if !ok {
		t.Fatalf("渠道缓存缺失")
	}
	detail := channelDetail(channel)
	kept := make([]model.ChannelGrantConfig, 0, len(detail.Grants))
	for _, grant := range detail.Grants {
		if grant.ModelName == "m0" && grant.KeyName == "k0" {
			continue
		}
		kept = append(kept, grant)
	}
	detail.Grants = kept

	_, mutation, err := ChannelUpdate(&detail, context.Background())
	if err == nil {
		t.Fatalf("提交后刷新失败未返回错误")
	}
	var post *PostCommitError
	if !errors.As(err, &post) {
		t.Fatalf("错误不是 PostCommitError: %T", err)
	}
	assertCommittedCascade(t, seed, []int{seed.itemB, seed.itemX})
	assertExactDeltas(t, seed, mutation, []int{seed.itemB, seed.itemX})
}

// 提交前失败不携带任何提交事实: 未发生的级联不得让路由前进代数。
func TestChannelPreCommitFailureCarriesNoMutation(t *testing.T) {
	seed := seedCommitChannel(t)

	mutation, err := ChannelDel(seed.channelID+999, context.Background())
	if err == nil || mutation != nil {
		t.Fatalf("提交前失败应只返回错误: mutation=%+v err=%v", mutation, err)
	}
	var post *PostCommitError
	if errors.As(err, &post) {
		t.Fatalf("提交前失败不得是 PostCommitError")
	}
	// 库内无任何变化。
	var itemCount int64
	db.GetDB().Model(&model.GroupItem{}).Where("group_id = ?", seed.groupID).Count(&itemCount)
	if itemCount != 3 {
		t.Fatalf("提交前失败改变了库: 成员数 = %d, 想要 3", itemCount)
	}
}
