# onet

移动端优先的 Web 终端，tmux 风格会话管理：多会话、关页不杀进程、断线重连（detach/reattach）。

单文件 Go 二进制（前端全部内嵌），可选 JSON 配置文件，零第三方运行时依赖。

- **移动端**（触屏）：termux 风格 —— 底部额外按键栏（默认包含 `^C` / `Enter` / `PgUp` / `PgDn`）、左缘滑入会话抽屉、长按选中行复制、惯性滚动、捏合缩放
- **桌面端**（鼠标 + 宽屏）：经典终端 —— tab 栏、右键菜单、快捷键
- **自适应切换**：按输入设备自动选择布局，config 可强制覆盖
- **面向 agent 的交互**：全屏 TUI（omp / less / vim）内拖拽会翻译成滚轮/方向键；滚动时出现「回到底部」浮标；断线重连与会话切换会重放终端缓冲

## 快速开始

```bash
# 构建（需要 Go 1.26+）
go build -o onet .

# 运行（默认监听 http://127.0.0.1:7681）
./onet

# 指定配置文件
./onet -config /path/to/config.json
```

打开浏览器访问 `http://127.0.0.1:7681` 即自动进入第一个会话。会话进程独立于页面存活：关闭页面、断网、刷新都不会杀掉 shell，重新打开后自动回到最近的会话（tmux 语义）。

也可以直接从 [Releases](../../releases) 下载对应平台的二进制，开箱即用（无需任何安装步骤）。

### 自更新

```bash
onet update          # 检查并安装最新版本
onet update -check   # 只检查，不下载
onet update -force   # 已是最新版本时也重新安装
onet version         # 查看当前版本
```

CLI 只替换二进制文件，不重启服务、不影响已有会话：更新完成后重启 onet 才生效（当前运行的进程继续用旧版本）。

也可以在网页里直接升级：终端操作菜单 `⋯` → **检查更新** → **安装并重启**。服务端会下载并校验最新 Release，然后就地切换进程：

- Linux / macOS / FreeBSD：`exec` 新二进制，保留 PID、PTY fd 与正在运行的 shell
- Windows：启动新进程并把 ConPTY/PTY 句柄复制过去，旧进程确认新进程开始服务后退出
- 会话列表、回放缓冲和 shell 现场都保留；页面会自动重连，通常只短暂断开约 1 秒
- 新二进制启动失败时旧进程会恢复读循环与监听并继续服务，页面收到失败提示

版本信息来自 GitHub Releases API（匿名限额 60 次/小时），设置 `GITHUB_TOKEN` 或 `GH_TOKEN` 可提高限额；受限网络用 `HTTPS_PROXY` 指定代理。源码构建的版本号是 `dev`，更新时会直接安装最新发布版。

## 配置

默认读取与二进制同目录的 `config.json`；不存在时使用全部默认值。参考 `config.example.json`：

```json
{
  "host": "127.0.0.1",
  "port": 7681,
  "token": "",
  "shell": "",
  "cwd": "",
  "scrollback": 5000,
  "font_size": 15,
  "ui": "auto",
  "extra_keys": ["ESC", "TAB", "CTRL", "ALT", "^C", "ENTER", "←", "↑", "↓", "→", "PGUP", "PGDN"],
  "log_file": "onet.log",
  "log_level": "info",
  "log_max_size_mb": 20,
  "log_max_backups": 5,
  "log_terminal_io": false
}
```

| 字段 | 默认 | 说明 |
|---|---|---|
| `host` | `127.0.0.1` | 监听地址 |
| `port` | `7681` | 监听端口 |
| `token` | `""`（空） | 认证 token；非空后所有数据接口要求认证（详见「安全」） |
| `shell` | `""` | 会话 shell；空则用平台默认（Windows: pwsh→powershell；Linux/macOS: `$SHELL`→bash） |
| `cwd` | `""` | 会话工作目录；空为用户主目录 |
| `scrollback` | `5000` | 终端回滚行数 |
| `font_size` | `15` | 初始字号（可在界面中调整） |
| `ui` | `"auto"` | 界面模式：`auto`（按输入设备自适应）、`mobile`（强制触屏布局）、`desktop`（强制桌面布局） |
| `extra_keys` | agent 常用 12 键 | 触屏额外按键栏；`ESC`/`TAB`/`ENTER`/`^C`/`PGUP`/`PGDN`/`HOME`/`END`/`BS`/`DEL`/`←↑↓→` 有特殊含义，`CTRL`/`ALT` 为 sticky 修饰键，其余字符串按字面量发送 |
| `log_file` | `<二进制目录>/onet.log` | 日志文件路径（JSONL）；空串 = 仅输出到 stderr。相对路径按配置文件所在目录解析 |
| `log_level` | `"info"` | `debug`/`info`/`warn`/`error`；`debug` 额外记录静态资源请求、resize、广播等高频事件，并附带源码位置 |
| `log_max_size_mb` | `20` | 单个日志文件上限（MB），超出后轮转到 `onet.log.1`；`0` = 不轮转 |
| `log_max_backups` | `5` | 保留的历史份数；`0` = 轮转时直接截断当前文件 |
| `log_terminal_io` | `false` | 开启后记录 PTY 原始输入/输出（base64），**可能包含密码、token 等敏感数据** |

## 日志

服务端把关键事件写成 JSONL（一行一个事件），默认 `<二进制目录>/onet.log`，单文件 20MB、保留 5 份历史（`onet.log.1` … `onet.log.5`）：

```json
{"time":"2026-10-19T14:00:00.123+08:00","level":"INFO","msg":"ws.command","conn":"c1","cmd":"attach","session":"sess-1"}
```

覆盖的事件：进程启动/关闭、HTTP 请求（含状态码、耗时，`/static/` 与 `/ws` 记 debug 级）、认证失败与登录、WebSocket 连接/命令/错误、会话创建/attach/detach/退出/重启/删除、PTY 启动与错误。查询串里的 `token` 参数写入日志前替换为 `***`。

浏览器端把自己产生的事件与未捕获异常（`error`/`unhandledrejection`/`console.warn|error`）通过已认证的 WebSocket 批量回传到同一文件（`client.log`），离线或登录前的日志暂存 `sessionStorage`，恢复后继续回传。

- 网页内查看：终端操作菜单 `⋯` → **日志**，可切换「客户端 / 服务端」、复制、下载合并后的 `.jsonl`
- 直接访问：`GET /logs?tail=2000`（末尾 N 行，1–5000）、`GET /logs?download=1`（合并全部分片下载）；两者与 `/config`、`/ws` 一样需要 token
- 终端原始输入/输出默认**不记录**；需要排查交互问题时再开 `log_terminal_io`，启动时会写一条 `app.insecure_log` 警告

## 安全

- 默认只监听 `127.0.0.1`，仅本机可访问，无认证。
- **对外开放前必须设置 `token`**：未携带有效 token 时，`/config`、`/logs`、`/update`、`/ws` 等接口返回 401，页面显示令牌输入页。token 可通过 cookie（登录页）、`?token=` 参数或 `Authorization: Bearer` 头传递。
- `/update` 的 `POST` 会下载并替换当前二进制，额外要求同源自定义头 `X-Onet-Update: 1`（防 CSRF）；拿到 token 等同于拥有该机器的 shell 权限。
- 传输为明文 HTTP；需要加密时请置于反向代理（nginx/Caddy）或 Tailscale Serve 之后。
- 会话内是完整 shell，请像对待任何终端一样对待它的访问权限。

## 移动端用法

- **底部按键栏**：默认 `ESC / TAB / CTRL / ALT / ^C / Enter / ←↑↓→ / PgUp / PgDn`
  - 点按发送字符；`CTRL`/`ALT` 点按 = 下一次按键带修饰后自动取消，长按 = 锁定直到再次点按
  - `^C` 直接发送中断；`Enter` 无需唤起软键盘；方向键长按连续发送；`PgUp`/`PgDn` 翻页
- **会话列表**：左上角 ☰ 或从左缘右滑打开抽屉；点条目切换会话；行尾 `⋯` 或长按行打开「重启 / 删除」菜单
- **滚动**：单指拖拽带惯性；滚动后右下角出现「回到底部」，有流式输出时显示「新输出」；全屏 TUI（omp / less / vim 等）内拖拽会翻译成滚轮事件，没有鼠标上报时由 xterm 转成 ↑/↓
- **长按选中**：长按终端选中整行，继续上下拖动扩展范围（靠近上下边缘会自动滚动），顶部工具条可「复制 / 全选 / 取消」；菜单里的「复制」优先复制选中内容
- **键盘弹起**：终端自动重排（visualViewport），点按界面控件不会收起输入法
- **捏合缩放**：双指缩放字号，字号保存在 localStorage
- **终端操作**：右上角 `⋯` 打开底部操作表：复制 / 粘贴 / 字号 / 重置终端 / 显示隐藏按键栏 / 日志 / 检查更新

## 桌面端用法（经典终端）

- **Tab 栏**：每个会话一个标签；标签尾部 `⋯` 打开「重启 / 删除」，`+` 新建
- **快捷键**：`Ctrl+Shift+C` 复制、`Ctrl+Shift+V` 粘贴、`Ctrl+Shift+N` 新建、`Ctrl+Shift+W` 删除当前会话、`Ctrl+滚轮` 缩放字号
- **右键菜单**：终端内右键，或标题栏 `⋯`，打开与移动端一致的操作菜单
- 工具栏 `⌨` 按钮可临时唤出触屏按键栏（localStorage 记忆）
- 深链 `?s=<会话名或ID>` 直接进入指定会话

## 开发

```bash
go vet ./...
go build -o onet .
```

前端为纯原生 JS（xterm.js vendored 于 `web/vendor/`，构建时 `go:embed` 内嵌），无框架无构建步骤。改动前端后重新 `go build` 即可。

## 工作原理

- 每个会话 = 一个 PTY 进程（Windows 用系统 ConPTY，Unix 用 forkpty），输出缓冲 64KB 环形缓冲
- 多客户端可同时连接同一会话：输出广播，任一客户端 resize 最后生效，输入全部转发
- 客户端断连 = tmux detach，会话继续运行；重新 attach 时重放缓冲内容
- 会话仅存内存：进程退出即消失（与 tmux 语义一致）

## 许可证

MIT，见 [LICENSE](LICENSE)。
