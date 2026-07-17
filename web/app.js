"use strict";

const $ = (sel) => document.querySelector(sel);

const isMac = /Mac|iP(hone|ad|od)/.test(navigator.platform || navigator.userAgent);

// --- icons ---
// One stroke-based set (lucide-style paths) for all UI chrome, so nothing
// falls back to platform emoji rendering. Sized via CSS (.icon).

const ICONS = {
  bell: '<path d="M10.27 21a2 2 0 0 0 3.46 0"/><path d="M6 8a6 6 0 0 1 12 0c0 4.5 1.41 5.96 2.74 7.33A1 1 0 0 1 20 17H4a1 1 0 0 1-.74-1.67C4.59 13.96 6 12.5 6 8"/>',
  columns: '<rect width="18" height="18" x="3" y="3" rx="2"/><path d="M12 3v18"/>',
  refresh: '<path d="M21 12a9 9 0 1 1-2.64-6.36L21 8"/><path d="M21 3v5h-5"/>',
  pencil: '<path d="M17 3a2.828 2.828 0 1 1 4 4L7.5 20.5 2 22l1.5-5.5L17 3z"/>',
  x: '<path d="M18 6 6 18"/><path d="m6 6 12 12"/>',
  plus: '<path d="M5 12h14"/><path d="M12 5v14"/>',
  grip: '<circle cx="9" cy="5" r="1"/><circle cx="9" cy="12" r="1"/><circle cx="9" cy="19" r="1"/><circle cx="15" cy="5" r="1"/><circle cx="15" cy="12" r="1"/><circle cx="15" cy="19" r="1"/>',
  chevronLeft: '<path d="m15 18-6-6 6-6"/>',
  chevronRight: '<path d="m9 18 6-6-6-6"/>',
  chevronDown: '<path d="m6 9 6 6 6-6"/>',
  linkOff: '<path d="M9 17H7A5 5 0 0 1 7 7"/><path d="M15 7h2a5 5 0 0 1 4 8"/><path d="M8 12h4"/><path d="m2 2 20 20"/>',
};

function icon(name) {
  return `<svg class="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${ICONS[name]}</svg>`;
}

$("#notify").innerHTML = icon("bell");
$("#split").innerHTML = icon("columns");
$("#refresh").innerHTML = icon("refresh");
$("#new-session button").innerHTML = icon("plus");

// --- persisted prefs ---

let fontSize = Math.max(9, Math.min(24, +localStorage.getItem("muxdeck-font") || 13));
let order = [];
try { order = JSON.parse(localStorage.getItem("muxdeck-order")) || []; } catch {}

function saveOrder() { localStorage.setItem("muxdeck-order", JSON.stringify(order)); }

// --- API helpers ---

async function api(path, opts = {}) {
  const res = await fetch(path, {
    headers: { "Content-Type": "application/json" },
    ...opts,
  });
  if (res.status === 401) {
    showLogin();
    throw new Error("unauthorized");
  }
  if (!res.ok) {
    let msg = res.statusText;
    try { msg = (await res.json()).error || msg; } catch {}
    throw new Error(msg);
  }
  return res.status === 204 ? null : res.json();
}

// --- login ---

function showLogin() {
  $("#login").hidden = false;
  $("#token").focus();
}

$("#login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const res = await fetch("/api/login", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ token: $("#token").value }),
  });
  if (res.ok) {
    $("#login").hidden = true;
    $("#login-error").hidden = true;
    $("#token").value = "";
    await refreshSessions();
    if (!panes.some((p) => p.session)) attachFromHash();
  } else {
    $("#login-error").hidden = false;
  }
});

// --- clipboard ---
// Copy flows: browser selection is copied at gesture end; tmux copy-mode
// yanks arrive as OSC 52 (set-clipboard, enabled server-side) and are
// forwarded to the browser clipboard. Paste: native Cmd/Ctrl+V on desktop,
// keybar button on touch.

function osc52ToClipboard(data) {
  const i = data.indexOf(";");
  if (i < 0) return;
  const b64 = data.slice(i + 1);
  if (b64 === "?") return; // clipboard read request: unsupported
  let text;
  try { text = new TextDecoder().decode(Uint8Array.from(atob(b64), (c) => c.charCodeAt(0))); }
  catch { return; }
  navigator.clipboard?.writeText(text).catch(() => {});
}

function copySelection(term) {
  if (term && term.hasSelection()) {
    navigator.clipboard?.writeText(term.getSelection()).catch(() => {});
  }
}

async function pasteToFocused() {
  const p = paneFor();
  if (!p?.term) return;
  try {
    const text = await navigator.clipboard.readText();
    if (text) p.term.paste(text);
  } catch { alert("clipboard read not permitted"); }
  p.term.focus();
}

// --- keyboard chords (Cmd+K / Cmd+F on mac, Ctrl+Shift+K / +F elsewhere;
// plain Ctrl combos belong to the shell) ---

function chord(e, key) {
  return e.key.toLowerCase() === key && !e.altKey &&
    (isMac ? e.metaKey && !e.ctrlKey : e.ctrlKey && e.shiftKey);
}

document.addEventListener("keydown", (e) => {
  if (chord(e, "k")) {
    e.preventDefault();
    $("#palette").hidden ? openPalette("switch") : closePalette();
  } else if (chord(e, "f") && paneFor()?.term) {
    e.preventDefault();
    paneFor().toggleFind(true);
  } else if (chord(e, "b")) {
    e.preventDefault();
    toggleSidebar();
  }
});

// --- panes ---

let panes = [];
let focusedPane = null;

function paneFor() { return focusedPane || panes[0]; }

function setFocus(p) {
  focusedPane = p;
  for (const q of panes) q.el.classList.toggle("focused", q === p);
}

class Pane {
  constructor() {
    this.session = null;
    this.term = null;
    this.fit = null;
    this.search = null;
    this.ws = null;
    this.retries = 0;
    this.retryTimer = null;
    this.deliberate = false; // detach in progress: ignore the ws close

    this.el = document.createElement("section");
    this.el.className = "pane";
    this.el.innerHTML = `
      <div class="pane-head">
        <span class="pane-title">no session</span>
        <span class="pane-tools">
          <button class="p-font" data-d="-1" title="smaller text">A&minus;</button>
          <button class="p-font" data-d="1" title="larger text">A+</button>
          <button class="p-find" title="find in scrollback">find</button>
          <button class="p-mouse" hidden title="tmux mouse mode: wheel/touch scrolls the session instead of the page">mouse</button>
          <button class="p-close" title="close pane">${icon("x")}</button>
        </span>
      </div>
      <div class="findbar" hidden>
        <input placeholder="find" spellcheck="false" autocomplete="off">
        <button class="f-prev" title="previous match">${icon("chevronLeft")}</button>
        <button class="f-next" title="next match">${icon("chevronRight")}</button>
        <button class="f-close" title="close">${icon("x")}</button>
      </div>
      <div class="pane-body">
        <div class="pane-term"></div>
        <div class="pane-msg">select or create a session</div>
      </div>`;

    this.el.addEventListener("pointerdown", () => setFocus(this));
    this.el.querySelector(".p-close").addEventListener("click", () => closePane(this));
    this.el.querySelector(".p-mouse").addEventListener("click", () => this.toggleMouse());
    this.el.querySelector(".p-find").addEventListener("click", () => this.toggleFind());
    for (const b of this.el.querySelectorAll(".p-font")) {
      b.addEventListener("click", () => setFontSize(fontSize + +b.dataset.d));
    }
    this.el.querySelector(".pane-msg").addEventListener("click", () => this.reconnectNow());

    const fb = this.el.querySelector(".findbar");
    this.findInput = fb.querySelector("input");
    this.findInput.addEventListener("input", () => this.findNext(true));
    this.findInput.addEventListener("keydown", (e) => {
      if (e.key === "Enter") (e.shiftKey ? this.findPrev() : this.findNext(false));
      if (e.key === "Escape") this.toggleFind(false);
    });
    fb.querySelector(".f-next").addEventListener("click", () => this.findNext(false));
    fb.querySelector(".f-prev").addEventListener("click", () => this.findPrev());
    fb.querySelector(".f-close").addEventListener("click", () => this.toggleFind(false));

    new ResizeObserver(() => this.sendResize()).observe(this.el.querySelector(".pane-term"));
  }

  msg(text) {
    const m = this.el.querySelector(".pane-msg");
    m.textContent = text || "";
    m.hidden = !text;
  }

  attach(name) {
    if (this.session) this.detach(true);
    this.session = name;
    this.el.querySelector(".pane-title").textContent = name;
    this.msg("");
    this.makeTerm();
    this.connect();
    this.refreshMouse();
    updateHash();
  }

  makeTerm() {
    if (this.term) this.term.dispose();
    this.term = new Terminal({
      fontFamily: "SF Mono, Menlo, Monaco, monospace",
      fontSize,
      cursorBlink: true,
      scrollback: 10000,
      macOptionClickForcesSelection: true,
      allowProposedApi: true, // Unicode11Addon's version switch needs it
      theme: { background: "#000000" },
    });
    this.fit = new FitAddon.FitAddon();
    this.search = new SearchAddon.SearchAddon();
    this.term.loadAddon(this.fit);
    this.term.loadAddon(this.search);
    this.term.loadAddon(new WebLinksAddon.WebLinksAddon((e, uri) => window.open(uri, "_blank", "noopener")));
    // Match tmux's modern wcwidth: with the default Unicode 6 tables,
    // glyphs like ⏺ and ⚡ (wide since Unicode 9) disagree with tmux's
    // layout and TUI columns drift — the "garbled box art" bug.
    this.term.loadAddon(new Unicode11Addon.Unicode11Addon());
    this.term.unicode.activeVersion = "11";
    this.term.parser.registerOscHandler(52, (data) => { osc52ToClipboard(data); return true; });
    this.term.onData((d) => this.send(applyCtrl(d)));
    this.term.attachCustomKeyEventHandler((e) => !(chord(e, "k") || chord(e, "f") || chord(e, "b")));
    const mount = this.el.querySelector(".pane-term");
    mount.innerHTML = "";
    this.term.open(mount);
    // WebGL renderer (must load after open): draws box/block glyphs itself
    // via customGlyphs, avoiding the DOM renderer's font-metric seams.
    // Falls back to the DOM renderer wherever a context isn't available.
    try {
      const gl = new WebglAddon.WebglAddon();
      gl.onContextLoss(() => gl.dispose());
      this.term.loadAddon(gl);
    } catch {}
    // Clipboard writes need transient activation, so copy at gesture end
    // rather than on every selection change.
    mount.addEventListener("mouseup", () => copySelection(this.term));
    mount.addEventListener("touchend", () => copySelection(this.term));
    this.fit.fit();
    this.term.focus();
  }

  connect() {
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    const ws = new WebSocket(`${proto}//${location.host}${keyApi(this.session, "/attach")}`);
    ws.binaryType = "arraybuffer";
    this.ws = ws;
    ws.onopen = () => {
      this.retries = 0;
      this.msg("");
      this.sendResize();
      refreshSessions();
    };
    ws.onmessage = (e) => {
      if (e.data instanceof ArrayBuffer) {
        this.term.write(new Uint8Array(e.data));
        return;
      }
      try {
        const m = JSON.parse(e.data);
        if (m.type === "error") this.term.write(`\r\n[muxdeck] ${m.data}\r\n`);
      } catch {}
    };
    ws.onclose = () => {
      if (!this.deliberate && this.session === name) this.scheduleReconnect();
    };
  }

  alive() {
    return this.ws && (this.ws.readyState === WebSocket.OPEN || this.ws.readyState === WebSocket.CONNECTING);
  }

  async scheduleReconnect() {
    const name = this.session;
    try {
      const sessions = await api("/api/sessions");
      if (!sessions.some((s) => s.name === name)) {
        this.detach();
        this.msg("session ended");
        refreshSessions();
        return;
      }
    } catch {} // server unreachable: keep retrying
    if (this.session !== name) return;
    const delay = Math.min(15000, 1000 * 2 ** this.retries++);
    this.msg(`disconnected — retrying in ${Math.round(delay / 1000)}s (tap to retry now)`);
    clearTimeout(this.retryTimer);
    this.retryTimer = setTimeout(() => {
      if (this.session === name && !this.alive()) this.connect();
    }, delay);
  }

  reconnectNow() {
    if (this.session && !this.alive()) {
      clearTimeout(this.retryTimer);
      this.msg("");
      this.connect();
    }
  }

  detach(reattaching = false) {
    this.deliberate = true;
    clearTimeout(this.retryTimer);
    if (this.ws) {
      this.ws.onclose = null;
      this.ws.close();
      this.ws = null;
    }
    if (this.term) {
      this.term.dispose();
      this.term = null;
      this.fit = null;
      this.search = null;
    }
    this.session = null;
    this.retries = 0;
    this.deliberate = false;
    this.toggleFind(false);
    this.el.querySelector(".p-mouse").hidden = true;
    if (!reattaching) {
      this.el.querySelector(".pane-title").textContent = "no session";
      this.msg("select or create a session");
      updateHash();
    }
  }

  send(data) {
    if (this.ws && this.ws.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify({ type: "input", data }));
    }
  }

  sendResize() {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN || !this.fit) return;
    this.fit.fit();
    this.ws.send(JSON.stringify({ type: "resize", cols: this.term.cols, rows: this.term.rows }));
  }

  async refreshMouse() {
    const btn = this.el.querySelector(".p-mouse");
    try {
      const { enabled } = await api(keyApi(this.session, "/mouse"));
      btn.hidden = false;
      btn.classList.toggle("armed", enabled);
    } catch { btn.hidden = true; }
  }

  async toggleMouse() {
    if (!this.session) return;
    const btn = this.el.querySelector(".p-mouse");
    const want = !btn.classList.contains("armed");
    try {
      await api(keyApi(this.session, "/mouse"), {
        method: "POST",
        body: JSON.stringify({ enabled: want }),
      });
      btn.classList.toggle("armed", want);
      this.term?.focus();
    } catch (err) { alert(err.message); }
  }

  toggleFind(show = this.el.querySelector(".findbar").hidden) {
    const fb = this.el.querySelector(".findbar");
    if (fb.hidden === !show) return;
    fb.hidden = !show;
    if (show) {
      this.findInput.select();
      this.findInput.focus();
    } else {
      this.search?.clearDecorations?.();
      this.term?.focus();
    }
  }

  findNext(incremental) {
    if (this.search && this.findInput.value) this.search.findNext(this.findInput.value, { incremental });
  }

  findPrev() {
    if (this.search && this.findInput.value) this.search.findPrevious(this.findInput.value);
  }
}

function addPane() {
  const p = new Pane();
  panes.push(p);
  $("#panes").appendChild(p.el);
  $("#panes").classList.toggle("split", panes.length > 1);
  $("#split").classList.toggle("armed", panes.length > 1);
  updateDivider();
  setFocus(p);
  return p;
}

function closePane(p) {
  if (panes.length === 1) {
    p.detach();
    return;
  }
  p.detach();
  p.el.remove();
  panes = panes.filter((q) => q !== p);
  $("#panes").classList.toggle("split", panes.length > 1);
  $("#split").classList.toggle("armed", panes.length > 1);
  updateDivider();
  if (focusedPane === p) setFocus(panes[0]);
  updateHash();
  refreshSessions();
}

$("#split").addEventListener("click", () => {
  if (panes.length === 1) addPane();
  else closePane(panes[panes.length - 1]);
});

// --- layout: sidebar width/collapse + split ratio, persisted ---

function sidebarW() {
  return Math.max(230, Math.min(480, +localStorage.getItem("muxdeck-sidebar-w") || 230));
}

function applySidebar() {
  document.documentElement.style.setProperty("--sidebar-w", `${sidebarW()}px`);
  $("#sidebar").classList.toggle("collapsed", localStorage.getItem("muxdeck-sidebar-collapsed") === "1");
}

function toggleSidebar() {
  const collapsed = $("#sidebar").classList.toggle("collapsed");
  localStorage.setItem("muxdeck-sidebar-collapsed", collapsed ? "1" : "");
}

// Horizontal drag: pointer capture keeps moves on the handle even when the
// cursor crosses the terminal canvas.
function hDrag(el, move) {
  el.addEventListener("pointerdown", (e) => {
    e.preventDefault();
    el.setPointerCapture(e.pointerId);
    el.classList.add("dragging");
    const onMove = (ev) => move(ev.clientX);
    const onUp = () => {
      el.classList.remove("dragging");
      el.removeEventListener("pointermove", onMove);
      el.removeEventListener("pointerup", onUp);
    };
    el.addEventListener("pointermove", onMove);
    el.addEventListener("pointerup", onUp);
  });
}

hDrag($("#sidebar-resizer"), (x) => {
  localStorage.setItem("muxdeck-sidebar-w", Math.round(Math.max(230, Math.min(480, x))));
  document.documentElement.style.setProperty("--sidebar-w", `${sidebarW()}px`);
});
$("#sidebar-resizer").addEventListener("dblclick", toggleSidebar);
$("#sidebar-expand").innerHTML = icon("chevronRight");
$("#sidebar").addEventListener("click", () => {
  if ($("#sidebar").classList.contains("collapsed")) toggleSidebar();
});

let divider = null;

function applySplit() {
  if (panes.length < 2) return;
  const ratio = +localStorage.getItem("muxdeck-split") || 0;
  panes[0].el.style.flex = ratio ? `0 0 ${(ratio * 100).toFixed(1)}%` : "";
}

function updateDivider() {
  if (panes.length === 2 && !divider) {
    divider = document.createElement("div");
    divider.id = "pane-divider";
    divider.title = "drag to resize · double-click for 50/50";
    $("#panes").insertBefore(divider, panes[1].el);
    hDrag(divider, (x) => {
      const r = $("#panes").getBoundingClientRect();
      const ratio = Math.max(0.15, Math.min(0.85, (x - r.left) / r.width));
      localStorage.setItem("muxdeck-split", ratio.toFixed(3));
      applySplit();
    });
    divider.addEventListener("dblclick", () => {
      localStorage.removeItem("muxdeck-split");
      applySplit();
    });
    applySplit();
  } else if (panes.length < 2 && divider) {
    divider.remove();
    divider = null;
    panes[0]?.el.style.removeProperty("flex");
  }
}

function updateHash() {
  const names = panes.map((p) => p.session).filter(Boolean);
  history.replaceState(null, "", names.length ? `#${names.map(encodeURIComponent).join(",")}` : location.pathname);
}

function attachFromHash() {
  const names = location.hash.slice(1).split(",").filter(Boolean).map(decodeURIComponent);
  if (names[0]) panes[0].attach(names[0]);
  if (names[1] && matchMedia("(min-width: 700px)").matches) addPane().attach(names[1]);
}

// --- session list ---

let lastSessions = [];
let remotes = []; // /api/remotes statuses
const remoteSessions = new Map(); // remote name -> its session list
const seenActivity = new Map(); // key -> last activity we consider seen
const agentStates = new Map(); // key -> last agent state, for waiting-transition alerts

// Sessions are identified by key: "name" locally, "remote:name" on a remote
// (session names can't contain ":", so the split is unambiguous). Keys flow
// through panes, the palette, the hash, and the activity/agent maps.

function keyApi(key, rest = "") {
  const i = key.indexOf(":");
  if (i < 0) return `/api/sessions/${encodeURIComponent(key)}${rest}`;
  return `/api/remotes/${encodeURIComponent(key.slice(0, i))}/proxy/sessions/${encodeURIComponent(key.slice(i + 1))}${rest}`;
}

function allSessions() {
  const out = orderedSessions().map((s) => ({ ...s, key: s.name }));
  for (const r of remotes) {
    for (const s of remoteSessions.get(r.name) || []) {
      out.push({ ...s, key: `${r.name}:${s.name}`, remote: r.name });
    }
  }
  return out;
}

let collapsedRemotes = {};
try { collapsedRemotes = JSON.parse(localStorage.getItem("muxdeck-remotes-collapsed")) || {}; } catch {}

// Two notification backends: the web Notification API in browsers, and the
// Tauri notification plugin in the desktop app (WKWebView has no web API;
// the plugin posts to macOS Notification Center instead — works even with
// the window closed).
const tauriNotif = window.__TAURI__?.notification;

async function notifGranted() {
  if (tauriNotif) return tauriNotif.isPermissionGranted();
  return typeof Notification !== "undefined" && Notification.permission === "granted";
}

async function notifyAgentWaiting(s) {
  if (!(await notifGranted())) return;
  const title = `${s.key}: agent needs input`;
  const body = s.agent.note || `${s.agent.agent} is waiting for you`;
  if (tauriNotif) {
    // Clicking a Notification Center alert focuses the app (macOS default);
    // per-notification click handlers aren't exposed by the plugin.
    tauriNotif.sendNotification({ title, body });
    return;
  }
  const n = new Notification(title, {
    body,
    tag: `muxdeck-agent-${s.key}`, // collapse repeats for the same session
  });
  n.onclick = () => {
    window.focus();
    paneFor().attach(s.key);
    n.close();
  };
}

function orderedSessions() {
  const idx = new Map(order.map((n, i) => [n, i]));
  return [...lastSessions].sort(
    (a, b) => (idx.has(a.name) ? idx.get(a.name) : 1e9) - (idx.has(b.name) ? idx.get(b.name) : 1e9)
  );
}

function renderTmuxMissing() {
  const ul = $("#sessions");
  ul.innerHTML = "";
  const li = document.createElement("li");
  li.className = "tmux-missing";
  const cmd = /Mac/.test(navigator.platform) ? "brew install tmux" : "sudo apt install tmux";
  const b = document.createElement("b");
  b.textContent = "muxdeck needs tmux";
  const p = document.createElement("p");
  p.textContent = "Install it, then hit refresh up top:";
  const code = document.createElement("code");
  code.textContent = cmd;
  code.title = "click to copy";
  code.addEventListener("click", () => {
    navigator.clipboard?.writeText(cmd);
    p.textContent = "Copied. Install it, then hit refresh up top:";
  });
  li.append(b, p, code);
  ul.appendChild(li);
}

async function refreshSessions() {
  let sessions;
  try {
    sessions = await api("/api/sessions");
  } catch (err) {
    if (err.message === "tmux-missing") renderTmuxMissing();
    return;
  }
  lastSessions = sessions;

  // Remote lists ride the same refresh tick; a dead remote must not stall
  // or break the local render, so failures just flip its state to down.
  try { remotes = await api("/api/remotes"); } catch { remotes = []; }
  await Promise.all(remotes.map(async (r) => {
    if (r.state !== "ok") { remoteSessions.delete(r.name); return; }
    try {
      remoteSessions.set(r.name, await api(`/api/remotes/${encodeURIComponent(r.name)}/proxy/sessions`));
    } catch (err) {
      remoteSessions.delete(r.name);
      r.state = "down";
      r.error = err.message;
    }
  }));

  const attached = new Set(panes.map((p) => p.session).filter(Boolean));
  for (const s of allSessions()) {
    // Viewing a session counts as seeing its output; new sessions start seen.
    if (attached.has(s.key) || !seenActivity.has(s.key)) seenActivity.set(s.key, s.activity);

    // Alert on the transition into waiting, not while it persists — and not
    // when the session is already on screen in a visible tab.
    const state = s.agent?.state;
    if (
      state === "waiting" &&
      agentStates.get(s.key) !== "waiting" &&
      agentStates.has(s.key) &&
      (document.hidden || !attached.has(s.key))
    ) {
      notifyAgentWaiting(s);
    }
    if (state) agentStates.set(s.key, state);
    else agentStates.delete(s.key);
  }

  const ul = $("#sessions");
  ul.innerHTML = "";
  for (const s of orderedSessions()) ul.appendChild(sessionRow({ ...s, key: s.name }, attached));
  for (const r of remotes) {
    ul.appendChild(remoteHeader(r));
    if (collapsedRemotes[r.name] || r.state !== "ok") continue;
    for (const s of remoteSessions.get(r.name) || []) {
      ul.appendChild(sessionRow({ ...s, key: `${r.name}:${s.name}`, remote: r.name }, attached));
    }
  }
  if (!$("#palette").hidden) renderPalette();
}

function sessionRow(s, attached) {
  const li = document.createElement("li");
  li.dataset.name = s.key;
  li.classList.toggle("active", attached.has(s.key));
  li.classList.toggle("remote-session", !!s.remote);

  // Drag-reorder is a local-list affordance; remote rows keep group order.
  const grip = document.createElement("span");
  grip.className = "grip";
  if (!s.remote) {
    grip.innerHTML = icon("grip");
    grip.title = "drag to reorder";
    initDrag(li, grip);
  }

  const name = document.createElement("span");
  name.className = "name";
  name.textContent = s.name;
  if (s.activity > seenActivity.get(s.key)) {
    const dot = document.createElement("span");
    dot.className = "dot";
    dot.title = "new output";
    name.append(" ", dot);
  }

  if (s.agent) {
    const badge = document.createElement("span");
    badge.className = `agent agent-${s.agent.state}`;
    const glyph = { working: "◐", waiting: "✋", idle: "○" }[s.agent.state] || "?";
    const cost = s.agent.cost_usd ? ` · $${s.agent.cost_usd.toFixed(2)}` : "";
    badge.textContent = `${glyph} ${s.agent.model || s.agent.agent}${cost}`;
    badge.title = `${s.agent.agent} is ${s.agent.state}` +
      (s.agent.note ? ` — ${s.agent.note}` : "") +
      ` · updated ${new Date(s.agent.updated_at).toLocaleTimeString()}`;
    name.append(" ", badge);
  }

  const meta = document.createElement("span");
  meta.className = "meta";
  meta.textContent = `${s.windows} win${s.windows === 1 ? "" : "s"}${s.attached > 0 ? " ●" : ""}`;
  li.title = `${s.key} · ${s.windows} window${s.windows === 1 ? "" : "s"} · created ${new Date(s.created).toLocaleString()}${s.attached > 0 ? " · attached" : ""}`;

  const rename = document.createElement("button");
  rename.className = "rename";
  rename.innerHTML = icon("pencil");
  rename.title = `rename ${s.key}`;
  rename.addEventListener("click", (e) => {
    e.stopPropagation();
    renameSession(s.key);
  });

  const kill = document.createElement("button");
  kill.className = "kill";
  kill.innerHTML = icon("x");
  kill.title = `kill ${s.key}`;
  kill.addEventListener("click", async (e) => {
    e.stopPropagation();
    if (!confirm(`Kill session "${s.key}"?`)) return;
    await killSession(s.key);
  });

  li.append(grip, name, meta, rename, kill);
  li.addEventListener("click", () => paneFor().attach(s.key));
  return li;
}

function remoteHeader(r) {
  const li = document.createElement("li");
  li.className = "remote-head";
  const chev = document.createElement("span");
  chev.className = "chev";
  chev.innerHTML = icon(collapsedRemotes[r.name] ? "chevronRight" : "chevronDown");
  const name = document.createElement("span");
  name.className = "name";
  name.textContent = r.name;
  const state = document.createElement("span");
  state.className = `rstate rstate-${r.state}`;
  if (r.state === "ok") {
    state.textContent = `${(remoteSessions.get(r.name) || []).length}`;
    li.title = `${r.name} · ${r.mode === "ssh" ? `ssh ${r.host}` : r.url}`;
  } else {
    state.innerHTML = icon("linkOff");
    li.title = `${r.name} · ${r.error || "unreachable"}`;
  }
  li.append(chev, name, state);
  li.addEventListener("click", () => {
    collapsedRemotes[r.name] = !collapsedRemotes[r.name];
    localStorage.setItem("muxdeck-remotes-collapsed", JSON.stringify(collapsedRemotes));
    refreshSessions();
  });
  return li;
}

function initDrag(li, grip) {
  grip.addEventListener("pointerdown", (e) => {
    e.preventDefault();
    e.stopPropagation();
    grip.setPointerCapture(e.pointerId);
    li.classList.add("dragging");
    const ul = $("#sessions");
    const move = (ev) => {
      const over = document.elementFromPoint(ev.clientX, ev.clientY)
        ?.closest("#sessions li:not(.remote-head):not(.remote-session)");
      if (over && over !== li) {
        const r = over.getBoundingClientRect();
        ul.insertBefore(li, ev.clientY < r.top + r.height / 2 ? over : over.nextSibling);
      }
    };
    grip.addEventListener("pointermove", move);
    grip.addEventListener("pointerup", () => {
      grip.removeEventListener("pointermove", move);
      li.classList.remove("dragging");
      order = [...ul.querySelectorAll("li:not(.remote-head):not(.remote-session)")].map((n) => n.dataset.name);
      saveOrder();
    }, { once: true });
  });
}

async function renameSession(oldKey, name = null) {
  const prefix = oldKey.includes(":") ? oldKey.slice(0, oldKey.indexOf(":") + 1) : "";
  name = name ?? prompt("rename session", oldKey.slice(prefix.length));
  if (!name || prefix + name === oldKey) return;
  try {
    await api(keyApi(oldKey, "/rename"), {
      method: "POST",
      body: JSON.stringify({ name }),
    });
  } catch (err) {
    alert(err.message);
    return;
  }
  const newKey = prefix + name;
  for (const p of panes) {
    if (p.session === oldKey) {
      p.session = newKey;
      p.el.querySelector(".pane-title").textContent = newKey;
    }
  }
  const i = order.indexOf(oldKey);
  if (i >= 0) { order[i] = newKey; saveOrder(); }
  if (seenActivity.has(oldKey)) {
    seenActivity.set(newKey, seenActivity.get(oldKey));
    seenActivity.delete(oldKey);
  }
  updateHash();
  refreshSessions();
}

$("#refresh").addEventListener("click", refreshSessions);

// Notification opt-in. Shown where a backend exists: the page Notification
// API in browsers, the Tauri plugin in the desktop app. Neither exists on
// iOS Safari — that needs Web Push, which muxdeck doesn't do yet.
if (tauriNotif || typeof Notification !== "undefined") {
  const btn = $("#notify");
  btn.hidden = false;
  const paint = async () => btn.classList.toggle("armed", await notifGranted());
  paint();
  btn.addEventListener("click", async () => {
    if (tauriNotif) {
      if (!(await tauriNotif.isPermissionGranted())) await tauriNotif.requestPermission();
    } else if (Notification.permission === "default") {
      await Notification.requestPermission();
    } else if (Notification.permission === "denied") {
      alert("Notifications are blocked for this site — enable them in browser settings.");
    }
    paint();
  });
}

$("#new-session").addEventListener("submit", async (e) => {
  e.preventDefault();
  const name = $("#new-name").value.trim();
  if (!name) return;
  await createSession(name);
  $("#new-name").value = "";
});

// --- command palette ---
// One overlay, several modes. "switch" is the spotlight default: fuzzy
// session matching, ":" for commands, and a create fallback when nothing
// matches. Sub-modes chain like shell prompts: :rename → pick → type new
// name, :kill → pick → y/N.

let pal = { mode: "switch", verb: null, arg: null, index: 0 };

const VALID_NAME = /^[A-Za-z0-9_-]+$/;

const PAL_COMMANDS = [
  { name: ":new",     hint: "create session",         run: () => setPalMode("new") },
  { name: ":rename",  hint: "rename session",         run: () => setPalMode("pick", "rename") },
  { name: ":kill",    hint: "kill session",           run: () => setPalMode("pick", "kill") },
  { name: ":split",   hint: "toggle split view",      run: () => { closePalette(); $("#split").click(); } },
  { name: ":sidebar", hint: "collapse/expand sidebar", run: () => { closePalette(); toggleSidebar(); } },
  { name: ":remote",  hint: "add or remove remotes",   run: () => setPalMode("remote") },
  { name: ":mouse",   hint: "toggle tmux mouse mode", run: () => { closePalette(); paneFor()?.toggleMouse(); } },
  { name: ":font +",  hint: "bigger text",  keep: true, run: () => setFontSize(fontSize + 1) },
  { name: ":font -",  hint: "smaller text", keep: true, run: () => setFontSize(fontSize - 1) },
  { name: ":refresh", hint: "refresh sessions",       run: () => { closePalette(); refreshSessions(); } },
];

function fuzzy(q) {
  q = q.toLowerCase();
  return (name) => {
    let i = 0;
    for (const c of name.toLowerCase()) if (c === q[i]) i++;
    return i >= q.length;
  };
}

function creatableKey(q) {
  const i = q.indexOf(":");
  if (i < 0) return VALID_NAME.test(q) && !lastSessions.some((s) => s.name === q);
  const host = q.slice(0, i), name = q.slice(i + 1);
  return VALID_NAME.test(name) &&
    remotes.some((r) => r.name === host && r.state === "ok") &&
    !(remoteSessions.get(host) || []).some((s) => s.name === name);
}

function palItems() {
  const q = $("#pal-input").value.trim();
  if (pal.mode === "pick")
    return allSessions().filter((s) => fuzzy(q)(s.key))
      .map((s) => ({ kind: "session", name: s.key, s }));
  if (pal.mode !== "switch") return [];
  if (q.startsWith(":"))
    return PAL_COMMANDS.filter((c) => c.name.startsWith(q)).map((c) => ({ kind: "command", ...c }));
  const items = allSessions().filter((s) => fuzzy(q)(s.key))
    .map((s) => ({ kind: "session", name: s.key, s }));
  if (q && creatableKey(q))
    items.push({ kind: "create", name: q });
  return items;
}

function renderPalette() {
  const ul = $(".pal-list");
  const items = palItems();
  pal.index = Math.max(0, Math.min(pal.index, items.length - 1));
  ul.innerHTML = "";
  items.forEach((it, i) => {
    const li = document.createElement("li");
    li.classList.toggle("selected", i === pal.index);
    const label = document.createElement("span");
    const right = document.createElement("span");
    right.className = "dim";
    if (it.kind === "session") {
      label.textContent = it.name;
      right.textContent = `${it.s.windows}w${it.s.attached > 0 ? " ●" : ""}`;
      if (it.s.agent) right.textContent += ` · ${{ working: "◐", waiting: "✋", idle: "○" }[it.s.agent.state] || ""}`;
    } else if (it.kind === "command") {
      label.textContent = it.name;
      label.className = "cmd";
      right.textContent = it.hint;
    } else {
      label.textContent = `+ create "${it.name}"`;
      label.className = "cmd";
      right.textContent = "new session";
    }
    li.append(label, right);
    li.addEventListener("click", () => palActivate(it));
    ul.appendChild(li);
  });
  updateCaret();
}

async function palActivate(it) {
  if (it.kind === "session") {
    if (pal.mode === "pick" && pal.verb === "rename") return setPalMode("input", "rename", it.name);
    if (pal.mode === "pick" && pal.verb === "kill") return setPalMode("confirm", "kill", it.name);
    closePalette();
    paneFor().attach(it.name);
  } else if (it.kind === "command") {
    it.run();
    if (it.keep) renderPalette();
  } else {
    await createSession(it.name);
  }
}

async function createSession(key) {
  const i = key.indexOf(":");
  const path = i < 0 ? "/api/sessions" : `/api/remotes/${encodeURIComponent(key.slice(0, i))}/proxy/sessions`;
  try {
    await api(path, { method: "POST", body: JSON.stringify({ name: key.slice(i + 1) }) });
  } catch (err) { alert(err.message); return; }
  closePalette();
  await refreshSessions();
  paneFor().attach(key);
}

async function killSession(key) {
  try { await api(keyApi(key), { method: "DELETE" }); }
  catch (err) { alert(err.message); }
  for (const p of panes) if (p.session === key) p.detach();
  refreshSessions();
}

// ":remote ❯" one-liners: "add jack ssh jack", "add lab ssh lab.example:9000 TOKEN",
// "add juneau url http://juneau:8300 TOKEN", "rm jack".
async function runRemoteCommand(line) {
  const words = line.split(/\s+/).filter(Boolean);
  const [verb, name] = words;
  try {
    if (verb === "rm" && name && words.length === 2) {
      await api(`/api/remotes/${encodeURIComponent(name)}`, { method: "DELETE" });
    } else if (verb === "add" && words.length >= 4 && words.length <= 5) {
      const [, , mode, target, token] = words;
      const body = { name, mode, token: token || "" };
      if (mode === "ssh") {
        const m = target.match(/^(.+?)(?::(\d+))?$/);
        body.host = m[1];
        if (m[2]) body.remote_port = +m[2];
      } else if (mode === "url") {
        body.url = target;
      } else {
        throw new Error("mode must be ssh or url");
      }
      await api("/api/remotes", { method: "POST", body: JSON.stringify(body) });
    } else {
      throw new Error("usage: add <name> ssh <host[:port]> [token] · add <name> url <url> [token] · rm <name>");
    }
  } catch (err) { alert(err.message); return; }
  closePalette();
  refreshSessions();
}

const PAL_MODES = {
  switch:  { prompt: "❯", ph: "session or :command", hint: "↑↓ move · ⏎ open · : commands · esc close" },
  new:     { prompt: "new ❯", ph: "session name (host:name for a remote)", hint: "⏎ create · esc back" },
  pick:    { prompt: null, ph: "which session?", hint: "↑↓ move · ⏎ select · esc back" },
  input:   { prompt: null, ph: "new name", hint: "⏎ rename · esc back" },
  confirm: { prompt: null, ph: "", hint: "y kill · n / esc back" },
  remote:  { prompt: ":remote ❯", ph: "add <name> ssh <host[:port]> [token] · add <name> url <url> [token] · rm <name>", hint: "⏎ run · esc back" },
};

function setPalMode(mode, verb = null, arg = null) {
  pal = { mode, verb, arg, index: 0 };
  const m = PAL_MODES[mode];
  const inp = $("#pal-input");
  inp.value = mode === "input" ? arg : "";
  $(".pal-mode").textContent =
    mode === "pick" ? `:${verb} ❯` :
    mode === "input" ? `:${verb} ${arg} ❯` :
    mode === "confirm" ? `:${verb} ${arg}? [y/N]` : m.prompt;
  inp.placeholder = m.ph && " " + m.ph; // leading space: the block caret sits on col 0
  inp.readOnly = mode === "confirm";
  $(".pal-hint").textContent = m.hint;
  renderPalette();
  inp.focus();
  if (mode === "input") inp.setSelectionRange(0, inp.value.length);
}

function openPalette(mode) {
  $("#palette").hidden = false;
  setPalMode(mode);
}

function closePalette() {
  $("#palette").hidden = true;
  paneFor()?.term?.focus();
}

function palBack() {
  if (pal.mode === "switch") closePalette();
  else setPalMode("switch");
}

function updateCaret() {
  const inp = $("#pal-input");
  const caret = $(".pal-caret");
  caret.hidden = pal.mode === "confirm" || document.activeElement !== inp;
  const pos = inp.selectionStart ?? inp.value.length;
  caret.style.left = `${pos}ch`;
  caret.textContent = inp.value[pos] || " ";
}

$("#palette").addEventListener("click", (e) => {
  if (e.target === $("#palette")) closePalette();
});

$("#pal-input").addEventListener("input", () => {
  pal.index = 0;
  renderPalette();
});
$("#pal-input").addEventListener("focus", updateCaret);
$("#pal-input").addEventListener("blur", updateCaret);
document.addEventListener("selectionchange", () => {
  if (!$("#palette").hidden) updateCaret();
});

$("#pal-input").addEventListener("keydown", async (e) => {
  if (e.key === "Escape") { e.preventDefault(); palBack(); return; }
  if (pal.mode === "confirm") {
    e.preventDefault();
    if (e.key.toLowerCase() === "y") { const name = pal.arg; closePalette(); await killSession(name); }
    else if (e.key === "Enter" || e.key.toLowerCase() === "n") palBack();
    return;
  }
  const items = palItems();
  if (e.key === "ArrowDown" || e.key === "Tab" && !e.shiftKey || e.ctrlKey && e.key === "n") {
    e.preventDefault(); pal.index++; renderPalette();
  } else if (e.key === "ArrowUp" || e.key === "Tab" && e.shiftKey || e.ctrlKey && e.key === "p") {
    e.preventDefault(); pal.index--; renderPalette();
  } else if (e.key === "Enter") {
    e.preventDefault();
    const name = $("#pal-input").value.trim();
    if (pal.mode === "new") { if (creatableKey(name)) await createSession(name); }
    else if (pal.mode === "input") {
      if (VALID_NAME.test(name)) { closePalette(); await renameSession(pal.arg, name); }
    } else if (pal.mode === "remote") { await runRemoteCommand(name); }
    else if (items[pal.index]) await palActivate(items[pal.index]);
  } else if (e.key === "Backspace" && !$("#pal-input").value && pal.mode !== "switch") {
    e.preventDefault(); palBack();
  }
});

// --- font size ---

function setFontSize(v) {
  fontSize = Math.max(9, Math.min(24, v));
  localStorage.setItem("muxdeck-font", fontSize);
  for (const p of panes) {
    if (p.term) {
      p.term.options.fontSize = fontSize;
      p.sendResize();
    }
  }
}

// --- touch keyboard bar ---
// The iOS/iPadOS software keyboard has no Esc/Tab/Ctrl/arrows; the bar fills
// the gap. Ctrl is a sticky modifier applied to the next typed character.

let ctrlArmed = false;

function applyCtrl(data) {
  if (!ctrlArmed || data.length !== 1) return data;
  const code = data.toUpperCase().charCodeAt(0);
  if (code >= 64 && code < 96) {
    setCtrl(false);
    return String.fromCharCode(code & 0x1f);
  }
  if (data === " ") {
    setCtrl(false);
    return "\x00";
  }
  return data;
}

function setCtrl(on) {
  ctrlArmed = on;
  $("#key-ctrl").classList.toggle("armed", on);
}

function initKeybar() {
  if (!matchMedia("(pointer: coarse)").matches) return;
  const bar = $("#keybar");
  bar.hidden = false;
  // pointerdown + preventDefault keeps focus (and the software keyboard) on
  // the terminal while tapping bar keys. The paste button is the exception:
  // clipboard reads need a full click activation (and iOS paste UI).
  bar.addEventListener("pointerdown", (e) => {
    const btn = e.target.closest("button");
    if (!btn || btn.id === "key-paste") return;
    e.preventDefault();
    if (btn.id === "key-ctrl") {
      setCtrl(!ctrlArmed);
    } else {
      paneFor()?.send(applyCtrl(btn.dataset.key));
    }
  });
  $("#key-paste").addEventListener("click", pasteToFocused);

  // Shrink the layout to the visual viewport so the terminal is not hidden
  // behind the software keyboard.
  if (window.visualViewport) {
    const vv = window.visualViewport;
    const adjust = () => {
      $("#app").style.height = `${vv.height}px`;
      panes.forEach((p) => p.sendResize());
    };
    vv.addEventListener("resize", adjust);
  }
}

// --- boot ---

if ("serviceWorker" in navigator) {
  navigator.serviceWorker.register("sw.js").catch(() => {});
}
applySidebar();
addPane();
initKeybar();
window.addEventListener("resize", () => panes.forEach((p) => p.sendResize()));
document.addEventListener("visibilitychange", () => {
  if (!document.hidden) panes.forEach((p) => p.reconnectNow());
});
refreshSessions().then(attachFromHash);
setInterval(() => { if (!document.hidden) refreshSessions(); }, 10000);
