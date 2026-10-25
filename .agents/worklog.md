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
- 结果: 完成。`go vet`、`go test`、Windows/Linux/darwin 构建通过；首次启动实测生成 43 字符 token 且 `/config` 未认证 401；Chromium 桌面/390×844 移动端设置面板；UI 改字号+改端口 8766→8770 保存并重启，`handoff.restored sessions=1`、旧端口关闭、新端口服务、会话保留；UI 轮换 token 后旧 token 401/新 token 200。提交 2386b87，CI 绿；tag v0.4.0 Release 8 资产；通过 `POST /update` 将 A 部署实例 0.3.1→0.4.0，`resumed=true`、1 个会话保留。注意：A 的既有 `config.json` 未改（仍 0.0.0.0 / 无 token / allow_insecure=true），安全默认只作用于新生成配置
- meta: commit=2386b87 tag=v0.4.0 files=config.go,config_web.go,config_test.go,main.go,server.go,handoff.go,handoff_unix.go,handoff_test.go,update_web.go,update_test.go,web/index.html,web/app.js,web/style.css,README.md,AGENTS.md,docs/site/index.html,.github/workflows/release.yml,.agents/worklog.md

