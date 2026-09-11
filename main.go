package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type object = map[string]any

func obj(v any) object     { m, _ := v.(map[string]any); return m }
func list(v any) []any     { a, _ := v.([]any); return a }
func str(v any) string     { s, _ := v.(string); return s }
func boolean(v any) bool   { b, _ := v.(bool); return b }
func number(v any) float64 { n, _ := v.(float64); return n }
func encode(v any) []byte  { b, _ := json.Marshal(v); return b }
func copyFields(to, from object, keys ...string) {
	for _, k := range keys {
		if from[k] != nil {
			to[k] = from[k]
		}
	}
}

type Config struct {
	Server struct {
		Host   string `toml:"host"`
		Port   int    `toml:"port"`
		APIKey string `toml:"api_key"`
	} `toml:"server"`
	Upstream struct {
		BaseURL        string            `toml:"base_url"`
		APIKey         string            `toml:"api_key"`
		UserAgent      string            `toml:"user_agent"`
		Timeout        int               `toml:"timeout"`
		MaxTokens      int               `toml:"max_tokens"`
		ModelProtocols map[string]string `toml:"model_protocols"`
	} `toml:"upstream"`
}

func printable(s string) bool {
	for _, c := range s {
		if c < 32 || c > 126 {
			return false
		}
	}
	return s != ""
}

func loadConfig(path string) (Config, error) {
	var c Config
	c.Server.Host, c.Server.Port = "127.0.0.1", 8000
	c.Upstream.Timeout, c.Upstream.MaxTokens = 120, 4096
	c.Upstream.UserAgent = "opencode-openai-adapter/1.0"
	// 不打印 TOML 原文，防止格式错误时将密钥写入日志。
	meta, err := toml.DecodeFile(path, &c)
	if err != nil {
		return c, fmt.Errorf("无法读取配置，请检查文件路径和 TOML 格式")
	}
	if len(meta.Undecoded()) > 0 {
		return c, errors.New("配置包含未知字段，请对照 config.example.toml")
	}
	u, err := url.Parse(c.Upstream.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return c, errors.New("upstream.base_url 必须是完整 HTTP(S) 地址，不带用户名、查询参数或片段")
	}
	if u.Scheme == "http" && u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1" {
		return c, errors.New("远程上游地址必须使用 HTTPS")
	}
	c.Upstream.BaseURL = strings.TrimRight(c.Upstream.BaseURL, "/")
	if !printable(c.Upstream.APIKey) || c.Upstream.APIKey == "YOUR_OPENCODE_API_KEY" || !printable(c.Server.APIKey) || !printable(c.Upstream.UserAgent) {
		return c, errors.New("请填写有效的 server.api_key、upstream.api_key 和 user_agent（可打印 ASCII）")
	}
	if c.Server.Port < 1 || c.Server.Port > 65535 || c.Upstream.Timeout < 1 || c.Upstream.Timeout > 86400 || c.Upstream.MaxTokens < 1 {
		return c, errors.New("port 应为 1–65535，timeout 为 1–86400 秒，max_tokens 必须为正数")
	}
	for _, p := range c.Upstream.ModelProtocols {
		if p != "chat" && p != "messages" && p != "responses" {
			return c, errors.New("model_protocols 仅支持 chat、messages、responses")
		}
	}
	return c, nil
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("无法生成随机会话 ID")
	}
	return hex.EncodeToString(b)
}

func sessionID(r *http.Request, body object, secret string) (string, error) {
	value := ""
	for _, key := range []string{"x-opencode-session", "x-session-id", "session_id", "x-conversation-id"} {
		if value == "" {
			value = r.Header.Get(key)
		}
	}
	for _, source := range []object{body, obj(body["metadata"])} {
		for _, key := range []string{"session_id", "conversation_id"} {
			if value == "" && source[key] != nil {
				var ok bool
				value, ok = source[key].(string)
				if !ok {
					return "", errors.New("会话 ID 必须是字符串")
				}
			}
		}
	}
	if value != "" {
		if len(value) > 256 || !printable(value) || strings.Contains(value, " ") {
			return "", errors.New("会话 ID 必须为 1–256 个非空白 ASCII 字符")
		}
		return value, nil
	}
	for _, raw := range list(body["messages"]) {
		if obj(raw)["role"] == "user" {
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write(encode([]any{body["user"], raw}))
			return hex.EncodeToString(mac.Sum(nil)[:16]), nil
		}
	}
	return newID(), nil
}

func errorBody(message, kind string) object {
	return object{"error": object{"message": message, "type": kind, "param": nil, "code": nil}}
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, message, kind string) {
	writeJSON(w, status, errorBody(message, kind))
}

func validate(body object) error {
	if str(body["model"]) == "" || len(list(body["messages"])) == 0 {
		return errors.New("model 和 messages 不能为空")
	}
	for _, raw := range list(body["messages"]) {
		m := obj(raw)
		switch str(m["role"]) {
		case "system", "developer", "user", "assistant", "tool":
		default:
			return errors.New("messages 包含无效角色")
		}
		if m["role"] == "tool" && str(m["tool_call_id"]) == "" {
			return errors.New("工具结果缺少 tool_call_id")
		}
		if m["tool_calls"] != nil {
			calls, ok := m["tool_calls"].([]any)
			if !ok || m["role"] != "assistant" {
				return errors.New("tool_calls 必须是 assistant 消息中的数组")
			}
			for _, call := range calls {
				t := obj(call)
				fn := obj(t["function"])
				if t["type"] != "function" || str(t["id"]) == "" || str(fn["name"]) == "" {
					return errors.New("无效的 function 工具调用")
				}
				if _, ok := fn["arguments"].(string); !ok {
					return errors.New("工具 arguments 必须是 JSON 字符串")
				}
			}
		}
	}
	for _, key := range []string{"stream", "parallel_tool_calls"} {
		if v, exists := body[key]; exists {
			if _, ok := v.(bool); !ok {
				return fmt.Errorf("%s 必须为布尔值", key)
			}
		}
	}
	for _, key := range []string{"metadata", "stream_options", "response_format"} {
		if v, exists := body[key]; exists && obj(v) == nil {
			return fmt.Errorf("%s 必须为对象", key)
		}
	}
	for _, key := range []string{"max_tokens", "max_completion_tokens", "n"} {
		if v, exists := body[key]; exists {
			n, ok := v.(float64)
			if !ok || n <= 0 || n > 1e9 || n != float64(int64(n)) {
				return fmt.Errorf("%s 必须为正整数", key)
			}
		}
	}
	return nil
}

type adapter struct {
	config Config
	client *http.Client
}

func newAdapter(c Config) *adapter {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = time.Duration(c.Upstream.Timeout) * time.Second
	return &adapter{c, &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (a *adapter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" && r.Method == http.MethodGet {
		writeJSON(w, 200, object{"status": "ok"})
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+a.config.Server.APIKey)) != 1 {
		writeError(w, 401, "本地 API key 无效", "authentication_error")
		return
	}
	models := r.URL.Path == "/v1/models" || r.URL.Path == "/models"
	chat := r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/chat/completions"
	if !models && !chat {
		writeError(w, 404, "接口不存在", "invalid_request_error")
		return
	}
	if (models && r.Method != http.MethodGet) || (chat && r.Method != http.MethodPost) {
		writeError(w, 405, "请求方法不支持", "invalid_request_error")
		return
	}
	var body object
	protocol, path, method, sid := "chat", "/models", http.MethodGet, newID()
	var payload []byte
	if chat {
		r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
		decoder := json.NewDecoder(r.Body)
		err := decoder.Decode(&body)
		if err == nil {
			var extra any
			if decoder.Decode(&extra) != io.EOF {
				err = errors.New("请求体必须仅包含一个 JSON 对象")
			}
		}
		if err != nil {
			writeError(w, 400, "无效 JSON 或请求超过 16 MiB", "invalid_request_error")
			return
		}
		if err = validate(body); err != nil {
			writeError(w, 400, err.Error(), "invalid_request_error")
			return
		}
		sid, err = sessionID(r, body, a.config.Server.APIKey)
		if err != nil {
			writeError(w, 400, err.Error(), "invalid_request_error")
			return
		}
		for _, prefix := range []string{"opencode-go/", "opencode/"} {
			body["model"] = strings.TrimPrefix(str(body["model"]), prefix)
		}
		for _, key := range []string{"session_id", "conversation_id", "metadata"} {
			delete(body, key)
		}
		protocol, err = selectProtocol(str(body["model"]), a.config)
		if err != nil {
			writeError(w, 400, err.Error(), "invalid_request_error")
			return
		}
		converted, err := convertRequest(body, protocol, a.config.Upstream.MaxTokens)
		if err != nil {
			writeError(w, 400, err.Error(), "invalid_request_error")
			return
		}
		payload, method = encode(converted), http.MethodPost
		path = map[string]string{"chat": "/chat/completions", "messages": "/messages", "responses": "/responses"}[protocol]
	}
	// 请求上下文绑定客户端断连，并限制上游总耗时；无需后台转发协程。
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(a.config.Upstream.Timeout)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, a.config.Upstream.BaseURL+path, bytes.NewReader(payload))
	if err != nil {
		writeError(w, 502, "上游地址无效", "upstream_error")
		return
	}
	req.Header.Set("Authorization", "Bearer "+a.config.Upstream.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", a.config.Upstream.UserAgent)
	req.Header.Set("x-opencode-session", sid)
	if protocol == "messages" {
		req.Header.Set("x-api-key", a.config.Upstream.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	res, err := a.client.Do(req)
	if err != nil {
		a.networkError(w, ctx)
		return
	}
	defer res.Body.Close()
	w.Header().Set("x-opencode-session", sid)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		message := string(raw)
		var data object
		if json.Unmarshal(raw, &data) == nil && str(obj(data["error"])["message"]) != "" {
			message = str(obj(data["error"])["message"])
		}
		for _, key := range []string{a.config.Upstream.APIKey, a.config.Server.APIKey} {
			message = strings.ReplaceAll(message, key, "[REDACTED]")
		}
		if len(message) > 2000 {
			message = message[:2000]
		}
		if message == "" {
			message = "上游拒绝请求"
		}
		if retry := res.Header.Get("Retry-After"); retry != "" {
			w.Header().Set("Retry-After", retry)
		}
		status := res.StatusCode
		if status < 400 {
			status = 502
		}
		writeError(w, status, message, "upstream_error")
		return
	}
	if chat && boolean(body["stream"]) {
		if !strings.Contains(res.Header.Get("Content-Type"), "text/event-stream") {
			writeError(w, 502, "上游未返回 SSE 流", "upstream_error")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		emit := func(v any) error {
			rc := http.NewResponseController(w)
			_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
			raw := encode(v)
			if v == "[DONE]" {
				raw = []byte("[DONE]")
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
				return err
			}
			return rc.Flush()
		}
		err := streamResponse(res.Body, protocol, str(body["model"]), boolean(obj(body["stream_options"])["include_usage"]), emit)
		if err != nil && r.Context().Err() == nil {
			_ = emit(errorBody("上游流式回复中断或格式无效", "upstream_error"))
		}
		return
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, (32<<20)+1))
	if err != nil {
		a.networkError(w, ctx)
		return
	}
	var data object
	if len(raw) > 32<<20 || json.Unmarshal(raw, &data) != nil || data == nil {
		writeError(w, 502, "上游返回无效 JSON 或响应超过 32 MiB", "upstream_error")
		return
	}
	if models {
		items, ok := data["data"].([]any)
		if !ok {
			writeError(w, 502, "模型列表格式无效", "upstream_error")
			return
		}
		filtered := make([]any, 0, len(items))
		for _, item := range items {
			if _, err := selectProtocol(str(obj(item)["id"]), a.config); err == nil {
				filtered = append(filtered, item)
			}
		}
		data["data"] = filtered
	} else {
		data, err = convertResponse(data, protocol, str(body["model"]))
		if err != nil {
			writeError(w, 502, "上游回复无效或生成失败", "upstream_error")
			return
		}
	}
	writeJSON(w, 200, data)
}

func (a *adapter) networkError(w http.ResponseWriter, ctx context.Context) {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		writeError(w, 504, "上游请求超时", "upstream_timeout")
	} else {
		writeError(w, 502, "无法读取上游回复", "upstream_error")
	}
}

func main() {
	configPath := flag.String("config", "", "config.toml 路径，默认读取当前目录或程序所在目录")
	flag.Parse()
	if *configPath == "" {
		*configPath = "config.toml"
		if _, err := os.Stat(*configPath); errors.Is(err, os.ErrNotExist) {
			if exe, err := os.Executable(); err == nil {
				*configPath = filepath.Join(filepath.Dir(exe), "config.toml")
			}
		}
	}
	c, err := loadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	a := newAdapter(c)
	server := &http.Server{Handler: a, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	listener, err := net.Listen("tcp", net.JoinHostPort(c.Server.Host, fmt.Sprint(c.Server.Port)))
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("OpenCode → OpenAI 已启动: http://%s/v1", listener.Addr())
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
