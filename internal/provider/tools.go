// tools.go 工具定义格式归一化：部分上游（Trae/Qoder）的 FunctionDefinition.parameters
// 只接受 JSON 字符串，而 OpenAI 协议里是 JSON Schema 对象，出站前统一转换。
package provider

import "encoding/json"

// NormalizeToolParamsString 原地归一化请求体 map 里的 tools 参数格式。
func NormalizeToolParamsString(obj map[string]any) {
	tools, ok := obj["tools"].([]any)
	if !ok {
		return
	}
	normalizeToolArr(tools)
}

// normalizeToolArr 遍历 tools，把 function.parameters 的对象/数组序列化为字符串。
// 返回是否有改动。
func normalizeToolArr(tools []any) bool {
	changed := false
	for _, t := range tools {
		tool, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			continue
		}
		p, exists := fn["parameters"]
		if !exists {
			continue
		}
		switch p.(type) {
		case map[string]any, []any:
			raw, err := json.Marshal(p)
			if err != nil {
				continue
			}
			fn["parameters"] = string(raw)
			changed = true
		}
	}
	return changed
}

// NormalizeToolTupleItems 原地把 tools[].function.parameters 里 draft-07 元组形态的
// items（数组）改写为等价的 anyOf 对象形态。WorkBuddy/CodeBuddy 上游的 schema 校验器
// 拒收元组 items（HTTP 400 code=11129 invalid_function_parameters），实测 anyOf 等价
// 形态可通过；additionalItems 一并删除（语义有损，但工具参数场景可接受）。
// 递归覆盖 properties/items/anyOf/oneOf/allOf/prefixItems/contains/additionalProperties/
// patternProperties/$defs。parameters 为字符串（Trae 形态）时跳过——那类上游校验器不同。
func NormalizeToolTupleItems(obj map[string]any) {
	tools, ok := obj["tools"].([]any)
	if !ok {
		return
	}
	for _, t := range tools {
		tool, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			continue
		}
		if p, ok := fn["parameters"].(map[string]any); ok {
			normalizeSchemaTupleItems(p)
		}
	}
}

// normalizeSchemaTupleItems 递归改写 JSON Schema 节点，返回是否有改动。
func normalizeSchemaTupleItems(node any) bool {
	switch v := node.(type) {
	case map[string]any:
		changed := false
		// items 为数组（tuple）→ {anyOf:[...]}
		if items, ok := v["items"].([]any); ok {
			branches := make([]any, 0, len(items))
			for _, it := range items {
				normalizeSchemaTupleItems(it)
				branches = append(branches, it)
			}
			if len(branches) == 0 {
				v["items"] = map[string]any{}
			} else {
				v["items"] = map[string]any{"anyOf": branches}
			}
			delete(v, "additionalItems")
			changed = true
		} else if items, ok := v["items"].(map[string]any); ok {
			if normalizeSchemaTupleItems(items) {
				changed = true
			}
		}
		if pi, ok := v["prefixItems"].([]any); ok {
			// 2020-12 tuple：prefixItems → items anyOf，并删除 prefixItems
			branches := make([]any, 0, len(pi))
			for _, it := range pi {
				normalizeSchemaTupleItems(it)
				branches = append(branches, it)
			}
			if len(branches) > 0 {
				if _, has := v["items"]; !has {
					v["items"] = map[string]any{"anyOf": branches}
				}
				delete(v, "prefixItems")
				delete(v, "additionalItems")
				changed = true
			}
		}
		if c, ok := v["contains"].(map[string]any); ok {
			if normalizeSchemaTupleItems(c) {
				changed = true
			}
		}
		for _, key := range []string{"anyOf", "oneOf", "allOf"} {
			if arr, ok := v[key].([]any); ok {
				for _, sub := range arr {
					if normalizeSchemaTupleItems(sub) {
						changed = true
					}
				}
			}
		}
		if props, ok := v["properties"].(map[string]any); ok {
			for _, sub := range props {
				if normalizeSchemaTupleItems(sub) {
					changed = true
				}
			}
		}
		if pp, ok := v["patternProperties"].(map[string]any); ok {
			for _, sub := range pp {
				if normalizeSchemaTupleItems(sub) {
					changed = true
				}
			}
		}
		switch ap := v["additionalProperties"].(type) {
		case map[string]any:
			if normalizeSchemaTupleItems(ap) {
				changed = true
			}
		}
		if defs, ok := v["$defs"].(map[string]any); ok {
			for _, sub := range defs {
				if normalizeSchemaTupleItems(sub) {
					changed = true
				}
			}
		}
		return changed
	case []any:
		changed := false
		for _, sub := range v {
			if normalizeSchemaTupleItems(sub) {
				changed = true
			}
		}
		return changed
	default:
		return false
	}
}
