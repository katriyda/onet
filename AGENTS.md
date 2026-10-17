# AGENTS.md

写给在 `onet` 仓库内工作的 AI agent（也适用于新贡献者）。与用户交流、本文件与 README 均用中文。

## 项目概览

onet 是**移动端优先的 Web 终端**，tmux 风格会话管理：多会话、关页不杀进程、断线重连（detach/reattach）。

- 单文件 Go 二进制（前端 `go:embed` 内嵌，xterm.js 已 vendor 入库），可选 `config.json`，部署零依赖
- 移动端 = termux 风格（底部按键栏、左滑抽屉、长按菜单、捏合缩放）；桌面端 = 经典终端（tab 栏、右键菜单、快捷键）；按输入设备自适应，配置 `ui` 可强制
- 仓库 `github.com/katriyda/onet`（公开，MIT）；当前版本线 v0.1.x；发布走 GitHub Releases
- 目标平台 8 个：windows amd64/arm64、linux amd64/arm64/armv7、darwin amd64/arm64、freebsd amd64

## 常用命令

```bash
go vet ./...             # 静态检查（CI 同款）
go build -o onet.exe .   # 构建（Windows；产物已 gitignore）
go test ./...            # 当前无测试文件；新增测试放这里（CI 会跑）
./onet                   # 运行 → http://127.0.0.1:7681（-config 指定配置路径）
```

- 本机 Windows + pwsh；Go 由 mise 安装，工具链跟随 go.mod（1.26.9，Go 自动切换）
- **无前端构建步骤**：改 `web/` 后重新 `go build` 即打包（编译期 embed）。`web/vendor/` 构建时必须存在，**必须保持入库**

## 架构地图（改动前先读对应文件）

| 文件 | 职责 |
|---|---|
| `main.go` | 入口：`-config`（默认二进制同目录 config.json）→ Load → NewServer → 信号优雅退出（KillAll） |
| `config.go` | `Config`/`DefaultConfig`/`Load`：文件缺失→全默认；存在→只回填显式字段后 `Validate`（host 非空、port 1–65535、scrollback>0、font_size>0、ui∈{auto,mobile,desktop}） |
| `session.go` | 核心：`Pty` 窄接口；`ringBuf` 64KB 重放缓冲；`Client` 发送队列 cap 256（满即丢帧，慢客户端不阻塞广播）；`Session`（run 读循环、Attach 先重放、Detach/Resize/Kill/Restart）；`Manager`（Create/Get/Find/List/Remove/Restart/KillAll，变更回调广播 sessions） |
| `server.go` | 路由 `/`、`/static/`、`/config`、`/login`、`/ws`；token 认证（cookie `onet_token` / `?token=` / `Authorization: Bearer`，恒时比较）；WS 协议处理 |
| `pty_windows.go` | 自研 ConPTY 后端（纯 P/Invoke，无 cgo）：退出→`ClosePseudoConsole`→排空→关句柄（规避关闭竞态）；spawn 失败重试 1 次；默认 shell pwsh→powershell |
| `pty_unix.go` | Unix 后端直用 `creack/pty`；默认 shell `$SHELL`→bash |
| `web/` | 原生 JS 无框架无构建：`index.html`/`app.js`/`style.css`（中文注释）+ `vendor/`（xterm.js 5.3.0 UMD + addon-fit） |

PTY 后端是唯一可替换点：业务代码只依赖 `session.go` 的 `Pty` 接口，不在别处 import 具体 PTY 库。

## WS 协议（改协议必须前后端同步）

- **二进制帧 = JSON 命令**；**文本帧 = 原始输入字节**（转发给当前 attach 的会话）
- 客户端→服务端：`list`、`create{name?}`、`attach{id}`、`detach`、`resize{id,cols,rows}`、`restart{id}`、`kill{id}`、`remove{id}`
- 服务端→客户端：`sessions`（会话增删改后广播；`[{id,name,pid,dead,exitCode,attached}]`）、`created{id,name}`、`attached{id}`、`detached`、`output{id,data:base64}`、`exit{id,code}`、`error{message}`
- 语义：一个连接同时只 attach 一个会话（切换 = detach + attach）；断连 = detach、进程存活（tmux 语义）；attach 已死会话 → error；`attach` 先重放 ringBuf 再登记客户端

## 前端约定与不变式（移动端）

- 界面控件绑定**一律用 `onTap()`**，不要直接 `click`：click 会抢走 xterm textarea 焦点 → 移动端输入法收起。onTap 内部的 click 监听（`detail===0`）负责键盘 Enter/Space 激活，**勿删**
- 软键盘保持：`CHROME_SEL`（`#header,#keys,#drawer,#ctx,#scrim,#ph-new`）在捕获阶段 `pointerdown`+`mousedown` 阻止默认（iOS 必须 mousedown，WebKit bug 322721）。新增“键盘弹起时会被点按”的控件要加进 `CHROME_SEL`
- 阻止默认后 `:active` 不可靠 → 点按反馈统一用 `.pressed` class（pointer 事件驱动）
- `modState` 键名小写（`ctrl`/`alt`）；按键栏 `dataset.key` 是配置原串 → 映射前先 `toLowerCase()`（历史 bug 来源）
- 布局跟随 `visualViewport`：`app.style.height` + `--vv-top`/`--vvh`（抽屉定位用），键盘弹起/收起自动重排并发 `resize`
- 模式判定：配置 `ui` 强制优先，否则 `(pointer:fine) && (min-width:768px)` → desktop，其余 mobile
- `/static/` 有 24h 缓存：**改 `web/` 中任何资源后，递增 `index.html` 里的 `?v=N`**

## 验证要求（完成定义）

- `go vet ./...` + `go build` 通过
- 行为改动必须**起服务端到端验证**，不能只靠编译：
  - 移动端用 Chromium 触屏仿真（390×844，hasTouch/isMobile）；桌面端用宽视口
  - 触摸类改动要验证 `document.activeElement` 仍为 `.xterm-helper-textarea`、按键字节真实送进 shell
- 协议改动：WS 冒烟（list → create → attach → 文本帧输入 → 断言 output 含回显 → 断开重连断言重放）
- 本地仿真覆盖不到真机（IME 细节、系统 contextmenu 合成）——受影响改动在交付说明中注明需真机复验

## 提交与发布

- 提交信息：英文、祈使句、单行（例：`Keep mobile soft keyboard open when tapping UI controls`）；依赖类改动加 `deps: ` 前缀
- 流程：改动 → 验证 → commit → `git push origin main` → CI 绿（`.github/workflows/ci.yml`：vet/build/test）
- 发版：轻量 tag `vX.Y.Z` → `git push origin vX.Y.Z` → release.yml 交叉编译 8 平台打包（zip/tar.gz，内含 README + config.example.json）自动建 Release
- 观察：`gh run watch <id>`、`gh release view vX.Y.Z --json assets`
- 不提交：`onet`/`*.exe` 二进制、`config.json`（可能含 token）、`tmp/`（.gitignore 已覆盖）

## 依赖策略

- 直接依赖仅 3 个：`coder/websocket`、`golang.org/x/sys`、`creack/pty`。保持最小依赖是项目取向
- 新增依赖前先评估：星数/维护活跃度（GitHub）、OSV/Go 漏洞库、`govulncheck ./...`。单人维护的新仓库、无更新记录的小库不适合作为关键依赖（`ptyterminal-go` 因此被移除并换为 creack/pty）
- 工具链漏洞随 Go 版本升级修复（mise），升级后跑 `govulncheck ./...` 复验

## 文档与安全

- `README.md`（中文）是功能/配置/用法的单一来源——行为或配置变更须同步更新；`config.example.json` 与 `Config` 字段保持一致
- 安全模型：默认 `127.0.0.1` 无认证；`token` 非空后 `/config`、`/ws` 强制认证；`/` 与 `/static/` 故意公开（登录页自举需要）；`/config` 不回传 token；传输明文 HTTP，TLS 交给反代/Tailscale
- 会话内是完整 shell——任何放宽监听/认证的改动须先向用户说明风险

## 协作偏好

- 中文交流与文档
- 较大功能/改动：先出计划、确认后再动手；实现与计划有偏离（实测驱动）时在交付说明中列出偏离点
- 交付必须附验证证据（实际运行结果、CI/Release 状态）；本地验证不到的（如真机行为）明确标注
