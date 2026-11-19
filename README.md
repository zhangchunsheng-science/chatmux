# chatmux

**一个二进制，多个聊天平台，一个大脑。**

chatmux 是一个 Go 编写的多平台 IM ↔ AI 模型网关：把飞书、钉钉、企业微信、Telegram、Slack、Discord 等聊天平台桥接到大模型（OpenAI 兼容接口 / Anthropic）或无头编码 CLI（claude / codex），支持流式回复、按话题的会话隔离、持久化去重、定时任务、用量记账和黑白名单。

设计参考并致敬两个项目：

- **[botmux](https://github.com/deepcoldy/botmux)**（飞书 ↔ AI 编码 CLI 网关）——借鉴其「每会话串行处理、follow-up 排队、消息持久去重、流式卡片更新、最终回复单独成消息」的运转模型；
- **[openclaw](https://github.com/openclaw/openclaw)**（多平台个人 AI 助手）——借鉴其「通道即插件、统一入站归一化、按能力分级流式 UX、入站准入策略（allowlist / mention 门控）」的通道层设计。

与两者的差别：chatmux 用 **Go 单二进制** 实现，核心零魔法——标准库优先，飞书/钉钉仅用官方 WebSocket SDK，其余协议（企业微信回调加解密、Telegram 长轮询、Discord Gateway、Slack Socket Mode）全部手写。

## 架构

```
飞书(WS/长连接)  ─┐                                  ┌─> OpenAI 兼容 API
钉钉(Stream)     ─┤   Channel ─> Bus ─> Agent       ├─> Anthropic API
企业微信(回调)    ─┼─  适配器        归一化   Runtime ─┤      (SSE 流式)
Telegram(轮询)   ─┤   (去重/解析)  消息总线 (会话/排队) ├─> claude -p --output-format stream-json
Slack(SocketMode)─┤                                  │    codex exec --json
Discord(Gateway) ─┘                                  │    (无头 CLI 代理，自带会话续跑)
                                        cron 定时 ────┘    generic: 任意行协议二进制
```

- **`internal/channel`** — `Channel`/`Sender`/`AttachmentDownloader` 接口 + 六个平台适配器
- **`internal/provider`** — OpenAI 兼容与 Anthropic 两个流式模型提供方
- **`internal/agent`** — 运行时：路由、每 (channel, chat, thread) 一个会话、
  同会话串行 turn、运行中收到的消息排队并提示、流式占位消息节流更新、
  完成后一次性落史并发最终回复；slash 命令与群 @ 门控也在这层
- **`internal/cliagent`** — 无头 CLI 代理运行器：每 turn 起一个进程，claude
  走 `-p --output-format stream-json --resume`，codex 走 `exec --json`，
  按 chatmux 锚点记 resume id，跨轮续会话；工具调用作为进度行流回聊天
- **`internal/cron`** — 5 字段 cron 调度器（分 时 日 月 周），到点向指定
  会话发合成消息（如「每天 9:30 站会总结」）
- **`internal/usage`** — 每 turn 一行 JSONL 用量记账（token/字符/时长），落
  `dataDir/usage.jsonl`
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

## 两种 agent 形态

**API agent**（默认）：直接调模型接口，chatmux 自己管历史。

```json
{ "name": "helper", "provider": "openai", "model": "gpt-4o-mini", "routes": [{"channel": "any"}] }
```

**CLI agent**：像 botmux 一样驱动无头编码 CLI，CLI 自带会话记忆（chatmux 按
锚点记 resume id，跨轮续跑），工具调用实时流回群里：

```json
{
  "name": "dev",
  "cli": { "type": "claude", "model": "sonnet", "workingDir": "/srv/project" },
  "routes": [{ "channel": "feishu", "chatId": "oc_xxxx" }]
}
```

`type` 支持 `claude` / `codex` / `gemini` / `generic`（generic 需给 `bin`，
可选 `promptArg`/`resumeFlag`；gemini 走 `gemini -p`，无原生 resume）。
API 型后端（MiniMax、DeepSeek、Moonshot、vLLM…）用 openai provider 换
`baseUrl` 即接。

## 聊天内命令

任何会话直接发：

| 命令 | 作用 |
|---|---|
| `/bg <任务>` | 丢后台跑——会话立即可用，完成后 @你 发回结果 |
| `/jobs` | 列出后台任务 |
| `/stop [bg\|任务id]` | 打断前台 / 后台任务 |
| `/new` | 全新会话（清历史 + 清 CLI resume） |
| `/status` | 当前会话状态（模式/模型/历史/后台任务数） |
| `/model [名称]` | 查看/切换本会话模型 |
| `/system [提示词]` | 查看/覆盖 system 提示词 |
| `/skills` · `/skill <名> <指令>` | 列出 / 使用技能 |
| `/flows` · `/flow <名> <输入>` | 列出 / 后台跑多步工作流 |

群聊默认需要 @机器人 才响应（agent 可配 `"groupMention": false` 关闭）。

## 引用上下文

回复别人的消息时，被引内容自动注入模型上下文（`[引用 老王 的消息]…`）。
飞书（回复消息）、Telegram、Discord、Slack（线程跟帖）均已接；飞书话题群
内跟帖不算引用，走话题锚点会话。

## 语音转写

```json
"transcribe": { "enabled": true, "model": "whisper-1", "baseUrl": "https://api.openai.com/v1" }
```

开启后语音消息自动转文字（OpenAI 兼容 audio/transcriptions，apiKey 缺省用
OPENAI_API_KEY），以 `[语音]` 段落并入提示词。Telegram voice / 飞书音频 /
Telegram 音频文件均支持。

## 技能与工作流

**技能**：`skillsDir` 目录下每个 `*.md` 是一个技能（文件名即技能名）。
`/skill review 请看这段代码` 把技能全文 + 你的指令一起发给 agent。

**工作流**：chatmux.json 定义多步链，`/flow` 后台执行，`{{prev}}` 代上一步
输出、`{{input}}` 代用户输入，每步可指定不同 agent：

```json
"workflows": [{
  "name": "translate-polish",
  "desc": "翻译再润色",
  "steps": [
    { "agent": "translator", "prompt": "把 {{input}} 翻译成英文" },
    { "agent": "writer", "prompt": "润色这段译文：{{prev}}" }
  ]
}]
```

## 定时任务

```json
"cron": [{ "name": "standup", "schedule": "30 9 * * 1-5", "channel": "feishu", "chatId": "oc_xxxx", "text": "总结昨天进展" }]
```

## 网页看板

浏览器打开 `http://127.0.0.1:8080/`：通道健康、运行时与会话指标、后台任务
列表、按 agent 的用量与 👍/👎 反馈统计，5 秒自动刷新（adminToken 输入一次
存本地）。

## 反馈按钮（飞书）

飞书最终回复以卡片发出，带 👍/👎 按钮；点按即时 toast 回执，结果计入
usage.jsonl 与看板统计。

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
- 群聊默认 @ 门控；CLI agent 的 `workingDir` 决定它能碰哪些文件
- 飞书 webhook 模式实现 sha256(timestamp+nonce+encryptKey+body) 验签 + ±1h 时间窗
- 企业微信回调按官方 AES-256-CBC + SHA1 签名规范，任何不匹配直接 403
- 会话去重存储与状态文件落在 `dataDir`（默认 `data/`），权限 0600
- 图片下载：飞书按 image_key 走 im 资源接口、Telegram 走 getFile，落到
  `dataDir/attachments/` 后以本地文件喂给视觉模型

## 状态

v0.3：v0.2 全量 + 后台长任务（/bg /jobs /stop 扩展）+ 引用上下文（飞书/TG/
Discord/Slack）+ 飞书 👍/👎 反馈卡片 + 语音转写（whisper 兼容）+ 网页看板 +
技能（skillsDir）+ 工作流（{{prev}}/{{input}} 链）+ gemini CLI 预设。
未做（欢迎 PR）：多平台按钮（目前仅飞书）、卡片表单、指标导出。

## License

MIT
