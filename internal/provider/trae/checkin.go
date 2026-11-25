package trae

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"chinacpa/internal/model"
	"chinacpa/internal/provider"
)

type checkinStatus struct {
	Enabled      *bool `json:"enable"`
	Checked      *bool `json:"checked_in"`
	DidCheck     *bool `json:"did_checked_in"`
	Credits      int64 `json:"credits"`
	ExtraCredits int64 `json:"extra_credits"`
}

func (s checkinStatus) signed() bool {
	return (s.Checked != nil && *s.Checked) || (s.DidCheck != nil && *s.DidCheck)
}

func (p *Provider) checkinRequest(ctx context.Context, a *model.Account, path string, dst any) error {
	// Windows 上的 Trae IDE 使用 req_source=1。两个端点都要求这个字段。
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ugHost+path, bytes.NewReader([]byte("{\"req_source\":1}")))
	if err != nil {
		return err
	}
	p.ugHeaders(req, a)
	req.Header.Set("X-App-Version", ideVersion)
	req.Header.Set("X-Device-Type", "windows")
	req.Header.Set("X-OS-Version", osVersion)
	raw, err := p.doJSON(req)
	if err != nil {
		return err
	}
	var result struct {
		Code int64 `json:"code"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		return &provider.Error{Kind: provider.ErrUpstream, Msg: "Trae 签到响应格式错误"}
	}
	if result.Code != 0 {
		kind := provider.ErrUpstream
		msg := "Trae 签到接口返回错误"
		switch result.Code {
		case 1001, 1002:
			kind, msg = provider.ErrAuth, "Trae 签到登录已失效"
		case 9074:
			kind, msg = provider.ErrRate, "Trae 签到繁忙，稍后重试"
		}
		code := strconv.FormatInt(result.Code, 10)
		return &provider.Error{Kind: kind, Code: code, Msg: msg + "（" + code + "）"}
	}
	if dst != nil {
		if err := json.Unmarshal(raw, dst); err != nil {
			return fmt.Errorf("Trae 签到状态解析失败: %w", err)
		}
	}
	return nil
}

func (p *Provider) checkinStatus(ctx context.Context, a *model.Account) (checkinStatus, error) {
	var status checkinStatus
	if err := p.checkinRequest(ctx, a, epCheckinStatus, &status); err != nil {
		return status, err
	}
	if !status.signed() && (status.Enabled == nil || (*status.Enabled && status.Checked == nil && status.DidCheck == nil)) {
		return status, &provider.Error{Kind: provider.ErrUpstream, Msg: "Trae 未返回有效的签到状态"}
	}
	return status, nil
}

func (p *Provider) Checkin(ctx context.Context, a *model.Account) (*model.CheckinResult, error) {
	if strings.EqualFold(a.Realm, "global") {
		return &model.CheckinResult{Info: true, Message: "Trae 每日积分签到仅在国内版开放"}, nil
	}
	before, err := p.checkinStatus(ctx, a)
	if err != nil {
		return nil, err
	}
	if before.signed() {
		return &model.CheckinResult{OK: true, Already: true, Message: "今天已签到"}, nil
	}
	if !*before.Enabled {
		return &model.CheckinResult{Info: true, Message: "当前 Trae 账号未开放每日签到"}, nil
	}
	// 状态里的 credits 是客户端展示的本次签到积分，不是账户余额。
	if err := p.checkinRequest(ctx, a, epCheckinClaim, nil); err != nil {
		return nil, err
	}
	after, err := p.checkinStatus(ctx, a)
	if err != nil {
		return nil, err
	}
	if !after.signed() {
		return &model.CheckinResult{Message: "Trae 暂未确认签到，稍后重新查询"}, nil
	}
	earned := before.Credits + before.ExtraCredits
	return &model.CheckinResult{OK: true, Message: "签到成功", CreditsEarned: max(0, earned)}, nil
}
