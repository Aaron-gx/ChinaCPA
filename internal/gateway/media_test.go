package gateway

import "testing"

func TestParseVideoFlags(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantText  string
		wantRatio string
		wantDur   int
	}{
		{"Ark 标准参数", "夕阳下奔跑的少年 --resolution 720p --duration 5 --ratio 16:9", "夕阳下奔跑的少年", "16:9", 5},
		{"等号形式", "一只猫 --duration=10 --ratio=9:16", "一只猫", "9:16", 10},
		{"ar 别名", "城市夜景 --ar 21:9", "城市夜景", "21:9", 0},
		{"未知参数保留", "测试 --custom x --duration 8", "测试 --custom x", "", 8},
		{"无参数", "纯提示词", "纯提示词", "", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ratio, duration := "", 0
			got := parseVideoFlags(c.in, &ratio, &duration)
			if got != c.wantText {
				t.Errorf("提示词 = %q，期望 %q", got, c.wantText)
			}
			if ratio != c.wantRatio {
				t.Errorf("ratio = %q，期望 %q", ratio, c.wantRatio)
			}
			if duration != c.wantDur {
				t.Errorf("duration = %d，期望 %d", duration, c.wantDur)
			}
		})
	}
}
