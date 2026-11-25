package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"

	"chinacpa/internal/config"
	"chinacpa/internal/model"
	"chinacpa/internal/pool"
	"chinacpa/internal/provider"
	"chinacpa/internal/store"
)

type checkinTestProvider struct {
	mu       sync.Mutex
	checkins map[string]int
	quota    chan struct{}
	result   *model.CheckinResult
	err      error
}

func (p *checkinTestProvider) ID() string          { return "test" }
func (p *checkinTestProvider) DisplayName() string { return "test" }
func (p *checkinTestProvider) StartLogin(context.Context, string) (*model.LoginSession, error) {
	return nil, nil
}
func (p *checkinTestProvider) PollLogin(context.Context, *model.LoginSession) (*model.PollResult, error) {
	return nil, nil
}
func (p *checkinTestProvider) ImportCredential(context.Context, string) (*model.Account, error) {
	return nil, nil
}
func (p *checkinTestProvider) RefreshToken(context.Context, *model.Account) (bool, error) {
	return false, nil
}
func (p *checkinTestProvider) Quota(_ context.Context, a *model.Account) (*model.QuotaInfo, error) {
	return &model.QuotaInfo{Provider: a.Provider, UID: a.UID}, nil
}
func (p *checkinTestProvider) Checkin(_ context.Context, a *model.Account) (*model.CheckinResult, error) {
	p.mu.Lock()
	p.checkins[a.UID]++
	p.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	if p.result != nil || p.err != nil {
		return p.result, p.err
	}
	return &model.CheckinResult{OK: true}, nil
}
func (p *checkinTestProvider) Models(context.Context, *model.Account) ([]model.ModelInfo, error) {
	return nil, nil
}
func (p *checkinTestProvider) ChatStream(context.Context, *model.Account, string, []byte) (*provider.ChatResult, error) {
	return &provider.ChatResult{}, nil
}

func TestCheckinAccountDeduplicatesRequestsAndProcessesEachAccount(t *testing.T) {
	pr := &checkinTestProvider{checkins: map[string]int{}}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pr.quota = make(chan struct{}, 4)
	st.OnQuotaChanged = func(*model.QuotaInfo) { pr.quota <- struct{}{} }
	pool, err := pool.New(st, []provider.Provider{pr})
	if err != nil {
		t.Fatal(err)
	}
	for _, uid := range []string{"a1", "a2"} {
		if err := pool.AddAccount(&model.Account{Provider: pr.ID(), UID: uid}); err != nil {
			t.Fatal(err)
		}
	}
	s := New(config.Default(), pool, st)

	accounts := pool.Accounts()
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if result := s.CheckinAccount(accounts[0]); !result.OK {
				t.Errorf("concurrent checkin failed: %+v", result)
			}
		}()
	}
	if result := s.CheckinAccount(accounts[1]); !result.OK {
		t.Fatalf("second account checkin failed: %+v", result)
	}
	wg.Wait()
	for i := 0; i < 2; i++ {
		select {
		case <-pr.quota:
		case <-time.After(time.Second):
			t.Fatal("successful checkin did not refresh account quota")
		}
	}

	pr.mu.Lock()
	defer pr.mu.Unlock()
	if pr.checkins["a1"] != 1 || pr.checkins["a2"] != 1 {
		t.Fatalf("each account must be checked in once, got %v", pr.checkins)
	}
}

func TestCheckinAccountMarksReloginAndRefreshesQuotaOnlyForConfirmedResults(t *testing.T) {
	for _, tt := range []struct {
		name    string
		result  *model.CheckinResult
		err     error
		relogin bool
		refresh int
	}{
		{name: "unsupported refreshes quota", err: provider.ErrNotSupported, refresh: 1},
		{name: "pending does not refresh", result: &model.CheckinResult{Message: "pending"}},
		{name: "already signed refreshes", result: &model.CheckinResult{OK: true, Already: true}, refresh: 1},
		{name: "invalid credentials require login", err: &provider.Error{Kind: provider.ErrAuth, Msg: "expired"}, relogin: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pr := &checkinTestProvider{checkins: map[string]int{}, result: tt.result, err: tt.err}
			pr.quota = make(chan struct{}, 4)
			st, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			st.OnQuotaChanged = func(*model.QuotaInfo) { pr.quota <- struct{}{} }
			p, err := pool.New(st, []provider.Provider{pr})
			if err != nil {
				t.Fatal(err)
			}
			if err := p.AddAccount(&model.Account{Provider: pr.ID(), UID: "a1"}); err != nil {
				t.Fatal(err)
			}
			a := p.Accounts()[0]
			res := New(config.Default(), p, st).CheckinAccount(a)
			if tt.err != nil && tt.err != provider.ErrNotSupported && res.OK {
				t.Fatalf("expected sign-in failure, got %+v", res)
			}
			if a.NeedsRelogin != tt.relogin {
				t.Fatalf("NeedsRelogin=%t, want %t", a.NeedsRelogin, tt.relogin)
			}
			if tt.refresh > 0 {
				select {
				case <-pr.quota:
				case <-time.After(time.Second):
					t.Fatal("confirmed result did not refresh quota")
				}
			} else {
				select {
				case <-pr.quota:
					t.Fatal("unconfirmed result must not trigger a quota refresh")
				case <-time.After(40 * time.Millisecond):
				}
			}
		})
	}
}
