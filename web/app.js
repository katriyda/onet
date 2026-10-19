/* onet — 前端 UI 与 WS 协议逻辑（双模式自适应：移动 termux 风格 / 桌面经典终端） */
'use strict';
(() => {

const $ = (sel, root = document) => root.querySelector(sel);
const clamp = (v, lo, hi) => Math.max(lo, Math.min(hi, v));
const icon = (name, cls = '') =>
  `<svg class="icon ${cls}" aria-hidden="true"><use href="#i-${name}"/></svg>`;

/* ---------------- 状态 ---------------- */
let cfg = null;
let mode = 'mobile';
let ws = null;
let term = null;
let fit = null;
let sessions = [];
let current = null;          // {id, name}
let pendingAttach = null;    // attach 请求已发出、attached 未到：期间接收重放输出
let booting = true;
let everConnected = false;
let keysVisible = true;
let reconnectDelay = 1000;
let reconnectTimer = null;
let disconnected = false;
let modState = { ctrl: 0, alt: 0 }; // 0=off 1=点按(仅下一次) 2=长按锁定
let unseenOutput = false;

let xtermViewport = null;
let lastFitKey = '';
let resizeTimer = null;

let menuOpen = false;
let menuTimer = null;
let drawerTimer = null;
let toastTimer = null;

let selection = { active: false, anchor: 0, focus: 0 };
let gesture = null;
let longPressTimer = null;
let momentumRaf = 0;
let lastScrollAt = 0;
let suppressClickUntil = 0;

/* ---------------- DOM ---------------- */
const app = $('#app');
const termEl = $('#term');
const placeholder = $('#placeholder');
const touchLayer = $('#touch-layer');
const mName = $('#m-sess-name'), dName = $('#d-sess-name');
const mDot = $('#m-dot'), dDot = $('#d-dot');
const exitBanner = $('#exit-banner'), exitText = $('#exit-text');
const keysBar = $('#keys');
const drawer = $('#drawer'), drawerList = $('#drawer-list'), drawerSub = $('#drawer-sub'), scrim = $('#scrim');
const dTabs = $('#d-tabs'), dKeysBtn = $('#d-keys-btn');
const menuLayer = $('#menu-layer'), menuEl = $('#menu'), menuScrim = $('#menu-scrim');
const scrollPill = $('#scroll-pill'), scrollPillText = $('#scroll-pill-text');
const selectBar = $('#select-bar'), selectInfo = $('#select-info');
const login = $('#login'), loginForm = $('#login-form'), loginErr = $('#login-err');
const toast = $('#toast');

/* ---------------- 启动 ---------------- */
async function boot() {
  const qs = new URLSearchParams(location.search);

  // 深链带 ?token= → 自动登录并去掉 URL 中的 token
  if (qs.get('token')) {
    const r = await fetch('/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ token: qs.get('token') }),
    });
    if (r.ok) {
      qs.delete('token');
      location.replace(location.pathname + (qs.toString() ? '?' + qs : ''));
      return;
    }
  }

  let resp;
  try {
    resp = await fetch('/config');
  } catch {
    showToast('无法连接服务器', 3000);
    return;
  }
  if (resp.status === 401) { showLogin(); return; }
  if (!resp.ok) { showToast('配置加载失败 (' + resp.status + ')', 3000); return; }

  cfg = await resp.json();
  applyMode();
  initTerm();
  connectWS();
}

function showLogin() {
  login.hidden = false;
  loginForm.addEventListener('submit', async (e) => {
    e.preventDefault();
    loginErr.hidden = true;
    const r = await fetch('/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ token: $('#login-token').value }),
    });
    if (r.ok) { location.reload(); }
    else { loginErr.textContent = '令牌错误'; loginErr.hidden = false; }
  });
  setTimeout(() => $('#login-token').focus(), 50);
}

/* ---------------- 模式判定 ---------------- */
function detectMode() {
  if (cfg.ui === 'mobile' || cfg.ui === 'desktop') return cfg.ui;
  const fineWide = matchMedia('(pointer: fine)').matches && matchMedia('(min-width: 768px)').matches;
  return fineWide ? 'desktop' : 'mobile';
}

function applyMode() {
  const next = detectMode();
  const changed = next !== mode;
  mode = next;
  document.body.classList.toggle('mode-mobile', mode === 'mobile');
  document.body.classList.toggle('mode-desktop', mode === 'desktop');

  if (changed) {
    // 没有显式偏好时跟随模式：移动端默认显示按键栏，桌面端默认隐藏
    const pref = localStorage.getItem('onet_keys');
    keysVisible = pref === '1' ? true : pref === '0' ? false : mode === 'mobile';
    if (term) renderKeys();
  }
  updateKeysVisibility();
  layout();
}

function updateKeysVisibility() {
  const show = !!(keysVisible && term);
  keysBar.classList.toggle('show', show);
  dKeysBtn.classList.toggle('active', keysVisible);
  layout();
}

matchMedia('(pointer: coarse)').addEventListener('change', applyMode);
matchMedia('(pointer: fine)').addEventListener('change', applyMode);
window.addEventListener('resize', () => { applyMode(); });

/* ---------------- 布局 / 尺寸 ---------------- */
let layoutRaf = 0;
function scheduleLayout() {
  if (layoutRaf) return;
  layoutRaf = requestAnimationFrame(() => {
    layoutRaf = 0;
    layout();
  });
}

function layout() {
  const vv = window.visualViewport;
  if (vv) {
    app.style.height = vv.height + 'px';
    document.documentElement.style.setProperty('--vv-top', vv.offsetTop + 'px');
    document.documentElement.style.setProperty('--vvh', vv.height + 'px');
  } else {
    app.style.height = '';
    document.documentElement.style.removeProperty('--vv-top');
    document.documentElement.style.removeProperty('--vvh');
  }
  fitTerm();
  updateScrollPill();
}

function fitTerm(force = false) {
  if (!fit || !term) return;
  const key = termEl.clientWidth + 'x' + termEl.clientHeight + 'x' + term.options.fontSize;
  if (!force && key === lastFitKey) return;
  lastFitKey = key;
  try { fit.fit(); } catch { /* 隐藏或零尺寸时忽略 */ }
}

function sendResize() {
  if (!current || !term) return;
  sendCmd({ type: 'resize', id: current.id, cols: term.cols, rows: term.rows });
}

function scheduleResize() {
  clearTimeout(resizeTimer);
  resizeTimer = setTimeout(sendResize, 80);
}

/* ---------------- 软键盘保持（点按界面不收起输入法） ---------------- */
// 点按界面控件时焦点不能离开 xterm 的 textarea，否则移动端输入法会收起。
// pointerdown 阻止默认对 Chromium 有效；iOS 需 mousedown（WebKit bug 322721：
// 元素挂了 click 监听时 pointerdown 的 preventDefault 挡不住焦点转移）。
const CHROME_SEL = '#header, #keys, #drawer, #scrim, #menu-layer, #select-bar, #placeholder, #scroll-pill, #touch-layer';
let focusStolen = false;

function keepKeyboard(ev) {
  if (!term || document.activeElement !== term.textarea) return; // 键盘没弹起，不干预
  const t = ev.target;
  if (!(t instanceof Element) || !t.closest(CHROME_SEL)) return;
  ev.preventDefault();
  if (focusStolen) return;
  focusStolen = true;
  document.addEventListener('pointerup', restoreKeyboard, { capture: true });
  document.addEventListener('pointercancel', restoreKeyboard, { capture: true });
}
// 兜底：个别浏览器仍丢掉焦点时，抬手瞬间把焦点还给终端（iOS 允许在手势内重新聚焦）
function restoreKeyboard() {
  document.removeEventListener('pointerup', restoreKeyboard, { capture: true });
  document.removeEventListener('pointercancel', restoreKeyboard, { capture: true });
  focusStolen = false;
  if (term && document.activeElement !== term.textarea) term.focus();
}
document.addEventListener('pointerdown', keepKeyboard, { capture: true });
document.addEventListener('mousedown', keepKeyboard, { capture: true });

// 界面动作统一走 pointer 事件（阻止默认后 click 在触屏上未必派发）。
// 滑动超过 10px（滚动/拖拽）不算点按；鼠标划出元素取消；键盘 Enter/Space 的 click（detail === 0）保留。
let lastTapAt = 0;
function onTap(el, fn) {
  let down = null;
  el.addEventListener('pointerdown', (ev) => {
    if (ev.pointerType === 'mouse' && ev.button !== 0) return;
    down = { x: ev.clientX, y: ev.clientY };
    el.classList.add('pressed');
  });
  el.addEventListener('pointerup', (ev) => {
    el.classList.remove('pressed');
    const start = down;
    down = null;
    if (!start || Math.hypot(ev.clientX - start.x, ev.clientY - start.y) > 10) return;
    lastTapAt = performance.now();
    fn(ev);
  });
  el.addEventListener('pointercancel', () => { down = null; el.classList.remove('pressed'); });
  el.addEventListener('pointerleave', (ev) => {
    if (ev.pointerType !== 'mouse') return;
    down = null;
    el.classList.remove('pressed');
  });
  el.addEventListener('click', (ev) => {
    if (ev.detail === 0 && performance.now() - lastTapAt > 700) fn(ev);
  });
}

function onPress(el, opts = {}) {
  let down = null, timer = null, longDone = false;
  const cancel = () => {
    clearTimeout(timer);
    timer = null;
    down = null;
    el.classList.remove('pressed');
  };
  el.addEventListener('pointerdown', (ev) => {
    if (ev.pointerType === 'mouse' && ev.button !== 0) return;
    down = { x: ev.clientX, y: ev.clientY };
    longDone = false;
    el.classList.add('pressed');
    if (opts.longPress) {
      timer = setTimeout(() => {
        timer = null;
        if (!down) return;
        longDone = true;
        el.classList.remove('pressed');
        opts.longPress({ clientX: down.x, clientY: down.y });
      }, opts.delay || 480);
    }
  });
  el.addEventListener('pointermove', (ev) => {
    if (!down) return;
    if (Math.hypot(ev.clientX - down.x, ev.clientY - down.y) > 10) cancel();
  });
  el.addEventListener('pointerup', (ev) => {
    if (!down) { el.classList.remove('pressed'); return; }
    const start = down;
    const wasLong = longDone;
    clearTimeout(timer);
    timer = null;
    down = null;
    el.classList.remove('pressed');
    if (wasLong) return;
    if (Math.hypot(ev.clientX - start.x, ev.clientY - start.y) > 10) return;
    lastTapAt = performance.now();
    if (opts.tap) opts.tap(ev);
  });
  el.addEventListener('pointercancel', cancel);
  el.addEventListener('pointerleave', (ev) => { if (ev.pointerType === 'mouse') cancel(); });
  el.addEventListener('click', (ev) => {
    if (ev.detail === 0 && !longDone && performance.now() - lastTapAt > 700 && opts.tap) opts.tap(ev);
  });
}

/* ---------------- xterm ---------------- */
function storedFontSize() {
  const n = parseInt(localStorage.getItem('onet_font') || '', 10);
  return clamp(Number.isFinite(n) ? n : (cfg.font_size || 15), 8, 32);
}

function initTerm() {
  term = new window.Terminal({
    fontSize: storedFontSize(),
    scrollback: cfg.scrollback || 5000,
    cursorBlink: true,
    allowProposedApi: true,
    theme: {
      background: '#0f0f13', foreground: '#e9e9ee',
      cursor: '#e9e9ee', cursorAccent: '#0f0f13',
      selectionBackground: '#365a9e', selectionInactiveBackground: '#2a3c5c',
    },
  });
  fit = new window.FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open(termEl);

  xtermViewport = term.element.querySelector('.xterm-viewport');
  if (xtermViewport) {
    xtermViewport.addEventListener('scroll', () => {
      if (atBottom()) unseenOutput = false;
      updateScrollPill();
    }, { passive: true });
  }

  term.onData((data) => {
    if (ws && ws.readyState === 1) ws.send(data); // 文本帧 = 原始输入
  });

  // 桌面快捷键
  term.attachCustomKeyEventHandler((ev) => {
    if (ev.type !== 'keydown') return true;
    if (mode !== 'desktop') return true;
    if (ev.ctrlKey && ev.shiftKey) {
      const k = ev.key.toLowerCase();
      if (k === 'c') { ev.preventDefault(); copySelectionOrAll(); return false; }
      if (k === 'v') { ev.preventDefault(); paste(); return false; }
      if (k === 'n') { ev.preventDefault(); createSession(); return false; }
      if (k === 'w') { ev.preventDefault(); removeCurrent(); return false; }
    }
    return true;
  });

  // Ctrl+滚轮调字号（桌面）
  termEl.addEventListener('wheel', (ev) => {
    if (!ev.ctrlKey || mode !== 'desktop') return;
    ev.preventDefault();
    setFontSize(term.options.fontSize + (ev.deltaY < 0 ? 1 : -1));
  }, { passive: false });

  // 右键菜单（桌面）
  termEl.addEventListener('contextmenu', (ev) => {
    if (mode !== 'desktop') return;
    ev.preventDefault();
    showMenu(terminalMenu(), { x: ev.clientX, y: ev.clientY });
  });

  term.onResize(() => { scheduleResize(); updateScrollPill(); });
  initTouchGestures();
  renderKeys();
  updateKeysVisibility();
  layout();

  if (window.visualViewport) {
    window.visualViewport.addEventListener('resize', scheduleLayout);
    window.visualViewport.addEventListener('scroll', scheduleLayout);
  }
}

function setFontSize(n, opts = {}) {
  if (!term) return;
  const fs = clamp(Math.round(n), 8, 32);
  if (fs === term.options.fontSize) return;
  term.options.fontSize = fs;
  localStorage.setItem('onet_font', String(fs));
  lastFitKey = '';
  fitTerm();
  scheduleResize();
  if (!opts.quiet) showToast('字号 ' + fs, 900);
}

/* ---------------- 滚动 / 选中 ---------------- */
function viewportEl() {
  if (!xtermViewport && term && term.element) {
    xtermViewport = term.element.querySelector('.xterm-viewport');
  }
  return xtermViewport;
}

function atBottom() {
  const v = viewportEl();
  if (!v) return true;
  return v.scrollTop + v.clientHeight >= v.scrollHeight - 2;
}

function isAltBuffer() {
  try { return term.buffer.active.type === 'alternate'; } catch { return false; }
}

function hasMouseTracking() {
  try { return term.modes.mouseTrackingMode !== 'none'; } catch { return false; }
}

function updateScrollPill() {
  if (!scrollPill) return;
  const show = !!(term && !isAltBuffer() && !atBottom());
  if (!show) {
    unseenOutput = false;
    scrollPill.hidden = true;
    return;
  }
  scrollPill.hidden = false;
  scrollPill.classList.toggle('new', unseenOutput);
  scrollPillText.textContent = unseenOutput ? '新输出 · 回到底部' : '回到底部';
}

function scrollToBottom() {
  if (!term) return;
  cancelMomentum();
  term.scrollToBottom();
  unseenOutput = false;
  updateScrollPill();
}

function screenRowHeight() {
  const screen = term && term.element && term.element.querySelector('.xterm-screen');
  if (!screen) return 18;
  return screen.getBoundingClientRect().height / Math.max(1, term.rows);
}

function lineAtPoint(clientY) {
  const screen = term && term.element && term.element.querySelector('.xterm-screen');
  if (!screen) return 0;
  const rect = screen.getBoundingClientRect();
  const rowH = rect.height / Math.max(1, term.rows);
  const row = clamp(Math.floor((clientY - rect.top) / rowH), 0, term.rows - 1);
  return clamp(term.buffer.active.viewportY + row, 0, term.buffer.active.length - 1);
}

function clearSelection() {
  if (term) {
    try { term.clearSelection(); } catch { /* noop */ }
  }
  selection = { active: false, anchor: 0, focus: 0 };
  selectBar.hidden = true;
}

function beginSelection(line) {
  selection = { active: true, anchor: line, focus: line };
  term.selectLines(line, line);
  updateSelectionBar();
  suppressClickUntil = performance.now() + 600;
}

function extendSelection(line) {
  if (!selection.active) return;
  selection.focus = line;
  term.selectLines(Math.min(selection.anchor, line), Math.max(selection.anchor, line));
  updateSelectionBar();
}

function updateSelectionBar() {
  if (!selection.active) { selectBar.hidden = true; return; }
  const count = Math.abs(selection.focus - selection.anchor) + 1;
  selectInfo.textContent = '已选 ' + count + ' 行';
  selectBar.hidden = false;
}

function autoScrollSelection(clientY) {
  if (!term) return;
  const screen = term.element.querySelector('.xterm-screen');
  if (!screen) return;
  const r = screen.getBoundingClientRect();
  const margin = screenRowHeight();
  if (clientY < r.top + margin) term.scrollLines(-1);
  else if (clientY > r.bottom - margin) term.scrollLines(1);
}

function copySelection() {
  if (!term) return;
  const text = term.getSelection();
  if (!text) { showToast('没有可复制的内容'); return; }
  const count = Math.abs(selection.focus - selection.anchor) + 1;
  writeClipboard(text)
    .then(() => { showToast('已复制 ' + count + ' 行'); clearSelection(); })
    .catch(() => showToast('复制失败'));
}

function selectAllBuffer() {
  if (!term) return;
  term.selectAll();
  selection = { active: true, anchor: 0, focus: Math.max(0, term.buffer.active.length - 1) };
  updateSelectionBar();
}

/* ---------------- 触屏手势（稳定层，避免 xterm 行节点替换导致丢事件） ---------------- */
const LONG_PRESS_MS = 420;
const EDGE_OPEN_PX = 22;

function touchDist(touches) {
  const dx = touches[0].clientX - touches[1].clientX;
  const dy = touches[0].clientY - touches[1].clientY;
  return Math.hypot(dx, dy);
}

function initTouchGestures() {
  touchLayer.addEventListener('touchstart', onTouchStart, { passive: false });
  touchLayer.addEventListener('touchmove', onTouchMove, { passive: false });
  touchLayer.addEventListener('touchend', onTouchEnd, { passive: false });
  touchLayer.addEventListener('touchcancel', onTouchCancel, { passive: false });
  touchLayer.addEventListener('contextmenu', (e) => e.preventDefault());
  touchLayer.addEventListener('click', onTouchLayerClick);
}

function cancelLongPress() {
  clearTimeout(longPressTimer);
  longPressTimer = null;
}

function onTouchStart(e) {
  if (mode !== 'mobile') return;
  cancelMomentum();

  if (selection.active) clearSelection();

  if (e.touches.length === 2) {
    cancelLongPress();
    gesture = {
      pinch: true,
      d: touchDist(e.touches),
      fs: term.options.fontSize,
    };
    e.preventDefault();
    return;
  }
  if (e.touches.length !== 1) return;

  const t = e.touches[0];
  const vp = viewportEl();
  gesture = {
    pinch: false,
    startX: t.clientX,
    startY: t.clientY,
    lastX: t.clientX,
    lastY: t.clientY,
    lastT: performance.now(),
    scrollStart: vp ? vp.scrollTop : 0,
    moved: false,
    scrolling: false,
    selecting: false,
    ignored: false,
    edge: t.clientX < EDGE_OPEN_PX,
    velocity: 0,
    wheelAccum: 0,
  };

  longPressTimer = setTimeout(() => {
    if (!gesture || gesture.moved || gesture.pinch) return;
    gesture.selecting = true;
    gesture.anchor = lineAtPoint(gesture.startY);
    beginSelection(gesture.anchor);
    navigator.vibrate?.(12);
  }, LONG_PRESS_MS);
}

function onTouchMove(e) {
  if (mode !== 'mobile' || !gesture) return;

  if (gesture.pinch) {
    if (e.touches.length === 2) {
      e.preventDefault();
      const ratio = touchDist(e.touches) / gesture.d;
      const fs = clamp(Math.round(gesture.fs * ratio), 8, 32);
      if (fs !== term.options.fontSize) setFontSize(fs, { quiet: true });
    }
    return;
  }

  if (e.touches.length !== 1) return;
  const t = e.touches[0];
  const dx = t.clientX - gesture.startX;
  const dy = t.clientY - gesture.startY;
  const now = performance.now();

  if (gesture.selecting) {
    e.preventDefault();
    if (now - lastScrollAt > 40) {
      autoScrollSelection(t.clientY);
      lastScrollAt = now;
    }
    extendSelection(lineAtPoint(t.clientY));
    suppressClickUntil = now + 600;
    return;
  }

  if (!gesture.moved && Math.hypot(dx, dy) > 6) {
    gesture.moved = true;
    cancelLongPress();
  }

  // 左缘右滑 → 打开会话抽屉
  if (gesture.edge && !gesture.moved) return;
  if (gesture.edge && dx > 8 && Math.abs(dx) > Math.abs(dy)) {
    if (dx > 36) {
      gesture.edge = false;
      gesture.ignored = true;
      cancelLongPress();
      openDrawer();
    }
    e.preventDefault();
    return;
  }

  if (!gesture.scrolling) {
    if (!gesture.moved) return;
    if (Math.abs(dy) >= Math.abs(dx)) gesture.scrolling = true;
    else { gesture.ignored = true; return; }
  }

  if (gesture.scrolling) {
    e.preventDefault();
    lastScrollAt = now;
    suppressClickUntil = now + 400;

    if (isAltBuffer() || hasMouseTracking()) {
      // 全屏 TUI（omp/less/vim…）：把拖拽翻译成滚轮，交给 xterm 处理
      // （无鼠标上报时 xterm 会转成 ↑/↓，有上报时发 SGR wheel）
      gesture.wheelAccum += t.clientY - gesture.lastY;
      const step = Math.max(12, screenRowHeight());
      while (Math.abs(gesture.wheelAccum) >= step) {
        const dir = gesture.wheelAccum > 0 ? 1 : -1;
        dispatchWheel(-dir * step, t.clientX, t.clientY);
        gesture.wheelAccum -= dir * step;
      }
    } else {
      const vp = viewportEl();
      if (vp) {
        const max = vp.scrollHeight - vp.clientHeight;
        vp.scrollTop = clamp(gesture.scrollStart - dy, 0, max);
        const dt = now - gesture.lastT;
        if (dt > 0) {
          const inst = (gesture.lastY - t.clientY) / dt; // 手指上滑为正
          gesture.velocity = gesture.velocity * 0.7 + inst * 0.3;
        }
      }
    }
    gesture.lastY = t.clientY;
    gesture.lastT = now;
  }
}

function onTouchEnd(e) {
  if (mode !== 'mobile') return;
  cancelLongPress();
  if (!gesture) return;
  const g = gesture;
  gesture = null;

  if (g.pinch) {
    showToast('字号 ' + term.options.fontSize, 800);
    return;
  }
  if (g.selecting) {
    e.preventDefault();
    suppressClickUntil = performance.now() + 600;
    return;
  }
  if (g.scrolling && !isAltBuffer() && !hasMouseTracking()) {
    if (Math.abs(g.velocity) > 0.08) startMomentum(g.velocity);
  }

  // 轻点终端：清掉可能残留的选择，或把焦点交回 xterm（软键盘弹出）
  if (!g.moved && !g.ignored && !g.selecting && !g.pinch && performance.now() >= suppressClickUntil) {
    if (selection.active) clearSelection();
    else if (term && current) term.focus();
  }
}

function onTouchCancel() {
  cancelLongPress();
  gesture = null;
}

function onTouchLayerClick() {
  if (mode !== 'mobile') return;
  if (performance.now() < suppressClickUntil) return;
  if (selection.active) { clearSelection(); return; }
  if (term && current) term.focus();
}

function dispatchWheel(deltaY, x, y) {
  if (!term || !term.element) return;
  term.element.dispatchEvent(new WheelEvent('wheel', {
    deltaY,
    deltaMode: 0,
    bubbles: true,
    cancelable: true,
    clientX: x,
    clientY: y,
    view: window,
  }));
}

function cancelMomentum() {
  if (momentumRaf) cancelAnimationFrame(momentumRaf);
  momentumRaf = 0;
}

function startMomentum(velocity) {
  cancelMomentum();
  let last = performance.now();
  const step = (now) => {
    const dt = Math.min(now - last, 40);
    last = now;
    const vp = viewportEl();
    if (!vp) { momentumRaf = 0; return; }
    const max = vp.scrollHeight - vp.clientHeight;
    const next = clamp(vp.scrollTop + velocity * dt, 0, max);
    if (next === vp.scrollTop || (next <= 0 && velocity < 0) || (next >= max && velocity > 0)) {
      momentumRaf = 0;
      return;
    }
    vp.scrollTop = next;
    velocity *= Math.pow(0.94, dt / 16.67);
    if (Math.abs(velocity) > 0.015) momentumRaf = requestAnimationFrame(step);
    else momentumRaf = 0;
  };
  momentumRaf = requestAnimationFrame(step);
}

/* ---------------- WS 协议 ---------------- */
function sendCmd(obj) {
  if (ws && ws.readyState === 1) {
    ws.send(new TextEncoder().encode(JSON.stringify(obj))); // 二进制帧 = JSON 命令
  }
}

function connectWS() {
  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  const token = new URLSearchParams(location.search).get('token');
  ws = new WebSocket(proto + '://' + location.host + '/ws' + (token ? '?token=' + encodeURIComponent(token) : ''));
  ws.binaryType = 'arraybuffer';

  ws.onopen = () => {
    const reconnected = everConnected;
    everConnected = true;
    reconnectDelay = 1000;
    booting = true;
    if (reconnected && term) {
      // 断线重连：服务端会重放 64KB 缓冲，先清屏避免内容重复
      current = null;
      pendingAttach = null;
      term.reset();
      clearSelection();
      unseenOutput = false;
      updateScrollPill();
    }
    if (disconnected) {
      disconnected = false;
      showToast('已重连', 1200);
    }
    sendCmd({ type: 'list' });
  };
  ws.onmessage = (ev) => {
    if (typeof ev.data === 'string') return; // 服务器只发二进制
    handleMsg(ev.data);
  };
  ws.onclose = () => {
    current = null;
    pendingAttach = null;
    clearSelection();
    updateStatus();
    setExitBanner(null);
    renderSessions();
    if (!disconnected) {
      disconnected = true;
      showToast('连接断开，重连中…', 2200);
    }
    clearTimeout(reconnectTimer);
    reconnectTimer = setTimeout(connectWS, reconnectDelay);
    reconnectDelay = Math.min(reconnectDelay * 2, 10000);
  };
  ws.onerror = () => ws.close();
}

function b64ToBytes(b64) {
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

function handleMsg(buf) {
  let msg;
  try { msg = JSON.parse(new TextDecoder().decode(buf)); } catch { return; }
  switch (msg.type) {
    case 'output': {
      const id = current ? current.id : (pendingAttach && pendingAttach.id);
      if (id && msg.id === id && term) {
        if (!atBottom()) unseenOutput = true;
        term.write(b64ToBytes(msg.data), () => updateScrollPill());
      }
      break;
    }
    case 'exit':
      showToast('会话 ' + msg.id + ' 已退出 (code ' + msg.code + ')', 2000);
      break;
    case 'sessions':
      onSessions(msg.sessions);
      break;
    case 'attached':
      if (pendingAttach && pendingAttach.id !== msg.id) break; // 忽略过期的 attach 响应
      pendingAttach = null;
      current = {
        id: msg.id,
        name: findSession(msg.id) ? (findSession(msg.id).name || findSession(msg.id).id) : msg.id,
      };
      placeholder.hidden = true;
      clearSelection();
      unseenOutput = false;
      updateStatus();
      renderSessions();
      fitTerm(true);
      scheduleResize();
      setTimeout(() => {
        if (current && current.id === msg.id) {
          scrollToBottom();
          updateScrollPill();
        }
      }, 40);
      break;
    case 'created':
      attachTo(msg.id);
      break;
    case 'detached':
      current = null;
      pendingAttach = null;
      placeholder.hidden = false;
      clearSelection();
      updateStatus();
      renderSessions();
      break;
    case 'error':
      if (booting && /已退出|不存在/.test(msg.message)) {
        booting = false;
        pendingAttach = null;
        sendCmd({ type: 'create', name: '' });
      } else {
        if (pendingAttach) {
          pendingAttach = null;
          const target = pickTarget();
          if (target) attachTo(target.id);
          else placeholder.hidden = false;
          updateStatus();
          renderSessions();
        }
        showToast(msg.message, 2500);
      }
      break;
  }
}

function findSession(id) {
  return sessions.find((s) => s.id === id) || null;
}

function onSessions(list) {
  sessions = Array.isArray(list) ? list : [];
  renderSessions();

  if (booting) {
    booting = false;
    const target = pickTarget();
    if (target) attachTo(target.id);
    else sendCmd({ type: 'create', name: '' });
    return;
  }

  if (pendingAttach) {
    // attach 请求在途；如果目标已被删除，交给 error 分支处理
    return;
  }

  const cur = current && findSession(current.id);
  if (!cur) {
    const hadCurrent = !!current;
    current = null;
    clearSelection();
    if (hadCurrent) {
      // 当前会话被移除：自动切到最近的存活会话，避免停在空白页
      const target = pickTarget();
      if (target) attachTo(target.id);
      else placeholder.hidden = false;
    } else if (!sessions.length) {
      placeholder.hidden = true;
      createSession();
    }
    updateStatus();
    setExitBanner(null);
  } else {
    updateStatus();
    setExitBanner(cur.dead ? cur : null);
  }
}

function pickTarget() {
  const q = new URLSearchParams(location.search).get('s');
  if (q) {
    const m = sessions.find((s) => s.id === q || s.name === q);
    if (m && !m.dead) return m;
  }
  for (let i = sessions.length - 1; i >= 0; i--) {
    if (!sessions[i].dead) return sessions[i];
  }
  return null;
}

/* ---------------- 会话操作 ---------------- */
function createSession() {
  sendCmd({ type: 'create', name: '' });
}

function attachTo(id) {
  if (!term) return;
  cancelMomentum();
  cancelLongPress();
  gesture = null;
  pendingAttach = { id };
  current = null;
  term.reset();
  clearSelection();
  unseenOutput = false;
  placeholder.hidden = true;
  updateStatus();
  updateScrollPill();
  sendCmd({ type: 'attach', id });
  if (mode === 'desktop' && term) term.focus();
}

function switchTo(s) {
  if (!s) return;
  if (s.dead) { showSessionMenu(s); return; }
  if (s.id === (current && current.id)) return;
  attachTo(s.id);
}

function restartSession(id) {
  if (current && current.id === id && term) {
    term.reset();
    clearSelection();
    unseenOutput = false;
    updateScrollPill();
  }
  sendCmd({ type: 'restart', id });
  if (current && current.id === id) setExitBanner(null);
}

function removeSession(id) {
  const s = findSession(id);
  if (s && !s.dead) {
    showMenu([
      { label: '取消', fn: () => {} },
      { label: '删除会话', icon: 'trash', danger: true, fn: () => sendCmd({ type: 'remove', id }) },
    ], {
      sheet: mode === 'mobile',
      title: '删除正在运行的会话？',
      message: '「' + (s.name || s.id) + '」的进程会被终止。',
    });
    return;
  }
  sendCmd({ type: 'remove', id });
}

function removeCurrent() {
  if (current) removeSession(current.id);
}

/* ---------------- 渲染 ---------------- */
function updateStatus() {
  const cur = current && findSession(current.id);
  const name = cur ? (cur.name || cur.id) : (current ? current.name : '—');
  const alive = cur ? !cur.dead : false;
  mName.textContent = name;
  dName.textContent = name;
  const cls = cur ? (alive ? 'on' : 'off') : 'off';
  mDot.className = 'dot ' + cls;
  dDot.className = 'dot ' + cls;
  document.title = name && name !== '—' ? name + ' — onet' : 'onet';
  setExitBanner(cur && cur.dead ? cur : null);
}

function setExitBanner(sess) {
  const wasHidden = exitBanner.hidden;
  if (!sess) {
    exitBanner.hidden = true;
  } else {
    exitText.textContent = '会话「' + (sess.name || sess.id) + '」已退出 (code ' + sess.exitCode + ')';
    exitBanner.hidden = false;
  }
  if (wasHidden !== exitBanner.hidden) layout();
}

function sessionMeta(s) {
  if (s.dead) return '已退出' + (s.exitCode ? ' · code ' + s.exitCode : '');
  return s.attached > 0 ? '运行中 · ' + s.attached + ' 在线' : '运行中';
}

function renderSessions() {
  renderTabs();
  renderDrawer();
  updateStatus();
}

function renderTabs() {
  dTabs.innerHTML = '';
  for (const s of sessions) {
    const active = s.id === (current && current.id);
    const tab = document.createElement('div');
    tab.className = 'tab' + (active ? ' active' : '') + (s.dead ? ' dead' : '');

    const main = document.createElement('button');
    main.type = 'button';
    main.className = 'tab-main';
    main.title = s.name || s.id;
    const dot = document.createElement('span');
    dot.className = 'dot ' + (s.dead ? 'off' : 'on');
    const name = document.createElement('span');
    name.className = 'tab-name';
    name.textContent = s.name || s.id;
    main.append(dot, name);
    onTap(main, () => switchTo(s));
    main.addEventListener('contextmenu', (ev) => {
      ev.preventDefault();
      showSessionMenu(s, { x: ev.clientX, y: ev.clientY });
    });

    const more = document.createElement('button');
    more.type = 'button';
    more.className = 'tab-more';
    more.setAttribute('aria-label', '会话操作');
    more.innerHTML = icon('more');
    onTap(more, () => showSessionMenu(s, { anchor: more }));

    tab.append(main, more);
    dTabs.appendChild(tab);
  }
  requestAnimationFrame(() => {
    const a = dTabs.querySelector('.tab.active');
    if (a) a.scrollIntoView({ block: 'nearest', inline: 'nearest' });
  });
}

function renderDrawer() {
  drawerSub.textContent = sessions.length + ' 个会话';
  drawerList.innerHTML = '';
  if (!sessions.length) {
    const li = document.createElement('li');
    li.className = 'drawer-empty';
    li.textContent = '还没有会话';
    drawerList.appendChild(li);
    return;
  }
  for (const s of sessions) {
    const active = s.id === (current && current.id);
    const li = document.createElement('li');
    li.className = 'session-row' + (active ? ' active' : '') + (s.dead ? ' dead' : '');
    if (active) li.setAttribute('aria-current', 'true');

    const main = document.createElement('button');
    main.type = 'button';
    main.className = 'session-main';
    const dot = document.createElement('span');
    dot.className = 'dot ' + (s.dead ? 'off' : 'on');
    const copy = document.createElement('span');
    copy.className = 'session-copy';
    const name = document.createElement('span');
    name.className = 'session-name';
    name.textContent = s.name || s.id;
    const meta = document.createElement('span');
    meta.className = 'session-meta';
    meta.textContent = sessionMeta(s);
    copy.append(name, meta);
    main.append(dot, copy);
    onPress(main, {
      tap: () => {
        if (s.dead) { showSessionMenu(s); return; }
        closeDrawer();
        switchTo(s);
      },
      longPress: () => showSessionMenu(s),
    });

    const more = document.createElement('button');
    more.type = 'button';
    more.className = 'session-more';
    more.setAttribute('aria-label', '会话操作');
    more.innerHTML = icon('more');
    onTap(more, () => showSessionMenu(s));

    li.append(main, more);
    drawerList.appendChild(li);
  }
}

/* ---------------- 抽屉 ---------------- */
function openDrawer() {
  if (mode !== 'mobile') return;
  cancelMomentum();
  hideMenu(true);
  clearTimeout(drawerTimer);
  drawer.hidden = false;
  scrim.hidden = false;
  void drawer.offsetWidth; // 强制回流，保证过渡从关闭位置开始
  drawer.classList.add('open');
  scrim.classList.add('open');
}

function closeDrawer() {
  if (drawer.hidden) return;
  drawer.classList.remove('open');
  scrim.classList.remove('open');
  clearTimeout(drawerTimer);
  drawerTimer = setTimeout(() => {
    drawer.hidden = true;
    scrim.hidden = true;
  }, 240);
}

onTap($('#m-menu-btn'), openDrawer);
onTap($('#m-title'), openDrawer);
onTap($('#drawer-close'), closeDrawer);
onTap($('#drawer-new'), () => { closeDrawer(); createSession(); });
onTap(scrim, closeDrawer);

// 抽屉内左滑关闭
let drawerSwipe = null;
drawer.addEventListener('touchstart', (e) => {
  if (e.touches.length !== 1) return;
  drawerSwipe = { x: e.touches[0].clientX, y: e.touches[0].clientY };
}, { passive: true });
drawer.addEventListener('touchmove', (e) => {
  if (!drawerSwipe || e.touches.length !== 1) return;
  const dx = e.touches[0].clientX - drawerSwipe.x;
  const dy = e.touches[0].clientY - drawerSwipe.y;
  if (dx < -28 && Math.abs(dx) > Math.abs(dy) * 1.4) {
    drawerSwipe = null;
    closeDrawer();
  }
}, { passive: true });
drawer.addEventListener('touchend', () => { drawerSwipe = null; }, { passive: true });

/* ---------------- 菜单（移动底部表 / 桌面浮层） ---------------- */
function showSessionMenu(s, opts = {}) {
  showMenu(sessionMenu(s), {
    title: s.name || s.id,
    message: sessionMeta(s),
    ...opts,
    sheet: opts.sheet !== undefined ? opts.sheet : mode === 'mobile',
  });
}

function showMenu(items, opts = {}) {
  hideMenu(true);
  const sheet = opts.sheet !== undefined ? opts.sheet : mode === 'mobile';
  menuLayer.className = sheet ? 'sheet' : 'popover';
  menuLayer.hidden = false;
  menuEl.innerHTML = '';

  if (opts.title || opts.message) {
    const head = document.createElement('div');
    head.className = 'menu-head';
    if (opts.title) {
      const t = document.createElement('div');
      t.className = 'menu-title';
      t.textContent = opts.title;
      head.appendChild(t);
    }
    if (opts.message) {
      const m = document.createElement('div');
      m.className = 'menu-sub';
      m.textContent = opts.message;
      head.appendChild(m);
    }
    menuEl.appendChild(head);
  }

  for (const item of items) {
    if (!item) continue;
    if (item.type === 'separator') {
      const sep = document.createElement('div');
      sep.className = 'menu-sep';
      menuEl.appendChild(sep);
    } else if (item.type === 'stepper') {
      menuEl.appendChild(buildStepper(item));
    } else {
      menuEl.appendChild(buildMenuItem(item));
    }
  }

  if (!sheet) {
    positionMenu(opts);
  } else {
    menuEl.style.left = '';
    menuEl.style.top = '';
  }

  void menuEl.offsetWidth; // 强制回流，保证过渡从关闭位置开始
  menuLayer.classList.add('open');
  menuOpen = true;
  document.addEventListener('keydown', onMenuKey);
}

function buildMenuItem(item) {
  const b = document.createElement('button');
  b.type = 'button';
  b.className = 'menu-item' + (item.danger ? ' danger' : '');
  b.setAttribute('role', 'menuitem');
  if (item.disabled) b.disabled = true;
  if (item.icon) b.insertAdjacentHTML('beforeend', icon(item.icon));
  const label = document.createElement('span');
  label.className = 'mi-label';
  label.textContent = item.label;
  b.appendChild(label);
  if (item.hint) {
    const hint = document.createElement('span');
    hint.className = 'mi-hint';
    hint.textContent = item.hint;
    b.appendChild(hint);
  }
  if (item.checked) b.insertAdjacentHTML('beforeend', icon('check', 'mi-check'));
  if (item.fn && !item.disabled) {
    onTap(b, () => {
      hideMenu();
      item.fn();
      if (mode === 'desktop' && term) {
        setTimeout(() => { if (!menuOpen) term.focus(); }, 0);
      }
    });
  }
  return b;
}

function buildStepper(item) {
  const row = document.createElement('div');
  row.className = 'menu-item menu-stepper';
  row.setAttribute('role', 'group');
  row.insertAdjacentHTML('beforeend', icon('font'));
  const label = document.createElement('span');
  label.className = 'mi-label';
  label.textContent = item.label || '字号';
  row.appendChild(label);

  const stepper = document.createElement('div');
  stepper.className = 'stepper';
  const dec = document.createElement('button');
  dec.type = 'button';
  dec.className = 'step-btn';
  dec.setAttribute('aria-label', '减小字号');
  dec.innerHTML = icon('minus');
  const val = document.createElement('span');
  val.className = 'stepper-val';
  val.textContent = term.options.fontSize;
  const inc = document.createElement('button');
  inc.type = 'button';
  inc.className = 'step-btn';
  inc.setAttribute('aria-label', '增大字号');
  inc.innerHTML = icon('plus');

  onTap(dec, () => {
    setFontSize(term.options.fontSize - 1, { quiet: true });
    val.textContent = term.options.fontSize;
  });
  onTap(inc, () => {
    setFontSize(term.options.fontSize + 1, { quiet: true });
    val.textContent = term.options.fontSize;
  });

  stepper.append(dec, val, inc);
  row.appendChild(stepper);
  return row;
}

function positionMenu(opts) {
  menuEl.style.left = '0px';
  menuEl.style.top = '0px';
  const vv = window.visualViewport;
  const ox = vv ? vv.offsetLeft : 0;
  const oy = vv ? vv.offsetTop : 0;
  const vw = vv ? vv.width : innerWidth;
  const vh = vv ? vv.height : innerHeight;
  const mw = menuEl.offsetWidth;
  const mh = menuEl.offsetHeight;
  let x, y;
  if (opts.anchor) {
    const r = opts.anchor.getBoundingClientRect();
    x = r.left - ox;
    y = r.bottom + 6 - oy;
    if (opts.align === 'right') x = r.right - mw - ox;
  } else if (opts.x !== undefined || opts.y !== undefined) {
    x = (opts.x !== undefined ? opts.x : 8) - ox;
    y = (opts.y !== undefined ? opts.y : 8) - oy;
  } else {
    // 没有锚点时居中（删除确认等）
    x = (vw - mw) / 2;
    y = (vh - mh) / 2;
  }
  const pad = 8;
  menuEl.style.left = clamp(x, pad, Math.max(pad, vw - mw - pad)) + 'px';
  menuEl.style.top = clamp(y, pad, Math.max(pad, vh - mh - pad)) + 'px';
}

function hideMenu(immediate = false) {
  if (!menuOpen && menuLayer.hidden) return;
  menuOpen = false;
  document.removeEventListener('keydown', onMenuKey);
  menuLayer.classList.remove('open');
  clearTimeout(menuTimer);
  if (immediate) {
    menuLayer.hidden = true;
    menuEl.innerHTML = '';
  } else {
    menuTimer = setTimeout(() => {
      menuLayer.hidden = true;
      menuEl.innerHTML = '';
    }, 200);
  }
}

function onMenuKey(e) {
  if (e.key === 'Escape') {
    e.preventDefault();
    hideMenu();
  }
}

document.addEventListener('keydown', (e) => {
  if (e.key !== 'Escape' || menuOpen) return;
  if (!drawer.hidden) { e.preventDefault(); closeDrawer(); }
  else if (selection.active) { e.preventDefault(); clearSelection(); }
});

menuScrim.addEventListener('pointerdown', (e) => {
  e.preventDefault();
  hideMenu();
});

function terminalMenu() {
  if (!term) return [];
  const items = [];
  if (!atBottom() && !isAltBuffer()) {
    items.push({ label: '回到底部', icon: 'arrow-down', fn: scrollToBottom });
  }
  const hasSel = term && term.hasSelection();
  items.push({ label: hasSel ? '复制选中' : '复制全部', icon: 'copy', fn: copySelectionOrAll });
  items.push({ label: '粘贴', icon: 'paste', fn: paste });
  items.push({ type: 'separator' });
  items.push({ type: 'stepper' });
  items.push({
    label: '重置终端',
    icon: 'refresh',
    fn: () => { term.reset(); clearSelection(); unseenOutput = false; updateScrollPill(); },
  });
  items.push({
    label: keysVisible ? '隐藏按键栏' : '显示按键栏',
    icon: 'keyboard',
    checked: keysVisible,
    fn: toggleKeys,
  });
  return items;
}

function sessionMenu(s) {
  const items = [];
  if (s.dead) items.push({ label: '重启会话', icon: 'refresh', fn: () => restartSession(s.id) });
  items.push({ label: '删除会话', icon: 'trash', danger: true, fn: () => removeSession(s.id) });
  return items;
}

function toggleKeys() {
  keysVisible = !keysVisible;
  localStorage.setItem('onet_keys', keysVisible ? '1' : '0');
  updateKeysVisibility();
}

/* ---------------- 剪贴板 ---------------- */
function writeClipboard(text) {
  if (navigator.clipboard && window.isSecureContext) {
    return navigator.clipboard.writeText(text).catch(() => legacyCopy(text));
  }
  return legacyCopy(text);
}
function legacyCopy(text) {
  const ta = document.createElement('textarea');
  ta.value = text;
  ta.style.cssText = 'position:fixed;opacity:0;left:-9999px';
  document.body.appendChild(ta);
  ta.select();
  let ok = false;
  try { ok = document.execCommand('copy'); } catch { /* noop */ }
  ta.remove();
  return ok ? Promise.resolve() : Promise.reject(new Error('copy failed'));
}
function copyAll() {
  if (!term) return;
  term.selectAll();
  const sel = term.getSelection();
  term.clearSelection();
  if (sel) writeClipboard(sel).then(() => showToast('已复制全部')).catch(() => showToast('复制失败'));
}
function copySelectionOrAll() {
  if (!term) return;
  if (term.hasSelection()) {
    const text = term.getSelection();
    writeClipboard(text).then(() => {
      showToast('已复制');
      term.clearSelection();
      selection = { active: false, anchor: 0, focus: 0 };
      selectBar.hidden = true;
    }).catch(() => showToast('复制失败'));
  } else {
    copyAll();
  }
}
async function paste() {
  if (!term) return;
  try {
    const text = await navigator.clipboard.readText();
    term.paste(text);
  } catch {
    showToast('无法读取剪贴板，请用系统粘贴');
  }
}

/* ---------------- 额外按键栏 ---------------- */
const DEFAULT_KEYS = ['ESC', 'TAB', 'CTRL', 'ALT', '^C', 'ENTER', '←', '↑', '↓', '→', 'PGUP', 'PGDN'];
const KEY_LABELS = {
  ENTER: 'Enter', RETURN: 'Enter', PGUP: 'PgUp', PGDN: 'PgDn',
  HOME: 'Home', END: 'End', BS: '⌫', DEL: 'Del', '^C': '^C',
};
const REPEAT_KEYS = new Set(['←', '↑', '↓', '→']);

function renderKeys() {
  keysBar.innerHTML = '';
  const keys = (cfg && cfg.extra_keys && cfg.extra_keys.length) ? cfg.extra_keys : DEFAULT_KEYS;
  keysBar.style.setProperty('--cols', String(Math.max(1, Math.ceil(keys.length / 2))));
  for (const k of keys) {
    const upper = k.toUpperCase();
    const isMod = upper === 'CTRL' || upper === 'ALT';
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'key' + (isMod ? ' mod' : '') + (upper === '^C' ? ' danger' : '');
    b.textContent = KEY_LABELS[upper] || k;
    b.dataset.key = k;
    b.setAttribute('aria-label', KEY_LABELS[upper] || k);

    let down = null, holdTimer = null, repeatTimer = null, held = false;
    const stop = () => {
      clearTimeout(holdTimer);
      clearInterval(repeatTimer);
      holdTimer = null;
      repeatTimer = null;
      held = false;
      down = null;
      b.classList.remove('pressed');
    };
    const startRepeat = () => {
      if (!REPEAT_KEYS.has(k) || repeatTimer) return;
      held = true;
      repeatTimer = setInterval(() => pressKey(k), 80);
    };

    b.addEventListener('pointerdown', (ev) => {
      if (ev.pointerType === 'mouse' && ev.button !== 0) return;
      down = { x: ev.clientX, y: ev.clientY };
      b.classList.add('pressed');
      if (isMod) {
        holdTimer = setTimeout(() => {
          holdTimer = null;
          held = true;
          toggleMod(upper.toLowerCase(), true); // 长按 = 锁定
        }, 500);
      } else if (REPEAT_KEYS.has(k)) {
        holdTimer = setTimeout(startRepeat, 420);
      }
    });
    b.addEventListener('pointerup', (ev) => {
      const start = down;
      const wasHeld = held;
      stop();
      if (!start) return;
      if (Math.hypot(ev.clientX - start.x, ev.clientY - start.y) > 10) return; // 划出按键 = 取消
      if (isMod) {
        if (!wasHeld) toggleMod(upper.toLowerCase(), false); // 点按 = 武装一次 / 取消
        return;
      }
      if (!wasHeld) pressKey(k);
    });
    b.addEventListener('pointermove', (ev) => {
      if (!down) return;
      if (Math.hypot(ev.clientX - down.x, ev.clientY - down.y) > 10) stop();
    });
    b.addEventListener('pointercancel', stop);
    b.addEventListener('pointerleave', (ev) => { if (ev.pointerType === 'mouse') stop(); });

    keysBar.appendChild(b);
  }
  updateModUI();
}

function toggleMod(name, isLongPress) {
  const s = modState[name];
  if (isLongPress) modState[name] = s === 2 ? 0 : 2;      // 长按：锁定/解锁
  else modState[name] = s === 1 ? 0 : 1;                  // 点按：武装一次 / 取消
  updateModUI();
}

function updateModUI() {
  for (const b of keysBar.querySelectorAll('.key.mod')) {
    const n = b.dataset.key.toLowerCase();
    b.classList.toggle('mod-on', modState[n] === 1);
    b.classList.toggle('mod-lock', modState[n] === 2);
  }
}

// 发送按键序列；使用后消费"点按"修饰（锁定修饰保持）。
function pressKey(k) {
  const upper = k.toUpperCase();
  let out = k;
  if (upper === 'ESC') out = '\x1b';
  else if (upper === 'TAB') out = '\t';
  else if (upper === 'ENTER' || upper === 'RETURN') out = '\r';
  else if (upper === '^C' || upper === 'CTRL+C') out = '\x03';
  else if (upper === 'PGUP') out = '\x1b[5~';
  else if (upper === 'PGDN') out = '\x1b[6~';
  else if (upper === 'HOME') out = '\x1b[H';
  else if (upper === 'END') out = '\x1b[F';
  else if (upper === 'BS') out = '\x7f';
  else if (upper === 'DEL') out = '\x1b[3~';
  else if (k === '←' || k === '↑' || k === '↓' || k === '→') {
    const code = { '←': 'D', '↑': 'A', '↓': 'B', '→': 'C' }[k];
    const mod = 1 + (modState.alt ? 2 : 0) + (modState.ctrl ? 4 : 0);
    out = mod === 1 ? '\x1b[' + code : '\x1b[1;' + mod + code;
  }

  if (out.length === 1 && out.charCodeAt(0) >= 32) {
    if (modState.ctrl) out = String.fromCharCode(out.charCodeAt(0) & 0x1f);
    if (modState.alt) out = '\x1b' + out;
  }

  // 消费点按修饰
  for (const n of ['ctrl', 'alt']) if (modState[n] === 1) modState[n] = 0;
  updateModUI();

  if (ws && ws.readyState === 1) ws.send(out);
  if (mode === 'desktop' && term) term.focus();
}

onTap($('#d-keys-btn'), toggleKeys);
onTap($('#d-more-btn'), () => { if (term) showMenu(terminalMenu(), { anchor: $('#d-more-btn'), align: 'right' }); });
onTap($('#m-more-btn'), () => { if (term) showMenu(terminalMenu(), { sheet: true }); });
onTap($('#d-new-btn'), createSession);
onTap($('#m-new-btn'), createSession);
onTap($('#ph-new'), createSession);
onTap($('#exit-restart'), () => { if (current) restartSession(current.id); });
onTap($('#exit-remove'), () => { if (current) removeCurrent(); });
onTap(scrollPill, scrollToBottom);
onTap($('#select-copy'), copySelection);
onTap($('#select-all'), selectAllBuffer);
onTap($('#select-cancel'), clearSelection);

/* ---------------- 轻提示 ---------------- */
function showToast(text, ms = 1800) {
  toast.textContent = text;
  toast.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { toast.hidden = true; }, ms);
}

/* ---------------- 启动 ---------------- */
boot();

})();
