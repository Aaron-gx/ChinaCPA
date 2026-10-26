package panel

import (
	"bufio"
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"chinacpa/internal/config"
	"chinacpa/internal/pool"
	"chinacpa/internal/store"
)

func TestPanelSSEBroadcast(t *testing.T) {
	tmp := t.TempDir()
	st, err := store.Open(tmp)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	cfg := &config.Config{
		PanelPassword: "testpassword",
		DataDir:       tmp,
	}
	pl, _ := pool.New(st, nil)
	p := New(cfg, pl, st, nil, nil, nil, nil)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth", p.handleAuth)
	mux.HandleFunc("/api/stream/events", p.authed(p.handleStreamEvents))

	ts := httptest.NewServer(mux)
	defer ts.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	// 1. 登录拿 Cookie
	loginResp, err := client.Post(ts.URL+"/api/auth", "application/json", strings.NewReader(`{"password":"testpassword"}`))
	if err != nil {
		t.Fatalf("auth failed: %v", err)
	}
	loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("login status: %d", loginResp.StatusCode)
	}

	// 2. 连接 SSE
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/stream/events", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("connect sse: %v", err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)

	// 读首包 connected
	firstLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read first line: %v", err)
	}
	if !strings.Contains(firstLine, "event: connected") {
		t.Fatalf("expected connected event, got: %q", firstLine)
	}

	// 3. 触发广播
	go func() {
		time.Sleep(50 * time.Millisecond)
		p.Broadcast("quota")
	}()

	// 读第二包 update
	gotUpdate := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read line: %v", err)
		}
		if strings.Contains(line, "event: update") {
			gotUpdate = true
			break
		}
	}

	if !gotUpdate {
		t.Fatal("did not receive update event via SSE")
	}
}
