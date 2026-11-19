// User-facing strings for the agent runtime, zh (default) and en.
// All chatmux replies resolve through Runtime.say so a single config
// knob (i18n.locale) switches the whole surface.
package agent

import "fmt"

type dict map[string]string

var zh = dict{
	"queued":           "已收到，正在处理上一条；再发 /stop 可打断当前任务，/bg 可丢后台。",
	"stop.fg":          "已请求打断当前任务。",
	"stop.fg.none":     "当前没有在处理的前台任务。",
	"stop.bg.done":     "已停止 %d 个后台任务。",
	"stop.bg.none":     "没有在跑的后台任务。",
	"stop.job.done":    "已停止后台任务 %s。",
	"stop.job.none":    "没有找到后台任务 %s（/jobs 查看）。",
	"bg.usage":         "用法: /bg <任务描述> — 丢到后台跑，会话继续可用",
	"bg.started":       "🚀 后台任务 %s 已启动，会话继续可用；/jobs 查看，/stop %s 停止。",
	"flow.started":     "🚀 工作流 %s 已后台启动（%s）；/jobs 查看，/stop %s 停止。",
	"jobs.none":        "没有在跑的后台任务。",
	"jobs.header":      "后台任务：",
	"jobs.line":        "· %s [%s] %s — 已跑 %s（/stop %s 停止）",
	"new.done":         "已开启新会话。",
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
		"/status — 查看当前会话状态\n" +
		"/model [名称] — 查看/切换模型\n" +
		"/system [提示词] — 查看/覆盖 system 提示词\n" +
		"/skills · /skill <名> <指令> — 使用技能\n" +
		"/flows · /flow <名> <输入> — 跑多步工作流\n" +
		"/forms · /form <名> — 发送表单卡片（飞书）\n" +
		"群里默认需要 @机器人 触发。",
}

var en = dict{
	"queued":           "Got it — still working on the previous message; /stop interrupts, /bg detaches.",
	"stop.fg":          "Interrupt requested.",
	"stop.fg.none":     "No foreground task running.",
	"stop.bg.done":     "Stopped %d background job(s).",
	"stop.bg.none":     "No background jobs running.",
	"stop.job.done":    "Stopped background job %s.",
	"stop.job.none":    "No background job %s (see /jobs).",
	"bg.usage":         "Usage: /bg <task> — run detached, chat stays free",
	"bg.started":       "🚀 Background job %s started; /jobs lists, /stop %s stops.",
	"flow.started":     "🚀 Workflow %s running detached (%s); /jobs lists, /stop %s stops.",
	"jobs.none":        "No background jobs running.",
	"jobs.header":      "Background jobs:",
	"jobs.line":        "· %s [%s] %s — running %s (/stop %s)",
	"new.done":         "New session started.",
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
		"/status — session status\n" +
		"/model [name] — show/switch model\n" +
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
