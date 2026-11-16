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
