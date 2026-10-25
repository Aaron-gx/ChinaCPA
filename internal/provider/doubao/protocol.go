// protocol.go 豆包（www.doubao.com）网页协议：payload 构造 / SSE ACK 解析 /
// 会话链轮询解析 / 模型名归一化。全部字段名逐字对齐 Doora v0.8.2 逆向结论
// （见《复刻与反代协议文档.md》与 参考实现_doubao_protocol.js）。
package doubao

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// timeNowMs 毫秒时间戳（协议字段 create_time_ms 等）。
func timeNowMs() int64 { return time.Now().UnixMilli() }

// 平台档案（doubao 站点；aid/botId/pcVersion 为协议关键常量，上游升级可能变动）。
const (
	dbOrigin    = "https://www.doubao.com"
	dbAid       = "497858"
	dbPcVersion = "3.39.2"
	dbBotID     = "7338286299411103781"

	dbVersionCode = "20800"

	blockTypeText       = 10000
	blockTypeAttachment = 10052

	abilityTypeVideo  = 17
	abilityTypeImage  = 3
	imageAbilityInner = 1

	agentModeCreation         = 2
	conversationUserAssistant = 2
)

// 上游失败关键词（命中即判定失败；额度类单独归类为 ErrQuota）。
var dbFailKeywords = []string{
	"生成失败", "无法生成", "额度不足", "积分不足", "免费次数用完",
	"免费生成次数用完", "明天再来免费生成", "可能涉及版权限制", "专属能力",
}

var dbQuotaKeywords = []string{"额度不足", "积分不足", "免费次数用完", "免费生成次数用完", "明天再来免费生成"}

// ── 随机标识 ─────────────────────────────────────────────

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// randomLocalId 16 位数字串（local_conversation_id 用）。
func randomLocalId() string {
	n, err := rand.Int(rand.Reader, big.NewInt(9e15))
	if err != nil {
		return fmt.Sprintf("%d", timeNowMs())
	}
	return strconv.FormatInt(n.Int64()+1e15, 10)
}

// randomDigits 生成 n 位数字串（设备标识用）。
func randomDigits(n int) string {
	d := make([]byte, n)
	for i := range d {
		v, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			d[i] = '7'
			continue
		}
		d[i] = byte('0' + v.Int64())
	}
	if d[0] == '0' {
		d[0] = '7'
	}
	return string(d)
}

func hexToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	const digits = "0123456789abcdef"
	out := make([]byte, n*2)
	for i, v := range b {
		out[i*2] = digits[v>>4]
		out[i*2+1] = digits[v&0xf]
	}
	return string(out)
}

// flowTrace x-flow-trace 头：04-<16hex>-<8hex>-01。
func flowTrace() string { return "04-" + hexToken(8) + "-" + hexToken(4) + "-01" }

// ── query 构造 ───────────────────────────────────────────

// deviceIDs 账号设备标识（导入时生成、落盘）。
type deviceIDs struct {
	DeviceID string `json:"device_id"`
	WebID    string `json:"web_id"`
}

// commonQuery /chat/completion 与 /im/chain/single 等共用 query（use-olympus-account 是关键开关）。
func commonQuery(ids deviceIDs, fp, region string) url.Values {
	q := url.Values{
		"version_code":           {dbVersionCode},
		"language":               {"zh"},
		"device_platform":        {"web"},
		"doubao_device_platform": {"web"},
		"aid":                    {dbAid},
		"real_aid":               {dbAid},
		"pkg_type":               {"release_version"},
		"device_id":              {ids.DeviceID},
		"pc_version":             {dbPcVersion},
		"doubao_pc_version":      {dbPcVersion},
		"web_id":                 {ids.WebID},
		"tea_uuid":               {ids.WebID},
		"region":                 {region},
		"sys_region":             {region},
		"samantha_web":           {"1"},
		"web_platform":           {"browser"},
		"web_tab_id":             {newUUID()},
		"tz_name":                {"Asia/Shanghai"},
		"fp":                     {fp},
		"msToken":                {""},
	}
	q.Set("use-olympus-account", "1")
	return q
}

// ── payload 构造 ─────────────────────────────────────────

func buildBaseOption(nowMs int64, uniqueKey string, needCreateConversation, deepThink bool) map[string]any {
	return map[string]any{
		"send_message_scene": "", "create_time_ms": nowMs, "collect_id": "",
		"is_audio": false, "answer_with_suggest": false, "tts_switch": false,
		"need_deep_think":     boolInt(deepThink),
		"click_clear_context": false, "from_suggest": false,
		"is_regen": false, "is_replace": false,
		"is_from_click_option": false, "is_from_click_softlink": false,
		"disable_sse_cache": false, "select_text_action": "", "is_select_text": false,
		"resend_for_regen": false, "scene_type": 0,
		"unique_key": uniqueKey, "start_seq": 0,
		"need_create_conversation": needCreateConversation,
		"conversation_init_option": map[string]any{"need_ack_conversation": true},
		"regen_query_id":           []any{},
		"edit_query_id":            []any{},
		"regen_instruction":        "",
		"no_replace_for_regen":     false,
		"message_from":             0,
		"shared_app_name":          "",
		"shared_app_id":            "",
		"sse_recv_event_options":   map[string]any{"support_chunk_delta": true},
		"is_ai_playground":         false,
		"is_old_user":              true,
		"recovery_option":          map[string]any{"is_recovery": false, "req_create_time_sec": nowMs / 1000, "append_sse_event_scene": 0},
		"message_storage_type":     0,
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func buildPlainTextMessage(text string) map[string]any {
	return map[string]any{
		"local_message_id": newUUID(),
		"content_block": []any{map[string]any{
			"block_type": blockTypeText,
			"content": map[string]any{
				"text_block":     map[string]any{"text": text, "icon_url": "", "icon_url_dark": "", "summary": ""},
				"pc_event_block": "",
			},
			"block_id":      newUUID(),
			"parent_id":     "",
			"meta_info":     []any{},
			"append_fields": []any{},
		}},
		"message_status": 0,
	}
}

// buildVideoAbility chat_ability（ability_type=17）。
func buildVideoAbility(model, ratio string, duration int, prompt string) map[string]any {
	param, _ := json.Marshal(map[string]any{
		"ratio":    ratio,
		"model":    model,
		"duration": duration,
		"input_box_content": map[string]any{
			"user_input_content":   prompt,
			"reply_message_format": "生成视频：%s",
		},
	})
	return map[string]any{"ability_type": abilityTypeVideo, "ability_param": string(param)}
}

// buildImageAbility chat_ability（ability_type=3，双层嵌套，内层 ability_type=1）。
func buildImageAbility(model, ratio, prompt string) map[string]any {
	inner, _ := json.Marshal(map[string]any{
		"model": model,
		"ratio": ratio,
		"input_box_content": map[string]any{
			"user_input_content":   prompt,
			"reply_message_format": "生成图片：%s",
		},
	})
	param, _ := json.Marshal(map[string]any{
		"ability_param": json.RawMessage(inner),
		"ability_type":  imageAbilityInner,
	})
	return map[string]any{"ability_type": abilityTypeImage, "ability_param": string(param)}
}

// submitPayload 顶层 payload。conversationID 为空 = 新会话（need_create_conversation）。
func submitPayload(conversationID, prompt string, ability map[string]any, deepThink bool) map[string]any {
	nowMs := timeNowMs()
	payload := map[string]any{
		"client_meta": map[string]any{
			"local_conversation_id": "local_" + randomLocalId(),
			"conversation_id":       conversationID,
			"bot_id":                dbBotID,
			"last_section_id":       "",
			"last_message_index":    nil,
		},
		"messages":     []any{buildPlainTextMessage(prompt)},
		"option":       buildBaseOption(nowMs, newUUID(), conversationID == "", deepThink),
		"user_context": []any{},
		"ext": map[string]any{
			"answer_with_suggest":           "0",
			"fp":                            "", // 由 provider 层注入（s_v_web_id）
			"sub_conv_firstmet_type":        "1",
			"collection_id":                 "",
			"conversation_init_option":      `{"need_ack_conversation":true}`,
			"commerce_credit_config_enable": "0",
		},
	}
	if ability != nil {
		payload["chat_ability"] = ability
	}
	return payload
}

// confirmPayload 确认轮（agent_mode=2，不带 ability）：上游反问参数时回一条「确认」。
func confirmPayload(conversationID, text string) map[string]any {
	nowMs := timeNowMs()
	option := buildBaseOption(nowMs, newUUID(), false, false)
	option["agent_mode"] = agentModeCreation
	option["support_lazy_fetch_stream"] = true
	option["general_task_param"] = map[string]any{
		"runtime_type":     0,
		"agent_task_param": map[string]any{},
		"agent_task_param_change": map[string]any{
			"runtime_changed": false, "device_changed": false, "sandbox_auth_type_changed": false,
		},
		"project_id":               "",
		"need_modify_conversation": false,
	}
	option["related_deleted_message_ids"] = map[string]any{}
	option["connector_info_list"] = []any{}
	delete(option, "conversation_init_option") // 追加消息不需要 ack 选项
	return map[string]any{
		"client_meta": map[string]any{
			"local_conversation_id": "local_" + randomLocalId(),
			"conversation_id":       conversationID,
			"bot_id":                dbBotID,
			"last_section_id":       "",
			"last_message_index":    nil,
		},
		"messages":     []any{buildPlainTextMessage(text)},
		"option":       option,
		"user_context": []any{},
		"ext": map[string]any{
			"answer_with_suggest":           "0",
			"fp":                            "",
			"sub_conv_firstmet_type":        "1",
			"collection_id":                 "",
			"conversation_init_option":      `{"need_ack_conversation":true}`,
			"commerce_credit_config_enable": "0",
		},
	}
}

// chainBody /im/chain/single 轮询请求体（cmd=3100）。
func chainBody(conversationID string) map[string]any {
	return map[string]any{
		"cmd": 3100,
		"uplink_body": map[string]any{
			"pull_singe_chain_uplink_body": map[string]any{
				"conversation_id":        conversationID,
				"anchor_index":           9007199254740991,
				"conversation_type":      3,
				"direction":              1,
				"limit":                  20,
				"ext":                    map[string]any{},
				"filter":                 map[string]any{"index_list": []any{}},
				"evaluate_ab_params":     "",
				"evaluate_common_params": "",
			},
		},
		"sequence_id": newUUID(),
		"channel":     2,
		"version":     "1",
	}
}

// ── SSE ACK 解析 ─────────────────────────────────────────

// sseAck SSE_ACK 事件解析结果。
type sseAck struct {
	ConversationID string
	SectionID      string
	QuestionID     string
}

// parseSseAck 从提交响应的 SSE 文本里取 conversation_id；STREAM_ERROR 时返回错误。
func parseSseAck(text string) (*sseAck, error) {
	normalized := strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	for _, packet := range strings.Split(normalized, "\n\n") {
		var event, data string
		for _, line := range strings.Split(packet, "\n") {
			trimmed := strings.TrimLeft(line, " ")
			switch {
			case strings.HasPrefix(trimmed, "event:"):
				event = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
			case strings.HasPrefix(trimmed, "data:"):
				data += strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			}
		}
		if event == "STREAM_ERROR" {
			return nil, fmt.Errorf("上游 STREAM_ERROR: %s", truncateRunes(data, 300))
		}
		if event == "SSE_ACK" {
			var payload struct {
				AckClientMeta struct {
					ConversationID string `json:"conversation_id"`
					SectionID      string `json:"section_id"`
				} `json:"ack_client_meta"`
				QueryList []struct {
					QuestionID string `json:"question_id"`
				} `json:"query_list"`
			}
			if err := json.Unmarshal([]byte(data), &payload); err != nil {
				return nil, fmt.Errorf("SSE_ACK 解析失败: %v", err)
			}
			ack := &sseAck{
				ConversationID: payload.AckClientMeta.ConversationID,
				SectionID:      payload.AckClientMeta.SectionID,
			}
			if len(payload.QueryList) > 0 {
				ack.QuestionID = payload.QueryList[0].QuestionID
			}
			return ack, nil
		}
	}
	return nil, fmt.Errorf("响应缺少 SSE_ACK（凭证或风控参数可能失效）")
}

// ── 会话链解析 ───────────────────────────────────────────

type chainBlock struct {
	BlockType int `json:"block_type"`
	Content   struct {
		TextBlock struct {
			Text string `json:"text"`
		} `json:"text_block"`
		CreationBlock struct {
			Creations []creationItem `json:"creations"`
		} `json:"creation_block"`
	} `json:"content"`
}

type creationItem struct {
	ID    string `json:"id"`
	Video *struct {
		VID         string `json:"vid"`
		Status      int    `json:"status"`
		DownloadURL string `json:"download_url"`
		Cover       *struct {
			ImageThumb *struct {
				URL string `json:"url"`
			} `json:"image_thumb"`
		} `json:"cover"`
	} `json:"video"`
	Image *struct {
		URL    string `json:"url"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
	} `json:"image"`
}

type chainMessage struct {
	UserType     int             `json:"user_type"`
	IndexInConv  float64         `json:"index_in_conv"`
	MessageID    string          `json:"message_id"`
	Ext          map[string]any  `json:"ext"`
	ContentBlock []chainBlock    `json:"content_block"`
	Content      json.RawMessage `json:"content"`
}

// blocks 兼容 content_block 顶层与 content.content_block 两处。
func (m *chainMessage) blocks() []chainBlock {
	if len(m.ContentBlock) > 0 {
		return m.ContentBlock
	}
	var wrapper struct {
		ContentBlock []chainBlock `json:"content_block"`
	}
	if json.Unmarshal(m.Content, &wrapper) == nil {
		return wrapper.ContentBlock
	}
	return nil
}

// chainResponse /im/chain/single 响应。
type chainResponse struct {
	DownlinkBody struct {
		PullSingeChainDownlinkBody struct {
			Messages []chainMessage `json:"messages"`
		} `json:"pull_singe_chain_downlink_body"`
	} `json:"downlink_body"`
}

// parseConversationText 最新一条非空助手文本（无则空串）。
func parseConversationText(resp *chainResponse) string {
	newest := ""
	var newestIdx float64 = -1 << 30
	for i := range resp.DownlinkBody.PullSingeChainDownlinkBody.Messages {
		m := &resp.DownlinkBody.PullSingeChainDownlinkBody.Messages[i]
		if m.UserType != conversationUserAssistant {
			continue
		}
		text := ""
		for _, b := range m.blocks() {
			if b.BlockType == blockTypeText && strings.TrimSpace(b.Content.TextBlock.Text) != "" {
				text = b.Content.TextBlock.Text
			}
		}
		if text == "" {
			continue
		}
		if m.IndexInConv > newestIdx {
			newestIdx = m.IndexInConv
			newest = text
		}
		if newest == "" {
			newest = text
		}
	}
	return newest
}

// creationResult 生成产物（视频或图片）。
type creationResult struct {
	RemoteTaskID string
	VID          string
	PlayURL      string // 视频：download_url；图片：image.url
	CoverURL     string
	ImageURLs    []string // 图片：全部生成图的 URL
	IsVideo      bool
}

// parseCreationResult 取已就绪的生成产物（video.status==3 或 image.url）。
func parseCreationResult(resp *chainResponse) *creationResult {
	var img *creationResult
	for i := range resp.DownlinkBody.PullSingeChainDownlinkBody.Messages {
		for _, b := range resp.DownlinkBody.PullSingeChainDownlinkBody.Messages[i].blocks() {
			for _, item := range b.Content.CreationBlock.Creations {
				if v := item.Video; v != nil && v.Status == 3 && v.DownloadURL != "" {
					r := &creationResult{
						RemoteTaskID: item.ID,
						VID:          v.VID,
						PlayURL:      v.DownloadURL,
						IsVideo:      true,
					}
					if v.Cover != nil && v.Cover.ImageThumb != nil {
						r.CoverURL = v.Cover.ImageThumb.URL
					}
					return r
				}
				if im := item.Image; im != nil && im.URL != "" {
					if img == nil {
						img = &creationResult{RemoteTaskID: item.ID, PlayURL: im.URL, CoverURL: im.URL}
					}
					img.ImageURLs = append(img.ImageURLs, im.URL)
				}
			}
		}
	}
	return img
}

// readClarifyState 最新助手消息是否标记「需要确认参数」（ext.is_creation_clarifying=="1"）。
func readClarifyState(resp *chainResponse) bool {
	var newestIdx float64 = -1 << 30
	marked := false
	seen := false
	for i := range resp.DownlinkBody.PullSingeChainDownlinkBody.Messages {
		m := &resp.DownlinkBody.PullSingeChainDownlinkBody.Messages[i]
		if m.UserType != conversationUserAssistant {
			continue
		}
		if m.IndexInConv >= newestIdx {
			newestIdx = m.IndexInConv
			if v, ok := m.Ext["is_creation_clarifying"].(string); ok && v == "1" {
				marked = true
			} else {
				marked = false
			}
			seen = true
		}
	}
	return seen && marked
}

// hitFailKeyword 命中失败关键词时返回关键词（额度类第二返回值真）。
func hitFailKeyword(text string) (string, bool) {
	for _, kw := range dbFailKeywords {
		if strings.Contains(text, kw) {
			isQuota := false
			for _, q := range dbQuotaKeywords {
				if q == kw {
					isQuota = true
					break
				}
			}
			return kw, isQuota
		}
	}
	return "", false
}

// ── 无水印原片候选选优 ───────────────────────────────────

// lrParamRe 匹配 URL 里的 lr= 水印参数（unwatermark 为无水印）。
var lrParamRe = regexp.MustCompile(`[?&]lr=([^&]*)`)

// pickCleanestVideoUrl unwatermark(lr 参数含 unwatermark) > 无 lr > 带水印。
func pickCleanestVideoUrl(candidates []string) string {
	rank := func(u string) int {
		m := lrParamRe.FindStringSubmatch(strings.TrimSpace(u))
		if m == nil {
			return 1
		}
		if strings.Contains(strings.ToLower(m[1]), "unwatermark") {
			return 0
		}
		return 2
	}
	best, bestRank := "", 3
	for _, c := range candidates {
		if strings.TrimSpace(c) == "" {
			continue
		}
		if r := rank(c); r < bestRank {
			best, bestRank = c, r
		}
	}
	return best
}

// ── 模型名归一化 ─────────────────────────────────────────

// 对外模型 ID ↔ 上游模型名。图片上游名带空格（"Seedream 5.0 Flash"），
// 视频上游名为 snake_case slug，对外统一用连字符 slug 提升客户端兼容性。

type modelKind int

const (
	modelText modelKind = iota
	modelImage
	modelVideo
)

type resolvedModel struct {
	Public   string // 对外 ID
	Upstream string // 上游 ability_param 用名
	Kind     modelKind
}

var imageModels = []resolvedModel{
	{"seedream-5.0-flash", "Seedream 5.0 Flash", modelImage},
	{"seedream-5.0-pro", "Seedream 5.0 Pro", modelImage},
	{"seedream-4.5", "Seedream 4.5", modelImage},
	{"seedream-4.0", "Seedream 4.0", modelImage},
}

var videoModels = []resolvedModel{
	{"seedance-2.5", "seedance_v2.5", modelVideo},
	{"seedance-2.0", "seedance_v2.0", modelVideo},
	{"seedance-2.0-pro", "seedance_v2.0_std", modelVideo},
	{"seedance-2.0-fast", "seedance_v2.0", modelVideo},
	{"seedance-2.0-mini", "seedance_v2.0_mini", modelVideo},
	{"seedance-1.0", "ic_mini", modelVideo},
}

var videoModelMaxDuration = map[string]int{
	"seedance_v2.5": 30, "seedance_v2.0_std": 15, "seedance_v2.0": 15,
	"seedance_v2.0_mini": 15, "ic_mini": 15,
}

const textModelID = "doubao-chat"

// normalizeKey 模型名归一化比较键：小写、去掉空格与 . _ - 。
func normalizeKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch r {
		case ' ', '.', '_', '-', '：', ':':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// resolveModel 归一化解析模型名；未命中图片/视频表则按文本对话处理。
func resolveModel(name string) resolvedModel {
	key := normalizeKey(name)
	for _, m := range imageModels {
		if normalizeKey(m.Public) == key || normalizeKey(m.Upstream) == key {
			return m
		}
	}
	for _, m := range videoModels {
		if normalizeKey(m.Public) == key || normalizeKey(m.Upstream) == key {
			return m
		}
	}
	return resolvedModel{Public: name, Upstream: "", Kind: modelText}
}

// sizeToRatio 分辨率字符串（1024x1024 / 2K 等）→ Seedream 比例（就近匹配）。
func sizeToRatio(size string) string {
	size = strings.ToLower(strings.TrimSpace(size))
	if size == "" {
		return "1:1"
	}
	parts := strings.FieldsFunc(size, func(r rune) bool { return r == 'x' || r == '*' || r == '×' })
	if len(parts) != 2 {
		return "1:1"
	}
	w, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	h, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return "1:1"
	}
	return closestRatio(w, h)
}

var commonRatios = [][2]int{{1, 1}, {3, 4}, {4, 3}, {9, 16}, {16, 9}, {21, 9}, {2, 3}, {3, 2}}

// closestRatio 就近匹配常用比例（宽高比误差最小者）。
func closestRatio(w, h int) string {
	target := float64(w) / float64(h)
	best := "1:1"
	bestDiff := 1e9
	for _, r := range commonRatios {
		diff := target - float64(r[0])/float64(r[1])
		if diff < 0 {
			diff = -diff
		}
		if diff < bestDiff {
			bestDiff = diff
			best = strconv.Itoa(r[0]) + ":" + strconv.Itoa(r[1])
		}
	}
	return best
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
