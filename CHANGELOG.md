# 更新日志

chatmux 逐轮打磨记录（对齐循环）。版本号与 git tag 一一对应；
README 保留最近几轮，这里是从 v0.1 起的完整历史。

## v0.6.45（对齐循环第 65 轮）

check 启动前最后一公里——(a) 监听端口占用预警：bind 失败（双实例最常
见的死法、权限不足）以前要到启动那一刻才见裸报错，现在 check 预先点名
"http.listen ... address already in use — the server would fail to bind"
（建议性，探测即释放）；(b) 未知 provider 与通道类型给拼写建议（"opena"→
openai、"feisu"→feishu），levenshtein 建议器提炼为 config.Suggest 共用。
测试 fixture 全部换临时端口（原硬编码 :8080 会随端口探测随机 flake——
本轮差点自己踩上）。实测四类问题一屏全中。

## v0.6.44（对齐循环第 64 轮）

healthz 降级点名——/healthz 降级时原来只有一句 "degraded"，现在点名坏
通道（按名排序）：`degraded: slack, tg1`。凌晨监控报警时"是哪个"是第一
个问题，答案现在就在探测响应里。渲染提取为 healthzBody 纯函数，
stubChan 假通道单测（零通道 200 ok / 两坏通道 503 排序点名），
/tmp/cm-smoke 实测假 token 通道被点名。

## v0.6.43（对齐循环第 63 轮）

/metrics 渲染守护——TestMetricsRendersAllSeries 从 main.go 源码提取全部
chatmux_* 指标族（# TYPE 声明 + gauge() 调用两种形态，≥10 族健全性下
限），起真实 server 断言每族都出现在活体输出里。渲染回归（改名/丢序
列/写坏条件）会静默删掉看板依赖的序列，现在变成测试失败。例外：反馈
两族需真实点赞才渲染，明确豁免。附带一轮 fd 泄漏审计：全仓 20 处
HTTP 调用点 body 关闭纪律全部干净（紧跟 defer Close）。

## v0.6.42（对齐循环第 62 轮）

panic 隔离——三类崩溃面全部兜住：(a) 通道适配器 Start panic（上游畸形
payload 打崩 nil map/越界）原样带死整个进程（六通道+网页控制台全灭），
superviseChannel 提取为可测函数并把 panic 转错误走既有指数退避重连；
(b) turn goroutine panic 原来会跳过全部清理——turns 表项永不删（该聊天
从此每条消息永久排队）、done 永不关（Busy/优雅停机挂死），清理改 defer
在 panic 路径同样执行；(c) /bg 任务 panic 原 leak 进 /jobs 永显运行中，
同样 defer 清理。日志带 debug.Stack()。测试：panicProvider 打崩
Stream → turn 不 wedge、聊天可再起新 turn、Busy 归零；/bg panic → jobs
表清空；panicChan 首连即崩 → superviseChannel 存活重连。全包
-race/-count=2 审计绿 + 本地冒烟 PASS。

## v0.6.41（对齐循环第 61 轮）

自更新绝不降级——apply 原只判 `latest == current`，/releases/latest
指向更旧 tag 时（重发旧 release、本地构建领先于发布）会静默把二进制
滚回旧版。现在按数值点段比较（0.6.10 > 0.6.9，不吃字典序坑），latest
不比当前新即视为已是最新、二进制分毫不动。表驱动 10 例比较器测试 +
降级拒装端到端测试（旧 release 在架、0.6.40 在跑 → UpToDate 且 exe
原样）。

## v0.6.40（对齐循环第 60 轮）

配置键名拼写守护——Go 的 encoding/json 静默丢弃未知键：`apikey` 拼错
apiKey、`lisen` 拼错 listen，功能静默失效且启动无任何报错。chatmux check
现在按 JSON 路径点名未知键并给近似建议（大小写不敏感命中明确提示 JSON
键名区分大小写；编辑距离 ≤2 给 "did you mean"；通道 options 是适配器私有
schema、跳过）。期望键集由反射从真实 Config struct 提取——schema 不可能
与解码器漂移。实测：agnts/lisen/apikey/workdir 四个拼写错误一次全抓。

## v0.6.39（对齐循环第 59 轮）

模型 agent 密钥预警——openai/anthropic agent 既没配 apiKey、对应环境
变量（OPENAI_API_KEY/ANTHROPIC_API_KEY）也没设时，每个 turn 运行时必
401；chatmux check 现在预先警告（建议性不阻启，与 dataDir 探测同理：
check 与服务可能不在同一 shell）。CLI agent 不受影响。实测渲染：
"no apiKey and OPENAI_API_KEY not set — model calls would fail auth at
runtime"。README 部署节补「配置体检」条目（check 子命令此前只活在轮
次记录里）。

## v0.6.38（对齐循环第 58 轮）

admin 路由鉴权守护——TestAdminRoutesRequireToken 从 main.go 源码提取
全部 /admin/* 路由，起真实 server 逐条断言无 token 请求一律 401（GET+
POST × 全路由）。首跑即抓到三个真 bug：/admin/stop、/admin/history、
/admin/send 的 handler 方法检查先于鉴权，未认证调用方拿到 405 而非 401
（路由语义泄漏）；已统一改为先 guard 后 method。测试用临时端口，-count=2
双跑真实执行不跳过。守护测试抓虫第四例（前三：示例配置、发布产物混装、
i18n 裸键名）。

## v0.6.37（对齐循环第 57 轮）

README 命令表防漂移守护——dispatch 命令集（agent.go 的 26 条 case "/x"）
逐个断言出现在 README 聊天内命令表：新增命令忘了补 README 行，从"读者
自己发现"变成测试失败。当前表完整（26/26 通过），守护的是未来。

## v0.6.36（对齐循环第 56 轮）

会话存储字节封顶——此前只有条数上限（40 条/500 会话），单条消息大小无
界：贴一段几 MB 的文档，整个正文原样进内存、每轮 Save 全量重编进
sessions.json。现在单条消息 64KB 封顶（保头保尾加截断标记），单会话历
史 400KB 封顶（约为回放字符预算的 5 倍，超预算先丢最旧对话对、永远保
最新一对）——比回放预算更老的历史本来就是磁盘死重。顺带：全包 -race +
-count=2 审计复检全绿（自 v0.6.24 以来首次全量，覆盖 47-55 轮全部并发
改动）。

## v0.6.35（对齐循环第 55 轮）

chatmux check 二次打磨——(1) 修误报：web 控制台通道启动时无条件挂载，但
校验器只认 channels[]，routes/cron/docwatch 指向 "web" 会被冤报"未知通
道"；(2) 补 dataDir 可写探测：目录建不出来/写不进时 usage.New 静默自禁
用（记账悄悄丢失，启动不报错），check 现在预先探测并给出警告（探测是
建议性的——check 与服务可能跑在不同用户下）。

## v0.6.34（对齐循环第 54 轮）

i18n 完整性收口——补两个守护测试：zh/en 键集合必须完全对齐（缺键会把
中文喂给英文用户）；包内所有 .say("key") 字面量必须真的有对应文案（扫
源码断言）。第二个测试当场抓到一个真 bug：/schedule 未知子命令回的是
裸键名 "sched.usage" 而不是用法说明——已补 zh/en 文案修复。

## v0.6.33（对齐循环第 53 轮补丁二）

发布流水线重构为"矩阵只构建、单 job 一次性发布"——v0.6.32 的清空步骤
放在每个矩阵 job 里，5 个并行 job 互相清掉对方刚传的产物，发布出来只剩
一对（其余下载全是 404）；现在矩阵产物流转走 artifact，release job 统一
算 sha256、清该 tag 旧产物、一次性上传，单一写入者无竞态。v0.6.31 的混
合集（强推 amend 所致）与 v0.6.32 的残缺集都留在原处，均属"校验拒装/
404 拿不到"的安全态，latest 从 v0.6.33 起恢复一致。

## v0.6.32（对齐循环第 53 轮补丁）

v0.6.31 的发布实测暴露两个问题并修复：(1) 我对已发布 tag 强推 amend，
两次 release 运行产出不同字节（buildvcs 嵌入 commit），且 release action
对同名产物跳过上传，留下"新 archive + 旧 sidecar"的混合集——恰好被本
轮新加的校验当场拒绝（updater 拒装 darwin-arm64 产物，功能符合设计，
其余三对产物校验一致）。修复：release 工作流上传前先清空该 tag 已有产
物，任何重跑都是一套全新一致的产物；流程规矩：tag 推送后不再 amend。
v0.6.32 同树重发，latest 指向一致产物集，v0.6.31 的 darwin-arm64 混合集
留在原地（校验会安全拒装，不产生损坏安装）。

## v0.6.31（对齐循环第 53 轮）

自更新下载校验——release 流水线为每个产物附 `<产物>.sha256` 边车文件
（sha256sum 格式，矩阵各发各的不抢写）；`chatmux update` 拉回产物后先比
对 SHA256，不匹配直接拒装（"refusing bad download"，二进制不动）；旧
release 无边车则跳过校验照常升级。防的是下载链路上的截断/篡改落到磁盘
上的可执行文件。三态测试：匹配通过 / 不匹配拒装 / 无边车跳过。

## v0.6.30（对齐循环第 52 轮）

韧性事件上指标——turn 看门狗触发与模型流启动重试此前只有 daemon 日志可
见，/metrics 现在暴露 `chatmux_turn_watchdog_fires_total` 与
`chatmux_stream_retries_total`（internal/metrics 原子计数，进程内全局）；
冒烟脚本死端口轮顺带断言重试计数 ≥1。metrics 包测试 0→1（门禁 24 包）。

## v0.6.29（对齐循环第 51 轮）

README 瘦身——逐轮打磨记录移入 CHANGELOG.md（v0.1 起完整历史，与 tag
一一对应），README 状态只留最近 6 轮 + 指引链接，首页回到"讲清产品"而
不是 245 行开发日志。

## v0.6.28（对齐循环第 50 轮）

冒烟进 CI——`ci.yml` 在 vet+test+build 之后跑 `scripts/smoke.sh`：从"本地
手动验证过一次"变成"每次 push 都端到端回归"，成功轮/失败轮/记账面/优雅
停机任何一环退化都会挡住合并。

## v0.6.27（对齐循环第 49 轮）

`scripts/smoke.sh`——端到端冒烟脚本入库：临时目录起一套独立实例，零凭据
零外网跑通完整对话（generic CLI agent 挂 /bin/echo 验证成功轮 + openai
agent 指向死端口验证失败轮），并核对全部记账面：网页聊天日志、usage.jsonl
错误记录、`chatmux_usage_errors_total` 指标、daemon turn 日志、TERM 优雅
退出；中途顺带跑 `chatmux check`。配套修复：channels[] 为空不再是启动硬
错误（web 控制台通道本就无条件挂载，零 IM 通道 = 纯网关实例，check 降为
提示）。

## v0.6.26（对齐循环第 48 轮）

模型流启动失败自动重试一次——连接抖动、瞬时 5xx 这类 blip 原来直接变成
用户可见的失败；启动阶段还没有任何输出，重试完全无感。2 秒退避可被取消
打断（/stop、看门狗、停机都生效），三处调用点（会话 turn、工作流/动态
派发、编排）统一走 streamWithRetry。中途出错（已产出内容）不重试，避免
重复输出。

## v0.6.25（对齐循环第 47 轮）

`chatmux check`——配置校验子命令：agents/routes/cron/workflows/docwatch/
i18n/http 语义检查 + 各通道真实构造（凭据形状、AES key 长度等启动时才爆
的错提前暴露），docwatch 路径未就绪为警告。顺手修了 example 两处自查发
现：wecom 占位 AES key 长度非法、docwatch 示例路径说明。example 现在
check 全绿。

## v0.6.24（对齐循环第 46 轮）

cli-sessions.json 封顶——CLI 恢复 id 表原来无界增长；现每条带时间戳、
超 2000 条按最陈旧淘汰，旧纯字符串格式自动迁移。顺带复检：全部包 -race
与 -count=2 全绿（自第 34 轮以来又改过多处并发路径）。

## v0.6.23（对齐循环第 45 轮）

Discord 语音转写补齐——附件分类原来靠 URL 后缀猜（大小写敏感、语音一律
算文件），现在优先用 content_type（image/*、audio/*），后缀仅兜底且不区
分大小写；transcribeAudio 支持无下载器通道的公开音频 URL（Discord CDN
语音消息）直接取回送转写。discord 包测试从 0 到 1。

## v0.6.22（对齐循环第 44 轮）

会话表瘦身——单会话历史本有 40 条上限，但会话数量无界：每个后台任务留一
个死 |job 会话常驻内存（每任务一个，永不回收），一次性群聊永久躺在
sessions.json。现在任务完成即重置其会话；Save 时清掉空会话（纯 Get 副
产物）；持久化按 LastUsed 封顶 500 条，最陈旧的先淘汰。

## v0.6.21（对齐循环第 43 轮）

钉钉附件落地——六通道附件覆盖收口。图片/语音/文件消息解析
content.downloadCode，经 app token（复用已有缓存）+ messageFiles/download
两步换取临时 URL 再取字节；需要渠道配 robotCode。至此六通道（飞书/钉钉/
企微/TG/Slack/Discord）图片全覆盖，语音转写覆盖飞书/TG/企微/钉钉。

## v0.6.20（对齐循环第 42 轮）

企微附件落地——file/voice 原来只有 media_id 没有下载器，语音消息被静默
丢弃（转写从未见过它们）、文件无人认领。企微实现 media/get 下载（错误走
HTTP 200+JSON 的坑用 Content-Type 分流），voice 归类改 audio 进转写链
路，image 补 Key 让下载优先于短时效 pic_url。

## v0.6.19（对齐循环第 41 轮）

Slack 图片修复——url_private 需要 bot token 才能取，原来直接把裸 URL 喂
给视觉模型（只能拿到登录页）。Slack 实现 AttachmentDownloader 带 Bearer
下载，attachImages 改为"有下载器的通道连 URL 附件也自己下"（下载失败回
退裸 URL，公开 CDN 行为不变）。slack 包测试从 0 到 1。企微文件/语音
（media_id 无下载器）与钉钉（不支持附件）为已知边界，README 能力表照实
标注。

## v0.6.18（对齐循环第 40 轮）

turn 看门狗——turn 上下文原来无期限，CLI 子进程挂死会永远占住会话（API
路径仅靠 HTTP 超时兜底）。现在每 turn 有预算（agent 级 turnTimeout，默认
30m；后台任务 4×），超时取消并记日志；被打断的 turn（/stop、超时、停机）
原来完全不记账，现在以 cancelled / turn budget exceeded 落 usage——"漏
记账"家族最后一个洞。

## v0.6.17（对齐循环第 39 轮）

优雅关机收尾——SIGTERM 原来只停 HTTP 后直接退出：Runtime.Shutdown()/
usage.Close() 从未被调用，进行中的 turn 被硬杀（记账与最后几条会话更新
丢失）。现在停机时先等在飞 turn 落地（8s 上限），再同步快照会话、关闭
usage 日志。

## v0.6.16（对齐循环第 38 轮）

usage.jsonl 尺寸轮转——原为无限追加，且 /insight/看板每次全量读盘，长跑
实例文件与读放大都无界；现在过 10MB 自动轮转为 usage.jsonl.1（留一份备
份），写入侧计数、失败降级为继续追加。

## v0.6.15（对齐循环第 37 轮）

下载限流从"截断"改"拒绝"——飞书/Telegram 的附件下载原用 LimitReader，
超 20MB 的文件被静默截成 20MB（图片损坏、语音半截，下游报莫名其妙的
错）；现 channel.ReadCapped 读到超限即整体报错（上限常量收敛到
channel.MaxAttachmentBytes 单一来源），下载失败（含超限）也记日志而非静
默跳过。channel 包测试从 0 到 1。

## v0.6.14（对齐循环第 36 轮）

附件大小上限——图片/语音下载原无上限，超大文件会被整个读进内存、落盘并
送视觉/转写 API；现在单附件 20MB 封顶，超限跳过并记日志。顺带排查确认
bg 任务表完成后即删、去重缓存有界 TTL、web 通道日志 500 条封顶——无其
他泄漏点。

## v0.6.13（对齐循环第 35 轮）

ask 超时清障——ask 的 Wait 超时/取消只返回错误不清理 Broker 表，僵尸问
题永久留在待答列表（/admin/ask 与看板）；现在放弃即移除（身份校验防误删
后继问题），Resolve 后的迟到放弃是空操作。带 race 检测的双测试覆盖。

## v0.6.12（对齐循环第 33 轮）

看板用量表补"均耗时"列（⏱ 三档格式，与 /insight 对齐）——平均时延至此
三个表面齐全：usage.jsonl（原始）→ /insight（文本）→ 看板（表格）。
smoke 实测 avgMs 聚合与列渲染正确。

## v0.6.11（对齐循环第 32 轮）

/insight 平均时延——每条 usage 记录早已存 DurationMs 但从未展示；Agg 增
加 avgMs（均值），/insight 行追加 ⏱340ms/⏱1.5s/⏱2m5s 三档格式。顺带排
查确认会话持久化每轮落盘（finish → persistSessions），崩溃安全无缺口。

## v0.6.10（对齐循环第 31 轮）

配置文档补全——config 结构体字段与 chatmux.json.example 程序化比对，补
三个未文档化字段：路由 userId（按用户过滤唤起 agent）、cron/docwatch 的
threadId（话题群定向）；三者均已逐一验证在代码里真实消费（非死字段）。

## v0.6.9（对齐循环第 30 轮）

记账闭环——runOnce（工作流步骤/动态派发/bg 任务共用）此前完全不记
usage，/orchestrate 的 callAgentText 错误口也漏；现在所有模型/CLI 调用点
（runTurn 三口、runCliTurn 三口、runOnce 两分支、callAgentText 全口）都
落账含 Error 字段，TestRunOnceAccounting 三态覆盖。

## v0.6.8（对齐循环第 29 轮）

CLI 轮次记账对齐——runCliTurn 的启动失败路径此前不记 usage、流内出错不
进 Error 字段、无 turn 日志（API 路径第 22-23 轮修过的问题在 CLI 路径的
翻版）；三出口全部补齐，并引入 cliRunner 接缝使 CLI 轮次可测
（TestCliTurnAccounting 三态）。

## v0.6.7（对齐循环第 28 轮）

turn 级运行日志——daemon.log 此前只有频道事件，模型轮次完全不可见；现在
每轮（成功/两种失败）各记一行
`turn status= agent= model= chat= dur_ms= in_tok= out_tok= out_chars= err=`，
运维不开看板也能跟流量。TestTurnLogged 捕获日志断言。

## v0.6.6（对齐循环第 27 轮）

会话历史字符预算——此前只按条数（40 条）截断，贴超长文档时几轮就能撑爆
模型上下文；现在回放历史前先做 8 万字符预算裁剪（从最旧对话对开始丢，
永远保留最近一对），TestTrimToBudget 覆盖三态。

## v0.6.5（对齐循环第 26 轮）

排队提示降噪——turn 进行中连发消息，此前每条都回一条"已排队"，长回复期
间被刷屏；改为每 turn 只提示一次，后续消息静默入队（turn 结束后仍合并成
一条派发）。附 TestQueuedNudgeOnce 双断言（恰好 1 次 ack + 排队消息全部
转入下轮）。

## v0.6.4（对齐循环第 25 轮）

频道重启退避——凭据无效等崩溃场景此前固定 5s 无限热重启（12 次/分钟打
上游 API + 刷日志）；改为指数退避 5s→10s→…→5min 封顶，连续健康运行
≥10min 归零重来；日志附本次运行时长。

## v0.6.3（对齐循环第 24 轮）

命令面终校——/help 实际输出、agent.go dispatch 命令集、README 命令表三
方核对（26 条 dispatch 全部有文档；README 中的 /admin/*、/app 均为
HTTP/容器路径，非聊天命令）。补两处：/help 增加动态派发 /<agent名> 行
（zh/en，此前聊天内无法发现该能力），README 表补 /help 行。

## v0.6.2（对齐循环第 23 轮）

失败调用记账补全——第 22 轮只覆盖了"流已建立后出错"（model.err）一条路
径，"Stream 直接返回错误"（model.fail，如密钥无效的 401）仍漏记；两路
径现已都落账，并有双路径单测覆盖。看板用量表加"失败"列，/metrics 新增
chatmux_usage_errors_total。

## v0.6.1（对齐循环第 22 轮）

失败调用记账——API 流出错（如 401）此前在 usage 落账前 return，/insight
与看板会漏掉失败；现在失败也记（Record.Error，Agg.Errors 计数，/insight
行显示"N 失败"）。CLI 路径失败本就落账。

## v0.6.0（对齐循环第 19-20 轮，收尾轮）

与 botmux 命令表全量对照——49 条命令逐条映射（对齐 / 对应实现 / 架构不
适用三类）；补 /dash（看板地址）与 /insight [天数]（按 agent 的近 N 天
调用量/tokens/反馈聚合，usage.SummarizeSince 时间窗）；其余 botmux 专属
命令（adopt/lane/term/queue/fork/repo wt 等）绑定其"每话题一个 PTY CLI
进程"架构，chatmux 的网关+会话轮次模型以 /new//agents/后台队列/工作流
覆盖对应场景，如实声明不逐条复刻。

## v0.5.8（对齐循环第 18 轮）

会议代理（飞书 VC）——订阅 vc.bot.meeting_* 与 vc.meeting.recording_ready_v1
事件，会议被邀/开始/结束/录制就绪映射为 meeting_* 合成事件，按
meeting.channel+chatId 配置转发通知到指定聊天并记 usage。如实边界：不含
自动入会、音频拉流与转写（需真机 SDK 与凭据），录制就绪只通知链接；被
邀入会明确提示"仅通知"。

## v0.5.7（对齐循环第 17 轮）

话题群进阶盘点收口（飞书/Slack/Discord 线程锚点与落回均已就位，v0.5.6
补齐 Telegram 论坛话题，套件对齐完成）+ Electron 桌面端壳（desktop/：
看板窗口 + 系统通知 + Dock 角标 + 托盘 + 单实例；语法/JSON 已校验，GUI
未在无头环境验证）。

## v0.5.6（对齐循环第 16 轮）

Telegram 话题群——论坛超群 message_thread_id 作为话题会话键（优先于回复
派生的 ThreadID），回复/文件自动带回 message_thread_id 落回原话题；
channel.Message 新增 ThreadIsTopic 标记。

## v0.5.5（对齐循环第 15 轮）

企微群事件——change_external_chat / chat.update 回调映射（create→建群、
dismiss→解散、update+add/del_member→成员进出），系统事件绕过用户白名
单，全部记 usage。企微自建应用没有"机器人进群"回调，平台能给的只有客户
群生命周期，故此通道不加入欢迎语逻辑（如实按平台能力对齐）。

## v0.5.4（对齐循环第 14 轮）

钉钉群事件对齐——Stream Mode 增订群会话事件（chat_add/remove_member、
chat_disband），机器人进出群按 robotCode / botUserId 严格匹配（配不上则
按普通成员事件记 usage），进群欢迎走 /v1.0/robot/groupMessages/send 主动
消息（access_token 缓存至过期前 5 分钟）；未配 robotCode 时欢迎明确报错
而不是静默失败。

## v0.5.3（对齐循环第 13 轮）

技能包升级——支持 Claude 式 `<name>/SKILL.md` 目录技能（目录内可放辅助
文件），frontmatter `description:` 自动进 /skills 列表（无描述截取正文首
行），注入正文剥离 frontmatter；平铺 `*.md` 兼容不变。

## v0.5.2（对齐循环第 12 轮）

Telegram 群事件对齐——getUpdates 增订 my_chat_member，进群（member/
administrator/restricted）发欢迎语，出群（kicked/left）静默记录，复用
v0.5.1 的 Event 机制。

## v0.5.1（对齐循环第 11 轮）

群套件起步——飞书机器人进群事件（im.chat.member.bot.added）自动欢迎语
（groupWelcome 可覆盖），移出群记 usage 事件；channel.Message 新增
Event 字段。

## v0.5.0（对齐循环第 10 轮）

oncall 值班——配置 members + daily/weekly 轮换 + anchor 起算，/oncall 与
/oncall next 查询，每天 00:05 自动检查换班并向配置聊天发 🔔 交接通知。

## v0.4.9（对齐循环第 9 轮）

fleet 多实例管理——~/.chatmux/fleet.json（或 -fleet / CHATMUX_FLEET）
列出多网关，chatmux fleet list 并行探测健康（版本/通道/agents，status
新增 version 字段）、status / send / ask 按实例代理。

## v0.4.8（对齐循环第 8 轮）

/orchestrate 并行编排——多 agent 扇出同一任务（无状态调用，完成即回帖
【agent】），可选 `+ 汇总agent` 综合各方答案；usage 记 mode=orchestrate。

## v0.4.7（对齐循环第 7 轮）

文档监视 docwatch——本地文件变更检测（LCS 行 diff + 摘录上限），变更即以
{{diff}} 触发 agent 提示词，首见只记基线。

## v0.4.6（对齐循环第 6 轮）

网页会话终端——看板内置聊天面板（web 通道：POST /admin/chat 进、GET
/admin/chat?since= 增量轮询出、Replace 原地更新流式回复）；首版 GitHub
Release v0.4.5 已由 tag 流水线发布，chatmux update 实测可用。

## v0.4.5（对齐循环第 5 轮）

CLI 适配器扩展（qwen / opencode / aider 预设，argv 提示词 + 纯文本输出）
+ 自更新（`chatmux version` / `chatmux update`，从 GitHub Releases 拉对
应平台产物并原子替换，internal/version 单一版本源）。

## v0.4.4（对齐循环第 4 轮）

MCP 服务——`chatmux mcp` 以 stdio MCP server 把 send/ask/status/history
暴露给 Claude Code 等 MCP 客户端（botmux 供宿主 agent 调用的对应面）；
新增 GET /admin/history。

## v0.4.3（对齐循环第 3 轮）

动态派发 `/<agent名> <任务>`（任意已配置 agent 直接接活，无需预定义命
令）、会话态周期任务 /schedule（中文时间短语 + 原生 cron，持久化恢复）、
看板会话板与 ask 待答表、ask GET 鉴权修复。

## v0.4.2

阻塞问答 ask（`chatmux ask` + ❓ 表单卡片 + 文本回答回退，每聊天一问、
后问覆盖前问）。

## v0.4.1（对照 botmux 盘点后的补差轮）

会话命名 /name、历史查看 /history、上下文回退 /rewind、重试 /retry、一
次性提醒 /remind（相对/绝对时间）、发文件到聊天（feishu/TG/Discord/
Slack，图片内联，`/admin/send file` 与 `chatmux send --file`）、agent 切
换 /agents · /agent（会话级 handoff，优先于路由，持久化）。

## v0.4

多平台 👍/👎 按钮（飞书/TG/Discord/Slack）+ 飞书卡片表单（forms[] +
/form）+ CLI 沙箱（docker 隔离）+ 会话持久化（重启不丢）+ i18n（zh/en）
+ Prometheus /metrics + 多实例通道（同名平台多账号，去重按实例隔离）+
部署化（Dockerfile / CI / tag 发布 / systemd / launchd）。

## v0.3

后台长任务（/bg /jobs /stop 扩展）+ 引用上下文（飞书/TG/Discord/Slack）
+ 语音转写（whisper 兼容）+ 网页看板 + 技能（skillsDir）+ 工作流
（{{prev}}/{{input}} 链）+ gemini CLI 预设。

## v0.2

CLI agent（claude/codex/gemini/generic，锚点记 resume id）；slash 命令；
群 @ 门控；cron 5 字段；usage JSONL；admin 端点+客户端 status/stop/send；
SetName 多实例。

## v0.1（2026-11-18）

首个可用版：六通道（飞书 WS/webhook、钉钉 Stream SDK、企微回调 AES 手
写、TG 长轮询、Slack SocketMode、Discord Gateway）+ OpenAI 兼容/
Anthropic 流式 provider + 会话/去重/排队运行时。
