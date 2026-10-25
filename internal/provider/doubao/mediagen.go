// mediagen.go 豆包图片（Seedream）与视频（Seedance）生成：
// 走 /chat/completion 的 chat_ability 能力位（图片=3 内层 1，视频=17），
// 轮询 /im/chain/single 取 creation_block 产物；视频完成后尽力经素材库
// （aispace homepage → node_info → get_download_info）解析无水印原片。
package doubao

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"chinacpa/internal/model"
	"chinacpa/internal/provider"
)

// unwatermarkParams 无水印兜底参数（追加到播放 URL 的 query）。
var unwatermarkParams = url.Values{"channel": {"no"}, "codec_type": {"8"}, "logo_type": {"unwatermarked"}}

// withUnwatermark 给 URL 追加无水印参数。
func withUnwatermark(u string) string {
	if u == "" {
		return u
	}
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return u + sep + unwatermarkParams.Encode()
}

// ── 图片生成 ─────────────────────────────────────────────

// GenerateImage Seedream 出图：提交 → 轮询 creation_block 收集全部 image.url。
func (p *Provider) GenerateImage(ctx context.Context, a *model.Account, modelName string, req provider.ImageRequest) (*provider.ImageResult, error) {
	rm := resolveModel(modelName)
	if rm.Kind != modelImage {
		return nil, fmt.Errorf("模型 %q 不是豆包图片模型（可选：%s）", modelName, joinPublic(imageModels))
	}
	ratio := strings.TrimSpace(req.Ratio)
	if ratio == "" {
		ratio = sizeToRatio(req.Size)
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	if qerr := p.checkQuota(a.UID, "image"); qerr != nil {
		return nil, qerr
	}

	unlock := p.lockAccount(a.UID)
	unlock.Lock()
	defer unlock.Unlock()

	text := "生成图片：" + req.Prompt + ratioNote(ratio)
	ack, err := p.submit(ctx, a, submitPayload("", text, buildImageAbility(rm.Upstream, ratio, req.Prompt), false))
	if err != nil {
		return nil, err
	}

	urls := []string{}
	seen := map[string]bool{}
	stable := 0
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cr, perr := p.pollChain(ctx, a, ack.ConversationID)
		if perr != nil {
			if pe, ok := perr.(*provider.Error); ok && pe.Kind == provider.ErrAuth {
				return nil, perr
			}
			time.Sleep(3 * time.Second)
			continue
		}
		if readClarifyState(cr) {
			_ = p.sendConfirm(ctx, a, ack.ConversationID, "确认")
			time.Sleep(2 * time.Second)
			continue
		}
		if t := parseConversationText(cr); t != "" {
			if kw, quota := hitFailKeyword(t); kw != "" {
				kind := provider.ErrUpstream
				if quota {
					kind = provider.ErrQuota
				}
				return nil, &provider.Error{Kind: kind, Msg: "豆包图片生成失败: " + truncateRunes(t, 300)}
			}
		}
		if c := parseCreationResult(cr); c != nil && len(c.ImageURLs) > 0 {
			for _, u := range c.ImageURLs {
				if !seen[u] {
					seen[u] = true
					urls = append(urls, u)
				}
			}
			// 已出图：再观察两轮，没有新增即收工
			if len(urls) >= len(c.ImageURLs) {
				stable++
				if stable >= 2 || (req.N > 0 && len(urls) >= req.N) {
					countImages(a.UID, rm.Public, int64(len(urls)))
					return &provider.ImageResult{URLs: urls, ConversationID: ack.ConversationID}, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2500 * time.Millisecond):
		}
	}
	if len(urls) > 0 {
		countImages(a.UID, rm.Public, int64(len(urls)))
		return &provider.ImageResult{URLs: urls, ConversationID: ack.ConversationID}, nil
	}
	return nil, &provider.Error{Kind: provider.ErrUpstream, Msg: "豆包图片生成超时"}
}

// ratioNote 比例后缀（auto 不加）。
func ratioNote(ratio string) string {
	if ratio == "" || ratio == "auto" {
		return ""
	}
	return "，画面比例 " + ratio
}

// ── 视频生成（异步任务）──────────────────────────────────

// CreateVideoTask Seedance 提交：读到 SSE_ACK 即返回会话句柄。
func (p *Provider) CreateVideoTask(ctx context.Context, a *model.Account, modelName string, req provider.VideoRequest) (*provider.VideoTaskHandle, error) {
	rm := resolveModel(modelName)
	if rm.Kind != modelVideo {
		return nil, fmt.Errorf("模型 %q 不是豆包视频模型（可选：%s）", modelName, joinPublic(videoModels))
	}
	ratio := strings.TrimSpace(req.Ratio)
	if ratio == "" {
		ratio = "16:9"
	}
	duration := req.Duration
	if duration <= 0 {
		duration = 10
	}
	if duration < 4 {
		duration = 4
	}
	if max, ok := videoModelMaxDuration[rm.Upstream]; ok && duration > max {
		duration = max
	}
	// 豆包站提示词需带时长与比例（doubao 档案 videoDurationInPrompt=true）
	text := "生成视频：" + req.Prompt + ratioNote(ratio) + fmt.Sprintf("，%ds", duration)

	if qerr := p.checkQuota(a.UID, "video"); qerr != nil {
		return nil, qerr
	}
	unlock := p.lockAccount(a.UID)
	unlock.Lock()
	ack, err := p.submit(ctx, a, submitPayload("", text, buildVideoAbility(rm.Upstream, ratio, duration, req.Prompt), false))
	unlock.Unlock()
	if err != nil {
		return nil, err
	}
	countVideos(a.UID, rm.Public)
	return &provider.VideoTaskHandle{
		ConversationID: ack.ConversationID,
		Model:          rm.Public,
		Prompt:         req.Prompt,
		Extra:          map[string]string{"confirm_rounds": "0"},
	}, nil
}

// PollVideoTask 单次进度查询（内部处理确认轮；无产物即 running）。
func (p *Provider) PollVideoTask(ctx context.Context, a *model.Account, h *provider.VideoTaskHandle) (*provider.VideoTaskStatus, error) {
	cr, err := p.pollChain(ctx, a, h.ConversationID)
	if err != nil {
		return nil, err
	}
	if readClarifyState(cr) {
		rounds := 0
		if v, ok := h.Extra["confirm_rounds"]; ok {
			fmt.Sscanf(v, "%d", &rounds)
		}
		if rounds < 4 {
			rounds++
			h.Extra["confirm_rounds"] = fmt.Sprintf("%d", rounds)
			_ = p.sendConfirm(ctx, a, h.ConversationID, "确认")
		}
		return &provider.VideoTaskStatus{Status: "running"}, nil
	}
	if c := parseCreationResult(cr); c != nil && c.IsVideo {
		finalURL := c.PlayURL
		unwatermarked := false
		if c.VID != "" {
			if resolved := p.resolveOriginal(ctx, a, c.VID); resolved != nil {
				candidates := []string{resolved[0], resolved[1], withUnwatermark(c.PlayURL), c.PlayURL}
				if clean := pickCleanestVideoUrl(candidates); clean != "" {
					finalURL = clean
					unwatermarked = true
				}
			}
		}
		return &provider.VideoTaskStatus{
			Status:        "succeeded",
			VideoURL:      finalURL,
			CoverURL:      c.CoverURL,
			VideoVID:      c.VID,
			Unwatermarked: unwatermarked,
		}, nil
	}
	if t := parseConversationText(cr); t != "" {
		if kw, quota := hitFailKeyword(t); kw != "" {
			msg := "豆包视频生成失败: " + truncateRunes(t, 300)
			_ = quota
			return &provider.VideoTaskStatus{Status: "failed", Error: msg}, nil
		}
	}
	return &provider.VideoTaskStatus{Status: "running"}, nil
}

// ── 素材库无水印原片 ─────────────────────────────────────

// resolveOriginal 三步取原片：homepage 找作品目录 → node_info 按 vid 找节点
// （节点入库有延迟，重试数次）→ get_download_info 取下载地址。
// 返回 [主地址, 备地址]，任一步失败返回 nil（best-effort，不影响主流程）。
func (p *Provider) resolveOriginal(ctx context.Context, a *model.Account, vid string) []string {
	if vid == "" {
		return nil
	}
	stepCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	defer func() { _ = recover() }() // 防御：上游结构异常不拖垮生成主流程

	ids := p.seedOf(a)
	q := commonQuery(ids, fpOf(a), regionOf(a))
	post := func(path string, body map[string]any) (any, int, error) {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		resp, err := p.dbPost(stepCtx, a, path, q, "application/json", raw, nil)
		if err != nil {
			return nil, 0, err
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		var parsed any
		if json.Unmarshal(data, &parsed) != nil {
			return nil, resp.StatusCode, fmt.Errorf("响应非 JSON")
		}
		return parsed, resp.StatusCode, nil
	}

	home, status, err := post("/samantha/aispace/homepage", map[string]any{})
	if err != nil || status != 200 {
		return nil
	}
	folderID := deepFindKey(home, func(k string) bool {
		return strings.Contains(strings.ToLower(k), "folder") || strings.Contains(strings.ToLower(k), "directory")
	}, 8)
	if folderID == "" {
		return nil
	}

	var nodeID string
	for i := 0; i < 8 && nodeID == ""; i++ {
		nodes, status, err := post("/samantha/aispace/node_info", map[string]any{
			"node_id": folderID, "need_full_path": true, "size": 50,
			"sort_param": map[string]any{"need_sort_config": true, "sort_order": 1, "sort_type": 0},
		})
		if err == nil && status == 200 {
			nodeID = deepFindNodeByVID(nodes, vid, 10)
		}
		if nodeID == "" {
			select {
			case <-stepCtx.Done():
				return nil
			case <-time.After(2 * time.Second):
			}
		}
	}
	if nodeID == "" {
		return nil
	}

	dl, status, err := post("/samantha/aispace/get_download_info", map[string]any{
		"requests": []any{map[string]any{"node_id": nodeID}},
	})
	if err != nil || status != 200 {
		return nil
	}
	return deepFindVideoURLs(dl, 10)
}

// deepFindKey 递归找键名匹配谓词的字符串值。
func deepFindKey(v any, keyMatch func(string) bool, depth int) string {
	if depth <= 0 {
		return ""
	}
	switch t := v.(type) {
	case map[string]any:
		for k, vv := range t {
			if s, ok := vv.(string); ok && s != "" && keyMatch(k) {
				return s
			}
		}
		for _, vv := range t {
			if s := deepFindKey(vv, keyMatch, depth-1); s != "" {
				return s
			}
		}
	case []any:
		for _, vv := range t {
			if s := deepFindKey(vv, keyMatch, depth-1); s != "" {
				return s
			}
		}
	}
	return ""
}

// deepFindNodeByVID 找包含指定 vid 字符串值的对象，返回其 node_id / id。
func deepFindNodeByVID(v any, vid string, depth int) string {
	if depth <= 0 {
		return ""
	}
	switch t := v.(type) {
	case map[string]any:
		for _, vv := range t {
			if s, ok := vv.(string); ok && s == vid {
				if id, ok := t["node_id"].(string); ok && id != "" {
					return id
				}
				if id, ok := t["id"].(string); ok && id != "" {
					return id
				}
			}
		}
		for _, vv := range t {
			if s := deepFindNodeByVID(vv, vid, depth-1); s != "" {
				return s
			}
		}
	case []any:
		for _, vv := range t {
			if s := deepFindNodeByVID(vv, vid, depth-1); s != "" {
				return s
			}
		}
	}
	return ""
}

// deepFindVideoURLs 递归收集 http(s) 视频地址（键或值含 video/play/.mp4）。
func deepFindVideoURLs(v any, depth int) []string {
	if depth <= 0 {
		return nil
	}
	var out []string
	switch t := v.(type) {
	case map[string]any:
		for k, vv := range t {
			if s, ok := vv.(string); ok && strings.HasPrefix(s, "http") &&
				(strings.Contains(strings.ToLower(k), "video") || strings.Contains(strings.ToLower(k), "play") || strings.Contains(strings.ToLower(s), ".mp4")) {
				out = append(out, s)
			}
			out = append(out, deepFindVideoURLs(vv, depth-1)...)
		}
	case []any:
		for _, vv := range t {
			out = append(out, deepFindVideoURLs(vv, depth-1)...)
		}
	}
	if len(out) > 4 {
		out = out[:4]
	}
	return out
}

// joinPublic 模型列表提示串。
func joinPublic(ms []resolvedModel) string {
	var ids []string
	for _, m := range ms {
		ids = append(ids, m.Public)
	}
	return strings.Join(ids, " / ")
}
