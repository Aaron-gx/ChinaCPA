package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"chinacpa/internal/model"
	"chinacpa/internal/pool"
	"chinacpa/internal/provider"
	"chinacpa/internal/store"
)

// fakeProvider 可控的假平台：按脚本依次返回错误或成功流，并记录调用次数。
type fakeProvider struct {
	id    string
	calls int32
	// script 每次调用的行为（超出长度时重复最后一个）
	script []fakeStep
}

type fakeStep struct {
	body io.ReadCloser
	res  *provider.ChatResult
	err  error
}

func (f *fakeProvider) ID() string          { return f.id }
func (f *fakeProvider) DisplayName() string { return f.id }

func (f *fakeProvider) StartLogin(context.Context, string) (*model.LoginSession, error) {
	return nil, provider.ErrNotSupported
}
func (f *fakeProvider) PollLogin(context.Context, *model.LoginSession) (*model.PollResult, error) {
	return nil, provider.ErrNotSupported
}
func (f *fakeProvider) ImportCredential(context.Context, string) (*model.Account, error) {
	return nil, provider.ErrNotSupported
}
func (f *fakeProvider) RefreshToken(context.Context, *model.Account) (bool, error) {
	return false, nil
}
func (f *fakeProvider) Quota(context.Context, *model.Account) (*model.QuotaInfo, error) {
	return nil, provider.ErrNotSupported
}
func (f *fakeProvider) Checkin(context.Context, *model.Account) (*model.CheckinResult, error) {
	return nil, provider.ErrNotSupported
}
func (f *fakeProvider) Models(context.Context, *model.Account) ([]model.ModelInfo, error) {
	return nil, nil
}

func (f *fakeProvider) ChatStream(_ context.Context, _ *model.Account, _ string, _ []byte) (*provider.ChatResult, error) {
	n := int(atomic.AddInt32(&f.calls, 1)) - 1
	step := f.script[len(f.script)-1]
	if n < len(f.script) {
		step = f.script[n]
	}
	if step.err != nil {
		return nil, step.err
	}
	return step.res, nil
}

// newGateway 构造带假 provider 与临时数据目录的网关（sleep 被替换为记录调用）。
func newGateway(t *testing.T, provs ...provider.Provider) (*Gateway, *[]time.Duration) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.SaveAccount(&model.Account{Provider: provs[0].ID(), UID: "u1"}); err != nil {
		t.Fatalf("save account: %v", err)
	}
	p, err := pool.New(st, provs)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	g := New(p, nil)
	var slept []time.Duration
	g.sleep = func(d time.Duration) { slept = append(slept, d) }
	return g, &slept
}

// okResult 返回一个成功的上游流（单条 SSE，含 finish）。
func okResult() *provider.ChatResult {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	return &provider.ChatResult{
		Body:        io.NopCloser(strings.NewReader(sse)),
		ContentType: "text/event-stream",
	}
}

// riskGateResult 上游风控闸门（11128）——按线上形态以 HTTP 400 返回。
func riskGateResult() *provider.ChatResult {
	body := []byte(`{"code":11128,"msg":"Illegal API invocation from an unapproved channel","requestId":"702ee7cde0a8c23c85eec6f435d7aa47"}`)
	return &provider.ChatResult{
		Status:   http.StatusBadRequest,
		RespBody: body,
		Err:      provider.RiskGateError(http.StatusBadRequest),
	}
}

// TestCallRoutedStreamRetryAfterRiskGate 整链撞上风控闸门后应退避一次并整体重跑，
// 第二次成功时请求最终成功，且退避时长符合策略。
func TestCallRoutedStreamRetryAfterRiskGate(t *testing.T) {
	fp := &fakeProvider{id: "wb", script: []fakeStep{
		{res: riskGateResult()}, // 第一轮整链：撞闸门
		{res: okResult()},       // 退避后第二轮：成功
	}}
	g, slept := newGateway(t, fp)
	w := httptest.NewRecorder()
	route := &pool.Route{Public: "m", Channels: []pool.ChannelTarget{{PID: "wb", Model: "m"}}}

	st := g.callRoutedStream(context.Background(), w, route, []byte(`{"model":"m"}`), "chat", "", "test")
	if st == nil {
		t.Fatalf("期望退避后成功，实际失败: %s", w.Body.String())
	}
	defer st.release()
	if len(*slept) != 1 || (*slept)[0] != riskGateRetryDelay {
		t.Fatalf("期望退避一次 %s，实际 %v", riskGateRetryDelay, *slept)
	}
	// 尝试链路应包含两次闸门失败 + 一次成功，便于面板排查
	got := strings.Join(st.attempts, "\n")
	if !strings.Contains(got, "✗") || !strings.Contains(got, "✓") {
		t.Fatalf("attempts 应同时含失败与成功记录: %q", got)
	}
	if !strings.Contains(got, "退避") {
		t.Fatalf("attempts 应记录退避动作: %q", got)
	}
}

// TestCallRoutedStreamNoRetryOnPlainError 非闸门错误不应触发退避重试。
func TestCallRoutedStreamNoRetryOnPlainError(t *testing.T) {
	fp := &fakeProvider{id: "wb", script: []fakeStep{
		{res: &provider.ChatResult{
			Status:   http.StatusBadRequest,
			RespBody: []byte(`{"msg":"bad param"}`),
			Err:      &provider.Error{Kind: provider.ErrClient, Status: http.StatusBadRequest, Msg: "bad param"},
		}},
	}}
	g, slept := newGateway(t, fp)
	w := httptest.NewRecorder()
	route := &pool.Route{Public: "m", Channels: []pool.ChannelTarget{{PID: "wb", Model: "m"}}}

	if st := g.callRoutedStream(context.Background(), w, route, []byte(`{"model":"m"}`), "chat", "", "test"); st != nil {
		st.release()
		t.Fatal("普通 400 不应成功")
	}
	if len(*slept) != 0 {
		t.Fatalf("普通错误不应退避，实际 %v", *slept)
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("应透传上游 400，实际 %d", w.Code)
	}
}

// TestCallRoutedStreamFailoverWithoutRetry 渠道转移成功时不占用退避重试。
func TestCallRoutedStreamFailoverWithoutRetry(t *testing.T) {
	gate := &fakeProvider{id: "wb", script: []fakeStep{{res: riskGateResult()}}}
	backup := &fakeProvider{id: "trae", script: []fakeStep{{res: okResult()}}}
	g, slept := newGateway(t, gate, backup)
	// 第二个渠道也需要有可用账号
	if err := g.pool.AddAccount(&model.Account{Provider: "trae", UID: "u2"}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	w := httptest.NewRecorder()
	route := &pool.Route{Public: "m", Channels: []pool.ChannelTarget{
		{PID: "wb", Model: "m"}, {PID: "trae", Model: "m"},
	}}

	st := g.callRoutedStream(context.Background(), w, route, []byte(`{"model":"m"}`), "chat", "", "test")
	if st == nil {
		t.Fatalf("期望渠道转移成功，实际: %s", w.Body.String())
	}
	defer st.release()
	if len(*slept) != 0 {
		t.Fatalf("渠道转移已成功，不应再退避，实际 %v", *slept)
	}
	if atomic.LoadInt32(&gate.calls) != 1 || atomic.LoadInt32(&backup.calls) != 1 {
		t.Fatalf("调用次数异常: gate=%d backup=%d", gate.calls, backup.calls)
	}
}
