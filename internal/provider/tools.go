// tools.go 工具定义格式归一化：部分上游（Trae/Qoder）的 FunctionDefinition.parameters
// 只接受 JSON 字符串，而 OpenAI 协议里是 JSON Schema 对象，出站前统一转换。
package provider

import (
	"encoding/json"
	"strings"
)

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

// patternMaxDepth：JSON Schema pattern 的括号嵌套深度阈值。超过视为"复杂正则"，
// WorkBuddy/CodeBuddy 上游校验器会判非法（HTTP 400 code=11129
// invalid_function_parameters）。实测 ZCode 的 REA 逆向插件工具 schema（环境键
// 正则，十余层嵌套 alternation）必触发；普通 pattern（邮箱/路径/枚举式）深度 ≤ 2，
// 保留不受影响。
const patternMaxDepth = 6

// NormalizeToolSchemaPatterns 删除 tools[].function.parameters 里嵌套过深的
// pattern 字段。pattern 仅是 JSON Schema 校验元数据，LLM function-calling 不会
// 执行它，删除不影响工具调用正确性，可根治上游对复杂正则的 11129 拒单。
func NormalizeToolSchemaPatterns(obj map[string]any) {
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
			mergeAllOfPatterns(p)
			walkDeleteComplexPatterns(p)
		}
	}
}

// mergeAllOfPatterns 把 allOf 里 ≥2 个纯 pattern 分支合并为单一 pattern。
// 上游校验器（WorkBuddy deepseek 后端）拒收 allOf 多 pattern 组合
// （HTTP 400 code=11129，实测仅在请求含 tool 往返消息时触发——
// ZCode 会话的 REA 插件工具 mcp__rea__binary_session 的 expected_server_path
// 是 3 个 pattern 的 allOf，首条工具调用轮必挂）。多个 pattern 语义是「全部
// 匹配」，等价 lookahead 串联；这里用 | 拼接——pattern 只是校验元数据，
// function-calling 不执行，宽松合并可接受。递归覆盖任意层级。
func mergeAllOfPatterns(node any) bool {
	switch v := node.(type) {
	case map[string]any:
		changed := false
		if allOf, ok := v["allOf"].([]any); ok && len(allOf) >= 2 {
			var merged []string
			rest := make([]any, 0, len(allOf))
			for _, branch := range allOf {
				bm, _ := branch.(map[string]any)
				if bm == nil {
					rest = append(rest, branch)
					continue
				}
				if pat, ok := bm["pattern"].(string); ok && len(bm) == 1 {
					merged = append(merged, pat)
					continue
				}
				mergeAllOfPatterns(branch)
				rest = append(rest, branch)
			}
			if len(merged) >= 2 {
				pat := "(?:" + strings.Join(merged, ")|(?:") + ")"
				if len(rest) == 0 {
					// 分支全是 pattern：合并为单 pattern，直接提升
					v["pattern"] = pat
					delete(v, "allOf")
				} else {
					v["allOf"] = append(rest, map[string]any{"pattern": pat})
				}
				changed = true
			}
		}
		for _, sub := range v {
			if mergeAllOfPatterns(sub) {
				changed = true
			}
		}
		return changed
	case []any:
		changed := false
		for _, sub := range v {
			if mergeAllOfPatterns(sub) {
				changed = true
			}
		}
		return changed
	default:
		return false
	}
}

// walkDeleteComplexPatterns 递归遍历 schema 任意层，删除上游拒收的 pattern 键
// （含 properties/items/anyOf/propertyNames 等任意位置）。返回是否有改动。
// 拒收形态（均实测 11129/11101）：
//  1. 括号嵌套深度超阈值（ZCode REA 插件的环境键正则）
//  2. 含 UTF-16 代理对区间转义 \uD800-\uDFFF —— 上游 proto JSON 解码器把它
//     解成孤立代理半字节，非法 UTF-8。REA 的 expected_server_path 三段 allOf
//     里就有一段；此前 allOf 多 pattern 组合挂的真正毒源就是它。
func walkDeleteComplexPatterns(node any) bool {
	switch v := node.(type) {
	case map[string]any:
		changed := false
		if p, ok := v["pattern"].(string); ok {
			if regexParenDepth(p) > patternMaxDepth || regexHasSurrogateEscape(p) {
				delete(v, "pattern")
				changed = true
			}
		}
		for _, sub := range v {
			if walkDeleteComplexPatterns(sub) {
				changed = true
			}
		}
		return changed
	case []any:
		changed := false
		for _, sub := range v {
			if walkDeleteComplexPatterns(sub) {
				changed = true
			}
		}
		return changed
	default:
		return false
	}
}

// regexHasSurrogateEscape 检查正则文本里是否含 \uD800-\uDFFF 形式的
// 代理对区间转义（大小写不敏感）。
func regexHasSurrogateEscape(pat string) bool {
	up := strings.ToUpper(pat)
	return strings.Contains(up, `\UD800`) || strings.Contains(up, `\UDFFF`) ||
		strings.Contains(up, `\UD7FF`) || strings.Contains(up, `\UE000`)
}

// regexParenDepth 返回正则里括号的最大嵌套深度（忽略字符类 [...] 内的括号）。
func regexParenDepth(pat string) int {
	depth, max := 0, 0
	esc, inClass := false, false
	for _, r := range pat {
		if esc {
			esc = false
			continue
		}
		switch r {
		case '\\':
			esc = true
		case '[':
			if !inClass {
				inClass = true
			}
		case ']':
			inClass = false
		case '(':
			if !inClass {
				depth++
				if depth > max {
					max = depth
				}
			}
		case ')':
			if !inClass && depth > 0 {
				depth--
			}
		}
	}
	return max
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
