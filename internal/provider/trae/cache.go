package trae

// markCachePrefix 给稳定的系统提示添加一个上游支持的 ephemeral 缓存断点。
func markCachePrefix(obj map[string]any) {
	messages, _ := obj["messages"].([]any)
	var prefixEnd map[string]any
	inPrefix := true
	for _, item := range messages {
		message, _ := item.(map[string]any)
		role, _ := message["role"].(string)
		inPrefix = inPrefix && (role == "system" || role == "developer")
		parts, _ := message["content"].([]any)
		for _, item := range parts {
			part, _ := item.(map[string]any)
			if part["cache_control"] != nil {
				return
			}
			if text, _ := part["text"].(string); inPrefix && part["type"] == "text" && text != "" {
				prefixEnd = part
			}
		}
	}
	tools, _ := obj["tools"].([]any)
	for _, item := range tools {
		tool, _ := item.(map[string]any)
		if tool["cache_control"] != nil {
			return
		}
	}
	if prefixEnd != nil {
		prefixEnd["cache_control"] = map[string]string{"type": "ephemeral"}
	}
}
