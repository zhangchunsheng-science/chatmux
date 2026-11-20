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

下载校验：release 为每个产物附 `<产物>.sha256` 边车文件，update 拉回后
先比对 SHA256 再替换（不匹配直接拒装）；边车是后来才有的，旧 release
没有则跳过校验照常升级。

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
  每 agent 调用数与 token、👍/👎 总量、uptime、看门狗触发数、流启动重试数
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
- **CI/发布**：push 跑 vet+test+build+端到端冒烟；打 `v*` tag 自动交叉编译
  linux/darwin/windows × amd64/arm64 并挂到 GitHub Release
- **冒烟**：`scripts/smoke.sh`——本地端到端（构建→check→起实例→成功轮
  +失败轮→记账核对→优雅停机），零凭据零外网

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

v0.6.35（对齐循环第 55 轮）：chatmux check 二次打磨——修 "web" 通道被
冤报未知通道路由的误报；补 dataDir 可写探测（不可写时 usage 记账静默丢
失，check 预先警告）。

v0.6.34（对齐循环第 54 轮）：i18n 完整性——zh/en 键对齐 + say() 字面量
全覆盖两个守护测试；当场抓到 /schedule 未知子命令回显裸键名的真 bug
（补 zh/en 文案修复）。

v0.6.33（第 53 轮补丁二）：发布流水线重构——矩阵只构建，单个 release
job 统一算校验和、清旧产物、一次性上传（v0.6.32 把清空放进每个矩阵 job，
并行互清只剩一对产物，实测暴露后重构）。

v0.6.32（第 53 轮补丁）：发布流水线资产清空——重跑/重打 tag 先清该 tag
已有产物再上传，杜绝新旧混合集（v0.6.31 实测踩中：强推 amend 后两次
构建不同字节，updater 校验当场拒装——校验按设计工作）。

v0.6.31（对齐循环第 53 轮）：自更新下载校验——release 每产物附 .sha256
边车，update 先比对再替换，不匹配拒装；旧 release 无边车跳过校验。

v0.6.30（对齐循环第 52 轮）：韧性事件上指标——看门狗触发数
（chatmux_turn_watchdog_fires_total）与流启动重试数
（chatmux_stream_retries_total）进 /metrics，此前只有 daemon 日志可
见；internal/metrics 原子计数包，冒烟死端口轮断言重试计数。
更早轮次（v0.1 → v0.6.22，对齐循环第 1–44 轮）见
[CHANGELOG.md](CHANGELOG.md)。

## License

MIT
