# OpenCode → OpenAI

用 Go 编写的本地转换器，让支持 OpenAI Chat Completions 的客户端调用 OpenCode Go / Zen。支持普通回复、实时 SSE、工具调用、用量统计和会话标识适配。

编译后是独立程序，无需安装 Python、Go 或其他运行环境。仅使用一个构建依赖 `BurntSushi/toml` 解析配置，其余功能使用 Go 标准库。

## 运行 EXE

1. 解压 Windows 发布包。
2. 将 `config.example.toml` 复制为 `config.toml`，填写上游地址和 API key。
3. 双击 `opencode-openai.exe`，或在终端执行：

```powershell
.\opencode-openai.exe -config config.toml
```

不指定 `-config` 时，优先读取当前工作目录的 `config.toml`，不存在时读取 EXE 所在目录的文件。修改配置后重启程序。终端按 `Ctrl+C` 停止。

客户端填写：

| 配置项 | 默认值 |
| --- | --- |
| 接口类型 | OpenAI / OpenAI Compatible（Chat Completions） |
| Base URL | `http://127.0.0.1:8000/v1` |
| API key | `local-opencode`，对应 `server.api_key` |
| 模型 | 例如 `minimax-m2.5`、`glm-5.3-flash`、`gpt-5.6-luna` |

需要完整接口地址的软件填写 `http://127.0.0.1:8000/v1/chat/completions`。上游密钥仅填写在配置文件中，不需要提供给客户端。

## 配置

```toml
[server]
host = "127.0.0.1"
port = 8000
api_key = "local-opencode"

[upstream]
base_url = "https://opencode.ai/zen/go/v1"
api_key = "YOUR_OPENCODE_API_KEY"
user_agent = "opencode-openai-adapter/1.0"
timeout = 120
max_tokens = 4096

[upstream.model_protocols]
# 精确模型名覆盖，仅在上游需要时启用：
# "minimax-m2.5" = "chat"
```

- Go 订阅地址：`https://opencode.ai/zen/go/v1`；Zen 地址：`https://opencode.ai/zen/v1`。不追加具体接口路径。
- `timeout`：一次上游请求的总超时秒数，包括流式生成；长回复可适当调大，最大 86400 秒。
- `max_tokens`：Messages 协议在客户端未指定输出上限时的默认值。
- 自动路由：Claude/Qwen → Messages；GPT/Grok/Muse → Responses；Go 的 MiniMax → Messages；其余 → Chat Completions。可通过 `model_protocols` 覆盖为 `chat`、`messages`、`responses`。
- `config.toml` 已被 Git 忽略，仓库仅保留无密钥模板。EXE 不内置任何 API key。

## 从源码编译

需要 Go 1.22+。Windows 本机执行：

```powershell
go build -buildvcs=false -trimpath -ldflags="-s -w" -o opencode-openai.exe .
```

Linux / macOS 编译 Windows 64 位版本：

```sh
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -buildvcs=false -trimpath -ldflags="-s -w" -o opencode-openai.exe .
```

本机开发运行：`go run . -config config.toml`。

发布时将 EXE、`config.example.toml`、`README.md`、`THIRD_PARTY_NOTICES` 打包上传 GitHub Release 即可；不要放入自己的 `config.toml`。

项目主要文件：

- `main.go`：配置、HTTP 服务、认证、会话、上游请求。
- `protocol.go`：请求与普通回复转换。
- `stream.go`：SSE 流式转换。
- `go.mod` / `go.sum`：固定依赖。

测试、临时文件和发布产物均被 `.gitignore` 排除，不包含在源码包中。

## 接口与会话

- `GET /v1/models`：读取当前上游模型列表。
- `POST /v1/chat/completions`：普通或流式生成。
- `GET /health`：本地服务健康检查，不检查上游；此接口免认证。
- 其他接口使用 `Authorization: Bearer <server.api_key>`。

优先使用 `x-opencode-session` 请求头标识会话，也兼容 `x-session-id`、`session_id`、`x-conversation-id` 请求头，以及请求体或 `metadata` 中的 `session_id` / `conversation_id`。响应头返回实际使用的 ID。`metadata` 在此转换器中仅用于会话标识。

建议每个对话生成一个唯一 ID 并在后续请求中复用。旧客户端不传 ID 时，根据首条用户消息和可选 `user` 字段生成稳定摘要；相同开场的独立对话可能共用 ID，历史裁剪掉首条消息后 ID 会变化。

## 兼容范围

- 支持文本、系统消息、历史对话、function 工具调用及结果回传。Messages/Responses 支持用户图片格式转换，实际视觉能力取决于模型。
- 跨协议转换仅支持 `n=1`。Messages 暂不支持 JSON `response_format`、`reasoning_effort`、strict 工具、对话中途插入系统消息；Responses 不支持 `stop`，工具结果只接受字符串。不能准确转换的顶层参数返回 400。
- 推理文字通过 `reasoning_content` 扩展返回；Chat 上游保留自身扩展。不支持推理签名/加密 reasoning 的无损往返。
- 不实现 Gemini 原生协议，以及 OpenAI 的 `/v1/responses`、Embedding、语音、文件、图片生成等接口。
- SSE 立即转发；客户端断开会取消上游请求。中途失败返回 error 事件，不伪造成功结束标志。请求体最大 16 MiB、普通响应最大 32 MiB、单个 SSE 事件最大 4 MiB。
- 如实发送自身 User-Agent，不冒充官方客户端；不改变 Go 的用途、额度或模型权限，不自动切换套餐或重试计费请求。

协议依据：[OpenCode Go](https://opencode.ai/docs/go)、[OpenCode Zen](https://opencode.ai/docs/zen)。第三方许可见 `THIRD_PARTY_NOTICES`。
