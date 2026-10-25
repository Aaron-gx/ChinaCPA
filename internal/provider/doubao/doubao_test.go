package doubao

import (
	"encoding/json"
	"strings"
	"testing"
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
