package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// SSE 以空行分帧，支持 CRLF、多行 data 和末尾无空行；设置上限避免无限缓冲。
func readEvents(reader io.Reader, consume func(string) (bool, error)) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	var data []string
	size := 0
	dispatch := func() (bool, error) {
		if len(data) == 0 {
			return false, nil
		}
		raw := strings.Join(data, "\n")
		data = nil
		size = 0
		return consume(raw)
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			done, err := dispatch()
			if err != nil {
				return err
			}
			if done {
				return nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line[5:], " ")
			size += len(value) + 1
			if size > 4<<20 {
				return errors.New("SSE 事件过大")
			}
			data = append(data, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	done, err := dispatch()
	if err != nil {
		return err
	}
	if done {
		return nil
	}
	return io.ErrUnexpectedEOF
}

type toolState struct {
	index     int
	arguments string
}

func streamResponse(reader io.Reader, protocol, model string, includeUsage bool, emit func(any) error) error {
	base := envelope(model, true)
	tokens := object{}
	tools := map[int]*toolState{}
	reason := "stop"
	chunk := func(delta object, finish any) error {
		value := object{}
		copyFields(value, base, "id", "object", "created", "model")
		value["choices"] = []any{object{"index": 0, "delta": delta, "finish_reason": finish}}
		return emit(value)
	}
	toolDelta := func(index int, delta object) error {
		delta["index"] = index
		return chunk(object{"tool_calls": []any{delta}}, nil)
	}
	if protocol != "chat" {
		if err := chunk(object{"role": "assistant", "content": ""}, nil); err != nil {
			return err
		}
	}
	err := readEvents(reader, func(raw string) (bool, error) {
		if raw == "[DONE]" {
			if protocol == "chat" {
				return true, nil
			}
			return false, io.ErrUnexpectedEOF
		}
		var event object
		if json.Unmarshal([]byte(raw), &event) != nil || event == nil {
			return false, errors.New("SSE JSON 无效")
		}
		kind := str(event["type"])
		if event["error"] != nil || kind == "error" || kind == "response.failed" {
			return false, errors.New("上游流式生成失败")
		}
		if protocol == "chat" {
			return false, emit(event)
		}
		index := int(number(event["index"]))
		if protocol == "responses" {
			index = int(number(event["output_index"]))
		}
		if protocol == "messages" {
			switch kind {
			case "message_start":
				for k, v := range obj(obj(event["message"])["usage"]) {
					tokens[k] = v
				}
			case "content_block_start":
				block := obj(event["content_block"])
				switch str(block["type"]) {
				case "tool_use":
					if tools[index] != nil {
						return false, errors.New("重复的工具索引")
					}
					t := &toolState{index: len(tools)}
					tools[index] = t
					if len(obj(block["input"])) > 0 {
						t.arguments = string(encode(block["input"]))
					}
					return false, toolDelta(t.index, object{"id": block["id"], "type": "function", "function": object{"name": block["name"], "arguments": t.arguments}})
				case "text":
					if str(block["text"]) != "" {
						return false, chunk(object{"content": block["text"]}, nil)
					}
				case "thinking":
					if str(block["thinking"]) != "" {
						return false, chunk(object{"reasoning_content": block["thinking"]}, nil)
					}
				}
			case "content_block_delta":
				delta := obj(event["delta"])
				switch str(delta["type"]) {
				case "text_delta":
					return false, chunk(object{"content": delta["text"]}, nil)
				case "thinking_delta":
					return false, chunk(object{"reasoning_content": delta["thinking"]}, nil)
				case "input_json_delta":
					t := tools[index]
					if t == nil {
						return false, errors.New("工具参数先于工具声明")
					}
					t.arguments += str(delta["partial_json"])
					return false, toolDelta(t.index, object{"function": object{"arguments": delta["partial_json"]}})
				}
			case "content_block_stop":
				if t := tools[index]; t != nil && t.arguments == "" {
					t.arguments = "{}"
					return false, toolDelta(t.index, object{"function": object{"arguments": "{}"}})
				}
			case "message_delta":
				for k, v := range obj(event["usage"]) {
					tokens[k] = v
				}
				if stop := str(obj(event["delta"])["stop_reason"]); stop != "" {
					reason = finishReason(stop)
				}
			case "message_stop":
				return true, nil
			}
		} else {
			switch kind {
			case "response.output_text.delta":
				return false, chunk(object{"content": event["delta"]}, nil)
			case "response.refusal.delta":
				return false, chunk(object{"refusal": event["delta"]}, nil)
			case "response.reasoning_summary_text.delta":
				return false, chunk(object{"reasoning_content": event["delta"]}, nil)
			case "response.output_item.added":
				item := obj(event["item"])
				if item["type"] == "function_call" {
					if tools[index] != nil {
						return false, errors.New("重复的工具索引")
					}
					t := &toolState{len(tools), str(item["arguments"])}
					tools[index] = t
					return false, toolDelta(t.index, object{"id": item["call_id"], "type": "function", "function": object{"name": item["name"], "arguments": t.arguments}})
				}
			case "response.function_call_arguments.delta":
				t := tools[index]
				if t == nil {
					return false, errors.New("工具参数先于工具声明")
				}
				t.arguments += str(event["delta"])
				return false, toolDelta(t.index, object{"function": object{"arguments": event["delta"]}})
			case "response.function_call_arguments.done":
				t := tools[index]
				if t == nil {
					return false, errors.New("工具参数先于工具声明")
				}
				if t.arguments == "" {
					t.arguments = str(event["arguments"])
					return false, toolDelta(t.index, object{"function": object{"arguments": t.arguments}})
				}
			case "response.completed", "response.incomplete":
				response := obj(event["response"])
				if response == nil {
					return false, errors.New("缺少最终 response")
				}
				if kind == "response.incomplete" {
					response["status"] = "incomplete"
				}
				tokens = obj(response["usage"])
				reason = responseReason(response, len(tools) > 0)
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return err
	}
	if protocol != "chat" {
		if err := chunk(object{}, reason); err != nil {
			return err
		}
		if includeUsage {
			value := object{}
			copyFields(value, base, "id", "object", "created", "model")
			value["choices"] = []any{}
			value["usage"] = convertUsage(tokens, protocol)
			if err := emit(value); err != nil {
				return err
			}
		}
	}
	return emit("[DONE]")
}
