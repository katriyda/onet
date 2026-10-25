/* onet — 前端 UI 与 WS 协议逻辑（双模式自适应：移动 termux 风格 / 桌面经典终端） */
'use strict';
(() => {

const $ = (sel, root = document) => root.querySelector(sel);
const clamp = (v, lo, hi) => Math.max(lo, Math.min(hi, v));
const versionLabel = (v) => {
  if (!v) return '';
  const s = String(v);
  return s === 'dev' ? 'dev' : 'v' + s.replace(/^v/i, '');
};
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
let stableTimer = null;
let disconnected = false;
let modState = { ctrl: 0, alt: 0 }; // 0=off 1=点按(仅下一次) 2=长按锁定
let unseenOutput = false;
let serverVersion = '';

// 回放/续传协议
const LAST_SESSION_KEY = 'onet_last_session';
const SESSION_SEQ = new Map(); // session id -> 最后收到的 output seq
let serverCaps = {};           // hello.capabilities；旧服务端为空对象
let lastSessionId = '';
let heartbeatTimer = null;
let lastServerMessageAt = 0;
let lastMissingTargetId = '';  // 避免同一缺失目标重复提示

// 交互阈值（手势/长按/点按统一，别再各写一套）
const LONG_PRESS_MS = 450;
const TAP_SLOP = 10;
const REPEAT_INTERVAL_MS = 80;
const HEARTBEAT_MS = 20000;
const HEARTBEAT_TIMEOUT_MS = 25000;

// 异步操作状态：失败/超时都要解锁，避免 UI 永久禁用
let creating = false;
let createTimer = null;
let pendingRemoveId = null;
let removingCurrent = false;
let removeTimer = null;
let restartingId = null;
let restartTimer = null;
let removing = false;

// 更新状态机：idle → checking → installing → restarting → done/failed
let updateState = 'idle';
let pendingUpdateVersion = '';
let updateBannerTimer = null;
let updateVerifyTimer = null;
let restartWatchdogTimer = null;

// 设置面板：保存请求互斥，token_set 来自服务端（GET 不回传 token 本身）
let settingsSaving = false;
let settingsTokenSet = false;
let settingsTrigger = null;

let xtermViewport = null;
let lastFitKey = '';
let resizeTimer = null;

let menuOpen = false;
let menuTimer = null;
let menuTrigger = null;
let drawerTimer = null;
let drawerTrigger = null;
let logTrigger = null;
let toastTimer = null;
let toastActiveKind = 'info';
let toastUntil = 0;

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
const logLayer = $('#log-layer'), logBody = $('#log-body'), logSub = $('#log-sub');
const logTabClient = $('#log-tab-client'), logTabServer = $('#log-tab-server');
const updateBanner = $('#update-banner'), updateText = $('#update-text');
const bootError = $('#boot-error'), bootErrorText = $('#boot-error-text'), bootRetry = $('#boot-retry');
const pasteLayer = $('#paste-layer'), pasteInput = $('#paste-input');
const pasteOk = $('#paste-ok'), pasteCancel = $('#paste-cancel');
const settingsLayer = $('#settings-layer'), settingsForm = $('#settings-form'), settingsSub = $('#settings-sub');
const settingsTokenInput = $('#set-token'), settingsClearToken = $('#set-clear-token');
const settingsTokenState = $('#set-token-state');

/* ---------------- 日志：客户端采集 → WS 回传 → 服务端 JSONL ---------------- */
// 浏览器侧事件/异常先入内存队列，WS 可用时按批回传；未发出的部分写入
// sessionStorage，登录后刷新也能续传。查看器保留最近 1000 条。
const CLIENT_LOG_MAX = 1000;
const LOG_QUEUE_MAX = 500;
const LOG_PERSIST_KEY = 'onet_log_queue';
const LOG_BATCH_MAX = 30;
const LOG_BATCH_BYTES = 60000;

const clientId = (() => {
  try {
    if (window.crypto && typeof window.crypto.randomUUID === 'function') return window.crypto.randomUUID();
  } catch { /* noop */ }
  return 'c-' + Date.now().toString(36) + '-' + Math.random().toString(36).slice(2, 10);
})();

const clientLogs = [];
const logQueue = [];
const nativeConsole = {
  log: console.log.bind(console),
  info: console.info.bind(console),
  debug: console.debug.bind(console),
  warn: console.warn.bind(console),
  error: console.error.bind(console),
};

let logFlushTimer = null;
let logPersistTimer = null;
let serverConnId = ''; // 当前 WS 连接标识（服务端 hello 帧回传）

function truncateLog(s, max) {
  const text = s == null ? '' : String(s);
  return text.length > max ? text.slice(0, max) + '…' : text;
}

function logEvent(level, event, msg = '', stack = '') {
  const entry = { source: 'client', ts: Date.now(), level, event };
  if (msg) entry.msg = truncateLog(msg, 2000);
  if (stack) entry.stack = truncateLog(stack, 2000);
  clientLogs.push(entry);
  if (clientLogs.length > CLIENT_LOG_MAX) clientLogs.splice(0, clientLogs.length - CLIENT_LOG_MAX);
  logQueue.push(entry);
  if (logQueue.length > LOG_QUEUE_MAX) logQueue.splice(0, logQueue.length - LOG_QUEUE_MAX);
  const out = nativeConsole[level] || nativeConsole.log;
  out('[onet] ' + event, msg || '');
  scheduleLogFlush();
  scheduleLogPersist();
}

function scheduleLogFlush() {
  clearTimeout(logFlushTimer);
  logFlushTimer = setTimeout(flushClientLogs, 200);
}

function scheduleLogPersist() {
  clearTimeout(logPersistTimer);
  logPersistTimer = setTimeout(persistLogQueue, 1000);
}

function persistLogQueue() {
  try {
    if (!logQueue.length) { sessionStorage.removeItem(LOG_PERSIST_KEY); return; }
    sessionStorage.setItem(LOG_PERSIST_KEY, JSON.stringify(logQueue.slice(-200)));
  } catch { /* sessionStorage 不可用（隐私模式）：放弃持久化 */ }
}

function restoreLogQueue() {
  let raw = null;
  try { raw = sessionStorage.getItem(LOG_PERSIST_KEY); } catch { return; }
  if (!raw) return;
  let items;
  try { items = JSON.parse(raw); } catch { return; }
  if (!Array.isArray(items)) return;
  for (const entry of items) {
    if (!entry || typeof entry !== 'object' || typeof entry.event !== 'string') continue;
    if (typeof entry.ts !== 'number' || typeof entry.level !== 'string') continue;
    clientLogs.push(entry);
    logQueue.push(entry);
  }
  if (clientLogs.length > CLIENT_LOG_MAX) clientLogs.splice(0, clientLogs.length - CLIENT_LOG_MAX);
  if (logQueue.length > LOG_QUEUE_MAX) logQueue.splice(0, logQueue.length - LOG_QUEUE_MAX);
}

// 每批最多 30 条 / 60KB；发不完的 100ms 后继续，WS 不可用时保持排队。
function flushClientLogs() {
  clearTimeout(logFlushTimer);
  logFlushTimer = null;
  if (!ws || ws.readyState !== 1) return;
  if (!logQueue.length) { persistLogQueue(); return; }

  const batch = [];
  let size = 0;
  while (logQueue.length && batch.length < LOG_BATCH_MAX) {
    const entry = logQueue[0];
    const len = JSON.stringify(entry).length;
    if (batch.length && size + len > LOG_BATCH_BYTES) break;
    batch.push(entry);
    size += len;
    logQueue.shift();
  }
  if (!batch.length) return;

  try {
    ws.send(new TextEncoder().encode(JSON.stringify({ type: 'log', client: clientId, entries: batch })));
  } catch {
    for (let i = batch.length - 1; i >= 0; i--) logQueue.unshift(batch[i]);
    return; // 本轮放弃，等下一条日志再触发
  }

  persistLogQueue();
  if (logQueue.length) {
    logFlushTimer = setTimeout(flushClientLogs, 100);
  }
}

// 查询串里的 token 类参数一律替换为 ***，日志永不落 token。
function safeLocation() {
  const params = new URLSearchParams(location.search);
  for (const key of Array.from(params.keys())) {
    if (key.toLowerCase().includes('token')) params.set(key, '***');
  }
  const qs = params.toString();
  return location.pathname + (qs ? '?' + qs : '');
}

function serializeLogArg(a) {
  if (a instanceof Error) return a.stack || (a.name + ': ' + a.message);
  if (typeof a === 'string') return a;
  if (a === null || typeof a !== 'object') return String(a);
  try { return JSON.stringify(a); } catch { return String(a); }
}

function joinLogArgs(args) {
  return Array.prototype.map.call(args, serializeLogArg).join(' ');
}

function logClipboardError(operation, err) {
  logEvent('warn', 'client.clipboard.error',
    'operation=' + operation + ' message=' + ((err && err.message) || String(err)));
}

function installLogHooks() {
  // 捕获阶段才能收到资源加载错误（error 事件不冒泡）。
  window.addEventListener('error', (ev) => {
    if (ev.target && ev.target !== window && ev.target.tagName) {
      const t = ev.target;
      logEvent('error', 'client.error',
        'resource failed: ' + t.tagName.toLowerCase() + ' ' + (t.src || t.href || ''));
      return;
    }
    if (ev.message === 'Script error.' && !ev.error) {
      // 跨域脚本不暴露细节，按 warn 记录，避免污染错误统计。
      logEvent('warn', 'client.error.opaque', ev.filename || '');
      return;
    }
    logEvent('error', 'client.error',
      (ev.message || 'unknown error') +
        (ev.filename ? ' (' + ev.filename + ':' + ev.lineno + ':' + ev.colno + ')' : ''),
      (ev.error && ev.error.stack) || '');
  }, true);

  window.addEventListener('unhandledrejection', (ev) => {
    const r = ev.reason;
    logEvent('error', 'client.unhandled_rejection',
      (r && (r.message || String(r))) || 'unknown rejection',
      (r && r.stack) || '');
  });

  // 包一层 console.warn/error（先输出再记录），便于把第三方库的告警也带回来。
  for (const level of ['warn', 'error']) {
    const native = nativeConsole[level];
    console[level] = (...args) => {
      native(...args);
      logEvent(level, 'client.console.' + level, joinLogArgs(args));
    };
  }

  window.addEventListener('online', () => logEvent('info', 'client.network', 'online'));
  window.addEventListener('offline', () => logEvent('warn', 'client.network', 'offline'));
  document.addEventListener('visibilitychange', () => {
    logEvent('debug', 'client.visibility', document.visibilityState);
    if (document.visibilityState === 'hidden') {
      flushClientLogs();
      persistLogQueue();
    }
  });
  window.addEventListener('pagehide', () => persistLogQueue());
}

/* ---------------- 启动 ---------------- */
// fetchWithTimeout 用 AbortController 覆盖旧 WebView（不支持 AbortSignal.timeout）。
async function fetchWithTimeout(url, opts = {}, ms = 8000) {
  const ctl = new AbortController();
  const timer = setTimeout(() => ctl.abort(), ms);
  try {
    return await fetch(url, { ...opts, signal: ctl.signal });
  } finally {
    clearTimeout(timer);
  }
}

function showBootError(message) {
  if (!bootError) {
    showToast(message, 4000, 'error');
    return;
  }
  if (bootErrorText) bootErrorText.textContent = message;
  bootError.hidden = false;
  if (bootRetry) bootRetry.focus();
}

async function boot() {
  logEvent('info', 'client.boot',
    'href=' + safeLocation() + ' ua=' + navigator.userAgent + ' lang=' + navigator.language);
  if (bootError) bootError.hidden = true;

  const qs = new URLSearchParams(location.search);

  // 深链带 ?token= → 自动登录并去掉 URL 中的 token
  if (qs.get('token')) {
    try {
      const r = await fetchWithTimeout('/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ token: qs.get('token') }),
      }, 10000);
      if (r.ok) {
        logEvent('info', 'client.login.ok', 'via=deep_link');
        qs.delete('token');
        persistLogQueue(); // 刷新会丢掉内存队列，先落盘
        location.replace(location.pathname + (qs.toString() ? '?' + qs : ''));
        return;
      }
      logEvent('warn', 'client.login.error', 'via=deep_link status=' + r.status);
    } catch (err) {
      logEvent('warn', 'client.login.error', 'via=deep_link err=' + ((err && err.message) || err));
    }
  }

  // /config 可能撞上服务启动竞态：3 次重试（500/1000/2000ms），每次 8s 超时。
  const waits = [500, 1000, 2000];
  let resp = null;
  let lastErr = null;
  for (let attempt = 0; attempt < 3; attempt++) {
    try {
      resp = await fetchWithTimeout('/config', { cache: 'no-store' }, 8000);
      lastErr = null;
      if (resp.status === 401) {
        logEvent('info', 'client.config.unauthorized');
        showLogin();
        return;
      }
      if (resp.ok) break;
      lastErr = new Error('HTTP ' + resp.status);
    } catch (err) {
      lastErr = err;
      resp = null;
    }
    if (attempt < waits.length - 1) await new Promise((r) => setTimeout(r, waits[attempt]));
  }
  if (!resp || !resp.ok) {
    const detail = lastErr ? ((lastErr && lastErr.message) || String(lastErr)) : 'unknown';
    logEvent('error', 'client.config.fetch_failed', detail);
    showBootError('无法连接服务器，请检查 onet 是否在运行');
    return;
  }

  try {
    cfg = await resp.json();
  } catch (err) {
    logEvent('error', 'client.config.parse_failed', (err && err.message) || String(err));
    showBootError('配置解析失败，请刷新页面重试');
    return;
  }
  serverVersion = cfg.version || '';
  logEvent('info', 'client.config.loaded',
    'ui=' + cfg.ui + ' scrollback=' + cfg.scrollback + ' font_size=' + cfg.font_size +
    ' version=' + serverVersion);
  applyMode();
  initTerm();
  connectWS();
}

let loginBound = false;
function showLogin() {
  clearUpdateBanner();
  if (bootError) bootError.hidden = true;
  if (!login) return;
  login.hidden = false;
  if (!loginBound) {
    loginBound = true;
    loginForm.onsubmit = async (e) => {
      e.preventDefault();
      loginErr.hidden = true;
      try {
        const r = await fetchWithTimeout('/login', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ token: $('#login-token').value }),
        }, 10000);
        if (r.ok) {
          logEvent('info', 'client.login.ok', 'via=form');
          persistLogQueue(); // reload 前落盘，登录后继续回传
          location.reload();
          return;
        }
        logEvent('warn', 'client.login.error', 'via=form status=' + r.status);
        loginErr.textContent = '令牌错误';
      } catch (err) {
        logEvent('warn', 'client.login.error', 'via=form err=' + ((err && err.message) || err));
        loginErr.textContent = '无法连接服务器';
      }
      loginErr.hidden = false;
    };
  }
  setTimeout(() => { const el = $('#login-token'); if (el) el.focus(); }, 50);
}

/* ---------------- 模式判定 ---------------- */
function detectMode() {
  if (!cfg) return mode;
  if (cfg.ui === 'mobile' || cfg.ui === 'desktop') return cfg.ui;
  const fineWide = matchMedia('(pointer: fine)').matches && matchMedia('(min-width: 768px)').matches;
  return fineWide ? 'desktop' : 'mobile';
}

function applyMode(smooth = false) {
  if (!cfg) return;
  const next = detectMode();
  const changed = next !== mode;
  mode = next;
  document.body.classList.toggle('mode-mobile', mode === 'mobile');
  document.body.classList.toggle('mode-desktop', mode === 'desktop');

  if (changed) {
    // 没有显式偏好时跟随模式：移动端默认显示按键栏，桌面端默认隐藏
    let pref = null;
    try { pref = localStorage.getItem('onet_keys'); } catch { /* noop */ }
    keysVisible = pref === '1' ? true : pref === '0' ? false : mode === 'mobile';
    if (term) renderKeys();
  }
  updateKeysVisibility();
  scheduleLayout(smooth);
}

function updateKeysVisibility() {
  const show = !!(keysVisible && term);
  keysBar.classList.toggle('show', show);
  dKeysBtn.classList.toggle('active', keysVisible);
  scheduleLayout();
}

matchMedia('(pointer: coarse)').addEventListener('change', () => applyMode());
matchMedia('(pointer: fine)').addEventListener('change', () => applyMode());
window.addEventListener('resize', () => { if (cfg) applyMode(true); });

/* ---------------- 布局 / 尺寸 ---------------- */
// 软键盘弹起/收起时，可视区高度是一帧一帧变的。以前每帧都 fit 一次：xterm
// 每跨过一个字符高度就 reflow 一次，还会把中间尺寸发给 PTY（ConPTY 重排 +
// shell 重绘），视觉上就是“卡一下、跳一行”。
// 现在：高度变化期间冻结 xterm 尺寸，只平移 #term 让底边贴着可视区连续滑动；
// 尺寸稳定 VP_SETTLE_MS 后再一次性 fit + resize。行数没变 → 不动画。
const VP_SETTLE_MS = 140;
const VP_EASE = 0.5;           // 大跳变时每帧逼近目标的比例（~4 帧收敛）
const VP_FOLLOW_PX = 48;       // 单次变化小于此值：直接贴住键盘边（不产生滞后）
const BOTTOM_INTENT_MS = 2000; // 触摸开始时在底部 → 这段时间内的视口变化仍锚底
const coarsePointer = matchMedia('(pointer: coarse)');

let layoutRaf = 0;
let layoutSmooth = false;
let termHeight = 0;            // 最近一次布局后 #term 的高度
let vpFrozenH = 0;             // 冻结期间 #term 的像素高度（0 = 未冻结）
let vpPan = false;             // 是否把底边钉在可视区底部（缓冲区未填满时不钉）
let vpTarget = 0;              // 目标位移（让底边贴住 term-wrap 底边）
let vpOffset = 0;              // 当前位移
let vpRaf = 0;
let vpSettleTimer = 0;
let bottomIntentAt = 0;        // 用户最近一次在底部点击/聚焦的时间，0 = 无意图
let vpForceBottom = false;     // 本次冻结是否来自“底部意图”
let anchorSeq = 0;             // 取消延后锚定的令牌
let lastScrollResetLogAt = 0;

function scheduleLayout(smooth = false) {
  if (smooth) layoutSmooth = true;
  if (layoutRaf) return;
  layoutRaf = requestAnimationFrame(() => {
    layoutRaf = 0;
    const s = layoutSmooth;
    layoutSmooth = false;
    layout(s);
  });
}

function layout(smooth = false) {
  // 先清掉页面自身的滚动：否则 vv.offsetTop 会把页面 scroll 也算进来，
  // app 会被双重偏移，用户只能靠手滚找回终端。
  resetPageScroll();
  const vv = window.visualViewport;
  if (vv) {
    const top = coarsePointer.matches
      ? clamp(vv.offsetTop, 0, Math.max(0, window.innerHeight - vv.height))
      : 0;
    app.style.top = top + 'px';
    app.style.height = vv.height + 'px';
    document.documentElement.style.setProperty('--vv-top', top + 'px');
    document.documentElement.style.setProperty('--vvh', vv.height + 'px');
  } else {
    app.style.top = '';
    app.style.height = '';
    document.documentElement.style.removeProperty('--vv-top');
    document.documentElement.style.removeProperty('--vvh');
  }
  syncViewport(smooth);
  updateScrollPill();
}

function resetPageScroll() {
  const y = window.scrollY || document.documentElement.scrollTop || document.body.scrollTop || 0;
  if (!y) return;
  if (performance.now() - lastScrollResetLogAt > 1000) {
    lastScrollResetLogAt = performance.now();
    logEvent('info', 'client.vp.scrollreset', 'sy=' + Math.round(y));
  }
  window.scrollTo(0, 0);
}
window.addEventListener('scroll', resetPageScroll, { passive: true });

function viewportHeight() {
  // 冻结期间 #term 的高度是内联固定的，必须看父容器（#term-wrap）的可用高度。
  const wrap = termEl.parentElement;
  return wrap ? wrap.clientHeight : termEl.clientHeight;
}

// 只有内容铺满屏幕且停在底部时，终端才在视觉上“底边钉住”；
// 短缓冲区（提示符在顶部、下面全是空白）平移会把可见内容推出屏幕。
// 用缓冲区坐标判断：此时容器可能已经缩小，像素级 atBottom() 会误判。
function bufferAtBottom() {
  try { const b = term.buffer.active; return b.viewportY >= b.baseY; } catch { return true; }
}

function canAnchorBottom() {
  try {
    const b = term.buffer.active;
    if (b.viewportY < b.baseY) return false;
    return bufferFillsScreen();
  } catch { return false; }
}

function syncViewport(smooth) {
  const h = viewportHeight();
  if (!term || !fit) { termHeight = h; return; }

  if (!vpFrozenH) {
    // 只有触屏设备（软键盘）走平滑路径；鼠标拖拽窗口保持逐帧 fit。
    if (!smooth || !coarsePointer.matches || !termHeight || h === termHeight) {
      termHeight = h;
      fitTerm();
      return;
    }
    vpFrozenH = termHeight;
    const bottomIntent = bottomIntentWanted();
    vpForceBottom = bottomIntent;
    // 全屏 TUI（alternate screen）绝不能平移：alt 缓冲没有回滚区，平移量无法在
    // 提交后被 scrollToBottom 补偿，应用收到 SIGWINCH 又会整屏重绘回自己的视图，
    // 结果就是“内容先上滑再被拉回”的滚动两下。只冻结行数，等应用重绘一次。
    vpPan = !isAltBuffer() && (canAnchorBottom() || (bottomIntent && bufferFillsScreen()));
    vpTarget = vpOffset = 0;
    termEl.style.height = vpFrozenH + 'px';
    if (vpPan) termEl.style.willChange = 'transform';
    logEvent('debug', 'client.vp.freeze',
      'h=' + vpFrozenH + ' pan=' + vpPan + ' intent=' + bottomIntent);
  }

  if (h !== termHeight || !vpSettleTimer) {
    clearTimeout(vpSettleTimer);
    vpSettleTimer = setTimeout(commitViewport, VP_SETTLE_MS);
  }
  termHeight = h;
  if (!vpPan) return;          // 短缓冲区/已滚回：保持顶部对齐，只冻结行数
  const nextTarget = h - vpFrozenH;
  const delta = Math.abs(nextTarget - vpTarget);
  vpTarget = nextTarget;
  if (delta <= VP_FOLLOW_PX) {
    // 键盘动画的逐帧事件：直接贴住，不能有跟随滞后（否则底部行会被键盘边裁掉）。
    if (vpRaf) { cancelAnimationFrame(vpRaf); vpRaf = 0; }
    vpOffset = vpTarget;
    applyViewportTransform();
  } else {
    // 视口一次性跳变（粗粒度 resize）：短缓动跟上，避免“瞬间移动”。
    stepViewport();
  }
}

function applyViewportTransform() {
  termEl.style.transform = 'translate3d(0,' + vpOffset.toFixed(2) + 'px,0)';
}

// 位移用 rAF 缓动而不是 CSS transition：每帧都有新目标时不会被重置，
// 提交前也能保证已经收敛到目标位置，不会在 fit 时再抖一下。
function stepViewport() {
  if (!vpFrozenH || vpRaf) return;
  vpRaf = requestAnimationFrame(vpStep);
}

function vpStep() {
  vpRaf = 0;
  if (!vpFrozenH) return;
  const diff = vpTarget - vpOffset;
  if (Math.abs(diff) < 0.5) {
    if (vpOffset === vpTarget) return;
    vpOffset = vpTarget;
  } else {
    vpOffset += diff * VP_EASE;
  }
  applyViewportTransform();
  if (vpOffset !== vpTarget) vpRaf = requestAnimationFrame(vpStep);
}

function endViewportFreeze() {
  if (!vpFrozenH) return false;
  if (vpRaf) { cancelAnimationFrame(vpRaf); vpRaf = 0; }
  clearTimeout(vpSettleTimer);
  vpSettleTimer = 0;
  vpFrozenH = 0;
  vpPan = false;
  vpForceBottom = false;
  vpTarget = vpOffset = 0;
  termEl.style.height = '';
  termEl.style.transform = '';
  termEl.style.willChange = '';
  return true;
}

function commitViewport() {
  if (!vpFrozenH) return;
  const forced = vpForceBottom;
  const panned = vpPan;
  const atEnd = forced || bufferAtBottom();
  endViewportFreeze();
  lastFitKey = '';
  fitTerm(true);               // 触发 term.onResize → 延迟 80ms 发一次 resize
  termHeight = viewportHeight();
  const vv = window.visualViewport;
  logEvent('info', 'client.vp.settle',
    'vvh=' + Math.round(vv ? vv.height : window.innerHeight) +
    ' top=' + Math.round(parseFloat(app.style.top) || 0) +
    ' ih=' + Math.round(window.innerHeight) +
    ' sy=' + Math.round(window.scrollY || 0) +
    ' rows=' + term.rows +
    ' bottom=' + (atEnd ? 1 : 0) +
    ' force=' + (forced ? 1 : 0) +
    ' pan=' + (panned ? 1 : 0) +
    ' alt=' + (isAltBuffer() ? 1 : 0));
  // fit 之后 viewport/scroll-area 的同步是延后的，同一帧 scrollToBottom 会落在旧
  // scrollHeight 上（差 1–2 行）。统一走延后重试，尺寸稳定后只回到底部一次。
  if (atEnd) settleAnchorBottom();
  else updateScrollPill();
  clearBottomIntent();
}

function fitTerm(force = false) {
  if (!fit || !term) return;
  if (vpFrozenH) return;       // 冻结期间保持行数，等 commit 再算
  const key = termEl.clientWidth + 'x' + termEl.clientHeight + 'x' + term.options.fontSize;
  if (!force && key === lastFitKey) return;
  lastFitKey = key;
  try { fit.fit(); } catch { /* 隐藏或零尺寸时忽略 */ }
}

function sendResize() {
  if (!current || !term) return;
  if (vpFrozenH) return;       // 中间尺寸不发给 PTY，commit 后会再排一次
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
// 滑动超过 TAP_SLOP 不算点按；鼠标划出元素取消；键盘 Enter/Space 的 click（detail === 0）保留。
let lastTapAt = 0;
function onTap(el, fn) {
  let down = null;
  el.addEventListener('pointerdown', (ev) => {
    if (el.disabled) return;
    if (ev.pointerType === 'mouse' && ev.button !== 0) return;
    down = { x: ev.clientX, y: ev.clientY };
    el.classList.add('pressed');
  });
  el.addEventListener('pointerup', (ev) => {
    el.classList.remove('pressed');
    const start = down;
    down = null;
    if (el.disabled || !start || Math.hypot(ev.clientX - start.x, ev.clientY - start.y) > TAP_SLOP) return;
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
    if (el.disabled) return;
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
    if (el.disabled) return;
    if (ev.pointerType === 'mouse' && ev.button !== 0) return;
    down = { x: ev.clientX, y: ev.clientY };
    longDone = false;
    el.classList.add('pressed');
    if (opts.longPress) {
      timer = setTimeout(() => {
        timer = null;
        if (!down || el.disabled) return;
        longDone = true;
        el.classList.remove('pressed');
        opts.longPress({ clientX: down.x, clientY: down.y });
      }, opts.delay || LONG_PRESS_MS);
    }
  });
  el.addEventListener('pointermove', (ev) => {
    if (!down) return;
    if (Math.hypot(ev.clientX - down.x, ev.clientY - down.y) > TAP_SLOP) cancel();
  });
  el.addEventListener('pointerup', (ev) => {
    if (!down) { el.classList.remove('pressed'); return; }
    const start = down;
    const wasLong = longDone;
    clearTimeout(timer);
    timer = null;
    down = null;
    el.classList.remove('pressed');
    if (el.disabled || wasLong) return;
    if (Math.hypot(ev.clientX - start.x, ev.clientY - start.y) > TAP_SLOP) return;
    lastTapAt = performance.now();
    if (opts.tap) opts.tap(ev);
  });
  el.addEventListener('pointercancel', cancel);
  el.addEventListener('pointerleave', (ev) => { if (ev.pointerType === 'mouse') cancel(); });
  el.addEventListener('click', (ev) => {
    if (el.disabled) return;
    if (ev.detail === 0 && !longDone && performance.now() - lastTapAt > 700 && opts.tap) opts.tap(ev);
  });
}

/* ---------------- xterm ---------------- */
function storedFontSize() {
  const n = parseInt(localStorage.getItem('onet_font') || '', 10);
  return clamp(Number.isFinite(n) ? n : (cfg.font_size || 15), 8, 32);
}

function initTerm() {
  if (!window.Terminal || !window.FitAddon || !window.FitAddon.FitAddon) {
    logEvent('error', 'client.term.missing',
      'Terminal=' + !!window.Terminal + ' FitAddon=' + !!window.FitAddon);
    showBootError('终端组件加载失败，请刷新页面');
    return;
  }
  term = new window.Terminal({
    fontSize: storedFontSize(),
    scrollback: cfg.scrollback || 5000,
    cursorBlink: true,
    allowProposedApi: true,
    theme: {
      background: '#08090b', foreground: '#e8eaed',
      cursor: '#35e07f', cursorAccent: '#08090b',
      selectionBackground: '#2a5c40', selectionInactiveBackground: '#1e3f2d',
    },
  });
  fit = new window.FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open(termEl);

  // 非触摸的聚焦路径（外接键盘/程序 focus）也记底部意图；只在确实在底部时刷新，
  // 不能用 false 覆盖 touchstart 刚记下的意图。
  if (term.textarea) {
    term.textarea.addEventListener('focus', () => {
      if (atBottom()) bottomIntentAt = performance.now();
    });
  }

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
    // 键盘动画期间 height 每帧变：走冻结 + 平移；scroll 只改 offsetTop，不 fit。
    window.visualViewport.addEventListener('resize', () => scheduleLayout(true));
    window.visualViewport.addEventListener('scroll', () => scheduleLayout());
  }
}

function setFontSize(n, opts = {}) {
  if (!term) return;
  const fs = clamp(Math.round(n), 8, 32);
  if (fs === term.options.fontSize) return;
  logEvent('debug', 'client.font', 'size=' + fs);
  endViewportFreeze();
  term.options.fontSize = fs;
  localStorage.setItem('onet_font', String(fs));
  lastFitKey = '';
  fitTerm();
  termHeight = viewportHeight();
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

// “底部意图”：浏览器在 focus/键盘弹起阶段可能先把 xterm viewport 推上去 1–2 行，
// 等到 commit 时再判断 atBottom 就会把“本来在底部”误判成“用户滚上去了”。
// 所以在触摸/聚焦那一刻（浏览器还没动手之前）记下来，短时间内的视口变化都锚底。
function noteBottomIntent() {
  if (!term || !current) return;
  bottomIntentAt = atBottom() ? performance.now() : 0;
}
function clearBottomIntent() { bottomIntentAt = 0; }
function bottomIntentWanted() {
  return bottomIntentAt > 0 && performance.now() - bottomIntentAt <= BOTTOM_INTENT_MS;
}
function bufferFillsScreen() {
  try {
    return isAltBuffer() || term.buffer.active.baseY > 0 || term.buffer.active.cursorY >= term.rows - 1;
  } catch { return false; }
}

function isAltBuffer() {
  try { return term.buffer.active.type === 'alternate'; } catch { return false; }
}

function hasMouseTracking() {
  try { return term.modes.mouseTrackingMode !== 'none'; } catch { return false; }
}

function updateScrollPill() {
  if (!scrollPill) return;
  // 键盘动画期间不弹“回到底部”：此时“不在底部”多半来自浏览器的中间滚动，
  // 提交后的延后锚定会重新判断，提前提示只会误导用户去手滚。
  if (vpFrozenH && vpForceBottom) {
    unseenOutput = false;
    scrollPill.hidden = true;
    return;
  }
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
  anchorSeq++;                 // 用户明确要底部：作废未完成的延后锚定链
  cancelMomentum();
  term.scrollToBottom();
  unseenOutput = false;
  updateScrollPill();
}

// 尺寸稳定后的底部锚定：xterm 5.x 的 viewport/scroll-area 同步是延后的
// （见 initTerm 的 attach 路径），fit 完同一帧 scrollToBottom 会落在旧
// scrollHeight 上。1+2 帧 + 60/140/300ms 重试，用户一滚动就由 anchorSeq 作废。
function settleAnchorBottom() {
  if (!term || !current) return;
  cancelMomentum();
  const token = ++anchorSeq;
  const apply = (log) => {
    if (token !== anchorSeq || !term) return;
    term.scrollToBottom();
    unseenOutput = false;
    updateScrollPill();
    if (log) {
      const v = viewportEl();
      if (v) {
        logEvent('info', 'client.vp.anchor',
          'sc=' + Math.round(v.scrollTop) + ' ch=' + Math.round(v.clientHeight) +
          ' sh=' + Math.round(v.scrollHeight) + ' bottom=' + (atBottom() ? 1 : 0) +
          ' rows=' + term.rows);
      }
    }
  };
  apply(false);
  requestAnimationFrame(() => { apply(false); requestAnimationFrame(() => apply(false)); });
  setTimeout(() => apply(false), 60);
  setTimeout(() => apply(false), 140);
  setTimeout(() => apply(true), 300);
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
  if (!text) { showToast('没有可复制的内容', 3000, 'warn'); return; }
  const count = Math.abs(selection.focus - selection.anchor) + 1;
  writeClipboard(text)
    .then(() => { showToast('已复制 ' + count + ' 行', 1800, 'success'); clearSelection(); })
    .catch((err) => { logClipboardError('copy-selection', err); showToast('复制失败', 5000, 'error'); });
}

function selectAllBuffer() {
  if (!term) return;
  term.selectAll();
  selection = { active: true, anchor: 0, focus: Math.max(0, term.buffer.active.length - 1) };
  updateSelectionBar();
}

/* ---------------- 触屏手势（稳定层，避免 xterm 行节点替换导致丢事件） ---------------- */
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

  // 在浏览器 focus/键盘滚动之前记住“点之前是否在底部”。
  noteBottomIntent();

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

  if (!gesture.moved && Math.hypot(dx, dy) > TAP_SLOP) {
    gesture.moved = true;
    cancelLongPress();
    // 手指确实开始拖动：放弃强制底部，并作废未完成的延后锚定。
    clearBottomIntent();
    vpForceBottom = false;
    anchorSeq++;
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
  anchorSeq++;
  clearBottomIntent();
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
// 会话定位存 localStorage（跨刷新、新标签页、WebView 重启都保留），
// sessionStorage 仅作隐私模式兜底。URL ?s= 仍是最高优先级。
function readStoredSession() {
  try {
    const v = localStorage.getItem(LAST_SESSION_KEY);
    if (v) return v;
  } catch { /* localStorage 不可用 */ }
  try {
    return sessionStorage.getItem(LAST_SESSION_KEY) || '';
  } catch { /* sessionStorage 也不可用 */ }
  return '';
}

function writeStoredSession(id) {
  try {
    if (id) localStorage.setItem(LAST_SESSION_KEY, id);
    else localStorage.removeItem(LAST_SESSION_KEY);
  } catch { /* 隐私模式：仅内存变量可用 */ }
  try {
    if (id) sessionStorage.setItem(LAST_SESSION_KEY, id);
    else sessionStorage.removeItem(LAST_SESSION_KEY);
  } catch { /* 同上 */ }
}

function rememberSession(id) {
  lastSessionId = id || '';
  writeStoredSession(lastSessionId);
  try {
    const url = new URL(location.href);
    if (id) url.searchParams.set('s', id);
    else url.searchParams.delete('s');
    history.replaceState(null, '', url.pathname + url.search + url.hash);
  } catch { /* file:// 等环境不支持 */ }
}

function sendCmd(obj) {
  if (obj && obj.type !== 'resize' && obj.type !== 'ping') {
    logEvent('debug', 'client.cmd', 'type=' + obj.type + (obj.id ? ' id=' + obj.id : ''));
  }
  if (ws && ws.readyState === 1) {
    ws.send(new TextEncoder().encode(JSON.stringify(obj))); // 二进制帧 = JSON 命令
  }
}

function stopHeartbeat() {
  clearInterval(heartbeatTimer);
  heartbeatTimer = null;
}

function startHeartbeat() {
  stopHeartbeat();
  if (serverCaps.ping !== true) return;
  lastServerMessageAt = Date.now();
  heartbeatTimer = setInterval(() => {
    if (!ws || ws.readyState !== 1) return;
    // 服务端 20s 无任何帧（含 pong/list/output）视为假连接，主动重连。
    if (Date.now() - lastServerMessageAt > HEARTBEAT_TIMEOUT_MS) {
      logEvent('warn', 'client.ws.stale', 'idle_ms=' + (Date.now() - lastServerMessageAt));
      ws.close();
      return;
    }
    sendCmd({ type: 'ping' });
  }, HEARTBEAT_MS);
}

function connectWS() {
  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  const token = new URLSearchParams(location.search).get('token');
  logEvent('debug', 'client.ws.connect', 'delay_ms=' + reconnectDelay + ' href=' + safeLocation());
  ws = new WebSocket(proto + '://' + location.host + '/ws' + (token ? '?token=' + encodeURIComponent(token) : ''));
  ws.binaryType = 'arraybuffer';

  ws.onopen = () => {
    const reconnected = everConnected;
    everConnected = true;
    booting = true;
    // 能力必须按新连接重置：回滚到旧服务端后不能继续发送 since_seq / 心跳。
    serverCaps = {};
    stopHeartbeat();
    clearTimeout(stableTimer);
    stableTimer = setTimeout(() => { reconnectDelay = 1000; }, 5000);
    logEvent('info', 'client.ws.open', 'reconnected=' + reconnected + ' href=' + safeLocation());
    if (disconnected) {
      disconnected = false;
      showToast('已重连', 1800, 'success');
    }
    // 重连不在本地清屏：由 attached 的 resumed/resumed=false 决定是否重置，
    // 这样短断线可以续传原会话与滚动位置。
    sendCmd({ type: 'list' });
  };
  ws.onmessage = (ev) => {
    if (typeof ev.data === 'string') return; // 服务器只发二进制
    handleMsg(ev.data);
  };
  ws.onclose = (ev) => {
    logEvent('warn', 'client.ws.close',
      'code=' + ev.code + ' reason=' + (ev.reason || '') + ' wasClean=' + ev.wasClean +
        ' next_delay_ms=' + reconnectDelay);
    stopHeartbeat();
    clearTimeout(stableTimer);
    clearSelection();
    updateStatus();
    renderSessions();
    if (!disconnected) {
      disconnected = true;
      showToast('连接断开，重连中…', 2200, 'warn');
    }
    if (updateState === 'restarting') startRestartWatchdog();
    clearTimeout(reconnectTimer);
    // 抖动避免多个页面同时重连；退避只在连接稳定 5s 后才重置。
    reconnectTimer = setTimeout(connectWS, reconnectDelay * (0.8 + Math.random() * 0.4));
    reconnectDelay = Math.min(reconnectDelay * 2, 10000);
  };
  ws.onerror = () => {
    logEvent('warn', 'client.ws.error', 'href=' + safeLocation());
    ws.close();
  };
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
  lastServerMessageAt = Date.now();
  switch (msg.type) {
    case 'hello':
      serverConnId = msg.conn || '';
      serverCaps = msg.capabilities || {};
      logEvent('info', 'client.hello',
        'conn=' + serverConnId + ' version=' + (msg.version || '') +
        ' caps=' + JSON.stringify(serverCaps));
      startHeartbeat();
      if (msg.version) handleHelloVersion(msg.version);
      break;
    case 'notice':
      logEvent(msg.level === 'error' ? 'error' : 'warn', 'client.notice', msg.message || '');
      if (msg.level === 'error' && updateState === 'restarting') {
        // 服务端交接失败时不会断开 WS，必须由 notice 结束“正在切换进程”。
        updateState = 'failed';
        pendingUpdateVersion = '';
        clearTimeout(restartWatchdogTimer);
        restartWatchdogTimer = null;
        showUpdateBanner(msg.message || '更新失败', 'error');
        setTimeout(() => { if (updateState === 'failed') updateState = 'idle'; }, 5000);
      }
      showToast(msg.message || '服务端提示', msg.level === 'error' ? 5000 : 4000,
        msg.level === 'error' ? 'error' : 'warn');
      break;
    case 'output': {
      // 兼容旧服务端“先 output 后 attached”：先缓存，等 attached 决定如何写入。
      if (pendingAttach) {
        // 切换会话时旧会话的尾帧可能还在路上，不能混进新终端。
        if (msg.id !== pendingAttach.id) return;
        const p = pendingAttach;
        p.early.push(msg);
        p.earlyBytes = (p.earlyBytes || 0) + (msg.data ? msg.data.length : 0);
        if (p.early.length > 256 || p.earlyBytes > 8 * 1024 * 1024) {
          logEvent('warn', 'client.attach.early_overflow', 'id=' + p.id + ' frames=' + p.early.length);
          attachTo(p.id, { resume: false });
        }
        return;
      }
      const id = current && current.id;
      if (!id || msg.id !== id || !term) return;
      if (typeof msg.seq === 'number') {
        const prev = SESSION_SEQ.get(msg.id);
        if (prev != null) {
          if (msg.seq <= prev) return; // 重复帧（重连/重放重叠）
          if (msg.seq !== prev + 1) {
            logEvent('warn', 'client.output.gap', 'from=' + prev + ' to=' + msg.seq);
            showToast('输出丢失，正在重新同步…', 3000, 'warn');
            attachTo(id, { resume: false });
            return;
          }
        }
        SESSION_SEQ.set(msg.id, msg.seq);
      }
      if (!atBottom()) unseenOutput = true;
      term.write(b64ToBytes(msg.data), () => updateScrollPill());
      break;
    }
    case 'exit':
      logEvent('warn', 'client.session.exit', 'id=' + msg.id + ' code=' + msg.code);
      showToast('会话 ' + msg.id + ' ' + (msg.code ? '已退出 · code ' + msg.code : '已退出'), 2500, 'warn');
      break;
    case 'sessions':
      logEvent('debug', 'client.sessions', 'count=' + (Array.isArray(msg.sessions) ? msg.sessions.length : 0));
      onSessions(msg.sessions);
      break;
    case 'attached': {
      if (!pendingAttach || pendingAttach.id !== msg.id) break; // 忽略过期的 attach 响应
      const early = pendingAttach.early;
      const wasAtBottom = pendingAttach.wasAtBottom;
      const attachedFirst = serverCaps.attachOrder === 'attached-first';
      pendingAttach = null;
      logEvent('info', 'client.attach.ok',
        'id=' + msg.id + ' resumed=' + !!msg.resumed + ' truncated=' + !!msg.truncated +
        ' seq=' + msg.seq);
      const s = findSession(msg.id);
      current = { id: msg.id, name: s ? (s.name || s.id) : msg.id };
      if (msg.truncated) {
        // 历史已截断：清屏 + 分隔线，避免把半行旧输出当新输出。
        SESSION_SEQ.delete(msg.id);
        if (term) { term.reset(); term.write('\r\n\x1b[0m'); }
        early.length = 0;
        showToast('连接中断，历史已截断', 5000, 'warn');
      } else if (!msg.resumed) {
        // 服务端做了完整重放（或旧服务端）：序号可能从头开始，必须丢弃旧 seq。
        SESSION_SEQ.delete(msg.id);
        if (term) term.reset();
      }
      if (!attachedFirst) {
        // 旧服务端在 attached 之前发了 output；按到达顺序补写。
        for (const m of early) {
          if (term && m.data) term.write(b64ToBytes(m.data), () => updateScrollPill());
          if (typeof m.seq === 'number') SESSION_SEQ.set(msg.id, m.seq);
        }
      }
      early.length = 0;
      placeholder.hidden = true;
      if (!msg.truncated && !msg.resumed) unseenOutput = false;
      rememberSession(msg.id);
      updateStatus();
      renderSessions();
      if (term) fitTerm(true);
      scheduleResize();
      setTimeout(() => {
        if (current && current.id === msg.id) {
          if (wasAtBottom) settleAnchorBottom();
          else updateScrollPill();
        }
      }, 40);
      break;
    }
    case 'created': {
      const wasAuto = autoCreating;
      autoCreating = false;
      logEvent('info', 'client.session.created', 'id=' + msg.id);
      clearCreateGuard();
      if (wasAuto) showToast('服务器没有会话，已新建', 3000, 'info');
      if (current && current.id === msg.id) break;
      attachTo(msg.id, { resume: false });
      break;
    }
    case 'detached':
      logEvent('info', 'client.session.detached', 'id=' + (msg.id || ''));
      // 旧会话的 detach 不能清掉正在进行的 attach。
      if (msg.id) {
        if (pendingAttach && pendingAttach.id === msg.id) pendingAttach = null;
        if (!current || current.id === msg.id) current = null;
      } else {
        current = null;
        pendingAttach = null;
      }
      updateStatus();
      renderSessions();
      break;
    case 'error':
      logEvent('warn', 'client.error_reply', msg.message);
      clearCreateGuard();
      if (pendingRemoveId) { clearRemoveGuard(); removingCurrent = false; }
      if (restartingId) clearRestartGuard();
      if (pendingAttach) {
        pendingAttach = null;
        if (booting && /已退出|不存在/.test(msg.message)) {
          booting = false;
          createSession({ auto: true });
          return;
        }
        const target = pickTarget();
        if (target) attachTo(target.id, { resume: false });
        else placeholder.hidden = false;
        updateStatus();
        renderSessions();
      }
      showToast(msg.message, 3500, 'warn');
      break;
  }
}

function findSession(id) {
  return sessions.find((s) => s.id === id) || null;
}

function onSessions(list) {
  sessions = Array.isArray(list) ? list : [];
  let cur = current && findSession(current.id);
  renderSessions();

  if (booting) {
    booting = false;
    const target = pickTarget();
    if (target) attachTo(target.id, { resume: !!SESSION_SEQ.get(target.id) });
    else createSession({ auto: true });
    return;
  }

  if (pendingAttach) {
    // attach 请求在途；如果目标已被删除，交给 error 分支处理
    return;
  }

  // 重启完成：当前会话需要重新 attach；其他会话只提示。
  if (restartingId) {
    const rs = findSession(restartingId);
    if (rs && !rs.dead) {
      const wasCurrent = current && current.id === restartingId;
      clearRestartGuard();
      if (wasCurrent) {
        attachTo(restartingId, { resume: false });
        return;
      }
      showToast('会话已重启', 2500, 'success');
    }
  }

  if (current && cur && cur.dead) {
    // 当前会话退出：保留在页面上，显示退出横幅与重启入口，不自动切走。
  } else if (current && !cur) {
    current = null;
    cur = null;
    clearSelection();
    const target = pickTarget();
    if (target) {
      attachTo(target.id, { resume: false });
    } else if (sessions.length) {
      createSession({ auto: true });
    } else {
      placeholder.hidden = false;
    }
  } else if (!current) {
    const target = pickTarget();
    if (target) attachTo(target.id, { resume: false });
  }

  if (removingCurrent && pendingRemoveId && !findSession(pendingRemoveId)) {
    showToast(sessions.length ? '当前会话已删除，已切换' : '当前会话已删除', 3000, 'warn');
    removingCurrent = false;
  }
  if (pendingRemoveId && !findSession(pendingRemoveId)) clearRemoveGuard();

  if (!sessions.length) {
    lastSessionId = '';
    writeStoredSession('');
  }

  placeholder.hidden = sessions.length > 0;
  updateStatus();
  setExitBanner(cur && cur.dead ? cur : null);
}

function pickTarget() {
  const q = new URLSearchParams(location.search).get('s');
  let stored = lastSessionId;
  if (!stored) stored = readStoredSession();
  const match = (ident) => (ident ? sessions.find((s) => s.id === ident || s.name === ident) : null);
  const fromUrl = match(q);
  if (fromUrl && !fromUrl.dead) return fromUrl;
  const fromStore = match(stored);
  if (fromStore && !fromStore.dead) return fromStore;

  const missing = q || stored;
  if (missing && missing !== lastMissingTargetId) {
    lastMissingTargetId = missing;
    showToast('指定的会话不存在或已退出，已切换到最近会话', 3000, 'warn');
  }

  // createdAt 最大者；旧服务端没有该字段时退回列表最后一个存活会话。
  let best = null;
  for (const s of sessions) {
    if (s.dead) continue;
    if (!best) { best = s; continue; }
    const a = typeof s.createdAt === 'number' ? s.createdAt : Number.NEGATIVE_INFINITY;
    const b = typeof best.createdAt === 'number' ? best.createdAt : Number.NEGATIVE_INFINITY;
    if (a >= b) best = s;
  }
  return best;
}

/* ---------------- 会话操作 ---------------- */
const NEW_BUTTONS = ['#m-new-btn', '#d-new-btn', '#ph-new', '#drawer-new'];

function setNewButtonsDisabled(disabled) {
  for (const sel of NEW_BUTTONS) {
    const el = $(sel);
    if (el) el.disabled = disabled;
  }
}

function clearCreateGuard() {
  creating = false;
  autoCreating = false;
  clearTimeout(createTimer);
  createTimer = null;
  setNewButtonsDisabled(false);
}

function clearRemoveGuard() {
  removing = false;
  pendingRemoveId = null;
  clearTimeout(removeTimer);
  removeTimer = null;
}

function clearRestartGuard() {
  restartingId = null;
  clearTimeout(restartTimer);
  restartTimer = null;
}

function createSession({ auto = false } = {}) {
  if (creating) return;
  if (!ws || ws.readyState !== 1) {
    showToast('连接已断开，无法新建', 3000, 'warn');
    return;
  }
  logEvent('info', 'client.session.create', 'auto=' + auto);
  creating = true;
  autoCreating = auto;
  setNewButtonsDisabled(true);
  sendCmd({ type: 'create', name: '', cols: term ? term.cols : 0, rows: term ? term.rows : 0 });
  clearTimeout(createTimer);
  createTimer = setTimeout(() => {
    if (!creating) return;
    clearCreateGuard();
    showToast('新建会话超时，请重试', 4000, 'error');
  }, 5000);
}
let autoCreating = false;

function attachTo(id, { resume = false } = {}) {
  if (!term) return;
  if (!ws || ws.readyState !== 1) {
    showToast('连接已断开，请稍候', 3000, 'warn');
    return;
  }
  const attachedFirst = serverCaps.attachOrder === 'attached-first';
  // 只有新服务端 + 本地确实有该会话序号时才续传；否则完整重放。
  const canResume = !!(resume && attachedFirst && SESSION_SEQ.has(id));
  const wasAtBottom = atBottom();
  logEvent('info', 'client.attach.request', 'id=' + id + ' resume=' + canResume);
  cancelMomentum();
  cancelLongPress();
  gesture = null;
  pendingAttach = {
    id,
    resume: canResume,
    sinceSeq: canResume ? SESSION_SEQ.get(id) : undefined,
    early: [],
    earlyBytes: 0,
    wasAtBottom,
  };
  current = null;
  if (!canResume) {
    term.reset();
    clearSelection();
    unseenOutput = false;
    // 完整重放的新序号可能小于旧值，旧 seq 必须丢弃，否则会被当成重复帧。
    SESSION_SEQ.delete(id);
  }
  placeholder.hidden = true;
  updateStatus();
  updateScrollPill();
  const cmd = { type: 'attach', id, cols: term.cols, rows: term.rows };
  if (canResume) cmd.since_seq = pendingAttach.sinceSeq;
  sendCmd(cmd);
  if (mode === 'desktop' && term) term.focus();
}

function switchTo(s) {
  if (!s) return;
  if (s.dead) { showSessionMenu(s); return; }
  if (s.id === (current && current.id)) return;
  logEvent('info', 'client.session.switch', 'from=' + (current ? current.id : '') + ' to=' + s.id);
  attachTo(s.id);
}

function restartSession(id) {
  if (restartingId) return;
  logEvent('info', 'client.session.restart', 'id=' + id);
  restartingId = id;
  if (current && current.id === id && term) {
    term.reset();
    clearSelection();
    unseenOutput = false;
    updateScrollPill();
  }
  sendCmd({ type: 'restart', id });
  if (current && current.id === id) setExitBanner(null);
  clearTimeout(restartTimer);
  restartTimer = setTimeout(() => {
    if (restartingId !== id) return;
    clearRestartGuard();
    showToast('重启超时，请重试', 4000, 'error');
  }, 5000);
}

// 破坏性操作统一二次确认：移动端底部操作表，桌面居中浮层。
function confirmDestructive({ title, message, confirmLabel = '删除', fn }) {
  showMenu([
    { label: '取消', fn: () => {} },
    { label: confirmLabel, icon: 'trash', danger: true, fn },
  ], { sheet: mode === 'mobile', title, message });
}

function removeSession(id) {
  if (removing) return;
  const s = findSession(id);
  logEvent('info', 'client.session.remove', 'id=' + id + ' running=' + !!(s && !s.dead));
  if (!s) return;
  const running = !s.dead;
  confirmDestructive({
    title: running ? '删除正在运行的会话？' : '删除会话？',
    message: '「' + (s.name || s.id) + '」' +
      (running ? '的进程会被终止，未保存的输出会丢失。' : '的会话记录和输出会被删除。'),
    confirmLabel: '删除',
    fn: () => {
      if (removing) return;
      removing = true;
      pendingRemoveId = id;
      removingCurrent = !!(current && current.id === id);
      sendCmd({ type: 'remove', id });
      clearTimeout(removeTimer);
      removeTimer = setTimeout(() => {
        if (pendingRemoveId !== id) return;
        clearRemoveGuard();
        removingCurrent = false;
        showToast('删除超时，请重试', 4000, 'error');
      }, 5000);
    },
  });
}

function removeCurrent() {
  if (current) removeSession(current.id);
}

/* ---------------- 渲染 ---------------- */
function exitLabel(s) {
  return s.exitCode ? '已退出 · code ' + s.exitCode : '已退出';
}
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
    exitText.textContent = '会话「' + (sess.name || sess.id) + '」' + exitLabel(sess);
    exitBanner.hidden = false;
  }
  if (wasHidden !== exitBanner.hidden) layout();
}

function sessionMeta(s) {
  if (s.dead) return exitLabel(s);
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
    if (active) main.setAttribute('aria-current', 'true');
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
        if (s.dead) {
          closeDrawer({ restoreFocus: false });
          showSessionMenu(s);
          return;
        }
        closeDrawer();
        switchTo(s);
      },
      longPress: () => {
        closeDrawer({ restoreFocus: false });
        showSessionMenu(s);
      },
    });

    const more = document.createElement('button');
    more.type = 'button';
    more.className = 'session-more';
    more.setAttribute('aria-label', '会话操作');
    more.innerHTML = icon('more');
    onTap(more, () => {
      closeDrawer({ restoreFocus: false });
      showSessionMenu(s);
    });

    li.append(main, more);
    drawerList.appendChild(li);
  }
}

/* ---------------- 抽屉 ---------------- */
function openDrawer() {
  if (mode !== 'mobile') return;
  cancelMomentum();
  hideMenu(true, false);
  clearTimeout(drawerTimer);
  drawerTrigger = document.activeElement;
  drawer.hidden = false;
  scrim.hidden = false;
  void drawer.offsetWidth; // 强制回流，保证过渡从关闭位置开始
  drawer.classList.add('open');
  scrim.classList.add('open');
  document.addEventListener('keydown', onDrawerKey);
  const closeBtn = $('#drawer-close');
  if (closeBtn) closeBtn.focus();
}

function closeDrawer({ restoreFocus = true } = {}) {
  document.removeEventListener('keydown', onDrawerKey);
  if (drawer.hidden) return;
  drawer.classList.remove('open');
  scrim.classList.remove('open');
  clearTimeout(drawerTimer);
  const trigger = drawerTrigger;
  drawerTrigger = null;
  drawerTimer = setTimeout(() => {
    drawer.hidden = true;
    scrim.hidden = true;
    if (restoreFocus && !menuOpen && menuLayer.hidden && focusable(trigger)) trigger.focus();
  }, 240);
}

function onDrawerKey(e) {
  if (e.key === 'Escape') {
    // 只关最上层：日志/设置/菜单/粘贴打开时留给它们处理。
    if (!logLayer.hidden || !settingsLayer.hidden || menuOpen || !pasteLayer.hidden) return;
    e.preventDefault();
    closeDrawer();
    return;
  }
  trapTab(e, drawer);
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
  hideMenu(true, false);
  menuTrigger = document.activeElement;
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
  const first = menuEl.querySelector('.menu-item:not(:disabled):not(.menu-stepper)');
  if (first) first.focus();
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
      hideMenu(false, false);
      item.fn();
      if (mode === 'desktop' && term) {
        setTimeout(() => {
          if (!menuOpen && logLayer.hidden && pasteLayer.hidden && settingsLayer.hidden) term.focus();
        }, 0);
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

function hideMenu(immediate = false, restoreFocus = true) {
  if (!menuOpen && menuLayer.hidden) {
    menuTrigger = null;
    return;
  }
  menuOpen = false;
  document.removeEventListener('keydown', onMenuKey);
  menuLayer.classList.remove('open');
  clearTimeout(menuTimer);
  const trigger = menuTrigger;
  menuTrigger = null;
  const finish = () => {
    menuLayer.hidden = true;
    menuEl.innerHTML = '';
    if (restoreFocus && focusable(trigger) && logLayer.hidden && pasteLayer.hidden && settingsLayer.hidden) {
      trigger.focus();
    }
  };
  if (immediate) finish();
  else menuTimer = setTimeout(finish, 200);
}

// Tab 在当前浮层内循环，避免焦点跑到背后的终端。
function trapTab(e, container) {
  if (e.key !== 'Tab') return false;
  const nodes = Array.from(container.querySelectorAll(
    'button:not(:disabled), [href], input:not(:disabled), textarea:not(:disabled), select:not(:disabled), [tabindex]:not([tabindex="-1"])'
  )).filter((el) => el instanceof HTMLElement && (el.offsetParent !== null || el === document.activeElement));
  if (!nodes.length) return false;
  const first = nodes[0];
  const last = nodes[nodes.length - 1];
  if (e.shiftKey && document.activeElement === first) {
    e.preventDefault();
    last.focus();
    return true;
  }
  if (!e.shiftKey && document.activeElement === last) {
    e.preventDefault();
    first.focus();
    return true;
  }
  return false;
}

function onMenuKey(e) {
  if (e.key === 'Escape') {
    if (!logLayer.hidden || !settingsLayer.hidden || !pasteLayer.hidden) return; // 上层优先
    e.preventDefault();
    hideMenu();
    return;
  }
  if (e.key === 'Tab') {
    trapTab(e, menuEl);
    return;
  }
  if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp' && e.key !== 'Home' && e.key !== 'End') return;
  const nodes = Array.from(menuEl.querySelectorAll('.menu-item:not(:disabled):not(.menu-stepper)'));
  if (!nodes.length) return;
  e.preventDefault();
  const idx = nodes.indexOf(document.activeElement);
  let next;
  if (e.key === 'Home') next = 0;
  else if (e.key === 'End') next = nodes.length - 1;
  else if (e.key === 'ArrowDown') next = idx < 0 ? 0 : (idx + 1) % nodes.length;
  else next = idx < 0 ? nodes.length - 1 : (idx - 1 + nodes.length) % nodes.length;
  nodes[next].focus();
}

document.addEventListener('keydown', (e) => {
  if (e.key !== 'Escape' || menuOpen) return;
  // 各浮层自己处理 Escape；这里只兜底清除终端选区。
  if (!drawer.hidden || !logLayer.hidden || !settingsLayer.hidden || !pasteLayer.hidden) return;
  if (selection.active) { e.preventDefault(); clearSelection(); }
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
  items.push({ type: 'separator' });
  items.push({ label: '日志', icon: 'file', fn: openLogs });
  items.push({ label: '设置', icon: 'settings', fn: openSettings });
  items.push({
    label: '检查更新',
    icon: 'download',
    hint: versionLabel(serverVersion),
    disabled: updateState !== 'idle',
    fn: checkUpdate,
  });
  return items;
}

function sessionMenu(s) {
  const items = [];
  if (s.dead) items.push({ label: '重启会话', icon: 'refresh', fn: () => restartSession(s.id) });
  items.push({ label: '删除会话', icon: 'trash', danger: true, fn: () => removeSession(s.id) });
  return items;
}

/* ---------------- 自更新 ---------------- */
function showUpdateBanner(text, kind = 'info', sticky = false) {
  if (!updateBanner || !updateText) return;
  updateText.textContent = text;
  updateBanner.dataset.kind = kind;
  updateBanner.hidden = false;
  clearTimeout(updateBannerTimer);
  updateBannerTimer = null;
  if (!sticky) {
    updateBannerTimer = setTimeout(() => { updateBanner.hidden = true; }, kind === 'error' ? 8000 : 5000);
  }
}

function clearUpdateBanner() {
  clearTimeout(updateBannerTimer);
  updateBannerTimer = null;
  clearTimeout(updateVerifyTimer);
  updateVerifyTimer = null;
  clearTimeout(restartWatchdogTimer);
  restartWatchdogTimer = null;
  if (updateBanner) updateBanner.hidden = true;
}

// hello 携带新版本号时校验更新是否真的生效；同一连接只保留一个验证定时器。
function handleHelloVersion(v) {
  const prev = serverVersion;
  serverVersion = v;
  clearTimeout(updateVerifyTimer);
  updateVerifyTimer = null;
  clearTimeout(restartWatchdogTimer);
  restartWatchdogTimer = null;

  if (!pendingUpdateVersion) {
    if (prev && v !== prev) showToast('已更新到 ' + versionLabel(v), 3500, 'success');
    return;
  }
  if (v !== prev) {
    updateState = 'done';
    pendingUpdateVersion = '';
    showUpdateBanner('已更新到 ' + versionLabel(v), 'success');
    setTimeout(() => { if (updateState === 'done') updateState = 'idle'; }, 5000);
    return;
  }
  // 重连后版本没变：给服务端 15s 完成切换，再判定失败。
  if (updateState === 'restarting') {
    updateVerifyTimer = setTimeout(() => {
      updateVerifyTimer = null;
      if (!pendingUpdateVersion || updateState !== 'restarting') return;
      updateState = 'failed';
      pendingUpdateVersion = '';
      showUpdateBanner('更新未生效，请查看服务端日志', 'error');
      setTimeout(() => { if (updateState === 'failed') updateState = 'idle'; }, 5000);
    }, 15000);
  }
}

// 进程切换后 WS 一直连不上：60s 兜底，避免“正在切换进程”永久停留。
function startRestartWatchdog() {
  clearTimeout(restartWatchdogTimer);
  restartWatchdogTimer = setTimeout(() => {
    restartWatchdogTimer = null;
    if (updateState !== 'restarting') return;
    updateState = 'failed';
    pendingUpdateVersion = '';
    showUpdateBanner('更新后未能重新连接，请查看服务端日志', 'error');
    setTimeout(() => { if (updateState === 'failed') updateState = 'idle'; }, 5000);
  }, 60000);
}

async function checkUpdate() {
  if (updateState !== 'idle') return;
  updateState = 'checking';
  logEvent('info', 'client.update.check', 'current=' + serverVersion);
  hideToast();
  showToast('正在检查更新…', 2000, 'info');
  let resp;
  try {
    resp = await fetchWithTimeout('/update', { cache: 'no-store' }, 10000);
  } catch (err) {
    updateState = 'idle';
    logEvent('error', 'client.update.check_failed', (err && err.message) || String(err));
    showToast('检查更新失败: 无法连接服务器', 4000, 'error');
    return;
  }
  if (resp.status === 401) {
    updateState = 'idle';
    hideToast();
    showLogin();
    return;
  }
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) {
    updateState = 'idle';
    logEvent('warn', 'client.update.check_error', data.error || ('status=' + resp.status));
    showToast(data.error || ('检查更新失败 (' + resp.status + ')'), 4500, 'error');
    return;
  }
  if (data.updating) {
    updateState = 'idle';
    showToast('已有更新正在进行', 3000, 'warn');
    return;
  }
  if (!data.available) {
    updateState = 'idle';
    showToast('已是最新版本 ' + versionLabel(data.current || serverVersion), 3000, 'info');
    return;
  }
  updateState = 'idle'; // 让“取消”后仍可再次检查
  logEvent('info', 'client.update.available', 'current=' + data.current + ' latest=' + data.latest);
  const handoff = !!(cfg && cfg.capabilities && cfg.capabilities.handoff);
  const risk = handoff ? '正在运行的会话会保留' : '正在运行的会话可能丢失，请先保存工作';
  showMenu([
    { label: '取消', fn: () => {} },
    {
      label: '安装 ' + versionLabel(data.latest) + ' 并重启',
      icon: 'download',
      fn: () => installUpdate(data.latest),
    },
  ], {
    sheet: mode === 'mobile',
    title: '发现新版本',
    message: '当前 ' + versionLabel(data.current || serverVersion) + ' → ' + versionLabel(data.latest) +
      '；更新会重启服务，' + risk,
  });
}

async function installUpdate(latest) {
  if (updateState !== 'idle') return;
  updateState = 'installing';
  logEvent('info', 'client.update.install', 'latest=' + latest);
  hideToast();
  showUpdateBanner('正在下载并安装 ' + versionLabel(latest) + '…', 'info', true);
  let resp;
  try {
    resp = await fetchWithTimeout('/update', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'X-Onet-Update': '1' },
      body: '{}',
    }, 120000);
  } catch (err) {
    updateState = 'failed';
    pendingUpdateVersion = '';
    logEvent('error', 'client.update.install_failed', (err && err.message) || String(err));
    showUpdateBanner('更新请求失败，请检查服务端日志', 'error');
    setTimeout(() => { if (updateState === 'failed') updateState = 'idle'; }, 5000);
    return;
  }
  const data = await resp.json().catch(() => ({}));
  if (resp.status === 401) {
    updateState = 'idle';
    pendingUpdateVersion = '';
    clearUpdateBanner();
    showLogin();
    return;
  }
  if (!resp.ok) {
    updateState = 'failed';
    logEvent('warn', 'client.update.install_error', data.error || ('status=' + resp.status));
    showUpdateBanner(data.error || ('更新失败 (' + resp.status + ')'), 'error');
    setTimeout(() => { if (updateState === 'failed') updateState = 'idle'; }, 5000);
    return;
  }
  if (!data.updated) {
    updateState = 'idle';
    showUpdateBanner('已是最新版本', 'info');
    return;
  }
  updateState = 'restarting';
  pendingUpdateVersion = data.to || latest;
  logEvent('info', 'client.update.restarting', 'from=' + data.from + ' to=' + data.to);
  showUpdateBanner('已安装 ' + versionLabel(data.to) + '，正在切换进程…', 'info', true);
  startRestartWatchdog();
}

/* ---------------- 设置（配置读写 + 应用重启） ---------------- */
function setSettingsBusy(busy) {
  settingsSaving = busy;
  for (const id of ['settings-save', 'settings-apply', 'settings-cancel', 'settings-close']) {
    const el = document.getElementById(id);
    if (el) el.disabled = busy;
  }
}

function refreshSettingsTokenState() {
  if (!settingsTokenState) return;
  let text;
  let on;
  if (settingsClearToken.checked) {
    text = '将清除';
    on = false;
  } else if (settingsTokenInput.value) {
    text = '将更新';
    on = true;
  } else {
    text = settingsTokenSet ? '已设置' : '未设置';
    on = settingsTokenSet;
  }
  settingsTokenState.textContent = text;
  settingsTokenState.classList.toggle('on', on);
}

function fillSettings(c) {
  const setVal = (id, v) => {
    const el = document.getElementById(id);
    if (el) el.value = v == null ? '' : String(v);
  };
  setVal('set-host', c.host);
  setVal('set-port', c.port);
  setVal('set-shell', c.shell);
  setVal('set-cwd', c.cwd);
  setVal('set-scrollback', c.scrollback);
  setVal('set-font-size', c.font_size);
  setVal('set-ui', c.ui || 'auto');
  setVal('set-extra-keys', Array.isArray(c.extra_keys) ? c.extra_keys.join('\n') : '');
  setVal('set-log-file', c.log_file);
  setVal('set-log-level', c.log_level || 'info');
  setVal('set-log-max-size', c.log_max_size_mb);
  setVal('set-log-max-backups', c.log_max_backups);
  const termIO = document.getElementById('set-log-terminal-io');
  if (termIO) termIO.checked = !!c.log_terminal_io;
  const insecure = document.getElementById('set-allow-insecure');
  if (insecure) insecure.checked = !!c.allow_insecure;

  settingsTokenSet = !!c.token_set;
  settingsTokenInput.value = '';
  settingsTokenInput.type = 'password';
  settingsClearToken.checked = false;
  refreshSettingsTokenState();
  if (settingsSub) settingsSub.textContent = c.config_path || 'config.json';
}

async function openSettings() {
  if (settingsSaving) return;
  settingsTrigger = document.activeElement;
  hideMenu(true, false);
  settingsLayer.hidden = false;
  if (term) term.blur();
  document.addEventListener('keydown', onSettingsKey);
  setSettingsBusy(true);
  const closeBtn = document.getElementById('settings-close');
  if (closeBtn) closeBtn.focus();
  logEvent('debug', 'client.settings.open');
  try {
    const resp = await fetchWithTimeout('/config', { cache: 'no-store' }, 8000);
    if (resp.status === 401) {
      setSettingsBusy(false);
      closeSettings(false);
      showLogin();
      return;
    }
    if (!resp.ok) throw new Error('HTTP ' + resp.status);
    fillSettings(await resp.json());
    setSettingsBusy(false);
    const host = document.getElementById('set-host');
    if (focusable(host)) host.focus();
  } catch (err) {
    setSettingsBusy(false);
    logEvent('warn', 'client.settings.fetch_failed', (err && err.message) || String(err));
    showToast('读取配置失败: ' + ((err && err.message) || err), 4500, 'error');
    closeSettings();
  }
}

function closeSettings(restoreFocus = true) {
  document.removeEventListener('keydown', onSettingsKey);
  if (settingsLayer.hidden) return;
  settingsLayer.hidden = true;
  const trigger = settingsTrigger;
  settingsTrigger = null;
  if (restoreFocus && focusable(trigger)) trigger.focus();
  else if (mode === 'desktop' && term) term.focus();
}

function onSettingsKey(e) {
  if (e.key === 'Escape') {
    if (settingsSaving) return;
    e.preventDefault();
    closeSettings();
    return;
  }
  const panel = document.getElementById('settings-panel');
  if (panel) trapTab(e, panel);
}

function splitExtraKeys(raw) {
  return raw.split(/\r?\n/).map((s) => s.trim()).filter(Boolean);
}

function settingsNumber(id, fallback = 0) {
  const el = document.getElementById(id);
  if (!el) return fallback;
  const n = Number(el.value);
  return Number.isFinite(n) ? Math.trunc(n) : fallback;
}

function settingsPayload(restart) {
  return {
    host: document.getElementById('set-host').value.trim(),
    port: settingsNumber('set-port'),
    token: settingsTokenInput.value,
    clear_token: settingsClearToken.checked,
    shell: document.getElementById('set-shell').value,
    cwd: document.getElementById('set-cwd').value,
    scrollback: settingsNumber('set-scrollback'),
    font_size: settingsNumber('set-font-size'),
    ui: document.getElementById('set-ui').value,
    extra_keys: splitExtraKeys(document.getElementById('set-extra-keys').value),
    log_file: document.getElementById('set-log-file').value.trim(),
    log_level: document.getElementById('set-log-level').value,
    log_max_size_mb: settingsNumber('set-log-max-size'),
    log_max_backups: settingsNumber('set-log-max-backups'),
    log_terminal_io: document.getElementById('set-log-terminal-io').checked,
    allow_insecure: document.getElementById('set-allow-insecure').checked,
    restart: !!restart,
  };
}

function generateSettingsToken() {
  if (!window.crypto || typeof window.crypto.getRandomValues !== 'function') {
    showToast('当前浏览器无法生成安全令牌', 4000, 'error');
    return;
  }
  const buf = new Uint8Array(32);
  window.crypto.getRandomValues(buf);
  let bin = '';
  for (const b of buf) bin += String.fromCharCode(b);
  const token = btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  settingsTokenInput.value = token;
  settingsTokenInput.type = 'text';
  settingsClearToken.checked = false;
  refreshSettingsTokenState();
  settingsTokenInput.focus();
  settingsTokenInput.select();
  showToast('已生成随机令牌，保存后生效；请妥善保存', 5000, 'warn');
}

async function saveSettings(restart) {
  if (settingsSaving) return;
  const payload = settingsPayload(restart);
  if (!payload.host) {
    showToast('监听地址不能为空', 3000, 'error');
    return;
  }
  setSettingsBusy(true);
  logEvent('info', 'client.config.save',
    'restart=' + restart + ' host=' + payload.host + ' port=' + payload.port +
    ' token=' + (payload.clear_token ? 'clear' : payload.token ? 'update' : 'keep'));
  try {
    const resp = await fetchWithTimeout('/config', {
      method: 'PUT',
      cache: 'no-store',
      headers: { 'Content-Type': 'application/json', 'X-Onet-Config': '1' },
      body: JSON.stringify(payload),
    }, 20000);
    const data = await resp.json().catch(() => ({}));
    if (resp.status === 401) {
      setSettingsBusy(false);
      closeSettings(false);
      showLogin();
      return;
    }
    if (!resp.ok) {
      setSettingsBusy(false);
      logEvent('warn', 'client.config.save_failed', data.error || ('status=' + resp.status));
      showToast(data.error || ('保存配置失败 (' + resp.status + ')'), 5000, 'error');
      return;
    }

    logEvent('info', 'client.config.saved',
      'restart=' + !!data.restarting + ' host=' + data.host + ' port=' + data.port +
      ' token_changed=' + !!data.token_changed);
    if (!restart) {
      setSettingsBusy(false);
      closeSettings(false);
      showToast('配置已保存，重启后生效', 4000, 'success');
      return;
    }

    closeSettings(false);
    if (data.token_changed) showToast('访问令牌已更新，重启后需使用新令牌', 4000, 'warn');
    const url = configRestartURL(data.host, data.port);
    logEvent('info', 'client.config.restart', 'url=' + url);
    await waitForConfigRestart(url);
    setSettingsBusy(false);
  } catch (err) {
    setSettingsBusy(false);
    logEvent('error', 'client.config.save_error', (err && err.message) || String(err));
    showToast('保存配置失败: 无法连接服务器', 5000, 'error');
  }
}

function isLoopbackName(host) {
  const h = String(host || '').trim().toLowerCase().replace(/^\[|\]$/g, '');
  return h === '' || h === 'localhost' || h === '127.0.0.1' || h === '::1';
}

// 新监听地址 → 浏览器可访问的 URL；通配地址沿用当前主机名，端口不变时保留当前 origin。
function configRestartURL(host, port) {
  const cur = location;
  const curPort = cur.port || (cur.protocol === 'https:' ? '443' : '80');
  let h = String(host || '').trim().toLowerCase();
  const wildcard = !h || h === '0.0.0.0' || h === '::' || h === '[::]';
  const curLoop = isLoopbackName(cur.hostname);
  const newLoop = isLoopbackName(h);
  const bare = h.replace(/^\[|\]$/g, '');
  const sameHost = wildcard || (newLoop && curLoop) || bare === cur.hostname.toLowerCase();
  if (sameHost && Number(port) === Number(curPort)) return cur.origin + '/';
  if (wildcard || (newLoop && curLoop)) h = cur.hostname;
  h = h.replace(/^\[|\]$/g, '');
  const hostPort = h.includes(':') ? '[' + h + ']' : h;
  return cur.protocol + '//' + hostPort + ':' + port + '/';
}

async function waitForConfigRestart(url) {
  showUpdateBanner('配置已保存，正在重启服务…', 'info', true);
  await new Promise((r) => setTimeout(r, 1200));
  const deadline = performance.now() + 30000;
  while (performance.now() < deadline) {
    try {
      await fetchWithTimeout(url, { cache: 'no-store', mode: 'no-cors' }, 3000);
      location.replace(url);
      return;
    } catch { /* 进程尚未监听，继续等 */ }
    await new Promise((r) => setTimeout(r, 600));
  }
  showUpdateBanner('配置已保存，但重启后无法连接；请手动刷新页面', 'error');
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
  if (!sel) { showToast('没有可复制的内容', 3000, 'warn'); return; }
  writeClipboard(sel)
    .then(() => showToast('已复制全部', 1800, 'success'))
    .catch((err) => { logClipboardError('copy-all', err); showToast('复制失败', 5000, 'error'); });
}
function copySelectionOrAll() {
  if (!term) return;
  if (term.hasSelection()) {
    const text = term.getSelection();
    if (!text) { showToast('没有可复制的内容', 3000, 'warn'); return; }
    writeClipboard(text).then(() => {
      showToast('已复制', 1800, 'success');
      term.clearSelection();
      selection = { active: false, anchor: 0, focus: 0 };
      selectBar.hidden = true;
    }).catch((err) => { logClipboardError('copy-selection', err); showToast('复制失败', 5000, 'error'); });
  } else {
    copyAll();
  }
}

// HTTP 局域网（非 secure context）没有剪贴板读权限，必须提供手动粘贴面板。
function openPasteSheet() {
  if (!pasteLayer || !pasteInput) {
    showToast('无法打开粘贴面板', 3000, 'warn');
    return;
  }
  if (term) term.blur();
  pasteInput.value = '';
  pasteLayer.hidden = false;
  pasteInput.focus();
  document.addEventListener('keydown', onPasteKey);
}

function closePasteSheet({ insert = false } = {}) {
  document.removeEventListener('keydown', onPasteKey);
  if (!pasteLayer || pasteLayer.hidden) return;
  const value = insert && pasteInput ? pasteInput.value : '';
  pasteLayer.hidden = true;
  if (pasteInput) pasteInput.blur();
  if (value && term) term.paste(value);
  // 桌面回到终端；移动端不强制聚焦，避免软键盘被重新拉起。
  if (mode === 'desktop' && term) term.focus();
}

function onPasteKey(e) {
  if (e.key === 'Escape') {
    e.preventDefault();
    closePasteSheet();
    return;
  }
  const panel = $('#paste-panel');
  if (panel) trapTab(e, panel);
}

async function paste() {
  if (!term) return;
  if (!window.isSecureContext || typeof navigator.clipboard?.readText !== 'function') {
    openPasteSheet();
    return;
  }
  try {
    const text = await navigator.clipboard.readText();
    if (!text) { openPasteSheet(); return; }
    term.paste(text);
  } catch (err) {
    logClipboardError('paste', err);
    openPasteSheet();
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
      repeatTimer = setInterval(() => pressKey(k), REPEAT_INTERVAL_MS);
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
        }, LONG_PRESS_MS);
      } else if (REPEAT_KEYS.has(k)) {
        holdTimer = setTimeout(startRepeat, LONG_PRESS_MS);
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
const TOAST_MS = { info: 1800, success: 1800, warn: 3000, error: 5000 };

function hideToast() {
  clearTimeout(toastTimer);
  toastTimer = null;
  toastActiveKind = 'info';
  toastUntil = 0;
  if (toast) toast.hidden = true;
}

// kind: info|success|warn|error。错误优先：活跃 error 不被普通提示覆盖；
// warn 可被更明确的 success/error 替换，避免“连接断开”在重连成功后继续停留。
function showToast(text, ms, kind = 'info') {
  if (!toast) return;
  const now = performance.now();
  if (now < toastUntil && toastActiveKind === 'error' && kind !== 'error') return;
  const duration = (ms === undefined || ms === null) ? (TOAST_MS[kind] || 1800) : ms;
  toast.textContent = text;
  toast.dataset.kind = kind;
  toast.setAttribute('role', kind === 'error' ? 'alert' : 'status');
  toast.hidden = false;
  clearTimeout(toastTimer);
  toastActiveKind = kind;
  toastUntil = now + duration;
  toastTimer = setTimeout(hideToast, duration);
}

/* ---------------- 日志面板 ---------------- */
let logSource = 'client';
let serverLogText = '';
let serverLogError = '';

function clientLogText() {
  if (!clientLogs.length) return '';
  return clientLogs.map((e) => JSON.stringify(e)).join('\n') + '\n';
}

function serverLogLines() {
  if (!serverLogText) return 0;
  return serverLogText.replace(/\n$/, '').split('\n').length;
}

function updateLogSub() {
  if (serverLogError) {
    logSub.textContent = '客户端 ' + clientLogs.length + ' 条 · ' + serverLogError;
    return;
  }
  logSub.textContent = '客户端 ' + clientLogs.length + ' 条 · 服务端 ' + serverLogLines() + ' 行';
}

function renderLogBody() {
  if (logSource === 'client') {
    logBody.textContent = clientLogText() || '暂无客户端日志';
  } else {
    logBody.textContent = serverLogText || serverLogError || '加载中…';
  }
  logBody.scrollTop = logBody.scrollHeight;
  updateLogSub();
}

function openLogs() {
  if (!term) return;
  flushClientLogs();
  persistLogQueue();
  logTrigger = document.activeElement;
  logLayer.hidden = false;
  term.blur(); // 让出焦点，面板内可以正常选中文本
  setLogSource('client');
  refreshServerLogs();
  const closeBtn = $('#log-close');
  if (closeBtn) closeBtn.focus();
  document.addEventListener('keydown', onLogKey);
  logEvent('debug', 'client.logs.open');
}

function focusable(el) {
  return !!(el && el.isConnected && el.offsetParent !== null);
}

function closeLogs() {
  document.removeEventListener('keydown', onLogKey);
  if (logLayer.hidden) return;
  logLayer.hidden = true;
  const trigger = logTrigger;
  logTrigger = null;
  if (focusable(trigger)) trigger.focus();
  else if (mode === 'desktop' && term) term.focus();
}

function onLogKey(e) {
  if (e.key === 'Escape') {
    e.preventDefault();
    closeLogs();
    return;
  }
  const panel = $('#log-panel');
  if (panel) trapTab(e, panel);
}

function setLogSource(src) {
  logSource = src;
  logTabClient.classList.toggle('active', src === 'client');
  logTabServer.classList.toggle('active', src === 'server');
  renderLogBody();
  if (src === 'server' && !serverLogText && !serverLogError) refreshServerLogs();
}

async function refreshServerLogs() {
  try {
    const r = await fetch('/logs?tail=2000', { cache: 'no-store' });
    if (r.status === 404) {
      serverLogText = '';
      serverLogError = '服务端文件日志未启用';
    } else if (!r.ok) {
      serverLogText = '';
      serverLogError = '服务端日志读取失败 (' + r.status + ')';
    } else {
      serverLogText = await r.text();
      serverLogError = '';
    }
  } catch (err) {
    serverLogText = '';
    serverLogError = '服务端日志读取失败';
    logEvent('warn', 'client.logs.fetch_failed', (err && err.message) || String(err));
  }
  if (logSource === 'server') renderLogBody();
  else updateLogSub();
}

async function refreshLogs() {
  flushClientLogs();
  persistLogQueue();
  if (logSource === 'server') {
    serverLogText = '';
    serverLogError = '';
    await refreshServerLogs();
    return;
  }
  renderLogBody();
}

async function copyLogs() {
  const text = logSource === 'client' ? clientLogText() : (serverLogText || serverLogError);
  if (!text) { showToast('没有日志', 1800, 'info'); return; }
  try {
    await writeClipboard(text);
    showToast('已复制', 1800, 'success');
  } catch (err) {
    logClipboardError('copy-logs', err);
    showToast('复制失败', 5000, 'error');
  }
}

function logStamp() {
  const d = new Date();
  const p = (n) => String(n).padStart(2, '0');
  return d.getFullYear() + p(d.getMonth() + 1) + p(d.getDate()) +
    '-' + p(d.getHours()) + p(d.getMinutes()) + p(d.getSeconds());
}

async function downloadLogs() {
  const clientText = clientLogText();
  let serverText = '';
  try {
    const r = await fetch('/logs?download=1', { cache: 'no-store' });
    if (r.ok) serverText = await r.text();
  } catch { /* 服务端部分拿不到时仍下载客户端日志 */ }
  const text = clientText + serverText;
  if (!text) { showToast('没有日志', 1800, 'info'); return; }

  const blob = new Blob([text], { type: 'application/x-ndjson' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = 'onet-logs-' + logStamp() + '.jsonl';
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
  logEvent('info', 'client.logs.download',
    'client_lines=' + (clientText ? clientText.replace(/\n$/, '').split('\n').length : 0) +
      ' server_bytes=' + serverText.length);
}

onTap($('#log-close'), closeLogs);
onTap($('#log-refresh'), refreshLogs);
onTap($('#log-copy'), copyLogs);
onTap($('#log-download'), downloadLogs);
onTap(logTabClient, () => setLogSource('client'));
onTap(logTabServer, () => setLogSource('server'));
logLayer.addEventListener('pointerdown', (e) => { if (e.target === logLayer) closeLogs(); });

/* ---------------- 设置绑定 ---------------- */
if (settingsForm) {
  settingsForm.addEventListener('submit', (e) => {
    e.preventDefault();
    saveSettings(true);
  });
}
onTap($('#settings-close'), () => closeSettings());
onTap($('#settings-cancel'), () => closeSettings());
onTap($('#settings-save'), () => saveSettings(false));
onTap($('#set-token-gen'), generateSettingsToken);
settingsTokenInput.addEventListener('input', () => {
  if (settingsTokenInput.value) settingsClearToken.checked = false;
  refreshSettingsTokenState();
});
settingsClearToken.addEventListener('change', () => {
  if (settingsClearToken.checked) {
    settingsTokenInput.value = '';
    settingsTokenInput.type = 'password';
  }
  refreshSettingsTokenState();
});
settingsLayer.addEventListener('pointerdown', (e) => {
  if (!settingsSaving && e.target === settingsLayer) closeSettings();
});

/* ---------------- 手动粘贴面板 ---------------- */
if (pasteOk) onTap(pasteOk, () => closePasteSheet({ insert: true }));
if (pasteCancel) onTap(pasteCancel, () => closePasteSheet());
if (pasteLayer) {
  pasteLayer.addEventListener('pointerdown', (e) => {
    if (e.target === pasteLayer) closePasteSheet();
  });
}
if (bootRetry) onTap(bootRetry, () => location.reload());

/* ---------------- 启动 ---------------- */
restoreLogQueue();
installLogHooks();
boot();

})();
