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

`type` 支持 `claude` / `codex` / `gemini` / `qwen` / `opencode` / `aider` /
`generic`（generic 需给 `bin`，可选 `promptArg`/`resumeFlag`；gemini/qwen 走
`-p`、opencode 走 `run`、aider 走 `--message`，均为 argv 提示词 + 纯文本
输出，无原生 resume；claude/codex 走流式 JSON 并自动 resume）。
API 型后端（MiniMax、DeepSeek、Moonshot、vLLM…）用 openai provider 换
`baseUrl` 即接。

## 聊天内命令

任何会话直接发：

| 命令 | 作用 |
|---|---|
| `/help` | 列出全部命令（群内发现入口） |
| `/bg <任务>` | 丢后台跑——会话立即可用，完成后 @你 发回结果 |
| `/jobs` | 列出后台任务 |
| `/stop [bg\|任务id]` | 打断前台 / 后台任务 |
| `/new` | 全新会话（清历史 + 清 CLI resume） |
| `/name <名>` | 给会话命名（/status 与看板显示） |
| `/history [轮数]` | 查看本会话最近对话 |
| `/rewind [轮数]` | 回退上下文（模型忘记最近 N 轮，聊天记录不动） |
| `/retry` | 重试上一轮（回退后重发） |
| `/remind <时间> <事由>` | 一次性提醒：+30m / +1h30m / 15:30 / 2026-11-20 15:30 |
| `/schedule add <时间> <事由>` | 周期任务：每天 09:30 / 工作日 9:00 / 每周一 10:00 / 每月 1 号 09:00 / 原生 cron；`del <id>` 删、`list` 列（重启保留） |
| `/status` | 当前会话状态（模式/模型/历史/后台任务数） |
| `/dash` | 网页看板与会话终端地址 |
| `/insight [天数]` | 用量洞察：近 N 天（默认 7）按 agent 的调用数/tokens/反馈 |
| `/model [名称]` | 查看/切换本会话模型 |
| `/agents` · `/agent <名>` | 查看/切换本会话 agent（handoff；`/agent auto` 恢复路由） |
| `/<agent名> <任务>` | 动态派发：任意已配置 agent 直接接活（如 `/reviewer 帮我审这段 diff`），后台执行、结果回本聊天 |
| `/orchestrate a,b [+ 汇总] 任务` | 并行编排：多 agent 同时做同一任务，完成即回帖；`+ 汇总agent` 最后综合各方答案 |
| `/oncall [next]` | 值班表：当前值班/换班时间/轮序；next 看接下来三班 |
| `/system [提示词]` | 查看/覆盖 system 提示词 |
| `/skills` · `/skill <名> <指令>` | 列出 / 使用技能 |
| `/flows` · `/flow <名> <输入>` | 列出 / 后台跑多步工作流 |
| `/forms` · `/form <名>` | 列出 / 发送表单卡片（飞书） |

群聊默认需要 @机器人 才响应（agent 可配 `"groupMention": false` 关闭）。

## 自更新

```bash
chatmux version   # v0.4.5 (darwin/arm64)
chatmux update    # 拉 GitHub latest release 并原子替换自身
```

## fleet 多实例管理

一台机器管多个 chatmux 网关（`~/.chatmux/fleet.json` 或 `-fleet 路径`，
或环境变量 `CHATMUX_FLEET`）：

```json
{"instances":[{"name":"prod","baseUrl":"http://10.0.0.2:8080","token":"…"},
              {"name":"lab","baseUrl":"http://127.0.0.1:8081"}]}
```

```bash
chatmux fleet list                  # 全实例健康（版本/通道/agents，并行探测）
chatmux fleet status prod           # 单实例完整状态
chatmux fleet send prod -to feishu:oc_xxx 文本
chatmux fleet ask prod -to feishu:oc_xxx 问题 -opt A -opt B
```

## MCP 服务（被 Claude Code 等调用）

chatmux 可作为 stdio MCP server 暴露给 MCP 客户端——botmux 供宿主
agent 调用的那套面：

```bash
claude mcp add chatmux -- chatmux mcp -config /path/to/chatmux.json
```

注册后宿主 agent 获得 4 个工具：

| 工具 | 作用 |
| --- | --- |
| `chatmux_send` | 发消息/文件进任意聊天 |
| `chatmux_ask` | 向聊天提问并阻塞等真人回答（选项走表单卡片） |
| `chatmux_status` | 网关健康快照（通道/运行时/agent） |
| `chatmux_history` | 读某聊天最近对话（GET /admin/history） |

## 阻塞问答（ask）

CLI agent 或运维在会话外向聊天提问并阻塞等答案——botmux ask 的对应件：

```bash
chatmux ask -to feishu:oc_xxxx "部署走哪个方案？" -opt 方案A -opt 方案B -timeout 600
```

飞书收到 ❓ 表单卡片（选项走下拉，提交即回）；其余平台收到文本提问，
该聊天下一条有效回复即为答案（选项题只认选项）。CLI agent 里配好
system 提示词即可把它当"问人工具"用。每个聊天同时只有一个待答问题，
后问覆盖前问。

群聊默认需要 @机器人 才响应（agent 可配 `"groupMention": false` 关闭）。

**群事件**：机器人被拉进飞书群（im.chat.member.bot.added）、Telegram 群
（my_chat_member → member/administrator）或钉钉群（chat_add_member 且成员
列表含 `robotCode`/`botUserId`）自动发欢迎语（配置 `groupWelcome` 可覆盖
默认文案，不配置则用 i18n 默认；钉钉欢迎走主动消息 API，需配 robotCode）；
被移出群（deleted / kicked / left / chat_remove_member）只记 usage 事件
不打扰。企微没有机器人进群回调，客户群变更（change_external_chat：
建群/解散/成员进出）同样记 usage 事件。

## 引用上下文

回复别人的消息时，被引内容自动注入模型上下文（`[引用 老王 的消息]…`）。
飞书（回复消息）、Telegram、Discord、Slack（线程跟帖）均已接；飞书话题群
内跟帖不算引用，走话题锚点会话；Telegram 论坛超群按 message_thread_id
（话题）隔离会话，回复自动落回原话题。

## 语音转写

```json
"transcribe": { "enabled": true, "model": "whisper-1", "baseUrl": "https://api.openai.com/v1" }
```

开启后语音消息自动转文字（OpenAI 兼容 audio/transcriptions，apiKey 缺省用
OPENAI_API_KEY），以 `[语音]` 段落并入提示词。Telegram voice / 飞书音频 /
Telegram 音频文件均支持。

## 技能与工作流

**技能**：`skillsDir` 目录下每个 `*.md` 是一个技能（文件名即技能名），
也支持 Claude 式 `<name>/SKILL.md` 目录技能（目录内可放模板等辅助文件）。
开头 `---\ndescription: …\n---` frontmatter 的描述进 `/skills` 列表，注入时
自动剥离。`/skill review 请看这段代码` 把技能正文 + 你的指令一起发给 agent。

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

配置态（重启不丢、随文件改）：

```json
"cron": [{ "name": "standup", "schedule": "30 9 * * 1-5", "channel": "feishu", "chatId": "oc_xxxx", "text": "总结昨天进展" }]
```

会话态：`/schedule add 每天 09:30 站会提醒`——聊天里直接建周期任务，
存 `dataDir/schedules.json`，重启自动恢复；`/schedule list` 列出、
`/schedule del <id>` 删除。时间短语支持 每天/工作日/每周末/每周一/每月 1 号
+ HH:MM，也接受原生 5 段 cron。

## 文档监视（docwatch）

本地文档一变，agent 就被叫起来看：

```json
"docwatch": [{ "name": "plan", "path": "docs/plan.md", "intervalSec": 60,
  "channel": "feishu", "chatId": "oc_xxxx",
  "prompt": "计划文档有更新，请总结变更：\n{{diff}}" }]
```

首次只记基线不触发；之后每次变更产出 +/- 行级差异摘录（上限可配长度，
LCS 行 diff）注入 `{{diff}}`，其余占位符 `{{name}}` `{{path}}`。

## 会议代理（飞书 VC）

在飞书开放平台订阅视频会议事件（vc.bot.meeting_invited_v1 /
meeting_started_v1 / meeting_ended_v1、vc.meeting.recording_ready_v1），
chatmux 把它们映射为 meeting_* 事件：

```json
"meeting": { "channel": "feishu", "chatId": "oc_xxx" }
```

被邀请入会、会议开始/结束、录制就绪（含时长与链接）都会通知到该聊天，
并记 usage（mode meeting:*）。如实边界：不自动入会、不拉音频流、不转写
——那需要桌面端 SDK 与真凭据；录制就绪通知的是回放链接。

## 值班（oncall）

```json
"oncall": { "members": ["张三", "李四", "王五"], "rotation": "daily",
            "anchor": "2026-11-01", "channel": "feishu", "chatId": "oc_xxxx" }
```

- `/oncall` 当前值班 + 换班时间 + 轮序；`/oncall next` 接下来三班
- rotation `daily`（默认）/`weekly`（周一起算）；anchor 之前钳到首位
- 每天 00:05 检查换班，变化即在配置的聊天发交接通知（🔔 值班交接：A → B）

## 网页看板

浏览器打开 `http://127.0.0.1:8080/`：通道健康、运行时与会话指标、后台任务
列表、按 agent 的用量与 👍/👎 反馈统计，5 秒自动刷新（adminToken 输入一次
存本地）。看板含**会话板**（每会话锚点/名称/agent/消息数/忙闲）与
**需要你回答**（ask 待答问题及挂起时间）两张表，以及**会话终端**——直接在
网页里和网关对话（web 通道，命令、动态派发、ask 全部可用，回复流式更新）。

## 桌面端（Electron 壳）

`desktop/` 目录是一个极薄的 Electron 壳：加载网关看板 + 会话终端，
轮询 `/admin/chat` 把新的 bot 回复弹成系统通知（macOS 带 Dock 角标，
点击通知回到窗口），托盘菜单显示/退出，单实例。所有逻辑仍在 Go
二进制里，壳只负责窗口与通知。

```bash
cd desktop
npm install
CHATMUX_URL=http://127.0.0.1:8080 CHATMUX_TOKEN=<adminToken> npm start
npm run dist   # 打包 dmg / AppImage / nsis（需真实桌面环境）
```

注：壳已过语法校验与 JSON 校验，但未在无头环境跑过 GUI——打包与托盘
图标（`desktop/icon.png`）请在桌面机上验证。

## 反馈按钮

最终回复在飞书（卡片按钮）、Telegram（inline keyboard）、Discord
（components 按钮）、Slack（blocks actions）都带 👍/👎；点按即时回执，
结果计入 usage.jsonl、看板与 /metrics。

## 表单卡片（飞书）

chatmux.json 的 `forms[]` 定义表单字段（input/textarea/select），`/form <名>`
发出卡片，群友填写提交后，内容被合成成一条用户消息发给该会话的 agent
处理——适合报 bug、登记需求等结构化入口：

```json
"forms": [{ "name": "bug", "desc": "报 bug", "fields": [
  { "name": "title", "label": "标题", "type": "input", "required": true },
  { "name": "sev", "label": "严重程度", "type": "select", "options": ["低","中","高"] }
]}]
```

## CLI 沙箱

CLI agent 可整体装进容器跑，宿主零接触（workingDir 挂到 /work，默认断网）：

```json
"cli": { "type": "claude", "workingDir": "/srv/project",
         "sandbox": { "mode": "docker", "image": "node:20-slim", "network": "none" } }
```

## 运维接口

- `GET /healthz` — 存活/降级探测（任一通道断开即 503）
- `GET /metrics` — Prometheus 指标：通道健康、活跃会话、在跑 turn/后台任务、
  每 agent 调用数与 token、👍/👎 总量、uptime
- `GET/POST /admin/status|stop|send|usage` — 管理 API（X-Admin-Token 保护）；
  `/admin/send` 带可选 `file` 字段即上传宿主文件到聊天（飞书/Telegram/
  Discord/Slack 支持，图片自动按照片/图片发）
- 客户端：`chatmux status|stop|send -to channel:chatId [-file 路径] 文本`
  （`-config` 可放子命令后）

会话与 CLI resume id 分别快照到 `dataDir/sessions.json` /
`dataDir/cli-sessions.json`（原子写），重启不丢上下文；回复语言用
`"i18n": {"locale": "en"}` 一键切换（默认 zh）。

## 部署

- **Docker**：`docker build -t chatmux .` 配置挂 `/app/chatmux.json`，数据卷
  `/app/data`
- **systemd**：`deploy/systemd/chatmux.service`（含读写目录加固）
- **launchd**：`deploy/launchd/com.zhangchunsheng.chatmux.plist`
- **CI/发布**：push 跑 vet+test+build；打 `v*` tag 自动交叉编译
  linux/darwin/windows × amd64/arm64 并挂到 GitHub Release

## 各平台接入要点

| 平台 | 传输 | 前置条件 | 流式编辑 |
|---|---|---|---|
| 飞书 | WebSocket 长连接（默认）或 webhook | 企业自建应用 + 机器人能力 + `im.message.receive_v1` 订阅 + 长连接模式 | ✅ 卡片 PATCH |
| 钉钉 | Stream Mode（官方 SDK） | 企业内部机器人 + Stream 模式开启；群事件需在开放平台订阅 chat_add_member / chat_remove_member / chat_disband，并配 `robotCode`（或 `botUserId`）以识别机器人自身 | ❌（sessionWebhook 仅整条发送） |
| 企业微信 | HTTP 回调（需公网可达 URL） | 自建应用 + 接收消息回调配置；AES 加解密按官方规范实现；群事件走客户群变更回调（change_external_chat，仅记事件） | ❌ |
| Telegram | 长轮询（无需公网） | @BotFather token | ✅ editMessageText |
| Slack | Socket Mode（无需公网） | xoxb + xapp token，订阅 message/app_mention 事件 | ✅ chat.update |
| Discord | Gateway WS（无需公网） | Bot token + Message Content Intent | ✅ PATCH message |

## 安全

- 每个 `channel.options` 支持 `allowUsers` / `allowChats`（空 = 不限制，生产务必配置）
- 群聊默认 @ 门控；CLI agent 的 `workingDir` 决定它能碰哪些文件
- 飞书 webhook 模式实现 sha256(timestamp+nonce+encryptKey+body) 验签 + ±1h 时间窗
- 企业微信回调按官方 AES-256-CBC + SHA1 签名规范，任何不匹配直接 403
- 会话去重存储与状态文件落在 `dataDir`（默认 `data/`），权限 0600
- 图片下载：飞书按 image_key 走 im 资源接口、Telegram 走 getFile、Slack
  带 bot token 拉 url_private、企微按 media_id 走 media/get、钉钉按
  downloadCode 走 messageFiles/download（需配 robotCode）、Discord 直接
  用公开 CDN URL，落到 `dataDir/attachments/` 后以本地文件喂给视觉模型。
  企微/钉钉语音同样取回后送转写

## 状态

v0.6.22（对齐循环第 44 轮）：会话表瘦身——单会话历史本有 40 条上限，但
会话数量无界：每个后台任务留一个死 |job 会话常驻内存（每任务一个，永不
回收），一次性群聊永久躺在 sessions.json。现在任务完成即重置其会话；
Save 时清掉空会话（纯 Get 副产物）；持久化按 LastUsed 封顶 500 条，最陈
旧的先淘汰。

v0.6.21（对齐循环第 43 轮）：钉钉附件落地——六通道附件覆盖收口。图片/
语音/文件消息解析 content.downloadCode，经 app token（复用已有缓存）+
messageFiles/download 两步换取临时 URL 再取字节；需要渠道配 robotCode。
至此六通道（飞书/钉钉/企微/TG/Slack/Discord）图片全覆盖，语音转写覆盖
飞书/TG/企微/钉钉。

v0.6.20（对齐循环第 42 轮）：企微附件落地——file/voice 原来只有 media_id
没有下载器，语音消息被静默丢弃（转写从未见过它们）、文件无人认领。企微
实现 media/get 下载（错误走 HTTP 200+JSON 的坑用 Content-Type 分流），
voice 归类改 audio 进转写链路，image 补 Key 让下载优先于短时效 pic_url。

v0.6.19（对齐循环第 41 轮）：Slack 图片修复——url_private 需要 bot token
才能取，原来直接把裸 URL 喂给视觉模型（只能拿到登录页）。Slack 实现
AttachmentDownloader 带 Bearer 下载，attachImages 改为"有下载器的通道
连 URL 附件也自己下"（下载失败回退裸 URL，公开 CDN 行为不变）。slack
包测试从 0 到 1。企微文件/语音（media_id 无下载器）与钉钉（不支持附件）
为已知边界，README 能力表照实标注。

v0.6.18（对齐循环第 40 轮）：turn 看门狗——turn 上下文原来无期限，CLI
子进程挂死会永远占住会话（API 路径仅靠 HTTP 超时兜底）。现在每 turn 有
预算（agent 级 turnTimeout，默认 30m；后台任务 4×），超时取消并记日志；
被打断的 turn（/stop、超时、停机）原来完全不记账，现在以 cancelled /
turn budget exceeded 落 usage——"漏记账"家族最后一个洞。

v0.6.17（对齐循环第 39 轮）：优雅关机收尾——SIGTERM 原来只停 HTTP 后直接
退出：Runtime.Shutdown()/usage.Close() 从未被调用，进行中的 turn 被硬杀
（记账与最后几条会话更新丢失）。现在停机时先等在飞 turn 落地（8s 上限），
再同步快照会话、关闭 usage 日志。

v0.6.16（对齐循环第 38 轮）：usage.jsonl 尺寸轮转——原为无限追加，且
/insight/看板每次全量读盘，长跑实例文件与读放大都无界；现在过 10MB
自动轮转为 usage.jsonl.1（留一份备份），写入侧计数、失败降级为继续追加。

v0.6.15（对齐循环第 37 轮）：下载限流从"截断"改"拒绝"——飞书/Telegram
的附件下载原用 LimitReader，超 20MB 的文件被静默截成 20MB（图片损坏、
语音半截，下游报莫名其妙的错）；现 channel.ReadCapped 读到超限即整体
报错（上限常量收敛到 channel.MaxAttachmentBytes 单一来源），下载失败
（含超限）也记日志而非静默跳过。channel 包测试从 0 到 1。

v0.6.14（对齐循环第 36 轮）：附件大小上限——图片/语音下载原无上限，超大
文件会被整个读进内存、落盘并送视觉/转写 API；现在单附件 20MB 封顶，超限
跳过并记日志。顺带排查确认 bg 任务表完成后即删、去重缓存有界 TTL、web
通道日志 500 条封顶——无其他泄漏点。

v0.6.13（对齐循环第 35 轮）：ask 超时清障——ask 的 Wait 超时/取消只返回
错误不清理 Broker 表，僵尸问题永久留在待答列表（/admin/ask 与看板）；
现在放弃即移除（身份校验防误删后继问题），Resolve 后的迟到放弃是空操作。
带 race 检测的双测试覆盖。

v0.6.12（对齐循环第 33 轮）：看板用量表补"均耗时"列（⏱ 三档格式，与
/insight 对齐）——平均时延至此三个表面齐全：usage.jsonl（原始）→
/insight（文本）→ 看板（表格）。smoke 实测 avgMs 聚合与列渲染正确。

v0.6.11（对齐循环第 32 轮）：/insight 平均时延——每条 usage 记录早已存
DurationMs 但从未展示；Agg 增加 avgMs（均值），/insight 行追加 ⏱340ms/
⏱1.5s/⏱2m5s 三档格式。顺带排查确认会话持久化每轮落盘（finish →
persistSessions），崩溃安全无缺口。

v0.6.10（对齐循环第 31 轮）：配置文档补全——config 结构体字段与
chatmux.json.example 程序化比对，补三个未文档化字段：路由 userId（按用户
过滤唤起 agent）、cron/docwatch 的 threadId（话题群定向）；三者均已逐一
验证在代码里真实消费（非死字段）。

v0.6.9（对齐循环第 30 轮）：记账闭环——runOnce（工作流步骤/动态派发/bg 任务
共用）此前完全不记 usage，/orchestrate 的 callAgentText 错误口也漏；现在所有
模型/CLI 调用点（runTurn 三口、runCliTurn 三口、runOnce 两分支、
callAgentText 全口）都落账含 Error 字段，TestRunOnceAccounting 三态覆盖。

v0.6.8（对齐循环第 29 轮）：CLI 轮次记账对齐——runCliTurn 的启动失败路径
此前不记 usage、流内出错不进 Error 字段、无 turn 日志（API 路径第 22-23 轮
修过的问题在 CLI 路径的翻版）；三出口全部补齐，并引入 cliRunner 接缝使
CLI 轮次可测（TestCliTurnAccounting 三态）。

v0.6.7（对齐循环第 28 轮）：turn 级运行日志——daemon.log 此前只有频道事件，
模型轮次完全不可见；现在每轮（成功/两种失败）各记一行
`turn status= agent= model= chat= dur_ms= in_tok= out_tok= out_chars= err=`，
运维不开看板也能跟流量。TestTurnLogged 捕获日志断言。

v0.6.6（对齐循环第 27 轮）：会话历史字符预算——此前只按条数（40 条）截断，
贴超长文档时几轮就能撑爆模型上下文；现在回放历史前先做 8 万字符预算裁剪
（从最旧对话对开始丢，永远保留最近一对），TestTrimToBudget 覆盖三态。

v0.6.5（对齐循环第 26 轮）：排队提示降噪——turn 进行中连发消息，此前每条
都回一条"已排队"，长回复期间被刷屏；改为每 turn 只提示一次，后续消息静默
入队（turn 结束后仍合并成一条派发）。附 TestQueuedNudgeOnce 双断言
（恰好 1 次 ack + 排队消息全部转入下轮）。

v0.6.4（对齐循环第 25 轮）：频道重启退避——凭据无效等崩溃场景此前固定 5s
无限热重启（12 次/分钟打上游 API + 刷日志）；改为指数退避 5s→10s→…→5min
封顶，连续健康运行 ≥10min 归零重来；日志附本次运行时长。

v0.6.3（对齐循环第 24 轮）：命令面终校——/help 实际输出、agent.go dispatch
命令集、README 命令表三方核对（26 条 dispatch 全部有文档；README 中的
/admin/*、/app 均为 HTTP/容器路径，非聊天命令）。补两处：/help 增加动态派发
/<agent名> 行（zh/en，此前聊天内无法发现该能力），README 表补 /help 行。

v0.6.2（对齐循环第 23 轮）：失败调用记账补全——第 22 轮只覆盖了"流已建立
后出错"（model.err）一条路径，"Stream 直接返回错误"（model.fail，如密钥无效
的 401）仍漏记；两路径现已都落账，并有双路径单测覆盖。看板用量表加"失败"
列，/metrics 新增 chatmux_usage_errors_total。

v0.6.1（对齐循环第 22 轮）：失败调用记账——API 流出错（如 401）此前在
usage 落账前 return，/insight 与看板会漏掉失败；现在失败也记（Record.Error，
Agg.Errors 计数，/insight 行显示"N 失败"）。CLI 路径失败本就落账。

v0.6.0（对齐循环第 19-20 轮，收尾轮）：与 botmux 命令表全量对照——49 条
命令逐条映射（对齐 / 对应实现 / 架构不适用三类）；补 /dash（看板地址）与
/insight [天数]（按 agent 的近 N 天调用量/tokens/反馈聚合，
usage.SummarizeSince 时间窗）；其余 botmux 专属命令（adopt/lane/term/queue/
fork/repo wt 等）绑定其"每话题一个 PTY CLI 进程"架构，chatmux 的网关+
会话轮次模型以 /new//agents/后台队列/工作流覆盖对应场景，如实声明不逐条复刻。

v0.5.8（对齐循环第 18 轮）：会议代理（飞书 VC）——订阅 vc.bot.meeting_*
与 vc.meeting.recording_ready_v1 事件，会议被邀/开始/结束/录制就绪映射为
meeting_* 合成事件，按 meeting.channel+chatId 配置转发通知到指定聊天并记
usage。如实边界：不含自动入会、音频拉流与转写（需真机 SDK 与凭据），
录制就绪只通知链接；被邀入会明确提示"仅通知"。

v0.5.7（对齐循环第 17 轮）：话题群进阶盘点收口（飞书/Slack/Discord 线程
锚点与落回均已就位，v0.5.6 补齐 Telegram 论坛话题，套件对齐完成）+
Electron 桌面端壳（desktop/：看板窗口 + 系统通知 + Dock 角标 + 托盘 +
单实例；语法/JSON 已校验，GUI 未在无头环境验证）。

v0.5.6（对齐循环第 16 轮）：Telegram 话题群——论坛超群 message_thread_id
作为话题会话键（优先于回复派生的 ThreadID），回复/文件自动带回
message_thread_id 落回原话题；channel.Message 新增 ThreadIsTopic 标记。

v0.5.5（对齐循环第 15 轮）：企微群事件——change_external_chat / chat.update
回调映射（create→建群、dismiss→解散、update+add/del_member→成员进出），
系统事件绕过用户白名单，全部记 usage。企微自建应用没有"机器人进群"回调，
平台能给的只有客户群生命周期，故此通道不加入欢迎语逻辑（如实按平台能力对齐）。

v0.5.4（对齐循环第 14 轮）：钉钉群事件对齐——Stream Mode 增订群会话事件
（chat_add/remove_member、chat_disband），机器人进出群按 robotCode /
botUserId 严格匹配（配不上则按普通成员事件记 usage），进群欢迎走
/v1.0/robot/groupMessages/send 主动消息（access_token 缓存至过期前 5 分钟）；
未配 robotCode 时欢迎明确报错而不是静默失败。

v0.5.3（对齐循环第 13 轮）：技能包升级——支持 Claude 式 `<name>/SKILL.md`
目录技能（目录内可放辅助文件），frontmatter `description:` 自动进 /skills
列表（无描述截取正文首行），注入正文剥离 frontmatter；平铺 `*.md` 兼容不变。

v0.5.2（对齐循环第 12 轮）：Telegram 群事件对齐——getUpdates 增订
my_chat_member，进群（member/administrator/restricted）发欢迎语，
出群（kicked/left）静默记录，复用 v0.5.1 的 Event 机制。

v0.5.1（对齐循环第 11 轮）：群套件起步——飞书机器人进群事件
（im.chat.member.bot.added）自动欢迎语（groupWelcome 可覆盖），
移出群记 usage 事件；channel.Message 新增 Event 字段。

v0.5.0（对齐循环第 10 轮）：oncall 值班——配置 members + daily/weekly
轮换 + anchor 起算，/oncall 与 /oncall next 查询，每天 00:05 自动检查
换班并向配置聊天发 🔔 交接通知。

v0.4.9（对齐循环第 9 轮）：fleet 多实例管理——~/.chatmux/fleet.json
（或 -fleet / CHATMUX_FLEET）列出多网关，chatmux fleet list 并行探测
健康（版本/通道/agents，status 新增 version 字段）、status / send / ask
按实例代理。

v0.4.8（对齐循环第 8 轮）：/orchestrate 并行编排——多 agent 扇出同一
任务（无状态调用，完成即回帖【agent】），可选 `+ 汇总agent` 综合各方
答案；usage 记 mode=orchestrate。

v0.4.7（对齐循环第 7 轮）：文档监视 docwatch——本地文件变更检测
（LCS 行 diff + 摘录上限），变更即以 {{diff}} 触发 agent 提示词，
首见只记基线。

v0.4.6（对齐循环第 6 轮）：网页会话终端——看板内置聊天面板（web 通道：
POST /admin/chat 进、GET /admin/chat?since= 增量轮询出、Replace 原地更新
流式回复）；首版 GitHub Release v0.4.5 已由 tag 流水线发布，chatmux update
实测可用。

v0.4.5（对齐循环第 5 轮）：CLI 适配器扩展（qwen / opencode / aider 预设，
argv 提示词 + 纯文本输出）+ 自更新（`chatmux version` / `chatmux update`，
从 GitHub Releases 拉对应平台产物并原子替换，internal/version 单一版本源）。

v0.4.4（对齐循环第 4 轮）：MCP 服务——`chatmux mcp` 以 stdio MCP server
把 send/ask/status/history 暴露给 Claude Code 等 MCP 客户端
（botmux 供宿主 agent 调用的对应面）；新增 GET /admin/history。

v0.4.3（对齐循环第 3 轮）：动态派发 `/<agent名> <任务>`（任意已配置 agent
直接接活，无需预定义命令）、会话态周期任务 /schedule（中文时间短语 + 原生
cron，持久化恢复）、看板会话板与 ask 待答表、ask GET 鉴权修复。

v0.4.2：阻塞问答 ask（`chatmux ask` + ❓ 表单卡片 + 文本回答回退，
每聊天一问、后问覆盖前问）。

v0.4.1（对照 botmux 盘点后的补差轮）：会话命名 /name、历史查看 /history、
上下文回退 /rewind、重试 /retry、一次性提醒 /remind（相对/绝对时间）、
发文件到聊天（feishu/TG/Discord/Slack，图片内联，`/admin/send file` 与
`chatmux send --file`）、agent 切换 /agents · /agent（会话级 handoff，
优先于路由，持久化）。

v0.4：多平台 👍/👎 按钮（飞书/TG/Discord/Slack）+ 飞书卡片表单
（forms[] + /form）+ CLI 沙箱（docker 隔离）+ 会话持久化（重启不丢）+ i18n
（zh/en）+ Prometheus /metrics + 多实例通道（同名平台多账号，去重按实例隔离）+
部署化（Dockerfile / CI / tag 发布 / systemd / launchd）。

v0.3：后台长任务（/bg /jobs /stop 扩展）+ 引用上下文（飞书/TG/Discord/Slack）
+ 语音转写（whisper 兼容）+ 网页看板 + 技能（skillsDir）+ 工作流
（{{prev}}/{{input}} 链）+ gemini CLI 预设。

## License

MIT
