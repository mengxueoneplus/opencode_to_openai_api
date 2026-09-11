package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

func selectProtocol(model string, c Config) (string, error) {
	if p := c.Upstream.ModelProtocols[model]; p != "" {
		return p, nil
	}
	for _, prefix := range []string{"gpt-", "grok-", "muse-"} {
		if strings.HasPrefix(model, prefix) {
			return "responses", nil
		}
	}
	if strings.HasPrefix(model, "claude-") || strings.HasPrefix(model, "qwen") || (strings.HasPrefix(model, "minimax-") && strings.Contains(c.Upstream.BaseURL+"/", "/go/")) {
		return "messages", nil
	}
	if strings.HasPrefix(model, "gemini-") || model == "" {
		return "", errors.New("模型为空或原生协议暂不支持，可使用 model_protocols 指定已支持的协议")
	}
	return "chat", nil
}

func contentParts(content any, protocol, role string) ([]any, error) {
	if content == nil {
		return []any{}, nil
	}
	if text, ok := content.(string); ok {
		if text == "" {
			return []any{}, nil
		}
		content = []any{object{"type": "text", "text": text}}
	}
	items, ok := content.([]any)
	if !ok {
		return nil, errors.New("content 必须为字符串、数组或 null")
	}
	result := make([]any, 0, len(items))
	for _, raw := range items {
		part := obj(raw)
		switch str(part["type"]) {
		case "text":
			text, ok := part["text"].(string)
			if !ok {
				return nil, errors.New("text 必须为字符串")
			}
			kind := "text"
			if protocol == "responses" {
				kind = "input_text"
				if role == "assistant" {
					kind = "output_text"
				}
			}
			result = append(result, object{"type": kind, "text": text})
		case "image_url":
			image := obj(part["image_url"])
			url := str(image["url"])
			if url == "" || role != "user" {
				return nil, errors.New("图片仅支持用户消息，image_url.url 不能为空")
			}
			if protocol == "responses" {
				detail := image["detail"]
				if detail == nil {
					detail = "auto"
				}
				result = append(result, object{"type": "input_image", "image_url": url, "detail": detail})
			} else {
				source := object{"type": "url", "url": url}
				if strings.HasPrefix(url, "data:") {
					header, data, ok := strings.Cut(url, ",")
					if !ok || !strings.HasSuffix(header, ";base64") {
						return nil, errors.New("图片 data URL 必须使用 base64 编码")
					}
					source = object{"type": "base64", "media_type": strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64"), "data": data}
				}
				result = append(result, object{"type": "image", "source": source})
			}
		default:
			return nil, fmt.Errorf("不支持 content 类型: %s", str(part["type"]))
		}
	}
	return result, nil
}

func convertRequest(body object, protocol string, defaultTokens int) (object, error) {
	if protocol == "chat" {
		return body, nil
	}
	allowed := " model messages stream stream_options max_tokens max_completion_tokens temperature top_p stop tools tool_choice parallel_tool_calls response_format reasoning_effort n user "
	for key, value := range body {
		if value != nil && !strings.Contains(allowed, " "+key+" ") {
			return nil, fmt.Errorf("此协议无法无损转换参数: %s", key)
		}
	}
	if body["n"] != nil && number(body["n"]) != 1 {
		return nil, errors.New("跨协议只支持 n=1")
	}
	out := object{"model": body["model"], "stream": boolean(body["stream"])}
	copyFields(out, body, "temperature", "top_p")
	limit := body["max_completion_tokens"]
	if limit == nil {
		limit = body["max_tokens"]
	}
	messages := make([]any, 0)
	system := make([]any, 0)
	if protocol == "messages" {
		if body["reasoning_effort"] != nil {
			return nil, errors.New("Messages 不支持通用 reasoning_effort")
		}
		if format := str(obj(body["response_format"])["type"]); format != "" && format != "text" {
			return nil, errors.New("Messages 暂不支持 JSON response_format")
		}
		if limit == nil {
			limit = defaultTokens
		}
		out["max_tokens"] = limit
		if stop := body["stop"]; stop != nil {
			if text, ok := stop.(string); ok {
				out["stop_sequences"] = []string{text}
			} else {
				out["stop_sequences"] = stop
			}
		}
	} else {
		if body["stop"] != nil {
			return nil, errors.New("Responses 不支持 stop")
		}
		out["store"] = false
		if limit != nil {
			out["max_output_tokens"] = limit
		}
		if body["reasoning_effort"] != nil {
			out["reasoning"] = object{"effort": body["reasoning_effort"]}
		}
		copyFields(out, body, "user")
		if body["response_format"] != nil {
			format := obj(body["response_format"])
			switch str(format["type"]) {
			case "json_schema":
				schema := obj(format["json_schema"])
				if schema == nil {
					return nil, errors.New("response_format 缺少 json_schema")
				}
				format = object{"type": "json_schema"}
				copyFields(format, schema, "name", "description", "schema", "strict")
			case "text", "json_object":
			default:
				return nil, errors.New("不支持 response_format 类型")
			}
			out["text"] = object{"format": format}
		}
	}
	for _, raw := range list(body["messages"]) {
		message := obj(raw)
		role := str(message["role"])
		if protocol == "responses" && role == "tool" {
			text, ok := message["content"].(string)
			if !ok {
				return nil, errors.New("Responses 的工具结果仅支持字符串")
			}
			messages = append(messages, object{"type": "function_call_output", "call_id": message["tool_call_id"], "output": text})
			continue
		}
		contentRole := role
		if role == "tool" {
			contentRole = "user"
		}
		blocks, err := contentParts(message["content"], protocol, contentRole)
		if err != nil {
			return nil, err
		}
		if protocol == "messages" && (role == "system" || role == "developer") {
			if len(messages) > 0 {
				return nil, errors.New("Messages 不支持对话中途插入系统消息")
			}
			system = append(system, blocks...)
			continue
		}
		if protocol == "messages" && role == "tool" {
			blocks = []any{object{"type": "tool_result", "tool_use_id": message["tool_call_id"], "content": blocks}}
			role = "user"
		}
		if protocol == "responses" && len(blocks) > 0 {
			messages = append(messages, object{"role": role, "content": blocks})
		}
		for _, rawCall := range list(message["tool_calls"]) {
			call := obj(rawCall)
			fn := obj(call["function"])
			if protocol == "messages" {
				var args object
				if json.Unmarshal([]byte(str(fn["arguments"])), &args) != nil || args == nil {
					return nil, errors.New("工具 arguments 必须是 JSON 对象字符串")
				}
				blocks = append(blocks, object{"type": "tool_use", "id": call["id"], "name": fn["name"], "input": args})
			} else {
				messages = append(messages, object{"type": "function_call", "call_id": call["id"], "name": fn["name"], "arguments": fn["arguments"]})
			}
		}
		if protocol == "messages" {
			if len(blocks) == 0 {
				return nil, errors.New("Messages 不接受空消息")
			}
			if len(messages) > 0 && obj(messages[len(messages)-1])["role"] == role {
				last := obj(messages[len(messages)-1])
				last["content"] = append(list(last["content"]), blocks...)
			} else {
				messages = append(messages, object{"role": role, "content": blocks})
			}
		}
	}
	if protocol == "messages" {
		out["messages"] = messages
		if len(system) > 0 {
			out["system"] = system
		}
	} else {
		out["input"] = messages
	}
	if body["tools"] != nil {
		tools, ok := body["tools"].([]any)
		if !ok {
			return nil, errors.New("tools 必须为数组")
		}
		converted := make([]any, 0, len(tools))
		for _, raw := range tools {
			tool := obj(raw)
			fn := obj(tool["function"])
			if tool["type"] != "function" || str(fn["name"]) == "" {
				return nil, errors.New("跨协议仅支持具名 function 工具")
			}
			if protocol == "messages" {
				if boolean(fn["strict"]) {
					return nil, errors.New("Messages 暂不支持 strict 工具")
				}
				schema := fn["parameters"]
				if schema == nil {
					schema = object{"type": "object", "properties": object{}}
				}
				converted = append(converted, object{"name": fn["name"], "description": str(fn["description"]), "input_schema": schema})
			} else {
				t := object{"type": "function", "strict": false}
				copyFields(t, fn, "name", "description", "parameters", "strict")
				converted = append(converted, t)
			}
		}
		out["tools"] = converted
	}
	if choice := body["tool_choice"]; choice != nil {
		if named := obj(choice); named != nil {
			name := str(obj(named["function"])["name"])
			if named["type"] != "function" || name == "" {
				return nil, errors.New("无效 tool_choice")
			}
			kind := "function"
			if protocol == "messages" {
				kind = "tool"
			}
			out["tool_choice"] = object{"type": kind, "name": name}
		} else {
			name := str(choice)
			if name != "auto" && name != "none" && name != "required" {
				return nil, errors.New("无效 tool_choice")
			}
			if protocol == "messages" {
				if name == "none" {
					delete(out, "tools")
				} else {
					if name == "required" {
						name = "any"
					}
					out["tool_choice"] = object{"type": name}
				}
			} else {
				out["tool_choice"] = name
			}
		}
	}
	if body["parallel_tool_calls"] != nil {
		if protocol == "responses" {
			out["parallel_tool_calls"] = body["parallel_tool_calls"]
		} else if len(list(out["tools"])) > 0 {
			if out["tool_choice"] == nil {
				out["tool_choice"] = object{"type": "auto"}
			}
			obj(out["tool_choice"])["disable_parallel_tool_use"] = !boolean(body["parallel_tool_calls"])
		}
	}
	return out, nil
}

func convertUsage(u object, protocol string) object {
	input, output := number(u["input_tokens"]), number(u["output_tokens"])
	cached, written := number(obj(u["input_tokens_details"])["cached_tokens"]), float64(0)
	if protocol == "messages" {
		cached, written = number(u["cache_read_input_tokens"]), number(u["cache_creation_input_tokens"])
		input += cached + written
	}
	result := object{"prompt_tokens": input, "completion_tokens": output, "total_tokens": input + output}
	if cached > 0 || written > 0 {
		result["prompt_tokens_details"] = object{"cached_tokens": cached, "cache_write_tokens": written}
	}
	if protocol == "responses" && u["output_tokens_details"] != nil {
		result["completion_tokens_details"] = u["output_tokens_details"]
	}
	return result
}
func finishReason(reason string) string {
	switch reason {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	case "refusal":
		return "content_filter"
	default:
		return "stop"
	}
}
func responseReason(data object, hasTools bool) string {
	if data["status"] == "incomplete" {
		if obj(data["incomplete_details"])["reason"] == "max_output_tokens" {
			return "length"
		}
		return "content_filter"
	}
	if hasTools {
		return "tool_calls"
	}
	return "stop"
}
func envelope(model string, stream bool) object {
	kind := "chat.completion"
	if stream {
		kind = "chat.completion.chunk"
	}
	return object{"id": "chatcmpl-" + newID(), "object": kind, "created": time.Now().Unix(), "model": model}
}
func convertResponse(data object, protocol, model string) (object, error) {
	if data["error"] != nil || data["status"] == "failed" {
		return nil, errors.New("上游生成失败")
	}
	if protocol == "chat" {
		if len(list(data["choices"])) == 0 {
			return nil, errors.New("缺少 choices")
		}
		return data, nil
	}
	field := "output"
	if protocol == "messages" {
		field = "content"
	}
	blocks, ok := data[field].([]any)
	if !ok {
		return nil, errors.New("上游回复结构无效")
	}
	text, reasoning, refusal := "", "", ""
	calls := make([]any, 0)
	for _, raw := range blocks {
		block := obj(raw)
		switch str(block["type"]) {
		case "text":
			text += str(block["text"])
		case "thinking":
			reasoning += str(block["thinking"])
		case "tool_use":
			calls = append(calls, object{"id": block["id"], "type": "function", "function": object{"name": block["name"], "arguments": string(encode(block["input"]))}})
		case "function_call":
			calls = append(calls, object{"id": block["call_id"], "type": "function", "function": object{"name": block["name"], "arguments": block["arguments"]}})
		case "message":
			for _, p := range list(block["content"]) {
				part := obj(p)
				if part["type"] == "output_text" {
					text += str(part["text"])
				}
				if part["type"] == "refusal" {
					refusal += str(part["refusal"])
				}
			}
		case "reasoning":
			for _, p := range list(block["summary"]) {
				reasoning += str(obj(p)["text"])
			}
		}
	}
	message := object{"role": "assistant", "content": text}
	if len(calls) > 0 {
		message["tool_calls"] = calls
		if text == "" {
			message["content"] = nil
		}
	}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	if refusal != "" {
		message["refusal"] = refusal
	}
	reason := responseReason(data, len(calls) > 0)
	if protocol == "messages" {
		reason = finishReason(str(data["stop_reason"]))
	}
	result := envelope(model, false)
	if str(data["id"]) != "" {
		result["id"] = data["id"]
	}
	result["choices"] = []any{object{"index": 0, "message": message, "finish_reason": reason}}
	result["usage"] = convertUsage(obj(data["usage"]), protocol)
	return result, nil
}
