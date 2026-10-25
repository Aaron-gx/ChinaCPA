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
