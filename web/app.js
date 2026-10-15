/* onet — 前端 UI 与 WS 协议逻辑（双模式自适应：移动 termux 风格 / 桌面经典终端） */
'use strict';
(() => {

const $ = (sel) => document.querySelector(sel);
const clamp = (v, lo, hi) => Math.max(lo, Math.min(hi, v));

/* ---------------- 状态 ---------------- */
let cfg = null;          // /config 返回的配置
let mode = 'mobile';     // 'mobile' | 'desktop'
let ws = null;
let term = null;
let fit = null;
let sessions = [];       // SessionInfo[]
let current = null;      // 当前 attach 的会话 {id, name}
let booting = true;      // 首次 sessions 响应前为 true
let keysVisible = true;
let reconnectDelay = 1000;
let modState = { ctrl: 0, alt: 0 };   // 0=off 1=点按(仅下一次) 2=长按锁定
let lpTimer = null, lpStart = null, pinch = null;

/* ---------------- DOM ---------------- */
const app = $('#app');
const termEl = $('#term');
const placeholder = $('#placeholder');
const mStatus = $('#m-status'), dBar = $('#d-bar');
const mName = $('#m-sess-name'), dName = $('#d-sess-name');
const mDot = $('#m-dot'), dDot = $('#d-dot');
const exitBanner = $('#exit-banner'), exitText = $('#exit-text');
const keysBar = $('#keys');
const drawer = $('#drawer'), drawerList = $('#drawer-list'), scrim = $('#scrim');
const ctx = $('#ctx');
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
    resp = await fetch('/config' + (qs.get('token') ? '?token=' + encodeURIComponent(qs.get('token')) : ''));
  } catch {
    showToast('无法连接服务器');
    return;
  }
  if (resp.status === 401) { showLogin(); return; }
  if (!resp.ok) { showToast('配置加载失败 (' + resp.status + ')'); return; }

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
  if (next !== mode) {
    mode = next;
    renderKeys();
  }
  document.body.classList.toggle('mode-mobile', mode === 'mobile');
  document.body.classList.toggle('mode-desktop', mode === 'desktop');
  // 按键栏可见性：移动端默认显示；桌面端由 ⌨ 按钮 + localStorage 记忆
  keysVisible = localStorage.getItem('onet_keys') === '1'
    ? true : localStorage.getItem('onet_keys') === '0'
    ? false : mode === 'mobile';
  updateKeysVisibility();
  fitTerm();
}

function updateKeysVisibility() {
  keysBar.classList.toggle('show', !!(keysVisible && term));
}

matchMedia('(pointer: coarse)').addEventListener('change', applyMode);
matchMedia('(pointer: fine)').addEventListener('change', applyMode);
window.addEventListener('resize', () => { applyMode(); layout(); });

/* ---------------- 布局 / 尺寸 ---------------- */
function layout() {
  const vv = window.visualViewport;
  if (vv) app.style.height = vv.height + 'px';
  else app.style.height = '';
  fitTerm();
}

function fitTerm() {
  if (!fit || !term) return;
  try { fit.fit(); } catch { /* 隐藏或零尺寸时忽略 */ }
}

function sendResize() {
  if (!current || !term) return;
  sendCmd({ type: 'resize', id: current.id, cols: term.cols, rows: term.rows });
}

let resizeTimer = null;
function scheduleResize() {
  clearTimeout(resizeTimer);
  resizeTimer = setTimeout(sendResize, 80);
}

/* ---------------- xterm ---------------- */
function initTerm() {
  term = new window.Terminal({
    fontSize: cfg.font_size || 15,
    scrollback: cfg.scrollback || 5000,
    cursorBlink: true,
    allowProposedApi: true,
    theme: {
      background: '#101014', foreground: '#e6e6e6',
      cursor: '#e6e6e6', cursorAccent: '#101014',
      selectionBackground: '#3b5b8c',
    },
  });
  fit = new window.FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open(termEl);

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
    showMenu(ev.clientX, ev.clientY, desktopMenu());
  });

  // 触屏：长按菜单 + 捏合缩放（移动）
  termEl.addEventListener('touchstart', (ev) => {
    if (mode !== 'mobile') return;
    if (ev.touches.length === 2) {
      cancelLongPress();
      pinch = { d: touchDist(ev.touches), fs: term.options.fontSize };
      ev.preventDefault();
    } else if (ev.touches.length === 1) {
      pinch = null;
      lpStart = { x: ev.touches[0].clientX, y: ev.touches[0].clientY };
      lpTimer = setTimeout(() => {
        if (lpStart) showMenu(lpStart.x, lpStart.y, mobileMenu());
      }, 700);
    }
  }, { passive: false });

  termEl.addEventListener('touchmove', (ev) => {
    if (mode !== 'mobile') return;
    if (pinch && ev.touches.length === 2) {
      ev.preventDefault();
      const ratio = touchDist(ev.touches) / pinch.d;
      setFontSize(Math.round(pinch.fs * ratio));
      return;
    }
    if (!lpTimer || !lpStart || ev.touches.length !== 1) return;
    const t = ev.touches[0];
    if (Math.abs(t.clientX - lpStart.x) > 10 || Math.abs(t.clientY - lpStart.y) > 10) {
      cancelLongPress();
    }
  }, { passive: true });

  const touchEnd = () => { pinch = null; cancelLongPress(); };
  termEl.addEventListener('touchend', touchEnd, { passive: true });
  termEl.addEventListener('touchcancel', touchEnd, { passive: true });

  term.onResize(() => scheduleResize());
  renderKeys();
  updateKeysVisibility();
  layout();

  // visualViewport（键盘弹起/收起、旋转）
  if (window.visualViewport) {
    window.visualViewport.addEventListener('resize', layout);
    window.visualViewport.addEventListener('scroll', layout);
  }
  window.addEventListener('resize', layout);
}

function cancelLongPress() {
  clearTimeout(lpTimer);
  lpTimer = null;
  lpStart = null;
}

function touchDist(touches) {
  const dx = touches[0].clientX - touches[1].clientX;
  const dy = touches[0].clientY - touches[1].clientY;
  return Math.hypot(dx, dy);
}

function setFontSize(n) {
  const fs = clamp(Math.round(n), 8, 32);
  if (fs === term.options.fontSize) return;
  term.options.fontSize = fs;
  fitTerm();
  scheduleResize();
  showToast('字号 ' + fs, 900);
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
    reconnectDelay = 1000;
    booting = true;
    sendCmd({ type: 'list' });
  };
  ws.onmessage = (ev) => {
    if (typeof ev.data === 'string') return; // 服务器只发二进制
    handleMsg(ev.data);
  };
  ws.onclose = () => {
    current = null;
    updateStatus();
    setExitBanner(null);
    renderSessions();
    showToast('连接断开，重连中…');
    setTimeout(connectWS, reconnectDelay);
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
    case 'output':
      if (msg.id === (current && current.id)) term.write(b64ToBytes(msg.data));
      break;
    case 'exit':
      showToast('会话 ' + (msg.id) + ' 已退出 (code ' + msg.code + ')', 2000);
      break;
    case 'sessions':
      onSessions(msg.sessions);
      break;
    case 'attached':
      current = { id: msg.id, name: findSession(msg.id) ? (findSession(msg.id).name || findSession(msg.id).id) : msg.id };
      placeholder.hidden = true;
      setExitBanner(null);
      updateStatus();
      renderSessions();
      fitTerm();
      scheduleResize();
      break;
    case 'created':
      sendCmd({ type: 'attach', id: msg.id });
      break;
    case 'detached':
      current = null;
      placeholder.hidden = false;
      renderSessions();
      break;
    case 'error':
      if (booting && /已退出|不存在/.test(msg.message)) {
        booting = false;
        sendCmd({ type: 'create', name: '' });
      } else {
        showToast(msg.message, 2500);
      }
      break;
  }
}

function findSession(id) {
  return sessions.find((s) => s.id === id) || null;
}

function onSessions(list) {
  sessions = list;
  renderSessions();
  if (booting) {
    booting = false;
    const target = pickTarget();
    if (target) sendCmd({ type: 'attach', id: target.id });
    else sendCmd({ type: 'create', name: '' });
    return;
  }
  // 当前会话可能已被移除或退出
  const cur = current && findSession(current.id);
  if (!cur) {
    current = null;
    placeholder.hidden = false;
    updateStatus();
    setExitBanner(null);
  } else {
    updateStatus();
    if (cur.dead) setExitBanner(cur);
    else setExitBanner(null);
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

function switchTo(s) {
  if (s.id === (current && current.id)) return;
  if (s.dead) { showToast('会话已退出，请重启', 2000); return; }
  sendCmd({ type: 'attach', id: s.id });
}

function restartSession(id) {
  sendCmd({ type: 'restart', id });
  if (current && current.id === id) setExitBanner(null);
}

function removeSession(id) {
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
  setExitBanner(cur && cur.dead ? cur : null);
}

function setExitBanner(sess) {
  if (!sess) { exitBanner.hidden = true; return; }
  exitText.textContent = '会话「' + (sess.name || sess.id) + '」进程已退出 (code ' + sess.exitCode + ')';
  exitBanner.hidden = false;
}

function renderSessions() {
  renderTabs();
  renderDrawer();
  updateStatus();
}

function renderTabs() {
  const box = $('#d-tabs');
  box.innerHTML = '';
  for (const s of sessions) {
    const tab = document.createElement('div');
    tab.className = 'tab' + (s.id === (current && current.id) ? ' active' : '') + (s.dead ? ' dead' : '');

    const dot = document.createElement('span');
    dot.className = 'dot ' + (s.dead ? 'off' : 'on');

    const name = document.createElement('span');
    name.className = 'tab-name';
    name.textContent = s.name || s.id;
    name.addEventListener('click', () => switchTo(s));

    tab.appendChild(dot);
    tab.appendChild(name);

    if (s.dead) {
      const rst = document.createElement('button');
      rst.className = 'tab-btn';
      rst.textContent = '↻';
      rst.title = '重启会话';
      rst.addEventListener('click', (ev) => { ev.stopPropagation(); restartSession(s.id); });
      tab.appendChild(rst);
    }

    const x = document.createElement('button');
    x.className = 'tab-btn';
    x.textContent = '×';
    x.title = '删除会话';
    x.addEventListener('click', (ev) => { ev.stopPropagation(); removeSession(s.id); });
    tab.appendChild(x);

    box.appendChild(tab);
  }
}

function renderDrawer() {
  drawerList.innerHTML = '';
  for (const s of sessions) {
    const li = document.createElement('li');
    li.className = 'd-item' + (s.id === (current && current.id) ? ' active' : '');

    const dot = document.createElement('span');
    dot.className = 'dot ' + (s.dead ? 'off' : 'on');

    const label = document.createElement('span');
    label.className = 'd-label';
    label.textContent = s.name || s.id;

    const meta = document.createElement('span');
    meta.className = 'd-meta';
    meta.textContent = (s.dead ? '已退出 ' : '') + (s.attached > 0 ? s.attached + ' 在线' : '');

    li.append(dot, label, meta);
    li.addEventListener('click', () => { closeDrawer(); switchTo(s); });
    li.addEventListener('contextmenu', (ev) => {
      ev.preventDefault();
      showMenu(ev.clientX, ev.clientY, sessionMenu(s));
    });
    drawerList.appendChild(li);
  }
}

/* ---------------- 抽屉（移动端） ---------------- */
function openDrawer() { drawer.hidden = false; scrim.hidden = false; requestAnimationFrame(() => { drawer.classList.add('open'); scrim.classList.add('open'); }); }
function closeDrawer() {
  drawer.classList.remove('open');
  scrim.classList.remove('open');
  setTimeout(() => { drawer.hidden = true; scrim.hidden = true; }, 200);
}
$('#m-menu-btn').addEventListener('click', openDrawer);
$('#drawer-new').addEventListener('click', () => { closeDrawer(); createSession(); });
scrim.addEventListener('click', closeDrawer);
// 左缘滑入
app.addEventListener('touchstart', (ev) => {
  if (mode !== 'mobile' || drawer.hidden || ev.touches.length !== 1) return;
  if (ev.touches[0].clientX < 16) openDrawer();
}, { passive: true });

/* ---------------- 上下文菜单 ---------------- */
function showMenu(x, y, items) {
  ctx.innerHTML = '';
  for (const it of items) {
    const b = document.createElement('button');
    b.textContent = it.label;
    b.className = it.danger ? 'danger' : '';
    b.addEventListener('click', () => { hideMenu(); it.fn(); });
    ctx.appendChild(b);
  }
  ctx.hidden = false;
  const rect = ctx.getBoundingClientRect();
  const pad = 8;
  ctx.style.left = clamp(x, pad, innerWidth - rect.width - pad) + 'px';
  ctx.style.top = clamp(y, pad, innerHeight - rect.height - pad) + 'px';
  setTimeout(() => document.addEventListener('click', hideOnce, true), 0);
}
function hideOnce() { hideMenu(); document.removeEventListener('click', hideOnce, true); }
function hideMenu() { ctx.hidden = true; }

function mobileMenu() {
  return [
    { label: '复制全部', fn: copyAll },
    { label: '粘贴', fn: paste },
    { label: '重置终端', fn: () => term.reset() },
    { label: '字号−', fn: () => setFontSize(term.options.fontSize - 1) },
    { label: '字号＋', fn: () => setFontSize(term.options.fontSize + 1) },
    { label: keysVisible ? '隐藏按键栏' : '显示按键栏', fn: toggleKeys },
  ];
}

function desktopMenu() {
  return [
    { label: '复制', fn: copySelectionOrAll },
    { label: '粘贴', fn: paste },
    { label: '重置终端', fn: () => term.reset() },
    { label: '字号−', fn: () => setFontSize(term.options.fontSize - 1) },
    { label: '字号＋', fn: () => setFontSize(term.options.fontSize + 1) },
  ];
}

function sessionMenu(s) {
  return [
    { label: '重启', fn: () => restartSession(s.id) },
    { label: '删除', danger: true, fn: () => removeSession(s.id) },
  ];
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
    writeClipboard(term.getSelection()).then(() => showToast('已复制')).catch(() => showToast('复制失败'));
    term.clearSelection();
  } else copyAll();
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
function renderKeys() {
  keysBar.innerHTML = '';
  const keys = (cfg && cfg.extra_keys && cfg.extra_keys.length) ? cfg.extra_keys : ['ESC','/','-','TAB','CTRL','ALT','←','↑','↓','→'];
  for (const k of keys) {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'key' + (k === 'CTRL' || k === 'ALT' ? ' mod' : '');
    b.textContent = k;
    b.dataset.key = k;

    let lp = null;
    let lastTouchEnd = 0;
    const modName = k.toLowerCase(); // modState 键为小写
    b.addEventListener('touchstart', (ev) => {
      if (k !== 'CTRL' && k !== 'ALT') return;
      lp = setTimeout(() => { lp = null; toggleMod(modName, true); }, 500);
    }, { passive: true });
    // 触屏：touchend 即触发按键；CTRL/ALT 为点按修饰（长按锁定）
    b.addEventListener('touchend', (ev) => {
      lastTouchEnd = Date.now();
      if (k === 'CTRL' || k === 'ALT') {
        if (lp) { clearTimeout(lp); lp = null; toggleMod(modName, false); }
        return;
      }
      pressKey(k);
    }, { passive: true });
    b.addEventListener('touchcancel', () => { clearTimeout(lp); lp = null; }, { passive: true });
    // 鼠标/桌面点击；触屏 touchend 后 500ms 内的 ghost click 忽略
    b.addEventListener('click', () => {
      if (Date.now() - lastTouchEnd < 500) return;
      if (k === 'CTRL' || k === 'ALT') { toggleMod(modName, false); return; }
      pressKey(k);
    });

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
  let ch = k;
  if (k === 'ESC') ch = '\x1b';
  else if (k === 'TAB') ch = '\t';
  else if (k === '←') ch = '\x1b[D';
  else if (k === '↑') ch = '\x1b[A';
  else if (k === '↓') ch = '\x1b[B';
  else if (k === '→') ch = '\x1b[C';

  let out = ch;
  if (modState.ctrl && ch.length === 1 && ch !== '\t' && ch !== '\x1b') {
    out = String.fromCharCode(ch.charCodeAt(0) & 0x1f);
  }
  if (modState.alt && ch.length === 1 && out.length === 1 && out !== '\x1b') {
    out = '\x1b' + out;
  }

  // 消费点按修饰
  for (const n of ['ctrl', 'alt']) if (modState[n] === 1) modState[n] = 0;
  updateModUI();

  if (ws && ws.readyState === 1) ws.send(out);
}

$('#d-keys-btn').addEventListener('click', toggleKeys);
$('#d-new-btn').addEventListener('click', createSession);
$('#m-new-btn').addEventListener('click', createSession);
$('#ph-new').addEventListener('click', createSession);
$('#exit-restart').addEventListener('click', () => { if (current) restartSession(current.id); });
$('#exit-remove').addEventListener('click', () => { if (current) removeCurrent(); });

/* ---------------- 轻提示 ---------------- */
let toastTimer = null;
function showToast(text, ms = 1800) {
  toast.textContent = text;
  toast.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { toast.hidden = true; }, ms);
}

/* ---------------- 启动 ---------------- */
boot();

})();
