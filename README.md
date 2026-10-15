# onet

移动端优先的 Web 终端，tmux 风格会话管理：多会话、关页不杀进程、断线重连（detach/reattach）。

单文件 Go 二进制（前端全部内嵌），可选 JSON 配置文件，零第三方运行时依赖。

- **移动端**（触屏）：termux 风格 —— 底部额外按键栏（可自定义）、左滑抽屉会话列表、长按菜单、捏合缩放
- **桌面端**（鼠标 + 宽屏）：经典终端 —— tab 栏、右键菜单、快捷键
- **自适应切换**：按输入设备自动选择布局，config 可强制覆盖
- 会话进程退出后保留并标记「已退出」，可一键重启或删除

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
  "extra_keys": ["ESC", "/", "-", "TAB", "CTRL", "ALT", "←", "↑", "↓", "→"]
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
| `extra_keys` | termux 经典 10 键 | 触屏额外按键栏；`ESC`/`TAB`/`←↑↓→` 有特殊含义，`CTRL`/`ALT` 为 sticky 修饰键，其余字符串按字面量发送 |

## 安全

- 默认只监听 `127.0.0.1`，仅本机可访问，无认证。
- **对外开放前必须设置 `token`**：未携带有效 token 时，`/config`、`/ws` 等接口返回 401，页面显示令牌输入页。token 可通过 cookie（登录页）、`?token=` 参数或 `Authorization: Bearer` 头传递。
- 传输为明文 HTTP；需要加密时请置于反向代理（nginx/Caddy）或 Tailscale Serve 之后。
- 会话内是完整 shell，请像对待任何终端一样对待它的访问权限。

## 移动端用法（termux 风格）

- **底部按键栏**：点按发送字符；`CTRL`/`ALT` 点按 = 下一次按键带修饰后自动取消，长按 = 锁定直到再次点按
- **会话列表**：左上角 ☰ 或从左缘右滑打开抽屉；点条目切换会话，长按条目弹「重启 / 删除」菜单
- **长按终端**：复制全部 / 粘贴 / 重置终端 / 字号± / 显示隐藏按键栏
- **捏合缩放**：双指缩放字号
- 键盘弹起时终端自动重排（visualViewport）

## 桌面端用法（经典终端）

- **Tab 栏**：每个会话一个标签；`×` 删除会话，`+` 新建，已退出会话显示 ↻ 可重启
- **快捷键**：`Ctrl+Shift+C` 复制、`Ctrl+Shift+V` 粘贴、`Ctrl+Shift+N` 新建、`Ctrl+Shift+W` 删除当前会话、`Ctrl+滚轮` 缩放字号
- **右键菜单**：复制 / 粘贴 / 重置终端 / 字号±
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
