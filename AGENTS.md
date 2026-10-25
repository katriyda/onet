# AGENTS.md

写给在 `onet` 仓库工作的 AI agent（也适用于新贡献者）。中文交流与文档。

本文档只写**代码里读不出来的东西**：意图、约定、踩过的坑、安全边界。架构、协议字段、命令细节自己 `grep` / 读源码 / `git log` 确认——与代码冲突时以代码为准，并顺手把文档改对。只有「安全」一节是硬线。

## 项目

移动端优先的 Web 终端，tmux 风格会话管理：多会话、关页不杀进程、断线重连。单文件 Go 二进制，前端 `go:embed` 内嵌，部署零依赖。移动端 = termux 风格，桌面端 = 经典终端，按输入设备自适应，`ui` 配置可强制。

## 构建

`go vet ./... && go build -o onet.exe .`；`./onet` 起在 `127.0.0.1:7681`。

两个容易踩的点：无前端构建步骤（改 `web/` 后重新 `go build` 即打包）；`web/vendor/` 必须存在（`go:embed` 硬要求，删了直接编译失败）。

## 前端坑（移动端）

每条都是踩过的坑，改之前确认没踩回去：

- 控件绑定用 `onTap()` 而非 `click`（click 抢 xterm 焦点 → 移动端键盘收起）；onTap 内部的 `detail===0` click 监听负责桌面键盘激活，别删
- 长按/行选中用 `onPress()`；长按终端 = 选中整行，终端菜单在右上角 `⋯`，别再把长按改回弹菜单
- **触屏手势必须挂在 `#touch-layer` 稳定层上**：xterm 6 的 DOM renderer 滚动时会 `replaceChildren` 替换行节点，直接监听 `.xterm`/行节点会在第一次 `touchmove` 后丢事件。不要给 `#touch-layer` 加 `pointer-events:none`
- 普通缓冲区自己改 `viewport.scrollTop`（像素级 + 惯性）；**alternate screen / 鼠标上报时必须把拖拽翻译成 `WheelEvent`**，交给 xterm 转成 ↑/↓ 或 SGR wheel，否则 omp/less/vim 里拖不动
- 服务端 attach 时**先发 `attached`（带 seq/resumed/truncated/from_seq），再发回放 `output`**；客户端用 `hello.capabilities.attachOrder === "attached-first"` 判定，新协议按 `since_seq` 续传，旧服务端则把先到的 output 缓存进 `pendingAttach.early` 等 attached 后补写。重连前不再无条件 `term.reset()`：只有 `resumed=false` 或 `truncated=true` 才清屏，短断线保留滚动位置。改协议时同步维护 capability，否则回滚会重复写屏
- `CHROME_SEL` 内的控件在捕获阶段 `pointerdown`+`mousedown` 阻止默认（iOS 必须 mousedown，WebKit bug 322721）以保持软键盘；新增"键盘弹起时会被点"的控件（菜单、选中条、滚动浮标、手势层）时加进去
- 阻止默认后 `:active` 不可靠 → 反馈用 `.pressed` class
- `modState` 键名小写；`dataset.key` 映射前先 `toLowerCase()`（历史 bug）
- 布局跟 `visualViewport`，键盘弹起时重排并 resize；固定层（抽屉/底部菜单）用 `--vv-top`/`--vvh`，否则会被软键盘盖住
- **移动端视口高度变化必须走冻结/提交路径**（`syncViewport`/`commitViewport`）：键盘动画期间 `#term` 用内联高度冻结行数、`translate3d` 让底边贴住可视区；高度稳定 `VP_SETTLE_MS` 后才 `fit + resize`。别改回每帧 `fit.fit()`——那会逐行 reflow 并把中间 cols/rows 发给 ConPTY，观感是“卡一下跳一行”；`fitTerm`/`sendResize` 在冻结期间的早退是故意的
- **底部意图要在浏览器动手之前记**：浏览器在 focus/键盘弹起阶段会先把 xterm viewport 推上去 1–2 行，等到提交时再判 `atBottom` 就会把“本来在底部”误判成“用户滚上去了”。`onTouchStart`/textarea focus 记 `bottomIntentAt`，`commitViewport` 用它决定 `vpForceBottom`；延后锚定 `settleAnchorBottom`（1+2 帧 + 60/140/300ms，靠 `anchorSeq` 作废）不能删——fit 后 viewport/scroll-area 是延后同步的，同帧 `scrollToBottom` 会差 1–2 行。用户一拖动（`onTouchMove`/`startMomentum`）就作废，别改成无条件拉回
- **TUI 不能平移**：`vpPan` 必须以 `!isAltBuffer() && !tuiPaints()` 为前提。alt 缓冲（less/vim）没有回滚区（`scrollHeight == clientHeight`），平移量提交后无法被 `scrollToBottom()` 补偿；内联 TUI（omp/htop，**普通缓冲**但持续用 `ESC[?25l` / 绝对定位 `ESC[r;cH` / `ESC[?2026h` 重绘自己那一屏）在平移后会被自己的重绘画回去——两者观感都是“内容先滑上去、再被拉回”的滚动两下。判定靠 `noteTermBytes()` 扫最近 1.5s 的输出（`writeOutput()` 是唯一写入口，别绕过它直接 `term.write`），只冻结行数、提交后让应用重排一次；普通逐行输出（shell/日志/编译）不会命中，平移保持原样，别一起删掉
- **`#app` 是固定壳**：`position:fixed` + JS 同时写 `top`（`vv.offsetTop`，coarse pointer 才跟）和 `height`（`vv.height`），`--vv-top` 用同一个 clamp 后的值；页面自身必须 `scrollY === 0`（`resetPageScroll` 兜底）。别改回只跟 `vv.height`——visual viewport 被平移时整个 app 会落到可见区外。chrome（`#header`/`#select-bar`/`#scroll-pill`/`#placeholder`）用 `touch-action` 隔离，抽屉/菜单/日志体保留 `pan-y`
- 桌面模式（鼠标窗口拖拽）不走冻结路径，保持逐帧 fit；判断依据是 `(pointer: coarse)`（触屏），别改成按布局模式或删掉——窄窗口 + 鼠标会因此丢掉实时重排
- `/static/` 有 24h 缓存：改 `web/` 资源后递增 `index.html` 的 `?v=N`，否则真机/浏览器会继续跑旧 JS
- 会话恢复顺序：URL `?s=` > `localStorage.onet_last_session` > `sessionStorage` 兜底。只在 `attached` 后写记录；Android WebView 刷新经常回退到 `/`（丢掉 `?s=`）且 `sessionStorage` 不跨实例，改用 localStorage 才能留在原会话，别改回只用 sessionStorage
- 新增操作一律走统一的 `showMenu()`：移动端底部操作表、桌面锚定浮层，同一套图标/文案/危险色，别再造第二套菜单

## 热更新（自更新）

网页 `⋯` → 检查更新走 `GET /update`；安装走 `POST /update`（必须带 `X-Onet-Update: 1`）。安装前会运行新二进制的 `version` 并比对 Release tag，避免把坏文件换上去。真正切换进程靠 `handoff_*.go`，不是原地热加载 Go 代码。

- 交接状态是临时 JSON（0600），路径经 `ONET_HANDOFF_STATE` 传给新进程；会话快照含 PTY 句柄、回放缓冲、PID、启动时间。状态版本变化要同步改 `handoffStateVersion`
- `Session.handoff` 为 true 时 `finishRun` 只停读，不 `Close` PTY、不标记 dead；旧进程退出前 `live` 仍保留。别在交接路径调用 `Close()`，否则 shell 被杀
- Unix：`dup` PTY fd 并清 CLOEXEC → 停止旧读循环 → 写状态 → `exec`。exec 保留 PID，shell 仍是子进程，新进程用 `Wait4` 接管；`exec` 失败必须把 dup fd 接回旧 `unixPty` 并重启读循环
- Windows：`HPCON` 是 `PseudoConsole*`，不是内核句柄，**不能 `DuplicateHandle`**。用 `ReadProcessMemory` 读内部 `hSignal`/`hPtyReference`/`hConPtyProcess`，逐个复制到子进程，再在子进程重建一个 `pseudoConsole` 结构作为 HPCON。`ClosePseudoConsole` 会 `HeapFree(HPCON)`，对 Go 分配的结构只能手动关三个句柄
- Windows 监听 socket 与 IOCP 绑定，跨进程复制不可靠：父进程写最终状态后关闭 listener，子进程自己 `net.Listen`；失败时必须恢复旧 pump 并重新 `net.Listen`。握手是 stdin/stdout 的 `LOAD`/`READY`/`GO`/`SERVING`
- 旧进程确认 `SERVING` 后才能断 WS 并退出；父进程正常退出路径**不能** `KillAll`。`main` 的 `handoffDone` 分支就是为此存在
- Windows 新进程必须 `CREATE_BREAKAWAY_FROM_JOB | DETACHED_PROCESS`：否则 supervisor/job object 杀旧进程时会把新进程一起带走。受限 job 会拒绝 breakaway，`cmd.Start` 失败后按回滚路径继续用旧进程服务（日志 `app.handoff_spawn_failed` / `app.handoff_failed`）
- `GET /healthz` 只对回环地址返回 `{status,version,resumed}`，供更新器确认新进程已监听；非回环 404，不暴露 token/会话
- 更新失败发生在停读之后时，Windows 用 `conptyConn.RestartRead()`、Unix 用 dup fd 恢复；这是回滚可用性的关键，别删

## 验证（完成定义）

`go vet` + `go build` 通过是底线。行为改动**起服务端到端验证**，不能只看编译：

- 移动端：Chromium 触屏仿真（390×844，hasTouch/isMobile）；触摸类改动确认 `activeElement` 仍为 `.xterm-helper-textarea`、字节真实送进 shell
- 协议：WS 冒烟（list → create → attach → 输入 → 断言回显 → 重连断言重放）
- 手段自选，目的是证明行为对；可复用的沉淀成脚本
- 真机行为（IME、contextmenu 合成）本地测不到，交付时注明需真机复验
- 交付附实际运行结果，不只写"已验证"

## 提交与发布

- 提交信息：英文祈使句单行；依赖改动加 `deps: ` 前缀
- 流程：改 → 验证 → commit → push main → CI 绿
- 发版：轻量 tag `vX.Y.Z` → push，CI 交叉编译多平台自动建 Release
- 不提交：`onet`/`*.exe`、`config.json`、`tmp/`

## 依赖

保持最小依赖（当前 3 个直接依赖）——这是取向，不是巧合。新增前评估 GitHub 活跃度与 OSV / `govulncheck`；单人维护的新仓库、无更新记录的小库不适合当关键依赖。工具链随 mise 升级，升级后跑 `govulncheck`。

## 安全（硬线）

首次启动自动生成 `config.json`：默认 `127.0.0.1` + 随机 256 位 token，文件权限 0600；已有配置保持原值（空 token + 回环仍兼容）。`token` 非空后 `/config`、`/logs`、`/update`、`/ws` 强制认证；`/` 与 `/static/` 故意公开（登录页自举）；`GET /config` 不回传 token（只给 `token_set`）。`PUT /config` 原子保存并可交接重启，必须带 `X-Onet-Config: 1`；`POST /update` 会替换当前二进制并切换进程，必须带 `X-Onet-Update: 1`。两个自定义头只防 CSRF，不是额外权限。改监听地址时 `handoffState.Rebind=true`，新进程重新 bind，不能复用旧 socket。传输明文 HTTP，TLS 交给反代/Tailscale。会话内是完整 shell——放宽监听/认证前必须先向用户说明风险并获确认。

## 协作

- 较大改动先出计划、确认后再动手；与计划有偏离时在交付说明中列出
- 不确定的边界先小范围探测或直接问，别闷头猜完再返工
- 交付附验证证据；本地测不到的明确标注