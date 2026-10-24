//go:build windows

package main

// pty_windows.go — Windows ConPTY 后端（纯 P/Invoke，无 cgo）。
//
// 实现要点（与 ptygo 的区别）：
//   - 输出由 pump 协程读取；进程退出（Done）后先 ClosePseudoConsole，
//     让 conhost 排空剩余输出并关闭管道写端，从而安全地让 pump 读到 EOF。
//   - 只在 pump 退出后才关闭管道句柄，避免"句柄上还有挂起的 ReadFile 时
//     关闭"这一 ConPTY 已知竞态（会导致进程崩溃或永久阻塞）。

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modKernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procCreatePseudoConsole               = modKernel32.NewProc("CreatePseudoConsole")
	procResizePseudoConsole               = modKernel32.NewProc("ResizePseudoConsole")
	procClosePseudoConsole                = modKernel32.NewProc("ClosePseudoConsole")
	procInitializeProcThreadAttributeList = modKernel32.NewProc("InitializeProcThreadAttributeList")
	procUpdateProcThreadAttribute         = modKernel32.NewProc("UpdateProcThreadAttribute")
	procDeleteProcThreadAttributeList     = modKernel32.NewProc("DeleteProcThreadAttributeList")
	procCreateProcessW                    = modKernel32.NewProc("CreateProcessW")
)

const (
	procThreadAttributePseudoConsole = 0x00020016
	extendedStartupinfoPresent       = 0x00080000
	createUnicodeEnvironment         = 0x00000400
	startfUseStdHandles              = 0x00000100
)

// startupInfoEx 对齐 STARTUPINFOEX。
type startupInfoEx struct {
	StartupInfo     windows.StartupInfo
	lpAttributeList uintptr
}

// conptyConn 是一个 ConPTY 会话，实现 Pty 接口。
// pseudoConsole 对应 winconpty.h 中的 PseudoConsole ABI（HPCON 实际指向它）。
// 热更新时 HPCON 本身不能跨进程复制，只能复制其中的三个内核句柄。
type pseudoConsole struct {
	hSignal        windows.Handle
	hPtyReference  windows.Handle
	hConPtyProcess windows.Handle
}

type conptyConn struct {
	hpc       windows.Handle
	inherited *pseudoConsole // 非 nil 表示 hpc 指向本进程内重建的结构
	pi        windows.ProcessInformation

	// ptyIn/ptyOut 是伪控制台侧的管道句柄，会话期间必须保持打开。
	ptyIn  windows.Handle
	ptyOut windows.Handle
	// cmdIn 我们写（子进程输入），cmdOut 我们读（子进程输出）。
	cmdInFile  *os.File
	cmdOutFile *os.File

	pending     []byte // Read 尚未消费完的剩余字节
	outCh       chan []byte
	outOnce     sync.Once
	done        chan struct{}
	pumpDone    chan struct{}
	exitCode    int
	readStopped atomic.Bool

	closeOnce        sync.Once
	closeConsoleOnce sync.Once
}

func conptyAvailable() bool {
	return procCreatePseudoConsole.Find() == nil
}

// waitLoop 等待进程退出并记录退出码。
func (c *conptyConn) waitLoop() {
	_, _ = windows.WaitForSingleObject(c.pi.Process, windows.INFINITE)
	var code uint32
	if err := windows.GetExitCodeProcess(c.pi.Process, &code); err == nil {
		c.exitCode = int(code)
	}
	close(c.done)
}

// pump 持续读取子进程输出到 outCh。
func (c *conptyConn) pump() {
	defer close(c.pumpDone)
	buf := make([]byte, 8192)
	closeOut := func() { c.outOnce.Do(func() { close(c.outCh) }) }
	for {
		if c.readStopped.Load() {
			closeOut()
			return
		}
		n, err := c.cmdOutFile.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			select {
			case c.outCh <- data:
			case <-c.done:
				closeOut()
				return
			}
		}
		if err != nil {
			closeOut()
			return
		}
		if c.readStopped.Load() {
			closeOut()
			return
		}
	}
}

// startConpty 创建会话；子进程初始化崩溃时重试 1 次。
func startConpty(app, cwd string, cols, rows int) (Pty, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		c, err := newConptyConn(app, cwd, cols, rows)
		if err != nil {
			lastErr = err
			continue
		}
		if c.waitReady(2 * time.Second) {
			return c, nil
		}
		lastErr = fmt.Errorf("子进程在 ConPTY 初始化期间退出（0xC0000142 类故障）")
		if attempt == 0 {
			appLog.Warn("pty.retry", "attempt", attempt+1, "err", lastErr)
		}
		_ = c.Close()
		time.Sleep(100 * time.Millisecond)
	}
	return nil, fmt.Errorf("ConPTY 启动失败: %w", lastErr)
}

// waitReady 等待首字节输出或超时：用于检测初始化期崩溃（0xC0000142）。
func (c *conptyConn) waitReady(timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case data, ok := <-c.outCh:
		if ok {
			c.pending = append(c.pending, data...)
			return true
		}
		return false // 无输出即退出 → 初始化崩溃
	case <-c.done:
		return false
	case <-timer.C:
		return true // 静默程序视为就绪
	}
}

func newConptyConn(app, cwd string, cols, rows int) (*conptyConn, error) {
	if !conptyAvailable() {
		return nil, fmt.Errorf("当前系统不支持 ConPTY（需 Windows 10 1809+）")
	}

	var ptyIn, cmdIn, cmdOut, ptyOut windows.Handle
	if err := windows.CreatePipe(&ptyIn, &cmdIn, nil, 0); err != nil {
		return nil, fmt.Errorf("CreatePipe(输入): %w", err)
	}
	if err := windows.CreatePipe(&cmdOut, &ptyOut, nil, 0); err != nil {
		_ = windows.CloseHandle(ptyIn)
		_ = windows.CloseHandle(cmdIn)
		return nil, fmt.Errorf("CreatePipe(输出): %w", err)
	}
	cleanupPipes := func() {
		_ = windows.CloseHandle(ptyIn)
		_ = windows.CloseHandle(cmdIn)
		_ = windows.CloseHandle(cmdOut)
		_ = windows.CloseHandle(ptyOut)
	}

	// COORD 按值传递：把 rows 放高 16 位、cols 放低 16 位。
	packed := uint32(uint16(rows))<<16 | uint32(uint16(cols))
	var hpc windows.Handle
	hr, _, _ := procCreatePseudoConsole.Call(
		uintptr(packed), uintptr(ptyIn), uintptr(ptyOut), 0,
		uintptr(unsafe.Pointer(&hpc)))
	if hr != 0 {
		cleanupPipes()
		return nil, fmt.Errorf("CreatePseudoConsole 失败: HRESULT 0x%08X", uint32(hr))
	}

	attrList, attrBuf, err := initAttrList(1)
	if err != nil {
		cleanupPipes()
		_, _, _ = procClosePseudoConsole.Call(uintptr(hpc))
		return nil, err
	}
	if err := updateAttr(attrList, procThreadAttributePseudoConsole, uintptr(hpc)); err != nil {
		deleteAttrList(attrList)
		cleanupPipes()
		_, _, _ = procClosePseudoConsole.Call(uintptr(hpc))
		return nil, err
	}
	defer deleteAttrList(attrList)

	si := &startupInfoEx{
		StartupInfo: windows.StartupInfo{
			Cb:    uint32(unsafe.Sizeof(startupInfoEx{})),
			Flags: startfUseStdHandles,
		},
		lpAttributeList: attrList,
	}

	cmdLine, err := windows.UTF16PtrFromString(quoteCmdLine(app))
	if err != nil {
		cleanupPipes()
		_, _, _ = procClosePseudoConsole.Call(uintptr(hpc))
		return nil, err
	}
	var cwdPtr *uint16
	if cwd != "" {
		if cwdPtr, err = windows.UTF16PtrFromString(cwd); err != nil {
			cleanupPipes()
			_, _, _ = procClosePseudoConsole.Call(uintptr(hpc))
			return nil, err
		}
	}

	// 传真实（非 NULL）的安全属性使 ConPTY 初始化更可靠。
	var sa windows.SecurityAttributes
	sa.Length = uint32(unsafe.Sizeof(sa))

	var pi windows.ProcessInformation
	r1, _, e1 := procCreateProcessW.Call(
		0, // lpApplicationName（由命令行首 token 决定）
		uintptr(unsafe.Pointer(cmdLine)),
		uintptr(unsafe.Pointer(&sa)), // lpProcessAttributes
		uintptr(unsafe.Pointer(&sa)), // lpThreadAttributes
		0,                            // bInheritHandles
		extendedStartupinfoPresent|createUnicodeEnvironment,
		0, // lpEnvironment（继承当前环境）
		uintptr(unsafe.Pointer(cwdPtr)),
		uintptr(unsafe.Pointer(si)),
		uintptr(unsafe.Pointer(&pi)),
	)
	if r1 == 0 {
		cleanupPipes()
		_, _, _ = procClosePseudoConsole.Call(uintptr(hpc))
		return nil, fmt.Errorf("CreateProcess 失败: %w", e1)
	}
	// 属性列表缓冲必须存活到 CreateProcess 完成（uintptr 不被 GC 追踪）。
	runtime.KeepAlive(attrBuf)

	c := &conptyConn{
		hpc:        hpc,
		pi:         pi,
		ptyIn:      ptyIn,
		ptyOut:     ptyOut,
		cmdInFile:  os.NewFile(uintptr(cmdIn), "conpty-in"),
		cmdOutFile: os.NewFile(uintptr(cmdOut), "conpty-out"),
		outCh:      make(chan []byte, 64),
		done:       make(chan struct{}),
		pumpDone:   make(chan struct{}),
	}
	go c.waitLoop()
	go c.pump()
	// 进程退出后：先关伪控制台（不关句柄）让 conhost 排空输出并关闭管道写端。
	// conhost 持有写端副本，排空可能拖延；短暂宽限后 CancelIoEx 强制解除
	// pump 的 ReadFile 阻塞，保证 outCh 关闭、run 循环必然收到 EOF。
	go func() {
		<-c.done
		c.closeConsole()
		select {
		case <-c.pumpDone:
		case <-time.After(200 * time.Millisecond):
			_ = windows.CancelIoEx(windows.Handle(c.cmdOutFile.Fd()), nil)
		}
	}()
	return c, nil
}

// newInheritedConpty 用热更新复制过来的句柄重建 ConPTY 对象。
// 句柄归属新进程；Close 时按正常生命周期关闭。
func newInheritedConpty(hs handoffSession) (Pty, error) {
	if len(hs.Handles) < 8 {
		return nil, fmt.Errorf("会话 %s 缺少 ConPTY 句柄", hs.ID)
	}
	pc := &pseudoConsole{
		hSignal:        windows.Handle(hs.Handles[0]),
		hPtyReference:  windows.Handle(hs.Handles[1]),
		hConPtyProcess: windows.Handle(hs.Handles[2]),
	}
	c := &conptyConn{
		hpc:        windows.Handle(uintptr(unsafe.Pointer(pc))),
		inherited:  pc,
		ptyIn:      windows.Handle(hs.Handles[3]),
		ptyOut:     windows.Handle(hs.Handles[4]),
		cmdInFile:  os.NewFile(uintptr(hs.Handles[5]), "conpty-in"),
		cmdOutFile: os.NewFile(uintptr(hs.Handles[6]), "conpty-out"),
		pi: windows.ProcessInformation{
			Process:   windows.Handle(hs.Handles[7]),
			ProcessId: uint32(hs.PID),
		},
		outCh:    make(chan []byte, 64),
		done:     make(chan struct{}),
		pumpDone: make(chan struct{}),
	}
	if c.cmdInFile == nil || c.cmdOutFile == nil || c.pi.Process == 0 ||
		pc.hSignal == 0 || pc.hPtyReference == 0 {
		return nil, fmt.Errorf("会话 %s 的 ConPTY 句柄无效", hs.ID)
	}
	go c.waitLoop()
	go c.pump()
	go func() {
		<-c.done
		c.closeConsole()
		select {
		case <-c.pumpDone:
		case <-time.After(200 * time.Millisecond):
			_ = windows.CancelIoEx(windows.Handle(c.cmdOutFile.Fd()), nil)
		}
	}()
	return c, nil
}

// ---------- Pty 接口实现 ----------

func (c *conptyConn) Read(p []byte) (int, error) {
	if len(c.pending) > 0 {
		n := copy(p, c.pending)
		c.pending = c.pending[n:]
		return n, nil
	}
	data, ok := <-c.outCh
	if !ok {
		return 0, io.EOF
	}
	n := copy(p, data)
	if n < len(data) {
		c.pending = data[n:]
	}
	return n, nil
}

func (c *conptyConn) Write(p []byte) (int, error) {
	return c.cmdInFile.Write(p)
}

func (c *conptyConn) Close() error {
	c.closeOnce.Do(func() {
		c.closeConsole()
		// 等待 pump 的 ReadFile 结束（conhost 已排空）后再关句柄。
		select {
		case <-c.pumpDone:
		case <-time.After(2 * time.Second):
		}
		_ = c.cmdInFile.Close()
		_ = c.cmdOutFile.Close()
		if c.pi.Process != 0 {
			_ = windows.CloseHandle(c.pi.Process)
		}
		if c.pi.Thread != 0 {
			_ = windows.CloseHandle(c.pi.Thread)
		}
		if c.ptyIn != 0 {
			_ = windows.CloseHandle(c.ptyIn)
		}
		if c.ptyOut != 0 {
			_ = windows.CloseHandle(c.ptyOut)
		}
	})
	return nil
}

func (c *conptyConn) closeConsole() {
	c.closeConsoleOnce.Do(func() {
		if c.inherited != nil {
			for _, h := range []windows.Handle{
				c.inherited.hSignal,
				c.inherited.hPtyReference,
				c.inherited.hConPtyProcess,
			} {
				if h != 0 {
					_ = windows.CloseHandle(h)
				}
			}
			c.inherited.hSignal = 0
			c.inherited.hPtyReference = 0
			c.inherited.hConPtyProcess = 0
			return
		}
		if c.hpc != 0 {
			_, _, _ = procClosePseudoConsole.Call(uintptr(c.hpc))
		}
	})
}

// StopRead 取消挂起的 ReadFile，让 pump 退出并关闭 outCh；不关闭控制台/句柄。
// CancelIoEx 只取消调用瞬间已挂起的 IO，因此循环重试直到 pump 真正退出。
func (c *conptyConn) StopRead() error {
	c.readStopped.Store(true)
	deadline := time.Now().Add(2 * time.Second)
	for {
		_ = windows.CancelIoEx(windows.Handle(c.cmdOutFile.Fd()), nil)
		select {
		case <-c.pumpDone:
			return nil
		case <-c.done:
			return nil
		default:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("停止 ConPTY 读取超时")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// RestartRead 在交接失败时重新启动 pump，继续使用原 ConPTY 句柄。
func (c *conptyConn) RestartRead() error {
	select {
	case <-c.done:
		return io.EOF
	default:
	}
	select {
	case <-c.pumpDone:
	case <-time.After(2 * time.Second):
		return fmt.Errorf("旧 pump 未退出，无法恢复读取")
	}
	c.pending = nil
	c.outCh = make(chan []byte, 64)
	c.pumpDone = make(chan struct{})
	c.outOnce = sync.Once{}
	c.readStopped.Store(false)
	go c.pump()
	return nil
}

func (c *conptyConn) Resize(cols, rows int) error {
	select {
	case <-c.done:
		return nil
	default:
	}
	packed := uint32(uint16(rows))<<16 | uint32(uint16(cols))
	hr, _, _ := procResizePseudoConsole.Call(uintptr(c.hpc), uintptr(packed))
	if hr != 0 {
		return fmt.Errorf("ResizePseudoConsole 失败: HRESULT 0x%08X", uint32(hr))
	}
	return nil
}

func (c *conptyConn) Kill() error {
	select {
	case <-c.done:
		return nil
	default:
	}
	return windows.TerminateProcess(c.pi.Process, 1)
}

func (c *conptyConn) Pid() int {
	return int(c.pi.ProcessId)
}

func (c *conptyConn) Done() <-chan struct{} {
	return c.done
}

func (c *conptyConn) ExitCode() int {
	select {
	case <-c.done:
		return c.exitCode
	default:
		return 0
	}
}

// ---------- 辅助 ----------

func initAttrList(count uint32) (uintptr, []byte, error) {
	var size uintptr
	r1, _, e1 := procInitializeProcThreadAttributeList.Call(0, uintptr(count), 0, uintptr(unsafe.Pointer(&size)))
	if r1 == 0 && e1 != windows.ERROR_INSUFFICIENT_BUFFER {
		return 0, nil, fmt.Errorf("InitializeProcThreadAttributeList(大小查询): %w", e1)
	}
	buf := make([]byte, size)
	r1, _, e1 = procInitializeProcThreadAttributeList.Call(
		uintptr(unsafe.Pointer(&buf[0])), uintptr(count), 0, uintptr(unsafe.Pointer(&size)))
	if r1 == 0 {
		return 0, nil, fmt.Errorf("InitializeProcThreadAttributeList: %w", e1)
	}
	return uintptr(unsafe.Pointer(&buf[0])), buf, nil
}

func updateAttr(list uintptr, attr, value uintptr) error {
	r1, _, e1 := procUpdateProcThreadAttribute.Call(list, 0, attr, value, unsafe.Sizeof(value), 0, 0)
	if r1 == 0 {
		return fmt.Errorf("UpdateProcThreadAttribute: %w", e1)
	}
	return nil
}

func deleteAttrList(list uintptr) {
	_, _, _ = procDeleteProcThreadAttributeList.Call(list, 0, 0)
}

func quoteCmdLine(app string) string {
	if strings.ContainsAny(app, " \t") && !strings.HasPrefix(app, `"`) && !strings.HasSuffix(app, `"`) {
		return `"` + app + `"`
	}
	return app
}

// ---------- 平台默认 shell ----------

func defaultShell() (string, error) {
	for _, sh := range []string{"pwsh", "powershell"} {
		if p, err := exec.LookPath(sh); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("未找到可用的 Windows shell（已尝试 pwsh、powershell）")
}

// spawnShell 拉起 shell（cfg.Shell 或 pwsh→powershell）。
func spawnShell(cfg Config, name string, cols, rows int) (Pty, error) {
	shell := cfg.Shell
	if shell == "" {
		var err error
		shell, err = defaultShell()
		if err != nil {
			return nil, err
		}
	}
	cwd := cfg.Cwd
	if cwd == "" {
		cwd, _ = os.UserHomeDir()
	}
	p, err := startConpty(shell, cwd, cols, rows)
	if err != nil {
		return nil, err
	}
	appLog.Info("pty.spawn", "session", name, "pid", p.Pid(), "shell", shell,
		"cwd", cwd, "cols", cols, "rows", rows)
	return p, nil
}
