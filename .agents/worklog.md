## 2026-10-19 13:00 — 移动端与 agent 交互体验重构
- session: s-20261019-onetux
- agent: omp (deepseek-v4.1-flash-fast)
- 目录: C:\Users\katri\code\onet
- 目标: 重新审视移动端交互，重点优化 agent（omp）操作与上下文查看体验，并统一移动/桌面菜单
- 做了什么:
  - 定位 xterm DOM renderer 滚动时替换行节点导致 touchmove 丢失的问题，新增稳定 `#touch-layer`，实现像素级拖拽 + 惯性滚动
  - 普通缓冲区改 `scrollTop`；alternate screen / 鼠标上报时把拖拽翻译成 `WheelEvent`，omp/less/vim 可滚动（用 less --mouse 验证 SGR wheel）
  - 长按终端改为整行选中，可拖动扩展/边缘自动滚动，顶部提供复制/全选/取消；复制走剪贴板
  - 新增「回到底部」浮标与流式「新输出」提示；修复 attach 重放早于 attached 导致首屏/切会话/刷新丢输出
  - 统一菜单组件：移动端底部操作表、桌面锚定浮层/居中确认，同一套图标、文案、危险色
  - 会话抽屉补齐行尾操作、长按菜单、左缘滑入/左滑关闭；修复原先左缘手势条件写反
  - 按键栏默认改为 agent 常用 12 键（^C/Enter/PgUp/PgDn），支持特殊键别名、方向键长按连发、Ctrl/Alt+方向键编码
  - 按键栏/退出横幅切换时重新 fit PTY；字号持久化；桌面按键栏紧凑居中；操作后焦点回终端
  - 更新 README 与 AGENTS.md 踩坑记录
- 产出: web/app.js、web/style.css、web/index.html、config.go、config.example.json、README.md、AGENTS.md
- 结果: 完成。已用移动端触屏仿真验证滚动惯性、选中复制、TUI 滚轮、菜单位置、抽屉手势、重放/切换、PTY 重排；真机 IME 与软键盘遮挡仍需复验
- meta: files=web/app.js,web/style.css,web/index.html,config.go,config.example.json,README.md,AGENTS.md,.agents/worklog.md

## 2026-10-19 23:02 — 全链路日志（服务端 JSONL + 浏览器回传 + 网页查看/下载）
- session: s-20261019-logs
- agent: omp (deepseek-v4.1-flash-fast)
- 目录: C:\Users\katri\code\onet
- 目标: 服务端关键事件落 JSONL（大小轮转）、浏览器事件/异常经已认证 WS 回传、网页新增日志面板可查看并下载合并日志
- 做了什么:
  - 新增 `logging.go`：`setupLogging`（文件+stderr 双写、打开失败回退、显式路径失败仅 stderr）、`rotatingWriter`（path→path.1..N 轮转/截断）、`tailFile`、`redactedQuery`、`logParts`/`streamLogFiles`、`handleClientLogs`
  - config 新增 `log_file`/`log_level`/`log_max_size_mb`/`log_max_backups`/`log_terminal_io`，校验非法值；相对 `log_file` 按配置文件目录解析；默认 `<二进制目录>/onet.log`
  - server：`withAccessLog`（statusRecorder + Unwrap 保住 WS Hijacker）、`/logs?tail=N|download=1`、WS `hello` 帧、命令/错误/认证/登录事件、`cmdErr` 统一错误日志；PTY 输入输出仅在 `log_terminal_io` 打开时记录
  - session/pty：创建/attach/detach/exit/restart/kill/remove/panic（run 循环加 recover + finishRun）、ConPTY 重试与 3s 兜底日志
  - 前端：客户端 logger（console.warn/error 包装、error/unhandledrejection/network/visibility）、批量回传（30 条/60KB）、sessionStorage 续传、各操作插桩；新增日志面板（客户端/服务端 tab、刷新、复制、下载合并 .jsonl），菜单「日志」入口，资源版本 `style.css?v=7`、`app.js?v=13`
  - 单测 `logging_test.go`（轮转/剪枝、tail 末行、截断首行、token 脱敏、级别解析）
- 产出: logging.go、logging_test.go、config.go、main.go、server.go、session.go、pty_unix.go、pty_windows.go、web/app.js、web/index.html、web/style.css、config.example.json、README.md、.gitignore
- 结果: 完成。go vet/test/build 通过；冒烟证明认证 401/200、WS hello→create→attach→echo→log→detach→remove、日志文件 11 类事件齐全、token 不落盘、1MB×3 轮转、info 级别过滤 debug；Chromium 验证面板开关/Esc/遮罩、客户端与服务端内容、下载文本同时含客户端与 `"msg":"ws.open"`、390×844 移动端全屏面板；真机 IME 与复制权限需复验
- meta: files=logging.go,logging_test.go,config.go,main.go,server.go,session.go,web/app.js,web/index.html,web/style.css,README.md,config.example.json,.gitignore

## 2026-10-25 00:45 — 网页一键热更新（保留运行中会话）
- session: s-20261025-hotupdate
- agent: omp (deepseek-v4.1-flash-fast)
- 目录: C:\Users\katri\code\onet
- 目标: 软件运行中从网页点击更新，下载新版本后不丢正在运行的 shell，自动切换到新版本
- 做了什么:
  - 新增 `GET/POST /update`：检查最新 Release、下载/解压/运行 `version` 预检、原子替换二进制；POST 要求 `X-Onet-Update: 1` 防 CSRF，更新期间冻结会话增删/重启
  - 新增 handoff 状态文件（0600，`ONET_HANDOFF_STATE`）承载会话快照、PTY 句柄、回放缓冲、PID/启动时间；`/config` 与 WS `hello` 增加版本字段
  - Unix：dup PTY/listener fd 并清 CLOEXEC，停旧读循环后 `exec`；新进程 Wait4 原 shell；exec 失败用 dup fd 恢复
  - Windows：新进程 + `LOAD/READY/GO/SERVING` 握手；ConPTY HPCON 不是内核句柄，用 `ReadProcessMemory` 读 `hSignal`/`hPtyReference`/`hConPtyProcess` 后逐个复制；子进程重建 PseudoConsole ABI；监听 socket 由子进程在父进程关闭后重新 bind，失败恢复 pump 并 relisten
  - 前端终端菜单新增「检查更新」，显示版本/确认对话框/下载与重启 toast；重连后按 hello 版本提示“已更新到 vX”；新增 notice 帧显示更新失败
  - 文档补充 CLI 与网页更新的差异、认证/CSRF 边界和平台实现坑；新增 handoff 状态往返、非法版本、env 去重、HPCON ABI 读取单测
- 产出: handoff.go、handoff_unix.go、handoff_windows.go、handoff_test.go、handoff_windows_test.go、update_web.go、main.go、server.go、session.go、pty_unix.go、pty_windows.go、update.go、web/app.js、web/index.html、README.md、AGENTS.md
- 结果: 完成。Windows 实测 dev→0.0.4→0.0.5 连续两次热更新，会话 PID 31660 与 shell 环境变量保持；故意用启动失败的新版本验证回滚：旧进程恢复读循环/监听并继续 0.0.5；Chromium 桌面/390×844 触屏仿真完成 UI 检查与更新 dev→0.0.3，会话保留；Linux 容器实测 dev→0.0.2，PID 20 与环境变量保留。go vet/test/build 与 linux/darwin/freebsd/windows arm64 交叉编译通过；macOS/FreeBSD 仅编译未运行，真机 IME 未复验
- meta: files=handoff.go,handoff_unix.go,handoff_windows.go,handoff_test.go,handoff_windows_test.go,update_web.go,main.go,server.go,session.go,pty_unix.go,pty_windows.go,update.go,web/app.js,web/index.html,README.md,AGENTS.md

## 2026-10-25 05:35 — 会话续传、更新可靠性与 UI 一致性
- session: s-20261025-reliability
- agent: omp (deepseek-v4.1-flash-fast)
- 目录: C:\Users\katri\code\onet
- 目标: 修复断线重连回到错误会话/历史截断、慢客户端静默丢输出、更新切换不可预期，以及删除/异步/粘贴/焦点/toast 等 UI 不一致
- 做了什么:
  - `session.go`：64KB 字节环形缓冲改为带 seq 的帧 ring（64KB–1MB，按 scrollback×64 clamp）；PTY 读取按 16KB/6ms 聚合；attach 改为 attached-first + `since_seq` 续传，truncated 后补 notice；慢客户端改为单 writer + 字节/帧上限，1013/强制断开，不再 `ws.send_drop`
  - 服务端：`create/attach` 接受 cols/rows；`createdAt` 稳定排序；handoff 持久化 seq/ring/createdAt；`/healthz`（仅回环）、`/update` updating 状态、Windows breakaway/detached 与失败回滚；config 非回环空 token 默认拒绝，新增 `allow_insecure`
  - 前端：重连续传/early 缓冲/缺口重同步、`?s=` 与 sessionStorage 记住会话、心跳与抖动退避；boot 重试与 `#boot-error`；更新状态机 + `#update-banner`；删除统一确认、create/remove/restart 防重入与超时、退出文案统一、HTTP 手动粘贴面板、菜单/抽屉/日志焦点管理与 Tab 循环、toast 分级与错误优先
  - 文档：README 配置/安全、AGENTS 的 attach 顺序与 handoff/healthz 约定；新增 `session_replay_test.go`
- 产出: session.go、server.go、config.go、main.go、handoff.go、handoff_unix.go、handoff_windows.go、update_web.go、session_replay_test.go、web/app.js、web/index.html、web/style.css、README.md、AGENTS.md、config.example.json；部署 config.json 已加 `allow_insecure:true` 并关闭 `log_terminal_io`
- 结果: 完成。`go test -count=1 ./...`、`go vet ./...`、`go build` 通过；Node WS 实测 5000 行断线重连 `resumed=true`、首帧 lastSeq+1、无重无缺；>320KB 后重连 `truncated=true` + notice；慢客户端 16s 触发 `ws.slow_client`、无 send_drop、重连可续传；本地 release 服务驱动 Windows 热更新成功（200/409 并发保护、0.2.0、resumed、PID/回放/滚动保留、`app.handoff_done`）；Chromium 验证 boot 重试/失败重试页、粘贴面板插入、菜单/抽屉/日志焦点与 Tab、删除确认文案、退出 code 0 文案、toast error 优先。限制：慢客户端 1013 帧因对端不读无法观察，按日志+强制断开验证；CDP offline 未真正断开 WS，心跳超时未端到端复验；handoff 失败回滚路径未构造触发器
- meta: commit=e03abd4 tag=v0.3.0 files=session.go,server.go,config.go,main.go,handoff.go,handoff_unix.go,handoff_windows.go,update_web.go,session_replay_test.go,web/app.js,web/index.html,web/style.css,README.md,AGENTS.md,config.example.json

## 2026-10-25 14:17 — 刷新后保持当前会话（localStorage 恢复）
- session: s-20261025-refresh
- agent: omp (deepseek-v4.1-flash-fast)
- 目录: C:\Users\katri\code\onet
- 目标: 网页刷新后仍停留在当前查看的会话，不再跳到最近创建的「最后一个会话」
- 做了什么:
  - 从线上日志定位根因：用户的 Android WebView（UA 含 `; wv`）多次 `client.boot href=/`，刷新会丢掉 URL `?s=`；旧实现只写 `sessionStorage`，WebView 重启/新实例后无记录，`pickTarget()` 回退到 `createdAt` 最大的会话
  - `web/app.js` 新增 `readStoredSession()`/`writeStoredSession()`：会话记录主存 `localStorage`（跨刷新、新标签页、WebView 重启），`sessionStorage` 仅隐私模式兜底；URL `?s=` 仍为最高优先级
  - `rememberSession()` 在 `attached` 后同时写 localStorage + sessionStorage + URL；空会话列表时两处一起清；`pickTarget()` 统一走 `readStoredSession()`
  - `web/index.html` 资源版本 `app.js?v=18` → `v=19`；README 与 AGENTS 更新会话恢复顺序与 WebView 坑
  - 隔离服务（127.0.0.1:7699，desktop/mobile 配置）Chromium 验证：新建 sess-1/sess-2 后切回 sess-1，清空 sessionStorage 并整页导航到 `/` 仍恢复 sess-1；新标签页打开 `/` 恢复 sess-1；`?s=sess-2` 深链覆盖存储并回写；存储指向不存在会话时回退最近会话并 toast
  - `go vet ./...`、`go build`、`go test ./...` 通过；CI 绿；tag v0.3.1 发布 8 平台资产
  - 通过 `POST /update` 将部署实例 `C:\Users\katri\A\onet-windows-amd64` 从 0.3.0 热更新到 0.3.1：`handoff.restored sessions=2`，`/config` 返回 `version=0.3.1`，`app.js?v=19` 已生效
- 产出: web/app.js、web/index.html、README.md、AGENTS.md；提交 a4863fe；tag v0.3.1（Release 8 资产）；线上实例已更新
- 结果: 完成。旧 WebView 页面需刷新一次加载 v19（首刷可能仍读旧 sessionStorage/URL，attach 后即写入 localStorage，此后刷新稳定留在当前会话）
- meta: commit=a4863fe tag=v0.3.1 files=web/app.js,web/index.html,README.md,AGENTS.md,.agents/worklog.md

## 2026-10-25 14:44 — 官网 docs/site 与 GitHub Pages
- session: s-20261025-docssite
- agent: omp (deepseek-v4.1-flash-fast)
- 目录: C:\Users\katri\code\onet
- 目标: 为 onet 建一个终端风格的英文官网，放在仓库 `docs/site`，发布到 GitHub Pages，并提供带 SHA256 校验的 Linux/macOS/FreeBSD 与 Windows 一键安装脚本
- 做了什么:
  - 新增 `docs/site/`：单页 `index.html`（hero/why/install/interface/config/security/updates）、`style.css`（复用 `web/style.css` 的色板、方角、48px 网格、等宽字体）、`script.js`（复制按钮 + 年份）、`favicon.svg`；无外部请求、无框架、无 Web 字体
  - 新增 `docs/site/install.sh`（POSIX sh）：OS/arch 映射 6 个 tar.gz，下载后按 release `checksums.txt` 校验，`cp`+`mv` 原子替换，`-v/-d/-h` 与环境变量，PATH 提示；旧 release 无校验文件时警告继续
  - 新增 `docs/site/install.ps1`（5.1+）：zip 下载/校验（.NET SHA256，避开本机 PSModulePath 里 PS7 模块导致的 `Get-FileHash` 缺失）、解压、运行中进程检查、用户 PATH 追加、版本自检；临时目录 finally 清理
  - `.github/workflows/release.yml` 在 artifact 合并后生成 `sha256sum onet-* > checksums.txt`；为 v0.3.1 补传 `checksums.txt`（8 行），`releases/latest/download/checksums.txt` 可访问
  - 用隔离实例（127.0.0.1:7682，`-X main.version=0.3.1`）真实截取桌面 1440×900 与移动 390×844 界面，避免触碰 7681 上的用户会话；截图 `-strip` 后 92KB / 106KB
  - 新增 `.github/workflows/pages.yml`（configure/upload/deploy-pages，路径过滤 `docs/site/**`），用 `gh api -X POST repos/katriyda/onet/pages -f build_type=workflow` 开通 Pages；README 标题下加官网链接
- 产出: docs/site/{index.html,style.css,script.js,favicon.svg,install.sh,install.ps1,assets/desktop.png,assets/mobile.png}、.github/workflows/pages.yml、.github/workflows/release.yml、README.md；提交 470d13b、93a41f2；Pages 已开通（build_type=workflow）
- 结果: 完成。`sh -n`/help/缺参退出 2 通过；容器内 `install.sh` 装出 0.3.1（校验无警告）、`-v v0.2.0` 走警告路径并装出 0.2.0；Windows 安装器装出 0.3.1；本地 Chromium 验证标题/6 个导航锚点/图片加载/复制到剪贴板/零 console 错误/仅同源请求，390×844 无横向溢出、导航可横滑；CI 与 Pages 均绿；线上 `https://katriyda.github.io/onet/` 校验通过，`install.ps1` 被 Pages 以 `application/octet-stream` 返回，Windows 命令改用 `raw.githubusercontent.com/.../install.ps1`（text/plain），重新部署后线上 `curl | sh` 与 `irm | iex` 均装出 0.3.1
- meta: commit=93a41f2 files=docs/site/index.html,docs/site/style.css,docs/site/script.js,docs/site/favicon.svg,docs/site/install.sh,docs/site/install.ps1,docs/site/assets/desktop.png,docs/site/assets/mobile.png,.github/workflows/pages.yml,.github/workflows/release.yml,README.md,.agents/worklog.md

## 2026-10-25 15:07 — 首次启动生成安全配置 + 网页设置/应用重启
- session: s-20261025-configui
- agent: omp (deepseek-v4.1-flash-fast)
- 目录: C:\Users\katri\code\onet
- 目标: 配置改为首次启动自动生成；默认安全（随机 token、仅本机监听）；网页可改配置、持久化，并能应用后重启
- 做了什么:
  - `config.go`：文件缺失时生成默认配置（`crypto/rand` 32 字节 RawURLEncoding token、host 127.0.0.1、allow_insecure=false）；`Save` 原子写（同目录临时文件 + rename、0600），配置目录内日志绝对路径自动转相对；token 长度/控制字符校验
  - `main.go`：首次生成时 stderr+HUP 日志提示配置文件路径（不打印 token）；`app.start` 记录 config 路径；交接状态支持 `Rebind`
  - `config_web.go`：`GET /config` 只回传 sanitized 配置 + `token_set`/`config_path`；`PUT /config` 要求 `X-Onet-Config: 1`，校验安全边界、原子保存、token 留空保持/`clear_token` 清除、可选交接重启；`configMu` 串行化写；重启前 400ms 刷响应
  - `handoff*.go`：`handoffState.Rebind`；Unix 改监听地址时不复制旧 listener，关闭后 exec 由新进程 bind（失败恢复读循环+relisten），Windows 仍由子进程重新 bind；更新路径 rebind=false
  - `web/`：新增设置面板（监听/认证/会话/界面/日志/安全全部字段），桌面浮层 + 移动全屏；随机令牌生成/清除、仅保存、保存并重启；重启后轮询新 origin 并跳转，端口变化自动改 URL；`app.js?v=20`、`style.css?v=11`
  - 测试/文档：新增 `config_test.go`（首次生成、token 随机、隐藏 token、持久化、清 token、非法非回环、CSRF、重启冲突）、handoff Rebind 往返；删除 `config.example.json`，release 包不再带模板；更新 README/AGENTS/docs 站点
- 产出: config.go、config_web.go、config_test.go、main.go、server.go、handoff.go、handoff_unix.go、handoff_test.go、update_web.go、update_test.go、web/{index.html,app.js,style.css}、README.md、AGENTS.md、docs/site/index.html、.github/workflows/release.yml
- 结果: 完成。`go vet`、`go test`、Windows/Linux/darwin 构建通过；首次启动实测生成 43 字符 token 且 `/config` 未认证 401；Chromium 桌面/390×844 移动端设置面板；UI 改字号+改端口 8766→8770 保存并重启，`handoff.restored sessions=1`、旧端口关闭、新端口服务、会话保留；UI 轮换 token 后旧 token 401/新 token 200。提交 2386b87，CI 绿；tag v0.5.0 Release（曾误发 v0.4.0，已撤回 tag 与 Release）；通过 `POST /update` 将 A 部署实例 0.3.1→0.5.0，`resumed=true`、1 个会话保留。注意：A 的既有 `config.json` 未改（仍 0.0.0.0 / 无 token / allow_insecure=true），安全默认只作用于新生成配置
- meta: commit=2386b87 tag=v0.5.0 files=config.go,config_web.go,config_test.go,main.go,server.go,handoff.go,handoff_unix.go,handoff_test.go,update_web.go,update_test.go,web/index.html,web/app.js,web/style.css,README.md,AGENTS.md,docs/site/index.html,.github/workflows/release.yml,.agents/worklog.md

## 2026-10-25 15:39 — 移动端软键盘弹起/收起：冻结行数 + 底边跟随
- session: s-20261025-vpresize
- agent: omp (deepseek-v4.1-flash-fast)
- 目录: C:\Users\katri\code\onet
- 目标: 消除移动端软键盘打开/关闭时终端逐行重排、底部跳动和 resize 风暴，让键盘动画期间内容连续跟随、手感稳定
- 做了什么:
  - 复现定位：`layout()` 在每个 `visualViewport` resize 帧都 `fit.fit()`，xterm 每跨一个字符高度 reflow 一次，并把中间 cols/rows 发给 ConPTY（shell 重绘），观感就是“卡一下跳一行”
  - `web/app.js` 新增冻结/提交状态机（`syncViewport`/`commitViewport`/`endViewportFreeze`）：高度变化期间用内联高度冻结 `#term` 行数，`translate3d` 让底边贴住 `#term-wrap` 底边；稳定 `VP_SETTLE_MS=140ms` 后一次性 fit；`fitTerm`/`sendResize` 冻结期间早退
  - 逐帧小步长（≤48px）直接贴住、无跟随滞后；视口一次性大跳变用 rAF 缓动（`VP_EASE=0.5`）；用 `(pointer: coarse)` 判定触屏，鼠标拖拽窗口保持逐帧 fit
  - 短缓冲区（`baseY=0` 且光标不在末行）或已滚回历史时不平移、只冻结行数，避免把顶部提示符推出屏幕；用 `buffer.viewportY/baseY` 判断底部，规避容器缩小后像素级 `atBottom()` 误判
  - 键盘关闭同样锚底，提交后按底部状态 `scrollToBottom`；字号变化前先结束冻结；同步 app.js?v=27 与 README/AGENTS/docs 站点说明
  - 发布 `v0.5.1`（轻量 tag）：Release workflow 产出 8 平台资产 + `checksums.txt`，Windows zip 校验一致且内嵌 `app.js?v=27`；通过 `POST /update` 将 A 部署实例 0.5.0→0.5.1，`handoff.restored sessions=1`
- 产出: web/app.js、web/index.html、AGENTS.md、README.md、docs/site/index.html；提交 d595435；tag/Release v0.5.1
- 结果: 完成。`go vet`/`go test`/`go build` 通过；隔离实例 7682 + Chromium 移动视口逐帧模拟（390×700→500，20px/帧）：冻结 70 帧内 xterm 高度恒定、屏幕仅重排 1 次、光标位移与可视区底边位移一致（-180/-180），PTY 每方向只收到 1 条 resize（31↔19）；短缓冲区光标零位移、无重排；细指针路径无冻结、逐帧重排；main 已推送（CI/Pages 绿）。发布 v0.5.1 后 `POST /update` 返回 `updated=true`，1s 内 `/healthz` 为 `version=0.5.1,resumed=true`，日志 `handoff.restored sessions=1`，部署页 `index.html` 已引用 `app.js?v=27`（已打开的旧页面需刷新才会加载新 JS）；真机 IME/键盘动画需用户复验
- meta: commit=d595435 tag=v0.5.1 files=web/app.js,web/index.html,AGENTS.md,README.md,docs/site/index.html


## 2026-10-25 16:16 — 移动端软键盘：可视区对齐 + 底部重新锚定
- session: s-20261025-kbanchor
- agent: omp (deepseek-v4.1-flash-fast)
- 目录: C:\Users\katri\code\onet
- 目标: 键盘弹起后终端仍能停在底部（不再需要手动滚两下）；页面/可视区不被键盘推走；用户主动上滑查看历史时不被强制拉回
- 做了什么:
  - `web/app.js` 新增底部意图 latch（`bottomIntentAt`/`vpForceBottom`）：`onTouchStart` 与 textarea focus 在浏览器 focus 滚动**之前**记录“点之前是否在底部”，冻结决策 `vpPan = canAnchorBottom() || (intent && bufferFillsScreen())`；`onTouchMove`/`startMomentum` 一旦拖动就清除意图并用 `anchorSeq` 作废未完成的锚定
  - 新增 `settleAnchorBottom()`：fit 后 1+2 帧 + 60/140ms/300ms 幂等重试 `term.scrollToBottom()`，替代同一帧滚动（xterm 5.x viewport/scroll-area 延后同步，同帧会差 1–2 行）；`commitViewport()` 先保存 `forced` 再解冻，日志升级为 INFO `client.vp.settle`（vvh/top/ih/sy/rows/bottom/force/pan）与 `client.vp.anchor`；attach 回放后的底部锚定复用同一路径
  - `#app` 改为固定壳：JS 同时写 `top`（`vv.offsetTop`，clamp，仅 coarse pointer）与 `height`（`vv.height`），`--vv-top` 用同一值；新增 `resetPageScroll()` + passive scroll 兜底；chrome 用 `touch-action` 隔离（header 限 mobile、`#keys`/`#d-tabs` pan-x、抽屉/菜单/日志/设置 pan-y）
  - 键盘冻结期间底部意图存在时抑制“回到底部”浮标，避免误导；`scrollToBottom()`（用户动作）递增 `anchorSeq` 取消延后链
  - 同步 `app.js?v=28` / `style.css?v=12`，README 移动端条目与 AGENTS 前端坑新增两条不变量
  - 发布 `v0.5.2`：Release 产出 8 平台资产 + `checksums.txt`；`POST /update` 将 A 部署实例 0.5.1→0.5.2（`handoff.restored sessions=1`）
- 产出: web/app.js、web/style.css、web/index.html、AGENTS.md、README.md；提交 6e830aa；tag/Release v0.5.2
- 结果: 完成。`go vet`/`go test`/`go build` 通过；隔离实例 7682 + Chromium 390×700（init script 强制 coarse pointer）逐项验证：app `position:fixed`/`top=0px`/`sy=0`/高度=vvh；短缓冲区冻结不平移、重排 1 次、光标 top 不变；满缓冲区冻结 transform 逐帧 -20…-200 且光标与可视区底边间距恒定 4px，提交后 screenH 544→340 仅变 1 次、atBottom；底部意图回归（合成 tap 后把 scrollTop 抬 40px）→ 冻结 `intent=true pan=true`、提交 `bottom=1 force=1 pan=1` 且锚回底部、浮标隐藏；上滑 120px 后 tap 再缩键盘保持 `atBottom=false` 且浮标可见；细指针页不冻结、逐帧重排；桌面 1280×800 布局与输入正常。日志确认 `client.vp.settle ... bottom=1 force=1 sy=0`、`client.vp.anchor bottom=1`。A 实例 `/healthz` 为 `version=0.5.2,resumed=true`，部署页引用 `app.js?v=28`。真机需强制刷新一次复验键盘行为（Android 日志 `vvh/ih` 若在键盘弹起时不变 = 宿主 `adjustPan`，web 层无解）
- meta: commit=6e830aa tag=v0.5.2 files=web/app.js,web/style.css,web/index.html,AGENTS.md,README.md


## 2026-10-25 16:43 — 全屏 TUI（alternate screen）不再平移视口
- session: s-20261025-altpan
- agent: omp (deepseek-v4.1-flash-fast)
- 目录: C:\Users\katri\code\onet
- 目标: 修掉软键盘弹起/收起时全屏 TUI（omp/less/vim）里的“滚动两下”：内容先上滑再被应用重绘拉回
- 做了什么:
  - 定位：alt 缓冲没有回滚区（`scrollHeight == clientHeight`），冻结期间的 `translate3d` 平移量在提交后无法被 `scrollToBottom()` 补偿；提交时 xterm 按新行数裁掉顶部，应用收到 SIGWINCH 再整屏重绘回自己的视图 → 上移 `rowsOld - rowsNew` 行再下移同样行数
  - `web/app.js` `syncViewport()` 冻结分支：`vpPan = !isAltBuffer() && (canAnchorBottom() || (bottomIntent && bufferFillsScreen()))`，附原因注释；普通缓冲区平移机制保持不变
  - `commitViewport()` 的 `client.vp.settle` 增加 `alt=0/1` 标记，便于真机判定走了哪条分支
  - `web/index.html` 升到 `app.js?v=29`；README 移动端「键盘弹起/收起」与 AGENTS 前端坑各补一条（alt 不能平移的不变量）
- 产出: web/app.js、web/index.html、README.md、AGENTS.md；提交 0a30b50；tag/Release v0.5.3
- 结果: 完成。`go vet`/`go test`/`go build` 通过；隔离实例 7682 + Chromium 390×710（CDP touch emulation 得到 `(pointer: coarse)`）：
  - A `less` 全屏：710→420 的 189 帧里 `#term` transform 恒为空、冻结高度 `562px`、rows 62→30、`ch=266`，`client.vp.settle pan=0 alt=1 bottom=1`；+150ms 顶行为空（应用重绘中），+700ms 顶行回到 `document.body.appendChild(a);`（与起始一致）；420→710 同样 transform 恒空、rows 30→62、`pan=0 alt=1`
  - B 普通缓冲区（80 行 + 提示符，atBottom）：26 帧带 `translate3d(0,-290px,0)`、冻结期 rows 恒 62、提交后 `sc=459=sh-ch`（atBottom），`pan=1 alt=0 bottom=1`，平移末帧与提交后可见内容一致（差 2px，属 fit 延后同步）
  - C 细指针页（无 touch 模拟）11 步 710→420：无内联高度/transform，rows 58→30 逐帧变化
  - 发布：`gh run watch` CI/Release 绿，9 个资产；`POST /update` 将 A 实例 0.5.2→0.5.3（`updated=true`，`/healthz` `version=0.5.3,resumed=true`，`handoff.restored sessions=1`，`onet.exe` 已换新，页面引用 `app.js?v=29`）
  - 真机需强刷一次：TUI 会话里弹起/收起键盘应看到 `client.vp.settle ... alt=1 pan=0`
- meta: commit=0a30b50 tag=v0.5.3 files=web/app.js,web/index.html,README.md,AGENTS.md


## 2026-10-25 17:10 — 内联 TUI（omp）键盘动画不再平移
- session: s-20261025-altpan
- agent: omp (deepseek-v4.1-flash-fast)
- 目录: C:\Users\katri\code\onet
- 目标: v0.5.3 后用户仍报“滚动两下”，指定会话是 omp（大量上下文）；定位并修掉 omp 场景
- 做了什么:
  - 现场取证：真机日志 `client.vp.settle` 缺 `alt=` 字段（16:53 起仍无）→ 手机页面还在跑 `app.js?v=28`；同时在 7681 会话上用 v29 实测 omp 为**普通缓冲**（`scrollHeight ≫ clientHeight`、`alt=0`、`pan=1`），`isAltBuffer()` 门控对 omp 完全不生效
  - 影响面判断：alt 门控只覆盖 less/vim 这类 alt 缓冲；omp/htop 是“普通缓冲 + 绝对定位重绘”的内联 TUI，平移后会被自己按新行数重画回去 → 仍是“滑上去再拉回”
  - `web/app.js` 新增 TUI 判定：`noteTermBytes()` 扫输出字节里的 `ESC[?25l` / `ESC[r;cH` / `ESC[r;cf` / 私有模式 `h`/`l`（含跨帧 12 字节回看），命中则记 `tuiPaintAt`；`tuiPaints()` = 最近 1.5s 有这类序列。`writeOutput()` 成为唯一写入入口（替换两处直接 `term.write(b64ToBytes(...))`）
  - 冻结决策改为 `vpPan = !(isAltBuffer() || tuiPaints()) && (...)`，`client.vp.freeze` 增加 `tui=`；`app.js?v=30`；README/AGENTS 的不变量从“alt 不能平移”扩成“TUI 不能平移（alt + 内联 TUI）”
- 产出: web/app.js、web/index.html、README.md、AGENTS.md；提交 279f693；tag/Release v0.5.4
- 结果: 完成。`go vet`/`go test`/`go build` 通过；隔离实例 7682（v30）：
  - 内联 TUI 模拟器（底部状态条 + 每 100ms 绝对定位重绘）：710→420 共 169 帧 transform 恒空、冻结高 `562px`、rows 62→30，`freeze ... tui=true pan=false`、`settle pan=0 alt=0 bottom=1`；420→710 同样 `pan=0`
  - 普通输出（`1..80 | % {"row $_"}`，atBottom）：transform 缓动帧齐全（-145…-290）、提交后 `sc=1035=sh-ch`，`freeze ... tui=false pan=true`、`settle pan=1 alt=0`
  - alt 回归（less → G）：transform 恒空、顶行不变，`settle pan=0 alt=1`
  - 现场 omp 会话（7681，v30 重载后）：90 帧 transform 恒空、冻结高 `562px`、rows 33→16，`settle vvh=420 pan=0 alt=0`、收起时 `vvh=710 pan=0`；截图对比：缩小时内容位置不变（仅底部被裁），提交后由 omp 自己重排一次
  - 发布：CI/Release 绿（9 资产）；`POST /update` 0.5.3→0.5.4（`/healthz version=0.5.4,resumed=true`，页面引用 `app.js?v=30`）
  - 真机仍需刷新页面（16:53 的日志证明手机仍在 v28）；刷新后 TUI 会话应为 `pan=0`（omp 也会带 `tui=true`）
- meta: commit=279f693 tag=v0.5.4 files=web/app.js,web/index.html,README.md,AGENTS.md


## 2026-10-25 17:35 — 键盘弹出/收起“抽动一下”：平移改为一帧贴住 + 提交同帧归位
- session: s-20261025-altpan
- agent: omp (deepseek-v4.1-flash-fast)
- 目录: C:\Users\katri\code\onet
- 目标: 用户报 v0.5.4 后“输入法弹出和关掉终端内容会抽动一下”，确认现象并修掉
- 做了什么:
  - 复现并量化（隔离实例 7682 + Chromium，15ms 采样）：v0.5.4 对 TUI 不平移 → 冻结期间可视区停在冻结网格的**顶部**（`first=sim transcript line 92`），提交后跳到**底部**（`line 121/307→297`），一次 ~30 行的瞬移 = 用户的“抽动”
  - 同时发现 v0.5.4 的 TUI 判定（扫 `ESC[?25l`/CUP）会误判普通 pwsh 输出（`freeze ... tui=true`，普通输出反而不再平移）——信号不够可靠，整块删除
  - `web/app.js`：`vpPan` 回到 `!isAltBuffer() && (...)`；平移改为**直接赋目标值**（删掉 `VP_EASE`/`VP_FOLLOW_PX`/`vpStep`/`vpTarget`/`vpOffset`/`vpRaf`），注释说明缓动会被看成“又滚了一下”；`commitViewport()` 在 `fitTerm(true)` 之后同帧 `term.scrollToBottom()`（延后重试只在差 1–2 行时补齐），消除“先露一帧旧滚动位置”
  - `app.js?v=31`；README/AGENTS 更新为“冻结期间只平移、必须一帧贴住、提交同帧归位；不要用输出序列猜 TUI”
- 产出: web/app.js、web/index.html、README.md、AGENTS.md；提交 0cd8541；tag/Release v0.5.5
- 结果: 完成。`go vet`/`go test`/`go build` 通过；隔离实例（TUI 模拟器 / 普通输出 / less）：
  - TUI 模拟器：冻结期间 transform 恒为单值 `-290px`（无缓动帧），可视首行 92→123（贴底），提交后 124/125（应用自绘 1 行内），`freeze pan=true`、`settle pan=1 alt=0`
  - 普通输出：`row 20 → row 51（贴底）→ row 52`，`sc=1044=sh-ch`，`pan=1 alt=0`
  - less（alt）：无 transform，`pan=0 alt=1`
  - 现场 omp 会话（7681，v31）：收缩 33→16、扩张 16→33，两个方向 transform 都是 16ms 内一次到位的单值，最后一行（omp 状态框）在两方向都不变位；`settle vvh=420/710 ... pan=1 alt=0`
  - 发布：CI/Release 绿（9 资产）；`POST /update` 0.5.4→0.5.5（`/healthz version=0.5.5,resumed=true`，页面引用 `app.js?v=31`）
  - 遗留：omp 收到 SIGWINCH 后自己的重绘（行数真的变了）仍是唯一可见变化；若真机仍觉得明显，下一轮可缩短 `VP_SETTLE_MS`/`scheduleResize` 让那次重绘落在键盘动画期间
- meta: commit=0cd8541 tag=v0.5.5 files=web/app.js,web/index.html,README.md,AGENTS.md


## 2026-10-25 17:48 — 键盘“抽动”：提交压进 IME 动画窗口 + 诊断字段
- session: s-20261025-altpan
- agent: omp (deepseek-v4.1-flash-fast)
- 目录: C:\Users\katri\code\onet
- 目标: 用户反馈“输入法弹出/关掉终端内容会抽动一下，之前版本一直都有”；把现象量化到最小可见变化次数
- 做了什么:
  - 换度量方式：先前用“相对 #term 顶部的固定偏移取文本”，平移时 term 顶本身也在动，导致误判“没变化”；改成**绝对屏幕 y**取文本（y=70/170/300），才量到真正的视觉位移
  - 结论：键盘占掉的 290px 必须让内容上移，且应用收到 SIGWINCH 必须重绘——最少是“一次上移 + 一次应用重绘”两个事件。旧参数（`VP_SETTLE_MS=140` + resize 防抖 80ms）把应用重绘推到 ~220ms，落在键盘动画结束之后，于是被看成第二次抽动
  - `web/app.js`：`VP_SETTLE_MS` 140→60，`scheduleResize` 80→40，让本地重排 + PTY resize + 应用重绘都落在系统键盘动画（200–300ms）内；`client.vp.settle` 增加 `n=`（本次冻结高度变化次数）与 `dt=`（首帧到提交的毫秒），用于判断真机是否逐帧上报、提交是否还在动画窗口内
  - `app.js?v=32`；AGENTS 补该不变量与诊断字段说明
- 产出: web/app.js、web/index.html、AGENTS.md；提交 9934eb9；tag/Release v0.5.6
- 结果: 完成。`go vet`/`go test`/`go build` 通过；隔离实例 + 现场会话实测（绝对 y 采样，15ms）：
  - TUI 模拟器：t=0 内容 L93/L104/L119 → t=16ms 平移后 L125/L137/blank（一次上移）→ t=96ms 提交 rows 30，内容同上（提交不再引起位移）→ 之后仅应用自绘 spinner 行
  - 现场 omp（7681，v32）：t=17ms transform `-290px` 一次到位；t=96ms rows 33→16，y70/y170/y300 文本**完全不变**（提交无位移）；t=143/175ms 仅状态栏 spinner 变化
  - 普通输出 `pan=1`（`sc=1035=sh-ch`）、less `pan=0 alt=1` 无回归；日志均为 `n=1 dt≈62–74`
  - 发布：CI/Release 绿（9 资产）；`POST /update` 0.5.5→0.5.6（`/healthz version=0.5.6,resumed=true`，页面引用 `app.js?v=32`）
  - 待真机确认：刷新后看 `client.vp.settle ... n= dt=`：`n>1` = 手机逐帧上报（平移跟着键盘走）；`n=1` = 只报最终高度（只能一次性跳）
- meta: commit=9934eb9 tag=v0.5.6 files=web/app.js,web/index.html,AGENTS.md


## 2026-10-25 18:00 — 键盘动画期间的位移改成“跟着键盘滑”，收起方向不冻结
- session: s-20261025-altpan
- agent: omp (deepseek-v4.1-flash-fast)
- 目录: C:\Users\katri\code\onet
- 目标: 用户反馈“不是抽动，是内容会滚动一下”；真机日志确认 `n=1`（只上报最终高度），平移一帧完成就会看成“滚一下”
- 做了什么:
  - 真机取证：7681 日志 `client.vp.settle ... n=1 dt=60`（偶尔 `n=3 dt=98`）→ 手机基本拿不到键盘逐帧高度，只能自己补动画
  - `web/app.js` 平移改回 rAF 缓动：新增 `VP_ANIM_MS=190` + ease-out（`panTo`/`panStep`），提交撞上动画时挂 `vpCommitPending`，等动画收尾再提交（避免移除 transform 时再跳一格）；`VP_SETTLE_MS` 保持 60，`scheduleResize` 40
  - 收起方向（可视区变大）不冻结：冻结网格比窗口矮，平移会在顶部露空白；改为直接 `fit + scrollToBottom + settleAnchorBottom`，多出来的行从回滚区补上
  - `app.js?v=33`（v0.5.7，缓动）、`app.js?v=34`（v0.5.8，收起方向）
- 产出: web/app.js、web/index.html；提交 161a28a、e193a53；tag/Release v0.5.7、v0.5.8
- 结果: 完成。`go vet`/`go test`/`go build` 通过；隔离模拟器 + 现场 omp 实测（绝对屏幕 y 采样）：
  - 弹起（710→420）：transform 逐帧 `-20 → -102 → -138 → -181 → -214 → -241 → -260 → -273 → -282 → -287 → -289 → -290`（约 190ms），提交后可视文本完全不变（`L125/L136`），再无跳变
  - 收起（420→710）：v0.5.7 缓动 + 顶部空白；v0.5.8 改成 16ms 内直接重排到 33 行（无空白、无冻结）
  - 发布：两版 CI/Release 均绿（9 资产），`/healthz` 0.5.8，页面引用 `app.js?v=34`
  - 未决：位移本身（~290px）在“输入行必须可见”的前提下无法消除；已向用户确认取舍（跟随滑动 vs 完全不 resize）
- meta: commit=e193a53 tag=v0.5.8 files=web/app.js,web/index.html


## 2026-10-25 18:50 — 找到“一直没修复成功”的根因：真机页面被 WebView 缓存钉在旧版本
- session: s-20261025-altpan
- agent: omp (deepseek-v4.1-flash-fast)
- 目录: C:\Users\katri\code\onet
- 目标: 用户报“不是抽动，是内容会滚动一下，还是没修复”；解释为什么每次发布都“没效果”
- 做了什么:
  - 对照真机日志时间线：18:41 手机 `/config` 读到 `version=0.2.0`（一小时前就已是 0.5.8），且 `client.vp.settle` 里 `dt=60`（v0.5.6 的抓拍值，v0.5.7 起的缓动应为 ~190ms）、没有新加的 `v=` 字段 → **手机跑的仍是 app.js?v=32（v0.5.6）**，之后 0.5.7/0.5.8 的改动从未到达真机
  - 根因：`/` 只发 `Cache-Control: no-cache`、`/config` **没有任何缓存头**；Android WebView 会把它们当“可缓存不重验”，页面就一直停在旧 HTML（进而引用旧 `app.js?v=N`）
  - `server.go`：`/` 改 `no-store, no-cache, must-revalidate` + `Pragma: no-cache` + `Expires: 0`；`/config` GET 加 `no-store`
  - `web/app.js`：新增 `APP_BUILD`（从 `<script src="?v=N">` 取），写进 `client.boot`（`app=`）与 `client.vp.settle`（`v=`），以后一眼就能看出真机跑的是哪份资源；`app.js?v=35`
  - AGENTS 补缓存不变量
- 产出: server.go、web/app.js、web/index.html、AGENTS.md；提交 a3e0260；tag/Release v0.5.9
- 结果: 完成。`go vet`/`go test`/`go build` 通过；隔离实例验证：`/` 响应头为 `no-store, no-cache, must-revalidate` + `Pragma`/`Expires`，页面 `app.js?v=35`，日志 `client.boot app=35`、`client.vp.settle ... n=1 dt=195 v=35`；发布 v0.5.9（9 资产），`/healthz version=0.5.9`，线上页面引用 `app.js?v=35`
  - 给用户的下一步：旧缓存条目仍可能在，首次用带查询串的地址打开（`?s=sess-1&v=35`）或清一次壳缓存；之后 `no-store` 保证不再回退
- meta: commit=a3e0260 tag=v0.5.9 files=server.go,web/app.js,web/index.html,AGENTS.md




