// media.go 图片 / 视频生成端点：
//
//	POST /v1/images/generations          （OpenAI Images 格式）
//	POST /api/v3/images/generations      （火山方舟 Ark 路径别名）
//	POST /api/v3/contents/generations/tasks  （Ark 视频任务：创建）
//	GET  /api/v3/contents/generations/tasks/{id}（Ark 视频任务：查询）
//	POST /v1/videos + GET /v1/videos/{id}（简化别名）
//
// 路由复用发布模型 / provider:model / 裸名解析；渠道内多账号失败转移。
// 视频为异步任务：创建即返回 task id，后台轮询上游会话，进程内存保存
// （重启丢失进行中任务，接口层与火山 Ark 协议保持一致）。
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"chinacpa/internal/model"
	"chinacpa/internal/pool"
	"chinacpa/internal/provider"
	"chinacpa/internal/reqlog"
	"chinacpa/internal/usage"
)

// registerMedia 挂载媒体端点（由 Register 调用）。
func (g *Gateway) registerMedia(mux *http.ServeMux) {
	mux.HandleFunc("/v1/images/generations", g.auth(g.handleImages))
	mux.HandleFunc("/api/v3/images/generations", g.auth(g.handleImages))
	mux.HandleFunc("/api/v3/contents/generations/tasks", g.auth(g.handleVideoCreate))
	mux.HandleFunc("/api/v3/contents/generations/tasks/", g.auth(g.handleVideoGet))
	mux.HandleFunc("/v1/videos", g.auth(g.handleVideoCreate))
	mux.HandleFunc("/v1/videos/", g.auth(g.handleVideoGet))
}

// ── 图片生成 ─────────────────────────────────────────────

type imagesReq struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	Size           string `json:"size"`
	Ratio          string `json:"ratio"`
	N              int    `json:"n"`
	ResponseFormat string `json:"response_format"`
}

func (g *Gateway) handleImages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST only")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read_body", err.Error())
		return
	}
	var req imagesReq
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_json", "请求体不是合法 JSON")
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeErr(w, http.StatusBadRequest, "missing_prompt", "缺少 prompt")
		return
	}
	route, err := g.pool.ResolveRoute(r.Context(), req.Model)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "model_not_found", err.Error())
		return
	}
	if route.Type == "video" {
		writeErr(w, http.StatusBadRequest, "wrong_endpoint",
			"模型 "+route.Public+" 是视频生成模型：请用 POST /api/v3/contents/generations/tasks（异步任务），不是 /v1/images/generations")
		return
	}
	t0 := time.Now()
	var lastErr error
	var lastAcct *model.Account
	for _, ch := range route.Channels {
		pr, ok := g.pool.Provider(ch.PID)
		if !ok {
			continue
		}
		ig, ok := pr.(provider.ImageGenerator)
		if !ok {
			continue // 该渠道平台不支持图片生成
		}
		res, acct, aerr := g.tryImage(r.Context(), ig, ch, req)
		if res != nil {
			g.bumpChannel(route, ch, true)
			g.pool.MarkSuccess(acct, ch.Model)
			// 按次计费：生成成功即按张数记账（模型单价走发布模型 per_call）
			g.recordMedia(route.Public, ch.PID, acct.UID, int64(len(res.URLs)), "images")
			g.logReqMedia("image", route.Public, ch.PID, acct.UID, true, int64(len(res.URLs)), t0)
			g.writeImagesResp(w, r, res, req)
			return
		}
		// 该渠道失败：记录并继续尝试下一个渠道（跨渠道失败转移）
		lastErr, lastAcct = aerr, acct
		g.bumpChannel(route, ch, false)
		g.logReqMedia("image", route.Public, ch.PID, "", false, 0, t0)
	}
	msg := "所选渠道均不支持图片生成（当前仅豆包 Seedream 提供）"
	if lastErr != nil {
		msg = lastErr.Error()
	}
	if lastAcct != nil {
		_ = lastAcct
	}
	writeErr(w, http.StatusBadGateway, "image_failed", msg)
}

// recordMedia 媒体按次用量记账（图片张数 / 视频条数）。
func (g *Gateway) recordMedia(pubModel, pid, uid string, calls int64, unit string) {
	if calls <= 0 {
		return
	}
	g.usage.Record(usage.Record{
		Provider: pid, Account: uid, Model: pubModel,
		Calls: calls, Unit: unit,
	})
}

// logReqMedia 媒体请求日志（费用按次计费口径）。
func (g *Gateway) logReqMedia(proto, pubModel, pid, uid string, ok bool, calls int64, start time.Time) {
	var cost float64
	if ok && calls > 0 {
		cost = g.usage.CostCalls(pubModel, calls)
	}
	if g.rlog == nil {
		return
	}
	g.rlog.Add(reqlog.Entry{
		Protocol: proto, Model: pubModel, Provider: pid, Account: uid,
		OK: ok, CostCNY: cost, TotalMS: time.Since(start).Milliseconds(),
	})
}

// tryImage 单渠道图片生成：账号轮询失败转移。
func (g *Gateway) tryImage(ctx context.Context, ig provider.ImageGenerator, ch pool.ChannelTarget, req imagesReq) (*provider.ImageResult, *model.Account, error) {
	tried := map[string]bool{}
	var lastErr error
	for {
		a, perr := g.pool.Pick(ch.PID, ch.Model, "")
		if perr != nil {
			break
		}
		if tried[a.UID] {
			break
		}
		tried[a.UID] = true
		if _, terr := g.pool.EnsureFreshToken(ctx, a); terr != nil {
			lastErr = terr
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
		res, cerr := ig.GenerateImage(cctx, a, ch.Model, provider.ImageRequest{
			Prompt: req.Prompt, Ratio: req.Ratio, Size: req.Size, N: req.N,
		})
		cancel()
		if cerr != nil {
			g.pool.MarkError(a, ch.Model, cerr)
			lastErr = cerr
			continue
		}
		if len(res.URLs) == 0 {
			lastErr = fmt.Errorf("上游未返回图片")
			continue
		}
		if req.N > 0 && len(res.URLs) > req.N {
			res.URLs = res.URLs[:req.N]
		}
		return res, a, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("平台 %s 无可用账号", ch.PID)
	}
	return nil, nil, lastErr
}

// writeImagesResp OpenAI Images 格式响应（b64_json 时服务端代取图并内嵌）。
func (g *Gateway) writeImagesResp(w http.ResponseWriter, r *http.Request, res *provider.ImageResult, req imagesReq) {
	type item struct {
		URL     string `json:"url,omitempty"`
		B64JSON string `json:"b64_json,omitempty"`
	}
	data := make([]item, 0, len(res.URLs))
	if req.ResponseFormat == "b64_json" {
		for _, u := range res.URLs {
			if b64, err := fetchAsB64(u); err == nil {
				data = append(data, item{B64JSON: b64})
			} else {
				data = append(data, item{URL: u}) // 取图失败退回 URL
			}
		}
	} else {
		for _, u := range res.URLs {
			data = append(data, item{URL: u})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"created": time.Now().Unix(),
		"data":    data,
		"usage": map[string]any{
			"generated_images": len(data),
			"output_tokens":    len(data) * 1327, // Seedream 单图输出 token 估算（方舟口径）
			"total_tokens":     len(data) * 1327,
		},
	})
}

// fetchAsB64 下载 URL 内容并 base64（带浏览器 UA，图片 CDN 一般无 referer 限制）。
func fetchAsB64(u string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// ── 视频任务（火山方舟 Ark 协议形态）──────────────────────

// videoTask 进程内任务记录。
type videoTask struct {
	ID            string
	Public        string // 对外模型名（记账/计价）
	PID           string
	AcctUID       string
	Acct          *model.Account
	Handle        *provider.VideoTaskHandle
	Prompt        string
	Status        string // queued / running / succeeded / failed
	VideoURL      string
	CoverURL      string
	Unwatermarked bool
	Error         string
	CreatedAt     int64
	UpdatedAt     int64
}

// videoTaskStore 任务表（内存态；重启即失，与上游会话无持久化对齐）。
type videoTaskStore struct {
	mu    sync.Mutex
	tasks map[string]*videoTask
}

func (s *videoTaskStore) put(t *videoTask) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[t.ID] = t
	// 顺带清理 6 小时前的旧任务
	cutoff := time.Now().Add(-6 * time.Hour).Unix()
	for id, old := range s.tasks {
		if old.CreatedAt < cutoff {
			delete(s.tasks, id)
		}
	}
}

func (s *videoTaskStore) get(id string) *videoTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return nil
	}
	// 返回深拷贝快照：调用方（videoTaskView）在锁外读，避免与后台轮询 update 竞态
	cp := *t
	return &cp
}

func (s *videoTaskStore) update(id string, fn func(t *videoTask)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.tasks[id]; ok {
		fn(t)
		t.UpdatedAt = time.Now().Unix()
	}
}

// ensureVideoTasks 惰性初始化任务表。
func (g *Gateway) ensureVideoTasks() {
	if g.videoTasks == nil {
		g.videoTasks = &videoTaskStore{tasks: map[string]*videoTask{}}
	}
}

// arkVideoCreateReq Ark 创建任务请求体（content 数组 + 平铺别名）。
type arkVideoCreateReq struct {
	Model    string `json:"model"`
	Prompt   string `json:"prompt"`
	Ratio    string `json:"ratio"`
	Duration int    `json:"duration"`
	Content  []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url"`
	} `json:"content"`
}

// videoFlagRe Ark 文本命令参数：--ratio 16:9 --duration 5 等。
var videoFlagRe = regexp.MustCompile(`--([a-zA-Z_]+)(?:[=\s]+([^\s]+))?`)

// parseVideoFlags 从提示词中剥离 Ark 风格命令参数（--ratio 16:9 --duration 5 等），
// 回填 ratio/duration；未知参数保留原样。返回净化后的提示词。
func parseVideoFlags(prompt string, ratio *string, duration *int) string {
	out := videoFlagRe.ReplaceAllStringFunc(prompt, func(m string) string {
		sub := videoFlagRe.FindStringSubmatch(m)
		key := strings.ToLower(sub[1])
		val := sub[2]
		switch key {
		case "ratio", "aspectratio", "ar":
			if val != "" {
				*ratio = val
			}
		case "duration":
			if d, err := strconv.Atoi(val); err == nil {
				*duration = d
			}
		case "resolution", "fps", "watermark", "seed", "camerafixed":
			// 记录但当前上游通道忽略（resolution 由模型/比例决定）
		default:
			return m // 未知参数保留原样
		}
		return ""
	})
	return strings.TrimSpace(out)
}

func (g *Gateway) handleVideoCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST only")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read_body", err.Error())
		return
	}
	var req arkVideoCreateReq
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_json", "请求体不是合法 JSON")
		return
	}
	// content 数组归一：文本段拼接，参考图暂不支持
	var texts []string
	hasImage := false
	for _, c := range req.Content {
		switch c.Type {
		case "text", "":
			if c.Text != "" {
				texts = append(texts, c.Text)
			}
		case "image_url":
			if c.ImageURL != nil && c.ImageURL.URL != "" {
				hasImage = true
			}
		}
	}
	prompt := strings.Join(texts, " ")
	if prompt == "" {
		prompt = req.Prompt
	}
	if hasImage {
		writeErr(w, http.StatusBadRequest, "unsupported", "图生视频 / 参考图暂不支持（上游附件上传通道未接入），当前仅文生视频")
		return
	}
	if strings.TrimSpace(prompt) == "" {
		writeErr(w, http.StatusBadRequest, "missing_prompt", "缺少 prompt")
		return
	}
	// 解析 --ratio / --duration / --resolution 命令参数（从提示词剥离）
	ratio, duration := req.Ratio, req.Duration
	prompt = parseVideoFlags(prompt, &ratio, &duration)

	route, err := g.pool.ResolveRoute(r.Context(), req.Model)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "model_not_found", err.Error())
		return
	}
	if route.Type == "image" {
		writeErr(w, http.StatusBadRequest, "wrong_endpoint",
			"模型 "+route.Public+" 是图片生成模型：请用 POST /v1/images/generations，不是视频任务接口")
		return
	}
	g.ensureVideoTasks()
	t0 := time.Now()
	var lastErr error
	for _, ch := range route.Channels {
		pr, ok := g.pool.Provider(ch.PID)
		if !ok {
			continue
		}
		vg, ok := pr.(provider.VideoGenerator)
		if !ok {
			continue
		}
		handle, acct, cerr := g.tryVideoCreate(r.Context(), vg, ch, prompt, ratio, duration)
		if cerr != nil {
			// 该渠道提交失败：记录并尝试下一个渠道（跨渠道失败转移）
			lastErr = cerr
			uid := ""
			if acct != nil {
				uid = acct.UID
			}
			g.bumpChannel(route, ch, false)
			g.logReqMedia("video", route.Public, ch.PID, uid, false, 0, t0)
			continue
		}
		if handle == nil {
			continue
		}
		id := newVideoTaskID()
		t := &videoTask{
			ID: id, Public: route.Public, PID: ch.PID, AcctUID: acct.UID,
			Acct: acct, Handle: handle, Prompt: prompt,
			Status: "running", CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix(),
		}
		g.videoTasks.put(t)
		g.bumpChannel(route, ch, true)
		go g.pollVideoTask(t)
		writeJSON(w, http.StatusOK, g.videoTaskView(t, nil))
		return
	}
	msg := "所选渠道均不支持视频生成（当前仅豆包 Seedance 提供）"
	if lastErr != nil {
		msg = lastErr.Error()
	}
	writeErr(w, http.StatusBadGateway, "video_create_failed", msg)
}

// tryVideoCreate 单渠道提交：账号轮询失败转移。返回句柄与账号
// （轮询必须用同一账号的 Cookie 访问其会话）。
func (g *Gateway) tryVideoCreate(ctx context.Context, vg provider.VideoGenerator, ch pool.ChannelTarget, prompt, ratio string, duration int) (*provider.VideoTaskHandle, *model.Account, error) {
	tried := map[string]bool{}
	var lastErr error
	var lastAcct *model.Account
	for {
		a, perr := g.pool.Pick(ch.PID, ch.Model, "")
		if perr != nil {
			break
		}
		if tried[a.UID] {
			break
		}
		tried[a.UID] = true
		if _, terr := g.pool.EnsureFreshToken(ctx, a); terr != nil {
			lastErr = terr
			lastAcct = a
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		handle, cerr := vg.CreateVideoTask(cctx, a, ch.Model, provider.VideoRequest{
			Prompt: prompt, Ratio: ratio, Duration: duration,
		})
		cancel()
		if cerr != nil {
			g.pool.MarkError(a, ch.Model, cerr)
			lastErr = cerr
			lastAcct = a
			continue
		}
		return handle, a, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("平台 %s 无可用账号", ch.PID)
	}
	return nil, lastAcct, lastErr
}

func (g *Gateway) handleVideoGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET only")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/v3/contents/generations/tasks/")
	id = strings.TrimPrefix(id, "/v1/videos/")
	id = strings.TrimSpace(id)
	if id == "" || strings.Contains(id, "/") {
		writeErr(w, http.StatusBadRequest, "invalid_task_id", "任务 ID 为空")
		return
	}
	if g.videoTasks == nil {
		writeErr(w, http.StatusNotFound, "task_not_found", "任务不存在（服务重启后进行中的任务会丢失）")
		return
	}
	t := g.videoTasks.get(id)
	if t == nil {
		writeErr(w, http.StatusNotFound, "task_not_found", "任务不存在（服务重启后进行中的任务会丢失）")
		return
	}
	writeJSON(w, http.StatusOK, g.videoTaskView(t, nil))
}

// videoTaskView Ark 查询响应形态。
func (g *Gateway) videoTaskView(t *videoTask, _ []byte) map[string]any {
	view := map[string]any{
		"id": t.ID, "model": t.Public, "status": t.Status,
		"created_at": t.CreatedAt, "updated_at": t.UpdatedAt,
		"unwatermarked": t.Unwatermarked,
	}
	if t.VideoURL != "" {
		view["content"] = map[string]any{"video_url": t.VideoURL}
		view["cover_url"] = t.CoverURL
	} else {
		view["content"] = nil
	}
	if t.Status == "failed" {
		view["error"] = map[string]any{"code": "VideoGenerationFailed", "message": t.Error}
	}
	return view
}

// pollVideoTask 后台轮询上游会话直到终态（成功/失败/超时 20 分钟）。
func (g *Gateway) pollVideoTask(t *videoTask) {
	t0 := time.Now()
	deadline := t0.Add(20 * time.Minute)
	pr, ok := g.pool.Provider(t.PID)
	if !ok {
		return
	}
	vg, ok := pr.(provider.VideoGenerator)
	if !ok {
		return
	}
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		st, err := vg.PollVideoTask(ctx, t.Acct, t.Handle)
		cancel()
		if err != nil {
			if pe := provider.AsError(err); pe.Kind == provider.ErrAuth {
				g.videoTasks.update(t.ID, func(v *videoTask) {
					v.Status = "failed"
					v.Error = "凭证失效: " + pe.Error()
				})
				g.logReqMedia("video", t.Public, t.PID, t.AcctUID, false, 0, t0)
				return
			}
			continue // 网络/上游抖动：下轮再试
		}
		switch st.Status {
		case "succeeded":
			g.videoTasks.update(t.ID, func(v *videoTask) {
				v.Status = "succeeded"
				v.VideoURL = st.VideoURL
				v.CoverURL = st.CoverURL
				v.Unwatermarked = st.Unwatermarked
			})
			g.pool.MarkSuccess(t.Acct, t.Handle.Model)
			// 按次计费：任务 succeeded 才记 1 条（豆包本地日配额仍在提交时计数，口径分开）
			g.recordMedia(t.Public, t.PID, t.AcctUID, 1, "videos")
			g.logReqMedia("video", t.Public, t.PID, t.AcctUID, true, 1, t0)
			return
		case "failed":
			g.videoTasks.update(t.ID, func(v *videoTask) {
				v.Status = "failed"
				v.Error = st.Error
			})
			g.logReqMedia("video", t.Public, t.PID, t.AcctUID, false, 0, t0)
			return
		}
	}
	g.videoTasks.update(t.ID, func(v *videoTask) {
		v.Status = "failed"
		v.Error = "视频生成超时（20 分钟）"
	})
	g.logReqMedia("video", t.Public, t.PID, t.AcctUID, false, 0, t0)
}

// newVideoTaskID Ark 风格任务 ID：cgt-<24hex>。
func newVideoTaskID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "cgt-" + hex.EncodeToString(b)
}
