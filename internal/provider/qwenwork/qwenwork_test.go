package qwenwork

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestSignMaterialRoundTrip 验证签名材料可被服务端视角还原：
// cosy-key → RSA 私钥解出 e，e 解 AES-CBC 得 userInfo（PKCS7 去填充）。
// 用临时生成的 RSA-1024 密钥对替换嵌入公钥做闭环校验。
func TestSignMaterialRoundTrip(t *testing.T) {
	orig := cachedPubKey
	defer func() { cachedPubKey = orig }()

	priv, err := rsaGenerate1024()
	if err != nil {
		t.Fatal(err)
	}
	cachedPubKey = &priv.PublicKey

	m, err := buildSignMaterial("tok-abc", "uid-1", "张三", "a@b.c")
	if err != nil {
		t.Fatal(err)
	}
	eBytes, err := rsa.DecryptPKCS1v15(nil, priv, mustB64(t, m.cosyKey))
	if err != nil {
		t.Fatalf("cosy-key 解密失败: %v", err)
	}
	if len(eBytes) != 16 {
		t.Fatalf("AES key 长度 %d，期望 16", len(eBytes))
	}
	info, err := base64.StdEncoding.DecodeString(m.info)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(eBytes)
	if block == nil {
		t.Fatal("aes cipher 构造失败")
	}
	if len(info) == 0 || len(info)%block.BlockSize() != 0 {
		t.Fatalf("info 密文长度非法: %d", len(info))
	}
	plain := make([]byte, len(info))
	cipher.NewCBCDecrypter(block, eBytes[:block.BlockSize()]).CryptBlocks(plain, info)
	pad := int(plain[len(plain)-1])
	if pad <= 0 || pad > block.BlockSize() {
		t.Fatalf("PKCS7 填充非法: %d", pad)
	}
	plain = plain[:len(plain)-pad]
	var ui map[string]any
	if err := json.Unmarshal(plain, &ui); err != nil {
		t.Fatalf("userInfo 解密失败: %v", err)
	}
	if ui["uid"] != "uid-1" || ui["security_oauth_token"] != "tok-abc" {
		t.Fatalf("userInfo 字段不符: %v", ui)
	}
}

// TestSignHeadersFormat 校验 authorization 结构与签名串拼接口径：
// signStr = base64(header) \n cosyKey \n ts \n body \n path(去 query、去 /algo)。
func TestSignHeadersFormat(t *testing.T) {
	m := &signMaterial{cosyKey: "KEY", info: "INFO", uid: "u1"}
	const ts int64 = 1700000000
	full := "https://gateway.qwenwork.cn/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common"
	h := signHeadersAt(m, full, `{"a":1}`, ts)

	parts := strings.SplitN(strings.TrimPrefix(h["Authorization"], "Bearer COSY."), ".", 2)
	if len(parts) != 2 {
		t.Fatalf("authorization 结构不符: %s", h["Authorization"])
	}
	want := fmt.Sprintf("%x", md5.Sum([]byte(parts[0]+"\nKEY\n1700000000\n"+`{"a":1}`+"\n"+"/api/v2/service/pro/sse/agent_chat_generation")))
	if parts[1] != want {
		t.Fatalf("md5 签名不符:\n got %s\nwant %s", parts[1], want)
	}
	if h["Cosy-Date"] != "1700000000" || h["Cosy-User"] != "u1" || h["Cosy-Key"] != "KEY" {
		t.Fatalf("Cosy 头不符: %v", h)
	}
}

// TestUnwrapLine 校验双层 SSE 解包口径（外层 wrapper / 内层 data: 前缀 / [DONE] / 空 {}）。
func TestUnwrapLine(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"外层字符串体", `data:{"headers":{},"body":"{\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}","statusCodeValue":200}`, `data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n"},
		{"外层对象体", `data:{"body":{"choices":[{"delta":{"content":"x"}}]}}`, `data: {"choices":[{"delta":{"content":"x"}}]}` + "\n\n"},
		{"内层再包 data:", `data:{"body":"data: {\"choices\":[]}"}`, `data: {"choices":[]}` + "\n\n"},
		{"上游 DONE", `data:{"body":"data: [DONE]"}`, ""},
		{"空对象", `data:{"body":"{}"}`, ""},
		{"裸 OpenAI chunk", `data:{"choices":[{"delta":{}}]}`, `data: {"choices":[{"delta":{}}]}` + "\n\n"},
		{"非 data 行", "event: ping", ""},
		// 网关错误帧：HTTP 200 + wrapper statusCodeValue≠200 → OpenAI error 事件
		{"wrapper 402", `data:{"headers":{"Content-Type":["application/json"]},"body":"{\"code\":\"402\",\"message\":\"Error in upstream response\"}","statusCodeValue":402,"statusCode":"Payment Required"}`,
			`data: {"error":{"message":"上游 HTTP 402 Payment Required: Error in upstream response","type":"upstream_error"}}` + "\n\n"},
		{"wrapper 503 无短语", `data:{"body":"{\"message\":\"Model catalog unavailable\"}","statusCodeValue":503}`,
			`data: {"error":{"message":"上游 HTTP 503: Model catalog unavailable","type":"upstream_error"}}` + "\n\n"},
		// 内层错误体（code+message 且无 choices，wrapper 状态 200）→ 转错误事件
		{"内层错误体", `data:{"body":"{\"code\":\"500\",\"message\":\"boom\"}","statusCodeValue":200}`,
			`data: {"error":{"message":"上游错误: boom","type":"upstream_error"}}` + "\n\n"},
		// 裸 error 事件透传
		{"裸 error 事件", `data:{"error":{"message":"x"}}`, `data: {"error":{"message":"x"}}` + "\n\n"},
		// usage-only 块（无 choices 非错误）补空 choices 后透传
		{"usage 块补 choices", `data:{"body":"{\"usage\":{\"total_tokens\":9}}","statusCodeValue":200}`,
			`data: {"choices":[],"usage":{"total_tokens":9}}` + "\n\n"},
	}
	for _, c := range cases {
		got := string(unwrapLine(c.in))
		if got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

// TestPKCEVerifier 官方口径：43..128 字符，字符集 A-Za-z0-9-._~。
func TestPKCEVerifier(t *testing.T) {
	for i := 0; i < 200; i++ {
		v := pkceVerifier()
		if len(v) < 43 || len(v) > 128 {
			t.Fatalf("verifier 长度越界: %d", len(v))
		}
		for _, c := range v {
			if !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~", c) {
				t.Fatalf("verifier 字符集越界: %q", c)
			}
		}
	}
}

func mustB64(t *testing.T, s string) []byte {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func rsaGenerate1024() (*rsa.PrivateKey, error) {
	return rsa.GenerateKey(rand.Reader, 1024)
}

// TestParseDailyClaim 校验每日积分 claim 响应解析。
func TestParseDailyClaim(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want dailyClaim
	}{
		{"领取成功", `{"code":"ok","data":{"should_show":true,"credits":200}}`, dailyClaim{ok: true, shouldShow: true, credits: 200}},
		{"无待领", `{"code":"ok","data":{"should_show":false,"credits":0}}`, dailyClaim{ok: true}},
		{"业务失败", `{"code":"error","errorMessage":"x"}`, dailyClaim{}},
		{"非法 JSON", `not-json`, dailyClaim{}},
	}
	for _, c := range cases {
		got := parseDailyClaim([]byte(c.in))
		if got != c.want {
			t.Errorf("%s:\n got %+v\nwant %+v", c.name, got, c.want)
		}
	}
}
