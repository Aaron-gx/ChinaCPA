package doubao

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"testing"

	"chinacpa/internal/provider"
)

func TestParseSseAck(t *testing.T) {
	ok := "event: SSE_ACK\ndata: {\"ack_client_meta\":{\"conversation_id\":\"c1\",\"section_id\":\"s1\"},\"query_list\":[{\"question_id\":\"q1\"}]}\n\n"
	ack, err := parseSseAck(ok)
	if err != nil {
		t.Fatalf("SSE_ACK 解析失败: %v", err)
	}
	if ack.ConversationID != "c1" || ack.SectionID != "s1" || ack.QuestionID != "q1" {
		t.Fatalf("字段解析错误: %+v", ack)
	}

	if _, err := parseSseAck("event: STREAM_ERROR\ndata: {\"code\":1}\n\n"); err == nil {
		t.Fatal("STREAM_ERROR 应报错")
	}
	if _, err := parseSseAck("event: other\ndata: {}\n\n"); err == nil {
		t.Fatal("缺少 SSE_ACK 应报错")
	}
	// CRLF 容错
	ack, err = parseSseAck(strings.ReplaceAll(ok, "\n", "\r\n"))
	if err != nil || ack.ConversationID != "c1" {
		t.Fatalf("CRLF 容错失败: %v %+v", err, ack)
	}
}

func chainResp(t *testing.T, raw string) *chainResponse {
	t.Helper()
	var cr chainResponse
	if err := json.Unmarshal([]byte(raw), &cr); err != nil {
		t.Fatalf("chain 响应解析失败: %v", err)
	}
	return &cr
}

func TestParseConversationText(t *testing.T) {
	cr := chainResp(t, `{"downlink_body":{"pull_singe_chain_downlink_body":{"messages":[
		{"user_type":1,"index_in_conv":1,"content_block":[{"block_type":10000,"content":{"text_block":{"text":"生成图片：猫"}}}]},
		{"user_type":2,"index_in_conv":2,"content_block":[{"block_type":10000,"content":{"text_block":{"text":"第一版"}}}]},
		{"user_type":2,"index_in_conv":4,"content_block":[{"block_type":10000,"content":{"text_block":{"text":"最终版回复"}}}]}
	]}}}`)
	if got := parseConversationText(cr); got != "最终版回复" {
		t.Fatalf("应取最新助手文本，得到 %q", got)
	}
}

func TestParseCreationResult(t *testing.T) {
	// 图片产物（两条消息、三个 creation）
	imgJSON := `{"downlink_body":{"pull_singe_chain_downlink_body":{"messages":[
		{"user_type":2,"content_block":[
			{"block_type":10000,"content":{"creation_block":{"creations":[
				{"id":"r1","image":{"url":"https://img/1.png","width":1024,"height":1024}},
				{"id":"r2","image":{"url":"https://img/2.png"}}
			]}}}
		]},
		{"user_type":2,"content_block":[
			{"block_type":10000,"content":{"creation_block":{"creations":[
				{"id":"r3","image":{"url":"https://img/3.png"}}
			]}}}
		]}
	]}}}`
	c := parseCreationResult(chainResp(t, imgJSON))
	if c == nil || len(c.ImageURLs) != 3 || c.ImageURLs[0] != "https://img/1.png" {
		t.Fatalf("图片产物解析错误: %+v", c)
	}

	// 视频产物（status=3 就绪）
	vidJSON := `{"downlink_body":{"pull_singe_chain_downlink_body":{"messages":[
		{"user_type":2,"content_block":[
			{"block_type":10000,"content":{"creation_block":{"creations":[
				{"id":"v1","video":{"vid":"vid001","status":3,"download_url":"https://v/1.mp4","cover":{"image_thumb":{"url":"https://v/1.jpg"}}}}
			]}}}
		]}
	]}}}`
	c = parseCreationResult(chainResp(t, vidJSON))
	if c == nil || !c.IsVideo || c.VID != "vid001" || c.PlayURL != "https://v/1.mp4" || c.CoverURL != "https://v/1.jpg" {
		t.Fatalf("视频产物解析错误: %+v", c)
	}

	// 视频 status<3：未就绪，不应返回
	pendingJSON := `{"downlink_body":{"pull_singe_chain_downlink_body":{"messages":[
		{"user_type":2,"content_block":[
			{"block_type":10000,"content":{"creation_block":{"creations":[
				{"id":"v2","video":{"vid":"vid002","status":2,"download_url":""}}
			]}}}
		]}
	]}}}`
	if c := parseCreationResult(chainResp(t, pendingJSON)); c != nil {
		t.Fatalf("未就绪视频不应返回: %+v", c)
	}
}

func TestReadClarifyState(t *testing.T) {
	cr := chainResp(t, `{"downlink_body":{"pull_singe_chain_downlink_body":{"messages":[
		{"user_type":2,"index_in_conv":2,"ext":{"is_creation_clarifying":"1"},"content_block":[{"block_type":10000,"content":{"text_block":{"text":"请确认参数"}}}]}
	]}}}`)
	if !readClarifyState(cr) {
		t.Fatal("应识别确认轮标记")
	}
	cr = chainResp(t, `{"downlink_body":{"pull_singe_chain_downlink_body":{"messages":[
		{"user_type":2,"index_in_conv":2,"ext":{"is_creation_clarifying":"0"},"content_block":[{"block_type":10000,"content":{"text_block":{"text":"生成中"}}}]}
	]}}}`)
	if readClarifyState(cr) {
		t.Fatal("非确认轮不应误报")
	}
}

func TestResolveModel(t *testing.T) {
	cases := []struct {
		in   string
		kind modelKind
		want string
	}{
		{"seedream-5.0-flash", modelImage, "Seedream 5.0 Flash"},
		{"Seedream 5.0 Flash", modelImage, "Seedream 5.0 Flash"},
		{"seedream_5_0_pro", modelImage, "Seedream 5.0 Pro"},
		{"seedream-4.5", modelImage, "Seedream 4.5"},
		{"seedance-2.5", modelVideo, "seedance_v2.5"},
		{"seedance_v2.0_std", modelVideo, "seedance_v2.0_std"},
		{"seedance-2.0", modelVideo, "seedance_v2.0"},
		{"seedance-1.0", modelVideo, "ic_mini"},
		{"doubao-chat", modelText, ""},
		{"随便什么", modelText, ""},
	}
	for _, c := range cases {
		got := resolveModel(c.in)
		if got.Kind != c.kind || got.Upstream != c.want {
			t.Errorf("resolveModel(%q) = %+v，期望 kind=%d upstream=%q", c.in, got, c.kind, c.want)
		}
	}
}

func TestSizeToRatio(t *testing.T) {
	cases := map[string]string{
		"1024x1024": "1:1",
		"2048X2048": "1:1",
		"1280x720":  "16:9",
		"720x1280":  "9:16",
		"1024x768":  "4:3",
		"":          "1:1",
		"adaptive":  "1:1",
	}
	for in, want := range cases {
		if got := sizeToRatio(in); got != want {
			t.Errorf("sizeToRatio(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestPickCleanestVideoUrl(t *testing.T) {
	watermarked := "https://v/a.mp4?lr=watermark"
	clean := "https://v/b.mp4?lr=tos-unwatermark"
	original := "https://v/c.mp4"
	got := pickCleanestVideoUrl([]string{watermarked, original, clean})
	if got != clean {
		t.Fatalf("应选无水印地址，得到 %q", got)
	}
	got = pickCleanestVideoUrl([]string{watermarked, original})
	if got != original {
		t.Fatalf("无 unwatermark 时应选无 lr 参数的原片，得到 %q", got)
	}
}

func TestHitFailKeyword(t *testing.T) {
	if kw, quota := hitFailKeyword("抱歉，免费次数用完啦"); kw == "" || !quota {
		t.Fatalf("额度关键词应命中且归类 quota: %q %v", kw, quota)
	}
	if kw, quota := hitFailKeyword("生成失败：内容不合规"); kw == "" || quota {
		t.Fatalf("普通失败应命中且不归类 quota: %q %v", kw, quota)
	}
	if kw, _ := hitFailKeyword("视频生成中"); kw != "" {
		t.Fatalf("正常文本不应命中: %q", kw)
	}
}

func TestBuildImageAbility(t *testing.T) {
	ab := buildImageAbility("Seedream 5.0 Flash", "16:9", "一只猫")
	raw, _ := json.Marshal(ab)
	s := string(raw)
	if !strings.Contains(s, `"ability_type":3`) {
		t.Fatalf("外层 ability_type 应为 3: %s", s)
	}
	var outer struct {
		AbilityType  int    `json:"ability_type"`
		AbilityParam string `json:"ability_param"`
	}
	if err := json.Unmarshal(raw, &outer); err != nil {
		t.Fatal(err)
	}
	var inner struct {
		AbilityParam struct {
			Model string `json:"model"`
			Ratio string `json:"ratio"`
		} `json:"ability_param"`
		AbilityType int `json:"ability_type"`
	}
	if err := json.Unmarshal([]byte(outer.AbilityParam), &inner); err != nil {
		t.Fatalf("内层 ability_param 应为 JSON: %v", err)
	}
	if inner.AbilityType != 1 || inner.AbilityParam.Model != "Seedream 5.0 Flash" || inner.AbilityParam.Ratio != "16:9" {
		t.Fatalf("内层结构错误: %+v", inner)
	}
}

func TestChainBodyShape(t *testing.T) {
	raw, _ := json.Marshal(chainBody("conv1"))
	s := string(raw)
	for _, want := range []string{`"cmd":3100`, "pull_singe_chain_uplink_body", `"conversation_id":"conv1"`, `"channel":2`} {
		if !strings.Contains(s, want) {
			t.Errorf("chainBody 缺少 %q: %s", want, s)
		}
	}
}

func TestCookieValue(t *testing.T) {
	cookie := "ttwid=abc; s_v_web_id=verify_xxx; sessionid=SID123"
	if got := cookieValue(cookie, "s_v_web_id"); got != "verify_xxx" {
		t.Fatalf("s_v_web_id 提取错误: %q", got)
	}
	if got := cookieValue(cookie, "sessionid"); got != "SID123" {
		t.Fatalf("sessionid 提取错误: %q", got)
	}
	if got := cookieValue(cookie, "missing"); got != "" {
		t.Fatalf("不存在字段应返回空: %q", got)
	}
}

func TestDailyCounters(t *testing.T) {
	uid := "test-uid"
	c := usageToday(uid)
	if c.images != 0 || c.videos != 0 || c.chats != 0 {
		t.Fatalf("初始计数应为 0: %+v", c)
	}
	countChat(uid)
	countImages(uid, "seedream-5.0-flash", 4)
	countVideos(uid, "seedance-2.5")
	c = usageToday(uid)
	if c.chats != 1 || c.images != 4 || c.videos != 1 {
		t.Fatalf("计数错误: %+v", c)
	}
	if c.byModel["seedream-5.0-flash"] != 4 || c.byModel["seedance-2.5"] != 1 {
		t.Fatalf("按模型明细错误: %v", c.byModel)
	}
}

func TestCheckQuota(t *testing.T) {
	uid := "quota-uid"
	p := New(2, 1) // 图片 2 张/日，视频 1 条/日
	if err := p.checkQuota(uid, "image"); err != nil {
		t.Fatalf("未达上限不应报错: %v", err)
	}
	countImages(uid, "seedream-5.0-flash", 2)
	if err := p.checkQuota(uid, "image"); err == nil {
		t.Fatal("图片达上限应返回 ErrQuota")
	}
	if err := p.checkQuota(uid, "video"); err != nil {
		t.Fatalf("视频未达上限不应报错: %v", err)
	}
	countVideos(uid, "seedance-2.5")
	if err := p.checkQuota(uid, "video"); err == nil {
		t.Fatal("视频达上限应返回 ErrQuota")
	}
	// 明细结构
	detail := p.buildQuotaDetail(uid)
	if detail["images_used_today"].(int64) != 2 || detail["videos_used_today"].(int64) != 1 {
		t.Fatalf("明细错误: %v", detail)
	}
	byModel := detail["by_model"].(map[string]int64)
	if byModel["seedream-5.0-flash"] != 2 || byModel["seedance-2.5"] != 1 {
		t.Fatalf("按模型明细错误: %v", byModel)
	}
}

// TestCounterPersistence 计数器跨重启恢复（SetCounterPersistence + RestoreAccountCounters）。
func TestCounterPersistence(t *testing.T) {
	// 模拟 store：内存 map
	saved := map[string]CounterSnapshot{}
	SetCounterPersistence(
		func(uid string, c CounterSnapshot) { saved[uid] = c },
		func(uid string) (CounterSnapshot, bool) { c, ok := saved[uid]; return c, ok },
	)
	defer SetCounterPersistence(nil, nil)

	countImages("u1", "seedream", 3)
	countVideos("u1", "seedance")
	countChat("u1")

	// 模拟重启：清空内存计数，再从快照恢复
	qmu.Lock()
	qcounters = map[string]*dbDayCounter{}
	qmu.Unlock()

	RestoreAccountCounters("u1")
	c := usageToday("u1")
	if c.images != 3 || c.videos != 1 || c.chats != 1 {
		t.Fatalf("重启恢复失败: images=%d videos=%d chats=%d", c.images, c.videos, c.chats)
	}
}

// TestClassifyJSONErrorBody 上游 HTTP 200 + JSON 错误体（登录失效/风控）的归类。
func TestClassifyJSONErrorBody(t *testing.T) {
	// 登录失效码 → ErrAuth
	err := classifyJSONErrorBody(`{"code":710012001,"msg":"登录已过期，请重新登录","message":"login invalid"}`)
	if err == nil || err.Kind != provider.ErrAuth {
		t.Fatalf("登录过期应归 ErrAuth，实际 %+v", err)
	}
	// 风控限流码 → ErrRate（短冷却、不罚号）
	for _, code := range []int64{710022002, 710022004} {
		err = classifyJSONErrorBody(`{"code":`+itoa(code)+`,"msg":"请求过于频繁"}`)
		if err == nil || err.Kind != provider.ErrRate {
			t.Fatalf("风控码 %d 应归 ErrRate，实际 %+v", code, err)
		}
	}
	// 其它错误码 → ErrUpstream
	err = classifyJSONErrorBody(`{"code":50000,"msg":"内部错误"}`)
	if err == nil || err.Kind != provider.ErrUpstream {
		t.Fatalf("普通错误应归 ErrUpstream，实际 %+v", err)
	}
	// 正常 SSE 文本 → nil（不误判）
	if e := classifyJSONErrorBody("event: SSE_ACK\ndata: {\"x\":1}\n\n"); e != nil {
		t.Fatalf("SSE 文本不应判错: %v", e)
	}
	// 正常 JSON（无 code/msg）→ nil
	if e := classifyJSONErrorBody(`{"ack_client_meta":{"conversation_id":"c"}}`); e != nil {
		t.Fatalf("正常 JSON 应答不应判错: %v", e)
	}
}

// TestParseSseAckAuthBody 登录失效体经 parseSseAck 归类为 ErrAuth。
func TestParseSseAckAuthBody(t *testing.T) {
	_, err := parseSseAck(`{"code":710012001,"msg":"登录已过期，请重新登录"}`)
	if err == nil {
		t.Fatal("应报错")
	}
	if pe, ok := err.(*provider.Error); !ok || pe.Kind != provider.ErrAuth {
		t.Fatalf("应归 ErrAuth，实际 %T %v", err, err)
	}
}

// TestResolveModelText 文本模型表：菜单反带的三元组与回落行为。
func TestResolveModelText(t *testing.T) {
	cases := []struct {
		in           string
		key          string
		agentMode    int
		deepThink    int
		expectPublic string
	}{
		{"doubao-fast", "0", 2, 0, "doubao-fast"},
		{"doubao-chat", "0", 2, 0, "doubao-chat"}, // 向后兼容别名
		{"doubao-expert", "3", 2, 3, "doubao-expert"},
		{"doubao-2.1-lite", "seed-lite-7b", 1, 4, "doubao-2.1-lite"},
		{"doubao-2.1-pro", "5", 1, 5, "doubao-2.1-pro"},
		{"doubao-auto", "9", 1, 4, "doubao-auto"},
		{"Doubao-2.1-Pro", "5", 1, 5, "doubao-2.1-pro"}, // 大小写/归一化
	}
	for _, c := range cases {
		rm := resolveModel(c.in)
		if rm.Kind != modelText {
			t.Errorf("%q 应为文本模型，实际 kind=%d", c.in, rm.Kind)
		}
		if rm.ModelItemKey != c.key || rm.AgentMode != c.agentMode || rm.DeepThink != c.deepThink {
			t.Errorf("%q 三元组错误: key=%q mode=%d deep=%d", c.in, rm.ModelItemKey, rm.AgentMode, rm.DeepThink)
		}
		if rm.Public != c.expectPublic {
			t.Errorf("%q public=%q 期望 %q", c.in, rm.Public, c.expectPublic)
		}
	}
	// 未知名回落快速，且透传原名
	rm := resolveModel("某个不存在的模型")
	if rm.Kind != modelText || rm.ModelItemKey != "0" || rm.AgentMode != 2 || rm.DeepThink != 0 {
		t.Fatalf("未知名应回落快速: %+v", rm)
	}
	if rm.Public != "某个不存在的模型" {
		t.Fatalf("未知名应透传原名, 实际 %q", rm.Public)
	}
	// 图片/视频不被文本表劫持
	if got := resolveModel("seedream-4.5"); got.Kind != modelImage {
		t.Fatalf("seedream 应是图片模型")
	}
	if got := resolveModel("seedance-2.0-pro"); got.Kind != modelVideo || got.Upstream != "seedance_v2.0_std" {
		t.Fatalf("seedance 应是视频模型: %+v", got)
	}
}

// TestTextSubmitPayloadModelFields 文本 payload 必须带模型选择三元组。
func TestTextSubmitPayloadModelFields(t *testing.T) {
	pro := resolveModel("doubao-2.1-pro")
	p := textSubmitPayload("", "hi", pro)

	ext, _ := p["ext"].(map[string]any)
	if got, _ := ext["model_item_key"].(string); got != "5" {
		t.Fatalf("ext.model_item_key=%v 期望 5", ext["model_item_key"])
	}
	opt, _ := p["option"].(map[string]any)
	if got, _ := opt["agent_mode"].(int); got != 1 {
		t.Fatalf("option.agent_mode=%v 期望 1", opt["agent_mode"])
	}
	if got, _ := opt["need_deep_think"].(int); got != 5 {
		t.Fatalf("option.need_deep_think=%v 期望 5", opt["need_deep_think"])
	}

	// 快速档：need_deep_think=0，agent_mode=2
	fast := resolveModel("doubao-fast")
	pf := textSubmitPayload("", "hi", fast)
	of, _ := pf["option"].(map[string]any)
	if of["need_deep_think"] != 0 || of["agent_mode"] != 2 {
		t.Fatalf("快速档参数错误: deep=%v mode=%v", of["need_deep_think"], of["agent_mode"])
	}
}

// 录制真实的同步模式事件序列（实测 dump_sse_raw.py 输出）。
const recordedSyncSSE = `event: SSE_HEARTBEAT
data: {}

event: SSE_ACK
data: {"query_list":[{"question_id":"q1"}],"ack_client_meta":{"conversation_id":"conv-1","section_id":"s1"}}

event: FULL_MSG_NOTIFY
data: {"message":{"conversation_id":"conv-1"}}

event: STREAM_MSG_NOTIFY
data: {"content":{"content_block":[{"block_type":10000,"content":{"text_block":{"text":"你"}}}]}}

event: STREAM_CHUNK
data: {"patch_op":[{"patch_object":1,"patch_value":{"content_block":[{"block_type":10000,"content":{"text_block":{"text":"好"}}}]}}]}

event: STREAM_CHUNK
data: {"patch_op":[{"patch_object":111,"patch_value":{"tts_content":"好"}}]}

event: CHUNK_DELTA
data: {"text":"吗"}

event: STREAM_CHUNK
data: {"patch_op":[{"patch_object":1,"patch_value":{"content_block":[{"block_type":10000,"content":{"text_block":{"text":""}}}]}}]}

event: SSE_REPLY_END
data: {"end_type":1,"msg_finish_attr":{"msgid":"m1","brief":"你好吗"}}

event: SSE_REPLY_END
data: {"end_type":2}

event: SSE_REPLY_END
data: {"end_type":3}

`

// TestInlineTextAcc 三路文本（notify/chunk/delta）按序拼接，brief 校正补尾。
func TestInlineTextAcc(t *testing.T) {
	rd := bufio.NewReader(strings.NewReader(recordedSyncSSE))
	acc := &inlineTextAcc{}
	var streamed strings.Builder
	ended := false
	for !ended {
		pkt, err := readSSEPacket(rd)
		if pkt == "" && err != nil {
			break
		}
		ev, data := splitSSEPacket(pkt)
		add, done, errMsg := acc.feedPacket(ev, data)
		if errMsg != "" {
			t.Fatalf("不应报错: %s", errMsg)
		}
		streamed.WriteString(add)
		acc.sb.WriteString(add)
		if done {
			ended = true
		}
	}
	if got := streamed.String(); got != "你好吗" {
		t.Fatalf("流式累积=%q 期望 你好吗", got)
	}
	if tail := acc.reconcile(); tail != "" {
		t.Fatalf("brief 与累积一致时不应补发, 实际补发 %q", tail)
	}
	if acc.brief != "你好吗" {
		t.Fatalf("brief 未捕获: %q", acc.brief)
	}
}

// TestInlineTextAccBriefTail brief 比累积长时（丢尾/丢头）应补齐差额。
func TestInlineTextAccBriefTail(t *testing.T) {
	acc := &inlineTextAcc{}
	acc.sb.WriteString("你好")
	acc.brief = "你好吗世界"
	if tail := acc.reconcile(); tail != "吗世界" {
		t.Fatalf("应补发尾部, 实际 %q", tail)
	}
	// 完全空累积 → 整段补发
	empty := &inlineTextAcc{brief: "全文"}
	if tail := empty.reconcile(); tail != "全文" {
		t.Fatalf("空累积应整段补发, 实际 %q", tail)
	}
	// 累积与 brief 不匹配 → 信任已流出的文本（不重复）
	mismatch := &inlineTextAcc{brief: "abc"}
	mismatch.sb.WriteString("xyz")
	if tail := mismatch.reconcile(); tail != "" {
		t.Fatalf("不匹配时不应补发, 实际 %q", tail)
	}
}

// TestStreamInlineEmitsOpenAIChunks 同步模式产出的 chunk 满足 OpenAI 契约。
func TestStreamInlineEmitsOpenAIChunks(t *testing.T) {
	live := &liveAck{
		ack:  &sseAck{ConversationID: "conv-1"},
		read: bufio.NewReader(strings.NewReader(recordedSyncSSE)),
		body: io.NopCloser(bytes.NewReader(nil)),
	}
	pr, pw := io.Pipe()
	go func() {
		(&Provider{}).streamInline(context.Background(), live, "doubao-fast", pw)
	}()
	raw, err := io.ReadAll(pr)
	if err != nil {
		t.Fatalf("读流失败: %v", err)
	}
	out := string(raw)
	if !strings.Contains(out, `"object":"chat.completion.chunk"`) {
		t.Fatal("chunk 缺少 object=chat.completion.chunk")
	}
	if !strings.Contains(out, `"created":`) {
		t.Fatal("chunk 缺少 created 字段")
	}
	if !strings.Contains(out, `"model":"doubao-fast"`) {
		t.Fatal("chunk 的 model 应为对外 ID")
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatal("缺少 [DONE] 收尾")
	}
	// 拼接所有 delta.content 应等于全文
	var got strings.Builder
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk) != nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			got.WriteString(chunk.Choices[0].Delta.Content)
			if chunk.Choices[0].FinishReason == "stop" {
				got.WriteString("")
			}
		}
	}
	if got.String() != "你好吗" {
		t.Fatalf("delta 拼接=%q 期望 你好吗", got.String())
	}
}

// itoa 测试辅助：int64 → 十进制串。
func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// TestCommonQueryMsToken 无真值时 msToken 参数必须整个省略（空值触发风控 710022002）。
func TestCommonQueryMsToken(t *testing.T) {
	ids := deviceIDs{DeviceID: "7123", WebID: "7456"}
	// 无 msToken：不应出现该参数
	q := commonQuery(ids, "fp1", "CN", "")
	if _, ok := q["msToken"]; ok {
		t.Fatalf("无 msToken 时不应携带该参数: %v", q)
	}
	// 有 msToken：应原样带上
	q2 := commonQuery(ids, "fp1", "CN", "tok123")
	if got := q2.Get("msToken"); got != "tok123" {
		t.Fatalf("msToken 应为 tok123，实际 %q", got)
	}
}
