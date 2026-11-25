package doubao

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"chinacpa/internal/model"
	"chinacpa/internal/provider"
)

// 豆包桌面端“今日签到领 200 额度”使用的国内活动组。这个值来自客户端的
// ug_checkin_remind 推送映射，而不是通用 landing 请求的默认活动组。
const doubaoCheckinGroup = "g_1229d8e3a1becc5ab073ebf150db7104"

const (
	doubaoLandingPath = "/alice/ug/campaign/landing/get"
	doubaoCheckinPath = "/alice/ug/campaign/landing/checkin"
)

// Checkin 读取桌面端签到活动，领取后必须确认 data.checked_in=true。
// 活动是按账号灰度开放的；没有资格时返回 Info，不把它当成网络故障。
func (p *Provider) Checkin(ctx context.Context, a *model.Account) (*model.CheckinResult, error) {
	landing, err := p.fetchCheckinLanding(ctx, a)
	if err != nil {
		return nil, err
	}
	if landing.checkedIn {
		return &model.CheckinResult{OK: true, Already: true, Message: "今天已签到", CreditsEarned: landing.credits}, nil
	}
	if !landing.available {
		return &model.CheckinResult{Info: true, Message: "当前账号暂无可用的豆包签到活动"}, nil
	}

	scene := landing.scene
	if scene == "" {
		scene = doubaoCheckinGroup
	}
	body, _ := json.Marshal(map[string]string{"scene": scene})
	q := commonQuery(p.seedOf(a), fpOf(a), regionOf(a), msTokenOf(a))
	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, err := p.dbPost(reqCtx, a, doubaoCheckinPath, q, "application/json", body, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := p.checkinHTTPError(resp.StatusCode, raw); err != nil {
		return nil, err
	}
	result, err := parseCampaignResult(raw)
	if err != nil {
		return nil, err
	}
	if result.checkedIn {
		return &model.CheckinResult{OK: true, Message: "签到成功", CreditsEarned: result.credits}, nil
	}
	return &model.CheckinResult{OK: false, Message: "豆包签到未确认，稍后将自动重试"}, nil
}

type campaignResult struct {
	checkedIn bool
	available bool
	scene     string
	credits   int64
}

func (p *Provider) fetchCheckinLanding(ctx context.Context, a *model.Account) (campaignResult, error) {
	body, _ := json.Marshal(map[string]any{
		"target_card_group_id": doubaoCheckinGroup,
		"badge_counts":         map[string]int{"incentive": 1},
	})
	q := commonQuery(p.seedOf(a), fpOf(a), regionOf(a), msTokenOf(a))
	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, err := p.dbPost(reqCtx, a, doubaoLandingPath, q, "application/json", body, nil)
	if err != nil {
		return campaignResult{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err := p.checkinHTTPError(resp.StatusCode, raw); err != nil {
		return campaignResult{}, err
	}
	return parseCampaignLanding(raw)
}

func (p *Provider) checkinHTTPError(status int, raw []byte) error {
	if status == http.StatusUnauthorized || status == http.StatusForbidden ||
		(status >= 300 && status < 400) {
		return authExpiredError(status, string(raw))
	}
	if status >= 400 {
		return &provider.Error{Kind: classifyStatus(status), Status: status,
			Msg: fmt.Sprintf("豆包签到接口 HTTP %d: %s", status, truncateRunes(string(raw), 200))}
	}
	return nil
}

func parseCampaignLanding(raw []byte) (campaignResult, error) {
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return campaignResult{}, fmt.Errorf("豆包签到资格响应解析失败: %w", err)
	}
	if err := campaignCodeError(root); err != nil {
		return campaignResult{}, err
	}
	r := campaignResult{scene: findCampaignScene(root)}
	data, _ := root.(map[string]any)["data"]
	r.checkedIn = directBool(data, "checked_in")
	r.credits = findCredits(root)
	r.available = r.checkedIn || r.scene != "" || hasCheckinButton(root)
	return r, nil
}

func parseCampaignResult(raw []byte) (campaignResult, error) {
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return campaignResult{}, fmt.Errorf("豆包签到领取响应解析失败: %w", err)
	}
	if err := campaignCodeError(root); err != nil {
		return campaignResult{}, err
	}
	data, _ := root.(map[string]any)["data"]
	return campaignResult{checkedIn: directBool(data, "checked_in"), credits: findCredits(data)}, nil
}

func campaignCodeError(root any) error {
	m, ok := root.(map[string]any)
	if !ok {
		return &provider.Error{Kind: provider.ErrUpstream, Msg: "豆包签到响应格式错误"}
	}
	if code, exists := m["code"]; exists {
		if n, ok := numberValue(code); ok && n != 0 && n != 200 {
			raw, _ := json.Marshal(root)
			if err := classifyJSONErrorBody(string(raw)); err != nil {
				return err
			}
			msg := findText(m)
			return &provider.Error{Kind: provider.ErrUpstream, Code: fmt.Sprint(n),
				Msg: "豆包签到活动接口返回错误: " + truncateRunes(msg, 160)}
		}
	}
	return nil
}

func findCampaignScene(v any) string {
	if scene := findStringField(v, "campaigngroupcode"); scene != "" {
		return scene
	}
	return findStringField(v, "scene")
}

func findStringField(v any, field string) string {
	var result string
	walkJSON(v, func(key string, value any) bool {
		if strings.ToLower(strings.ReplaceAll(key, "_", "")) == field {
			if s, ok := value.(string); ok && strings.TrimSpace(s) != "" {
				result = strings.TrimSpace(s)
				return false
			}
		}
		return true
	})
	return result
}

func hasCheckinButton(v any) bool {
	found := false
	walkJSON(v, func(key string, value any) bool {
		k := strings.ToLower(strings.ReplaceAll(key, "_", ""))
		if k == "interactiontype" || k == "buttontype" {
			if n, ok := numberValue(value); ok && n == 2 {
				found = true
				return false
			}
			s, _ := value.(string)
			if strings.Contains(strings.ToLower(s), "check") || strings.Contains(s, "签到") {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

func directBool(v any, key string) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	value, ok := m[key]
	if !ok {
		return false
	}
	if b, ok := value.(bool); ok {
		return b
	}
	if n, ok := numberValue(value); ok {
		return n != 0
	}
	if s, ok := value.(string); ok {
		return s == "1" || strings.EqualFold(s, "true")
	}
	return false
}

func findCredits(v any) int64 {
	var credits int64
	walkJSON(v, func(key string, value any) bool {
		k := strings.ToLower(strings.ReplaceAll(key, "_", ""))
		switch k {
		case "credits", "creditsearned", "rewardamount", "rewardcredits", "amount":
			if n, ok := numberValue(value); ok && n > 0 {
				credits = n
				return false
			}
		}
		return true
	})
	return credits
}

func findText(v any) string {
	var text string
	walkJSON(v, func(key string, value any) bool {
		if text != "" {
			return false
		}
		if s, ok := value.(string); ok {
			k := strings.ToLower(strings.ReplaceAll(key, "_", ""))
			if k == "msg" || k == "message" || k == "title" || k == "content" || k == "text" {
				text = s
				return false
			}
		}
		return true
	})
	return text
}

func numberValue(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), n == float64(int64(n))
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	case string:
		var i int64
		_, err := fmt.Sscan(strings.TrimSpace(n), &i)
		return i, err == nil
	default:
		return 0, false
	}
}

func walkJSON(v any, visit func(string, any) bool) bool {
	switch t := v.(type) {
	case map[string]any:
		for key, value := range t {
			if !visit(key, value) || !walkJSON(value, visit) {
				return false
			}
		}
	case []any:
		for _, value := range t {
			if !walkJSON(value, visit) {
				return false
			}
		}
	}
	return true
}
