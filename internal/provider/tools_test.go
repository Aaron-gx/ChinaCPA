package provider

import (
	"encoding/json"
	"testing"
)

func mustObj(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestNormalizeToolTupleItems(t *testing.T) {
	obj := mustObj(t, `{"tools":[
	  {"type":"function","function":{"name":"a","parameters":{"type":"object","properties":{
	     "p":{"type":"array","items":[{"type":"number"},{"type":"string"}],"additionalItems":false}}}}},
	  {"type":"function","function":{"name":"b","parameters":{"type":"object","properties":{
	     "x":{"type":"object","properties":{"y":{"type":"array","prefixItems":[{"type":"integer"},{"type":"null"}]}}}}}}},
	  {"type":"function","function":{"name":"c","parameters":{"type":"object","properties":{
	     "z":{"type":"array","items":{"type":"object","properties":{"w":{"type":"array","items":[{"type":"boolean"}]}}}}}}}}
	]}`)
	NormalizeToolTupleItems(obj)

	tools := obj["tools"].([]any)

	// a: tuple → anyOf，additionalItems 删除
	pa := tools[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
	items := pa["properties"].(map[string]any)["p"].(map[string]any)["items"].(map[string]any)
	anyOf, ok := items["anyOf"].([]any)
	if !ok || len(anyOf) != 2 {
		t.Fatalf("tuple 未转 anyOf: %v", items)
	}
	if _, has := pa["properties"].(map[string]any)["p"].(map[string]any)["additionalItems"]; has {
		t.Fatal("additionalItems 应删除")
	}

	// b: prefixItems → items anyOf
	pb := tools[1].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
	x := pb["properties"].(map[string]any)["x"].(map[string]any)["properties"].(map[string]any)["y"].(map[string]any)
	if _, has := x["prefixItems"]; has {
		t.Fatal("prefixItems 应删除")
	}
	if _, ok := x["items"].(map[string]any)["anyOf"].([]any); !ok {
		t.Fatalf("prefixItems 未转 items.anyOf: %v", x)
	}

	// c: 深层嵌套 tuple 也改写
	pc := tools[2].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
	w := pc["properties"].(map[string]any)["z"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)["w"].(map[string]any)
	if _, ok := w["items"].(map[string]any)["anyOf"].([]any); !ok {
		t.Fatalf("嵌套 tuple 未改写: %v", w)
	}
}

func TestNormalizeToolTupleItemsSkipsNonObject(t *testing.T) {
	// parameters 为字符串（Trae 形态）不动；无 tools 不炸
	obj := mustObj(t, `{"tools":[{"type":"function","function":{"name":"a","parameters":"{\"type\":\"object\"}"}}]}`)
	NormalizeToolTupleItems(obj)
	p := obj["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"]
	if _, ok := p.(string); !ok {
		t.Fatalf("字符串 parameters 不应被改动: %T", p)
	}
	NormalizeToolTupleItems(mustObj(t, `{"messages":[]}`))
}

func TestRegexParenDepth(t *testing.T) {
	cases := map[string]int{
		`^[a-z]+$`:                          0,
		`^(?:foo|bar)$`:                     1,
		`^(?:a(?:b(?:c|d))?)$`:              3,
		`^[^\u0000]+$`:                        0,   // 字符类里的括号不算
		`^(?:[^\u0000R][^\u0000]*|R(?:x|y))$`:   2,
	}
	for pat, want := range cases {
		if got := regexParenDepth(pat); got != want {
			t.Errorf("regexParenDepth(%q)=%d 期望 %d", pat, got, want)
		}
	}
	// REA 环境键正则（真实触发形态）：深度远超阈值
	big := `^(?:[^\u0000R][^\u0000]*|R(?:[^\u0000E][^\u0000]*|E(?:[^\u0000A][^\u0000]*|A(?:[^\u0000_][^\u0000]*|_(?:[^\u0000P][^\u0000]*|P(?:[^\u0000R][^\u0000]*|R(?:[^\u0000O][^\u0000]*|O(?:[^\u0000C][^\u0000]*|C(?:[^\u0000E][^\u0000]*|E(?:[^\u0000S][^\u0000]*|S(?:[^\u0000S][^\u0000]*|S(?:[^\u0000_][^\u0000]*|_(?:[^\u0000R][^\u0000]*|R(?:[^\u0000U][^\u0000]*|U(?:[^\u0000N][^\u0000]*|N(?:[^\u0000_][^\u0000]*|_(?:[^\u0000I][^\u0000]*|I(?:[^\u0000D][^\u0000]*|D[^\u0000]+)?)?)?)?)?)?)?)?)?)?)?)?)?)?)?)?)?)?)?)$`
	if regexParenDepth(big) <= patternMaxDepth {
		t.Fatalf("REA 正则深度 %d 应超过阈值 %d", regexParenDepth(big), patternMaxDepth)
	}
}

func TestNormalizeToolSchemaPatterns(t *testing.T) {
	obj := mustObj(t, `{"tools":[
	  {"type":"function","function":{"name":"a","parameters":{"type":"object","properties":{
	     "k":{"pattern":"^(?:[^\u0000R][^\u0000]*|R(?:[^\u0000E][^\u0000]*|E(?:[^\u0000A][^\u0000]*|A(?:[^\u0000_][^\u0000]*|_(?:[^\u0000P][^\u0000]*|P(?:[^\u0000R][^\u0000]*|R(?:[\u0000])+))?)?)?)?)?)?$","type":"string"},
	     "env":{"propertyNames":{"pattern":"^(?:a|b)$","type":"string"},"type":"object"}}}}},
	  {"type":"function","function":{"name":"b","parameters":{"type":"object","properties":{
	     "path":{"pattern":"^/[a-z]+$","type":"string"}}}}}]}`)
	NormalizeToolSchemaPatterns(obj)

	pa := obj["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
	props := pa["properties"].(map[string]any)
	k := props["k"].(map[string]any)
	if _, has := k["pattern"]; has {
		t.Fatal("深层嵌套 pattern 应被删除")
	}
	// 浅 pattern（propertyNames 深度 1）保留
	env := props["env"].(map[string]any)["propertyNames"].(map[string]any)
	if _, has := env["pattern"]; !has {
		t.Fatal("浅 pattern 应保留")
	}
	// 另一个工具的浅 pattern 保留
	pb := obj["tools"].([]any)[1].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
	if _, has := pb["properties"].(map[string]any)["path"].(map[string]any)["pattern"]; !has {
		t.Fatal("普通 pattern 应保留")
	}
}
