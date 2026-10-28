package pool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"chinacpa/internal/model"
	"chinacpa/internal/provider"
	"chinacpa/internal/pubmodel"
	"chinacpa/internal/store"
)

// newTestPool 构造一个带两个同 provider 账号的池（UID 排序后 a1<a2）。
func newTestPool(t *testing.T) (*Pool, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	for _, uid := range []string{"a2", "a1"} {
		if err := st.SaveAccount(&model.Account{Provider: "test", UID: uid}); err != nil {
			t.Fatalf("save account: %v", err)
		}
	}
	p, err := New(st, nil)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	return p, st
}

// testPubManager 从临时文件加载发布模型管理器。
func testPubManager(t *testing.T, models []*pubmodel.Model) *pubmodel.Manager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.json")
	raw := []byte(`{"models":[{"id":"pub","channels":[]}]}`)
	if len(models) > 0 {
		b, err := marshalConfig(models)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		raw = b
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	m, err := pubmodel.Load(path, "")
	if err != nil {
		t.Fatalf("load pubmodel: %v", err)
	}
	return m
}

func marshalConfig(models []*pubmodel.Model) ([]byte, error) {
	type cfg struct {
		Models []*pubmodel.Model `json:"models"`
	}
	return json.Marshal(cfg{Models: models})
}

// TestPickRoundRobin sessionKey 为空时按轮询分摊到不同账号。
func TestPickRoundRobin(t *testing.T) {
	p, _ := newTestPool(t)
	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		a, err := p.Pick("test", "m1", "")
		if err != nil {
			t.Fatalf("pick: %v", err)
		}
		seen[a.UID]++
	}
	if len(seen) != 2 {
		t.Fatalf("期望轮询到 2 个账号，实际 %d 个: %v", len(seen), seen)
	}
	for uid, n := range seen {
		if n < 2 {
			t.Fatalf("账号 %s 分配过少: %d", uid, n)
		}
	}
}

// TestPickSticky 同一 sessionKey 固定同一账号。
func TestPickSticky(t *testing.T) {
	p, _ := newTestPool(t)
	a1, _ := p.Pick("test", "m1", "sess-1")
	for i := 0; i < 5; i++ {
		a, err := p.Pick("test", "m1", "sess-1")
		if err != nil {
			t.Fatalf("pick: %v", err)
		}
		if a.UID != a1.UID {
			t.Fatalf("黏性失效：期望 %s 实际 %s", a1.UID, a.UID)
		}
	}
	other := map[string]bool{}
	for i := 0; i < 4; i++ {
		a, _ := p.Pick("test", "m1", "sess-x"+string(rune('a'+i)))
		other[a.UID] = true
	}
	if len(other) < 2 {
		t.Fatalf("不同会话应覆盖多个账号，实际 %v", other)
	}
}

// TestMarkErrorNeedsRelogin 凭证失效标记账号并停止调度。
func TestMarkErrorNeedsRelogin(t *testing.T) {
	p, _ := newTestPool(t)
	// 两个账号都标记为需重新登录
	for _, a := range p.Accounts() {
		p.MarkError(a, "m1", &provider.Error{Kind: provider.ErrAuth, Msg: "expired"})
	}
	if !p.Accounts()[0].NeedsRelogin {
		t.Fatal("期望账号被标记需重新登录")
	}
	if _, err := p.Pick("test", "m1", ""); err == nil {
		t.Fatal("全部需重新登录的账号不应再被选中")
	}
	// 恢复后重新可用
	if !p.ClearNeedsRelogin("test", "a1") {
		t.Fatal("ClearNeedsRelogin 应返回 true")
	}
	if _, err := p.Pick("test", "m1", ""); err != nil {
		t.Fatalf("恢复后应可再选: %v", err)
	}
}

// TestAddAccountClearsRelogin 重新导入账号时清除标记。
func TestAddAccountClearsRelogin(t *testing.T) {
	p, _ := newTestPool(t)
	a, _ := p.Pick("test", "m1", "")
	p.MarkError(a, "m1", &provider.Error{Kind: provider.ErrAuth, Msg: "expired"})
	if !a.NeedsRelogin {
		t.Fatal("前置：需标记")
	}
	re := &model.Account{Provider: "test", UID: a.UID, AccessToken: "new"}
	if err := p.AddAccount(re); err != nil {
		t.Fatalf("add account: %v", err)
	}
	if re.NeedsRelogin {
		t.Fatal("重新导入应清除需重新登录标记")
	}
}

// stubProvider 最小 Provider 实现（路由测试只需要 ID）。
type stubProvider struct{ id string }

func (s *stubProvider) ID() string { return s.id }
func (s *stubProvider) DisplayName() string {
	return s.id
}
func (s *stubProvider) StartLogin(ctx context.Context, callbackBase string) (*model.LoginSession, error) {
	return nil, nil
}
func (s *stubProvider) PollLogin(ctx context.Context, sess *model.LoginSession) (*model.PollResult, error) {
	return nil, nil
}
func (s *stubProvider) ImportCredential(ctx context.Context, raw string) (*model.Account, error) {
	return nil, nil
}
func (s *stubProvider) RefreshToken(ctx context.Context, a *model.Account) (bool, error) {
	return false, nil
}
func (s *stubProvider) Quota(ctx context.Context, a *model.Account) (*model.QuotaInfo, error) {
	return nil, nil
}
func (s *stubProvider) Checkin(ctx context.Context, a *model.Account) (*model.CheckinResult, error) {
	return nil, nil
}
func (s *stubProvider) Models(ctx context.Context, a *model.Account) ([]model.ModelInfo, error) {
	return nil, nil
}
func (s *stubProvider) ChatStream(ctx context.Context, a *model.Account, modelName string, body []byte) (*provider.ChatResult, error) {
	return nil, nil
}

// TestResolveRouteRoundRobin round_robin 渠道轮转起点。
func TestResolveRouteRoundRobin(t *testing.T) {
	p, _ := newTestPool(t)
	p.provs = map[string]provider.Provider{"t1": &stubProvider{"t1"}, "t2": &stubProvider{"t2"}}
	ctx := context.Background()
	m := testPubManager(t, []*pubmodel.Model{{
		ID: "pub", Strategy: "round_robin",
		Channels: []pubmodel.Channel{{Provider: "t1", Model: "m"}, {Provider: "t2", Model: "m"}},
	}})
	p.published = m
	heads := map[string]bool{}
	for i := 0; i < 4; i++ {
		r, err := p.ResolveRoute(ctx, "pub")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		heads[r.Channels[0].PID] = true
	}
	if len(heads) < 2 {
		t.Fatalf("round_robin 应产生轮转起点，实际 %v", heads)
	}
}

// TestResolveRoutePriority 默认优先级保持配置顺序。
func TestResolveRoutePriority(t *testing.T) {
	p, _ := newTestPool(t)
	p.provs = map[string]provider.Provider{"t1": &stubProvider{"t1"}, "t2": &stubProvider{"t2"}}
	ctx := context.Background()
	m := testPubManager(t, []*pubmodel.Model{{
		ID: "pub",
		Channels: []pubmodel.Channel{{Provider: "t1", Model: "m"}, {Provider: "t2", Model: "m"}},
	}})
	p.published = m
	for i := 0; i < 4; i++ {
		r, err := p.ResolveRoute(ctx, "pub")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if r.Channels[0].PID != "t1" {
			t.Fatalf("priority 应固定从第 1 个渠道开始，实际 %s", r.Channels[0].PID)
		}
	}
}

// TestPickLeastInflight 实时负载路由：繁忙账号（inflight 高）被跳过，选最空闲的。
func TestPickLeastInflight(t *testing.T) {
	p, _ := newTestPool(t)
	// 给 a1 压上 5 个进行中请求，a2 空闲
	p.Acquire(&model.Account{Provider: "test", UID: "a1"})
	p.Acquire(&model.Account{Provider: "test", UID: "a1"})
	p.Acquire(&model.Account{Provider: "test", UID: "a1"})
	p.Acquire(&model.Account{Provider: "test", UID: "a1"})
	p.Acquire(&model.Account{Provider: "test", UID: "a1"})
	// 连续选 6 次，应全部命中空闲的 a2
	for i := 0; i < 6; i++ {
		a, err := p.Pick("test", "m1", "")
		if err != nil {
			t.Fatalf("pick: %v", err)
		}
		if a.UID != "a2" {
			t.Fatalf("应选最空闲账号 a2，实际 %s", a.UID)
		}
	}
	// 释放 a1 一个后，a1 仍比 a2 忙（4 vs 0），继续选 a2
	p.Release(&model.Account{Provider: "test", UID: "a1"})
	a, _ := p.Pick("test", "m1", "")
	if a.UID != "a2" {
		t.Fatalf("a1 仍忙，应选 a2，实际 %s", a.UID)
	}
	// 全部释放后两者负载相同，应能分配到 a1
	p.Release(&model.Account{Provider: "test", UID: "a1"})
	p.Release(&model.Account{Provider: "test", UID: "a1"})
	p.Release(&model.Account{Provider: "test", UID: "a1"})
	p.Release(&model.Account{Provider: "test", UID: "a1"})
	found := false
	for i := 0; i < 10; i++ {
		a, _ := p.Pick("test", "m1", "")
		if a.UID == "a1" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("负载相同后应能选到 a1（轮询兜底）")
	}
}

// TestAcquireRelease 计数正确增减。
func TestAcquireRelease(t *testing.T) {
	p, _ := newTestPool(t)
	a := &model.Account{Provider: "test", UID: "a1"}
	if n := p.Inflight("test", "a1"); n != 0 {
		t.Fatalf("初始应 0，实际 %d", n)
	}
	p.Acquire(a)
	p.Acquire(a)
	if n := p.Inflight("test", "a1"); n != 2 {
		t.Fatalf("acquire 后应 2，实际 %d", n)
	}
	p.Release(a)
	p.Release(a)
	if n := p.Inflight("test", "a1"); n != 0 {
		t.Fatalf("release 后应 0，实际 %d", n)
	}
	// 过度 release 不应变负数
	p.Release(a)
	if n := p.Inflight("test", "a1"); n != 0 {
		t.Fatalf("过度 release 应保持 0，实际 %d", n)
	}
}

func TestLiveLinks(t *testing.T) {
	p, _ := newTestPool(t)
	if got := p.Live(); len(got) != 0 {
		t.Fatalf("初始应空，实际 %+v", got)
	}
	p.LiveUp("zcode", "workbuddy")
	p.LiveUp("zcode", "workbuddy")
	p.LiveUp("curl", "trae")
	links := p.Live()
	if len(links) != 2 {
		t.Fatalf("应 2 条链路，实际 %+v", links)
	}
	// 按 count 降序：zcode|workbuddy(2) 在前
	if links[0].Client != "zcode" || links[0].Provider != "workbuddy" || links[0].Count != 2 {
		t.Fatalf("首位应 zcode→workbuddy×2，实际 %+v", links[0])
	}
	if links[1].Client != "curl" || links[1].Count != 1 {
		t.Fatalf("次位应 curl→trae×1，实际 %+v", links[1])
	}
	p.LiveDown("zcode", "workbuddy")
	p.LiveDown("zcode", "workbuddy")
	p.LiveDown("curl", "trae")
	// 空 client 归入「未知」且能正确配对释放
	p.LiveUp("", "qwen")
	if got := p.Live(); len(got) != 1 || got[0].Client != "未知" {
		t.Fatalf("空 client 应归入未知，实际 %+v", got)
	}
	p.LiveDown("", "qwen")
	if got := p.Live(); len(got) != 0 {
		t.Fatalf("全部释放后应空，实际 %+v", got)
	}
	// 过度 LiveDown 不应变负
	p.LiveDown("zcode", "workbuddy")
	if got := p.Live(); len(got) != 0 {
		t.Fatalf("过度释放应保持空，实际 %+v", got)
	}
}
