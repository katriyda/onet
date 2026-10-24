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
