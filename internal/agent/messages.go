// User-facing strings for the agent runtime, zh (default) and en.
// All chatmux replies resolve through Runtime.say so a single config
// knob (i18n.locale) switches the whole surface.
package agent

import "fmt"

type dict map[string]string

var zh = dict{
	"queued":              "已收到，正在处理上一条；再发 /stop 可打断当前任务，/bg 可丢后台。",
	"stop.fg":             "已请求打断当前任务。",
	"stop.fg.none":        "当前没有在处理的前台任务。",
	"stop.bg.done":        "已停止 %d 个后台任务。",
	"stop.bg.none":        "没有在跑的后台任务。",
	"stop.job.done":       "已停止后台任务 %s。",
	"stop.job.none":       "没有找到后台任务 %s（/jobs 查看）。",
	"bg.usage":            "用法: /bg <任务描述> — 丢到后台跑，会话继续可用",
	"bg.started":          "🚀 后台任务 %s 已启动，会话继续可用；/jobs 查看，/stop %s 停止。",
	"flow.started":        "🚀 工作流 %s 已后台启动（%s）；/jobs 查看，/stop %s 停止。",
	"jobs.none":           "没有在跑的后台任务。",
	"jobs.header":         "后台任务：",
	"jobs.line":           "· %s [%s] %s — 已跑 %s（/stop %s 停止）",
	"new.done":            "已开启新会话。",
	"name.cur":            "当前会话名: %s\n用法: /name <新名字> 重命名（仅本会话标识，不改群名）",
	"name.set":            "会话已命名: %s",
	"history.none":        "本会话还没有历史（/new 之后重新累计）。",
	"history.header":      "最近 %d 轮对话：",
	"history.line":        "· %s: %s\n  ↳ %s",
	"history.tail":        "· %s: %s（未回复）",
	"rewind.usage":        "用法: /rewind [轮数] — 回退上下文，默认 1 轮（不删聊天记录，只影响模型记忆）",
	"rewind.done":         "已回退 %d 轮，剩余历史 %d 条。",
	"retry.none":          "没有可重试的对话（先正常聊一轮）。",
	"remind.usage":        "用法: /remind <时间> <事由> — 时间支持 +30m / +1h30m / 15:30 / 2026-11-20 15:30",
	"remind.bad":          "看不懂这个时间：%s（支持 +30m / +1h30m / 15:30 / 2026-11-20 15:30）",
	"remind.set":          "⏰ 已设提醒：%s（%s 后）",
	"remind.fire":         "⏰ 提醒（%s 前设置）：%s",
	"agents.none":         "没有配置任何 agent。",
	"agents.header":       "可用 agent（/agent <名> 切换，/agent auto 恢复路由）",
	"agents.line":         "%s %s — %s",
	"agent.usage":         "用法: /agent <名> 切换本会话的 agent；/agent auto 恢复按路由自动选择",
	"agent.none":          "没有 agent %s（/agents 查看）。",
	"agent.set":           "本会话已切到 agent %s（历史保留，下一条消息生效）。",
	"agent.auto":          "已恢复按路由自动选择 agent。",
	"dispatch.usage":      "用法: /%s <任务> — 让 agent %s 在本会话后台跑这个任务",
	"orchestrate.usage":   "用法: /orchestrate agent1,agent2[,…] [+ 汇总agent] 任务 — 并行派发，结果依次回帖，可选汇总",
	"orchestrate.started": "🚀 已并行派发给 %d 个 agent（%s），完成后逐个回帖。",
	"orchestrate.noagent": "未配置的 agent：%s（/agents 查看）。",
	"orchestrate.part":    "【%s】\n%s",
	"orchestrate.merged":  "【汇总 · %s】\n%s",
	"orchestrate.fail":    "【%s】调用失败：%v",
	"oncall.none":         "没有配置值班表（chatmux.json 的 oncall.members）。",
	"oncall.now":          "🕐 当前值班：%s（%s 换班）\n轮序：%s（/oncall next 看后面）",
	"oncall.next.header":  "接下来的值班：",
	"oncall.next.line":    "· %s：%s → %s",
	"group.welcome":       "👋 你好，我是 chatmux 机器人。群里 @我 + 问题即可对话；发 /help 看全部命令。",
	"usage.sched":         "/schedule add <时间> <内容> · /schedule list · /schedule del <id>",
	"sched.none":          "没有会话内定时任务（/schedule add 每天 9:30 站会总结）。",
	"sched.header":        "本实例的会话内定时任务：",
	"sched.line":          "· %s [%s] %s（/schedule del %s 删除）",
	"sched.add.usage":     "用法: /schedule add <时间> <内容>",
	"sched.bad":           "看不懂时间 %s（支持：每天/工作日/每周三/每周末/每月1日 + HH:MM，或原生 cron）",
	"sched.add.ok":        "✅ 定时任务 %s 已加（%s），下次触发 %s。",
	"sched.del.usage":     "用法: /schedule del <id>",
	"sched.del.none":      "没有定时任务 %s（/schedule list 查看）。",
	"sched.del.ok":        "已删除定时任务 %s。",

	"status.fmt":       "chatmux %s · 会话 %s · 模式 %s · 模型 %s · 历史 %d 条 · 后台任务 %d 个",
	"status.busy":      "处理中",
	"status.idle":      "空闲",
	"model.cur":        "当前模型: %s\n用法: /model <模型名> 切换",
	"model.set":        "模型已切换为 %s，下一条消息生效。",
	"system.cur":       "当前 system 提示词:\n%s\n\n用法: /system <新提示词>；/new 恢复默认",
	"system.set":       "system 提示词已更新。",
	"skills.none":      "没有已安装技能。把 *.md 放到 skillsDir 并重启。",
	"skills.header":    "可用技能（/skill <名> [指令] 使用）：",
	"skills.line":      "· %s — %s",
	"skill.none":       "没有技能 %s（/skills 查看）。",
	"skill.usage":      "用法: /skill %s <补充指令>",
	"flows.none":       "没有配置工作流。chatmux.json 的 workflows[] 定义多步链。",
	"flows.header":     "可用工作流（/flow <名> [输入] 后台执行）：",
	"flows.line":       "· %s — %s",
	"flow.none":        "没有工作流 %s（/flows 查看）。",
	"flow.usage":       "用法: /flow %s <输入>",
	"forms.none":       "没有配置表单。chatmux.json 的 forms[] 定义卡片表单。",
	"forms.header":     "可用表单（/form <名> 发送）：",
	"forms.line":       "· %s — %s",
	"form.none":        "没有表单 %s（/forms 查看）。",
	"form.usage":       "用法: /form <表单名>",
	"form.unsupported": "该通道暂不支持表单卡片（目前支持：飞书）。",
	"form.sent":        "已发送表单 %s，请填写并提交。",
	"form.err":         "表单发送失败: %v",
	"provider.none":    "模型提供方未配置。",
	"model.fail":       "模型调用失败: %s",
	"model.err":        "模型出错: %s",
	"cli.fail":         "启动 CLI 失败: %s",
	"empty.reply":      "(空回复)",
	"interrupted":      "[已打断]",
	"bg.done":          "✅ 后台任务 %s（%s）完成：\n\n%s",
	"bg.stopped":       "⛔ 后台任务 %s（工作流 %s）已停止。",
	"flow.fail":        "⚠️ 工作流 %s 失败: %v",
	"flow.done":        "✅ 工作流 %s（%s）完成：\n\n%s",
	"voice.tag":        "[语音]",
	"quote.anon":       "某人",
	"quote.fmt":        "[引用 %s 的消息]\n%s\n---\n%s",
	"help": "chatmux 命令：\n" +
		"/stop [bg|任务id] — 打断前台/后台任务\n" +
		"/bg <任务> — 丢后台跑，会话不被占用\n" +
		"/jobs — 列出后台任务\n" +
		"/new — 开启全新会话（清历史/CLI 会话）\n" +
		"/name <名> — 给会话命名（看板/状态里显示）\n" +
		"/history [轮数] — 查看本会话最近对话\n" +
		"/rewind [轮数] · /retry — 回退上下文 / 重试上一轮\n" +
		"/remind <时间> <事由> — 一次性提醒（+30m / 15:30 / 2026-11-20 15:30）\n" +
		"/schedule — 会话内定时任务（每天 9:30 站会总结）\n" +
		"/orchestrate a,b [+ 汇总] 任务 — 并行多 agent 派发（可选汇总）\n" +
		"/oncall [next] — 查看值班（当前/后续，配置 oncall.members）\n" +
		"/status — 查看当前会话状态\n" +
		"/model [名称] — 查看/切换模型\n" +
		"/agents · /agent <名> — 查看/切换本会话的 agent（handoff）\n" +
		"/system [提示词] — 查看/覆盖 system 提示词\n" +
		"/skills · /skill <名> <指令> — 使用技能\n" +
		"/flows · /flow <名> <输入> — 跑多步工作流\n" +
		"/forms · /form <名> — 发送表单卡片（飞书）\n" +
		"群里默认需要 @机器人 触发。",
}

var en = dict{
	"queued":              "Got it — still working on the previous message; /stop interrupts, /bg detaches.",
	"stop.fg":             "Interrupt requested.",
	"stop.fg.none":        "No foreground task running.",
	"stop.bg.done":        "Stopped %d background job(s).",
	"stop.bg.none":        "No background jobs running.",
	"stop.job.done":       "Stopped background job %s.",
	"stop.job.none":       "No background job %s (see /jobs).",
	"bg.usage":            "Usage: /bg <task> — run detached, chat stays free",
	"bg.started":          "🚀 Background job %s started; /jobs lists, /stop %s stops.",
	"flow.started":        "🚀 Workflow %s running detached (%s); /jobs lists, /stop %s stops.",
	"jobs.none":           "No background jobs running.",
	"jobs.header":         "Background jobs:",
	"jobs.line":           "· %s [%s] %s — running %s (/stop %s)",
	"new.done":            "New session started.",
	"name.cur":            "Session name: %s\nUsage: /name <new-name> (labels this session only)",
	"name.set":            "Session named: %s",
	"history.none":        "No history yet in this session (counts from /new).",
	"history.header":      "Last %d exchanges:",
	"history.line":        "· %s: %s\n  ↳ %s",
	"history.tail":        "· %s: %s (unanswered)",
	"rewind.usage":        "Usage: /rewind [n] — roll context back n exchanges (default 1; chat log stays, model memory shrinks)",
	"rewind.done":         "Rewound %d exchange(s), %d messages left.",
	"retry.none":          "Nothing to retry (chat one turn first).",
	"remind.usage":        "Usage: /remind <time> <note> — time as +30m / +1h30m / 15:30 / 2026-11-20 15:30",
	"remind.bad":          "Can't parse that time: %s (try +30m / +1h30m / 15:30 / 2026-11-20 15:30)",
	"remind.set":          "⏰ Reminder set: %s (in %s)",
	"remind.fire":         "⏰ Reminder (set %s ago): %s",
	"agents.none":         "No agents configured.",
	"agents.header":       "Agents (/agent <name> to switch, /agent auto to restore routing)",
	"agents.line":         "%s %s — %s",
	"agent.usage":         "Usage: /agent <name> switches this session's agent; /agent auto restores routing",
	"agent.none":          "No agent %s (see /agents).",
	"agent.set":           "Session switched to agent %s (history kept, effective next message).",
	"agent.auto":          "Back to route-based agent selection.",
	"dispatch.usage":      "Usage: /%s <task> — run agent %s on it in this chat (background)",
	"orchestrate.usage":   "Usage: /orchestrate agent1,agent2[,…] [+ merger] task — parallel fan-out, replies as they land, optional merge",
	"orchestrate.started": "🚀 Fanned out to %d agents (%s); replies arrive as they finish.",
	"orchestrate.noagent": "No agent %s (see /agents).",
	"orchestrate.part":    "【%s】\n%s",
	"orchestrate.merged":  "【merged · %s】\n%s",
	"orchestrate.fail":    "【%s】failed: %v",
	"oncall.none":         "No on-call roster configured (oncall.members in chatmux.json).",
	"oncall.now":          "🕐 On duty now: %s (shift ends %s)\nRotation: %s (/oncall next for more)",
	"oncall.next.header":  "Upcoming shifts:",
	"oncall.next.line":    "· %s: %s → %s",
	"group.welcome":       "👋 Hi, I'm the chatmux bot. @me with a question in groups; /help lists commands.",
	"usage.sched":         "/schedule add <when> <text> · /schedule list · /schedule del <id>",
	"sched.none":          "No in-chat schedules yet (/schedule add daily 9:30 standup).",
	"sched.header":        "In-chat schedules:",
	"sched.line":          "· %s [%s] %s (/schedule del %s)",
	"sched.add.usage":     "Usage: /schedule add <when> <text>",
	"sched.bad":           "Can't parse time %s (每天/工作日/每周三/每周末/每月1日 + HH:MM, or raw cron)",
	"sched.add.ok":        "✅ Schedule %s added (%s), next fire %s.",
	"sched.del.usage":     "Usage: /schedule del <id>",
	"sched.del.none":      "No schedule %s (see /schedule list).",
	"sched.del.ok":        "Schedule %s deleted.",

	"status.fmt":       "chatmux %s · session %s · mode %s · model %s · history %d · bg jobs %d",
	"status.busy":      "busy",
	"status.idle":      "idle",
	"model.cur":        "Current model: %s\nUsage: /model <name> to switch",
	"model.set":        "Model switched to %s, effective next message.",
	"system.cur":       "Current system prompt:\n%s\n\nUsage: /system <prompt>; /new restores default",
	"system.set":       "System prompt updated.",
	"skills.none":      "No skills installed. Drop *.md files into skillsDir and restart.",
	"skills.header":    "Skills (/skill <name> [instruction]):",
	"skills.line":      "· %s — %s",
	"skill.none":       "No skill %s (see /skills).",
	"skill.usage":      "Usage: /skill %s <instruction>",
	"flows.none":       "No workflows configured. Define workflows[] in chatmux.json.",
	"flows.header":     "Workflows (/flow <name> [input], runs detached):",
	"flows.line":       "· %s — %s",
	"flow.none":        "No workflow %s (see /flows).",
	"flow.usage":       "Usage: /flow %s <input>",
	"forms.none":       "No forms configured. Define forms[] in chatmux.json.",
	"forms.header":     "Forms (/form <name> to send):",
	"forms.line":       "· %s — %s",
	"form.none":        "No form %s (see /forms).",
	"form.usage":       "Usage: /form <form-name>",
	"form.unsupported": "This channel has no form cards yet (supported: feishu).",
	"form.sent":        "Form %s sent — fill it in and submit.",
	"form.err":         "Failed to send form: %v",
	"provider.none":    "Model provider not configured.",
	"model.fail":       "Model call failed: %s",
	"model.err":        "Model error: %s",
	"cli.fail":         "Failed to start CLI: %s",
	"empty.reply":      "(empty reply)",
	"interrupted":      "[interrupted]",
	"bg.done":          "✅ Background job %s (%s) finished:\n\n%s",
	"bg.stopped":       "⛔ Background job %s (workflow %s) stopped.",
	"flow.fail":        "⚠️ Workflow %s failed: %v",
	"flow.done":        "✅ Workflow %s (%s) finished:\n\n%s",
	"voice.tag":        "[voice]",
	"quote.anon":       "someone",
	"quote.fmt":        "[quoting %s]\n%s\n---\n%s",
	"help": "chatmux commands:\n" +
		"/stop [bg|job-id] — interrupt foreground/background\n" +
		"/bg <task> — run detached, chat stays free\n" +
		"/jobs — list background jobs\n" +
		"/new — fresh session (history + CLI resume)\n" +
		"/name <label> — name this session (shows in status)\n" +
		"/history [n] — recent exchanges of this session\n" +
		"/rewind [n] · /retry — roll back context / retry last turn\n" +
		"/remind <time> <note> — one-shot reminder (+30m / 15:30 / 2026-11-20 15:30)\n" +
		"/schedule — in-chat recurring schedules (daily 9:30 standup)\n" +
		"/orchestrate a,b [+ merger] task — parallel fan-out with optional merge\n" +
		"/oncall [next] — duty roster (current/upcoming, from oncall.members)\n" +
		"/status — session status\n" +
		"/model [name] — show/switch model\n" +
		"/agents · /agent <name> — show/switch this session's agent (handoff)\n" +
		"/system [prompt] — show/override system prompt\n" +
		"/skills · /skill <name> <instruction> — skills\n" +
		"/flows · /flow <name> <input> — multi-step workflows\n" +
		"/forms · /form <name> — send a form card (feishu)\n" +
		"Groups require @bot by default.",
}

// say resolves a message key in the runtime's locale.
func (r *Runtime) say(key string, args ...any) string {
	locale := "zh"
	if r.cfg != nil && r.cfg.I18n != nil && r.cfg.I18n.Locale != "" {
		locale = r.cfg.I18n.Locale
	}
	var d dict = zh
	if locale == "en" {
		d = en
	}
	s, ok := d[key]
	if !ok {
		s = zh[key]
	}
	if s == "" { // unknown key: pass it through verbatim (debuggability)
		return key
	}
	if len(args) > 0 {
		return fmt.Sprintf(s, args...)
	}
	return s
}
