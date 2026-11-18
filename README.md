# chatmux

**一个二进制，多个聊天平台，一个大脑。**

chatmux 是一个 Go 编写的多平台 IM ↔ AI 模型网关：把飞书、钉钉、企业微信、Telegram、Slack、Discord 等聊天平台桥接到大模型（OpenAI 兼容接口 / Anthropic），支持流式回复、按话题的会话隔离、持久化去重和黑白名单。

设计参考并致敬两个项目：

- **[botmux](https://github.com/deepcoldy/botmux)**（飞书 ↔ AI 编码 CLI 网关）——借鉴其「每会话串行处理、follow-up 排队、消息持久去重、流式卡片更新、最终回复单独成消息」的运转模型；
- **[openclaw](https://github.com/openclaw/openclaw)**（多平台个人 AI 助手）——借鉴其「通道即插件、统一入站归一化、按能力分级流式 UX、入站准入策略（allowlist / mention 门控）」的通道层设计。

与两者的差别：chatmux 用 **Go 单二进制** 实现，核心零魔法——标准库优先，飞书/钉钉仅用官方 WebSocket SDK，其余协议（企业微信回调加解密、Telegram 长轮询、Discord Gateway、Slack Socket Mode）全部手写。

## 架构

```
飞书(WS/长连接)  ─┐                                  ┌─> OpenAI 兼容 API
钉钉(Stream)     ─┤   Channel ─> Bus ─> Agent       ├─> Anthropic API
企业微信(回调)    ─┼─  适配器        归一化   Runtime ─┤      (SSE 流式)
Telegram(轮询)   ─┤   (去重/解析)  消息总线 (会话/排队) └─> 任意 baseUrl 网关
Slack(SocketMode)─┤                                  (DeepSeek/Moonshot/vLLM/one-api…)
Discord(Gateway) ─┘
```

- **`internal/channel`** — `Channel`/`Sender` 接口 + 六个平台适配器
- **`internal/provider`** — OpenAI 兼容与 Anthropic 两个流式模型提供方
- **`internal/agent`** — 运行时：路由、每 (channel, chat, thread) 一个会话、
  同会话串行 turn、运行中收到的消息排队并提示、流式占位消息节流更新、
  完成后一次性落史并发最终回复
- **`internal/dedup`** — 文件持久化的 at-least-once 去重（TTL + 容量上限），
  借鉴 botmux 的 message_id 去重（飞书 WS 会按 15s/5min/1h/6h 梯度重推）
- **`internal/session`** — 会话历史（含话题/线程锚点，借鉴 openclaw 的
  `<chat>[:topic][:sender]` 会话 ID 语法）

## 快速开始

```bash
go build -o chatmux .
cp chatmux.json.example chatmux.json   # 填入你的通道凭据和模型 key
./chatmux -config chatmux.json
```

模型 key 也可以走环境变量：`OPENAI_API_KEY` / `ANTHROPIC_API_KEY`（配置文件里的 `apiKey`/`baseUrl` 优先，可按 agent 覆盖——任何 OpenAI 兼容网关都能接）。

## 各平台接入要点

| 平台 | 传输 | 前置条件 | 流式编辑 |
|---|---|---|---|
| 飞书 | WebSocket 长连接（默认）或 webhook | 企业自建应用 + 机器人能力 + `im.message.receive_v1` 订阅 + 长连接模式 | ✅ 卡片 PATCH |
| 钉钉 | Stream Mode（官方 SDK） | 企业内部机器人 + Stream 模式开启 | ❌（sessionWebhook 仅整条发送） |
| 企业微信 | HTTP 回调（需公网可达 URL） | 自建应用 + 接收消息回调配置；AES 加解密按官方规范实现 | ❌ |
| Telegram | 长轮询（无需公网） | @BotFather token | ✅ editMessageText |
| Slack | Socket Mode（无需公网） | xoxb + xapp token，订阅 message/app_mention 事件 | ✅ chat.update |
| Discord | Gateway WS（无需公网） | Bot token + Message Content Intent | ✅ PATCH message |

## 安全

- 每个 `channel.options` 支持 `allowUsers` / `allowChats`（空 = 不限制，生产务必配置）
- 飞书 webhook 模式实现 sha256(timestamp+nonce+encryptKey+body) 验签 + ±1h 时间窗
- 企业微信回调按官方 AES-256-CBC + SHA1 签名规范，任何不匹配直接 403
- 会话去重存储与状态文件落在 `dataDir`（默认 `data/`），权限 0600

## 状态

v0.1 骨架完成：六个通道适配器 + 双 provider + 会话/去重/排队运行时 + 单测。
未做（欢迎 PR）：CLI 子进程代理（像 botmux 驱动 claude/codex）、卡片交互按钮、
图片下载管线（当前只透传 URL/file_key）、多账号、指标面板。

## License

MIT
