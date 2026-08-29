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
  branch: '<path d="M6 3v12"/><circle cx="18" cy="6" r="3"/><circle cx="6" cy="18" r="3"/><path d="M18 9a9 9 0 0 1-9 9"/>',
  plug: '<path d="M9 2v6"/><path d="M15 2v6"/><path d="M6 8h12v4a6 6 0 0 1-6 6 6 6 0 0 1-6-6Z"/><path d="M12 18v4"/>',
};

function icon(name) {
  return `<svg class="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${ICONS[name]}</svg>`;
}

$("#notify").innerHTML = icon("bell");
$("#split").innerHTML = icon("columns");
$("#refresh").innerHTML = icon("refresh");
$("#new-session").innerHTML = icon("plus");

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
          <button class="p-refresh" hidden title="refresh view">${icon("refresh")}</button>
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
        <div class="pane-view" hidden></div>
        <div class="pane-msg">select or create a session</div>
      </div>`;

    this.el.addEventListener("pointerdown", () => setFocus(this));
    this.el.querySelector(".p-close").addEventListener("click", () => closePane(this));
    this.el.querySelector(".p-mouse").addEventListener("click", () => this.toggleMouse());
    this.el.querySelector(".p-find").addEventListener("click", () => this.toggleFind());
    this.el.querySelector(".p-refresh").addEventListener("click", () => this.refreshView());
    this.view = null;
    this.viewTimer = null;
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
    if (this.view) this.closeView();
    if (this.session) this.detach(true);
    this.session = name;
    this.el.querySelector(".pane-title").textContent = name;
    this.msg("");
    this.makeTerm();
    this.connect();
    this.refreshMouse();
    updateHash();
  }

  // --- viewer mode: the pane shows a rendered document instead of a
  // terminal. view = {type: "diff"} or {type: "md", path}, plus key: the
  // session whose cwd anchors the content. Polls for changes while visible.

  openView(view) {
    if (this.session) this.detach(true);
    this.view = view;
    this.viewStamp = null;
    this.msg("");
    this.el.querySelector(".pane-term").hidden = true;
    this.el.querySelector(".pane-view").hidden = false;
    this.el.querySelector(".p-refresh").hidden = false;
    this.el.querySelector(".pane-title").textContent =
      view.type === "diff" ? `diff · ${view.key}` : `${view.path} · ${view.key}`;
    this.refreshView();
    clearInterval(this.viewTimer);
    this.viewTimer = setInterval(() => {
      if (!document.hidden) this.refreshView();
    }, 3000);
    updateHash();
  }

  closeView() {
    clearInterval(this.viewTimer);
    this.viewTimer = null;
    if (this.mushWS) { this.mushWS.onclose = null; this.mushWS.close(); this.mushWS = null; }
    this.view = null;
    this.viewStamp = null;
    const v = this.el.querySelector(".pane-view");
    v.hidden = true;
    v.innerHTML = "";
    this.el.querySelector(".pane-term").hidden = false;
    this.el.querySelector(".p-refresh").hidden = true;
    this.el.querySelector(".pane-title").textContent = "no session";
    this.msg("select or create a session");
  }

  // --- mush run pane: a viewer over a run's journal. The daemon replays the
  // journal and tails it over a websocket; the ledger row rides along as
  // `_run` / `_state` frames. Approval cards answer over the same socket —
  // the daemon routes the decision to whoever owns the run (its own engine,
  // or the mush serve behind serve.sock). view = {type: "mush", host, runId}.

  openMush(host, run) {
    if (this.session) this.detach(true);
    if (this.view) this.closeView();
    this.view = { type: "mush", host: host || "", runId: run.id, run };
    this.el.querySelector(".pane-term").hidden = true;
    const v = this.el.querySelector(".pane-view");
    v.hidden = false;
    v.className = "pane-view mush-view";
    this.el.querySelector(".p-refresh").hidden = false;
    this.setMushTitle(run);
    this.msg("");
    this.connectMush();
    refreshSessions();
  }

  setMushTitle(run) {
    this.el.querySelector(".pane-title").textContent =
      `mush · ${run.state ? run.state.replace("_", " ") + " · " : ""}${runLabel(run).slice(0, 60)}`;
  }

  connectMush() {
    if (this.mushWS) { this.mushWS.onclose = null; this.mushWS.close(); }
    const { host, runId } = this.view;
    const box = this.el.querySelector(".pane-view");
    box.innerHTML = "";
    const head = mushRunHeader(host, this.view.run, this);
    box.appendChild(head.el);
    const r = mushRenderer(box, (frame) => this.mushWS?.send(JSON.stringify(frame)));
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    const ws = new WebSocket(`${proto}//${location.host}${mushApi(host, `/${encodeURIComponent(runId)}/stream`)}`);
    this.mushWS = ws;
    ws.onmessage = (e) => {
      let env;
      try { env = JSON.parse(e.data); } catch { return; }
      if (env.type === "_run" || env.type === "_state") {
        this.view.run = env.data;
        head.update(env.data);
        this.setMushTitle(env.data);
        if (env.type === "_state") refreshSessions();
        return;
      }
      r.render(env);
    };
    ws.onclose = () => r.note("stream closed — ↻ to reconnect");
  }

  async refreshView() {
    if (!this.view) return;
    if (this.view.type === "mush") { this.connectMush(); return; }
    const { type, key, path } = this.view;
    const box = this.el.querySelector(".pane-view");
    try {
      if (type === "diff") {
        const { root, text } = await api(keyApi(key, "/diff"));
        const stamp = `${root}\n${text}`;
        if (stamp === this.viewStamp) return;
        this.viewStamp = stamp;
        box.className = "pane-view diff-view";
        box.innerHTML = "";
        box.appendChild(renderDiff(root, text));
      } else {
        const { text } = await api(keyApi(key, `/file?path=${encodeURIComponent(path)}`));
        if (text === this.viewStamp) return;
        this.viewStamp = text;
        box.innerHTML = DOMPurify.sanitize(marked.parse(text));
        box.className = "pane-view md-view";
      }
    } catch (err) {
      box.className = "pane-view";
      box.textContent = err.message;
    }
  }

  makeTerm() {
    if (this.term) this.term.dispose();
    this.term = new Terminal({
      fontFamily: "SF Mono, Menlo, Monaco, monospace",
      fontSize,
      cursorBlink: true,
      // 3k rows ≈ a few MB per pane; 10k made attached panes the biggest
      // JS-heap line item once Claude Code TUIs filled them.
      scrollback: 3000,
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
    } catch (e) {
      console.warn("muxdeck: WebGL unavailable, falling back to DOM renderer", e);
    }
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
      // List the host the session actually lives on — checking a remote
      // key against the local list would declare it dead on any blip.
      const i = name.indexOf(":");
      const listPath = i < 0 ? "/api/sessions" : `/api/remotes/${encodeURIComponent(name.slice(0, i))}/proxy/sessions`;
      const sessions = await api(listPath);
      if (!sessions.some((s) => s.name === name.slice(i + 1))) {
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
  if (p.view) p.closeView();
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
  paintSidebarToggle();
}

function toggleSidebar() {
  const collapsed = $("#sidebar").classList.toggle("collapsed");
  localStorage.setItem("muxdeck-sidebar-collapsed", collapsed ? "1" : "");
  paintSidebarToggle();
}

function paintSidebarToggle() {
  const collapsed = $("#sidebar").classList.contains("collapsed");
  $("#sidebar-toggle").innerHTML = icon(collapsed ? "chevronRight" : "chevronLeft");
  $("#sidebar-toggle").title = collapsed ? "Expand sidebar" : "Collapse sidebar";
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
$("#sidebar-toggle").addEventListener("click", toggleSidebar);

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
let mushRuns = new Map(); // host ("" = local) → /api/mush/runs payload
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

  // mush runs ride the same tick: the local ledger plus each live remote's.
  // A daemon without mush answers available:false and an empty list.
  await refreshRuns();

  const ul = $("#sessions");
  ul.innerHTML = "";
  ul.appendChild(sectionHeader("Local", lastSessions.length));
  for (const s of orderedSessions()) ul.appendChild(sessionRow({ ...s, key: s.name }, attached));
  if (remotes.length) ul.appendChild(sectionHeader("Remotes", remotes.length));
  for (const r of remotes) {
    ul.appendChild(remoteHeader(r));
    if (collapsedRemotes[r.name] || r.state !== "ok") continue;
    for (const s of remoteSessions.get(r.name) || []) {
      ul.appendChild(sessionRow({ ...s, key: `${r.name}:${s.name}`, remote: r.name }, attached));
    }
  }
  const runs = allRuns();
  if (runs.length) {
    const head = sectionHeader("Runs", runs.length);
    head.title = "mush runs · click to collapse";
    head.style.cursor = "pointer";
    head.addEventListener("click", () => {
      runsCollapsed = !runsCollapsed;
      localStorage.setItem("muxdeck-runs-collapsed", runsCollapsed ? "1" : "");
      refreshSessions();
    });
    ul.appendChild(head);
    if (!runsCollapsed) for (const r of runs.slice(0, RUNS_SHOWN)) ul.appendChild(runRow(r));
  }
  if (!$("#palette").hidden) renderPalette();
}

const RUNS_SHOWN = 12;
let runsCollapsed = localStorage.getItem("muxdeck-runs-collapsed") === "1";

async function refreshRuns() {
  const hosts = ["", ...remotes.filter((r) => r.state === "ok").map((r) => r.name)];
  await Promise.all(hosts.map(async (host) => {
    try {
      const res = await api(mushApi(host, "?limit=30"));
      if (res.available) mushRuns.set(host, res); else mushRuns.delete(host);
    } catch { mushRuns.delete(host); }
  }));
  for (const host of [...mushRuns.keys()]) if (!hosts.includes(host)) mushRuns.delete(host);
}

// allRuns flattens every host's ledger rows, newest first (ids sort
// chronologically by construction).
function allRuns() {
  const out = [];
  for (const [host, res] of mushRuns) for (const r of res.runs || []) out.push({ ...r, host });
  return out.sort((a, b) => (a.id < b.id ? 1 : a.id > b.id ? -1 : 0));
}

const RUN_LIVE = new Set(["queued", "planning", "implementing", "awaiting_approval", "verifying"]);
const RUN_OK = new Set(["done", "pr_open", "merged"]);

function runState(r) {
  return r.state === "awaiting_approval" ? "wait" : RUN_LIVE.has(r.state) ? "live" : RUN_OK.has(r.state) ? "ok" : "bad";
}

function runLabel(r) {
  if (r.source === "issue" && r.source_ref) return `#${r.source_ref}`;
  if (r.source === "schedule" && r.source_ref) return `⏰ ${r.source_ref}`;
  return r.source_ref || r.id;
}

function runDuration(r) {
  const start = Date.parse(r.started_at || r.created_at);
  if (!start || start < 0) return "";
  const end = Date.parse(r.finished_at);
  const secs = Math.max(0, Math.round(((end > 0 ? end : Date.now()) - start) / 1000));
  if (secs < 60) return `${secs}s`;
  if (secs < 3600) return `${Math.floor(secs / 60)}m`;
  return `${Math.floor(secs / 3600)}h${Math.floor((secs % 3600) / 60)}m`;
}

function runRow(r) {
  const li = document.createElement("li");
  li.className = "run-row";
  li.dataset.run = `${r.host}:${r.id}`;
  li.classList.toggle("active", panes.some((p) => p.view?.type === "mush" && p.view.runId === r.id && (p.view.host || "") === r.host));
  const dot = document.createElement("span");
  dot.className = `run-dot ${runState(r)}`;
  dot.title = r.state;
  const copy = document.createElement("span");
  copy.className = "session-copy";
  const name = document.createElement("span");
  name.className = "name";
  name.textContent = runLabel(r);
  const meta = document.createElement("span");
  meta.className = "meta";
  const proj = (r.project || "").split("/").filter(Boolean).pop() || "";
  meta.textContent = [r.host, proj, r.state.replace("_", " "), r.model, runDuration(r), r.verdict && r.verdict !== "-" ? r.verdict : ""]
    .filter(Boolean).join(" · ");
  copy.append(name, meta);
  li.append(dot, copy);
  li.title = `${r.id}\n${r.project}`;
  li.addEventListener("click", () => viewerPane().openMush(r.host, r));
  return li;
}

function sectionHeader(label, count) {
  const li = document.createElement("li");
  li.className = "section-head";
  const name = document.createElement("span");
  name.textContent = label;
  const total = document.createElement("span");
  total.className = "section-count";
  total.textContent = count;
  li.append(name, total);
  return li;
}

// The leaf and its parent are what identify a working directory at sidebar
// width; the full path stays in the row tooltip.
function shortPath(p) {
  const parts = p.split("/").filter(Boolean);
  return parts.slice(-2).join("/") || p;
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

  const copy = document.createElement("span");
  copy.className = "session-copy";
  const name = document.createElement("span");
  name.className = "name";
  name.textContent = s.name;
  if (s.activity > seenActivity.get(s.key)) {
    const dot = document.createElement("span");
    dot.className = "dot";
    dot.title = "new output";
    name.append(" ", dot);
  }

  const detail = document.createElement("span");
  detail.className = "detail";
  const meta = document.createElement("span");
  meta.className = "meta";
  // Where a session is working says more at a glance than how its windows are
  // arranged, so the layout counts fall back to the row tooltip, which
  // already carries them.
  meta.textContent = s.path
    ? `${shortPath(s.path)}${s.command ? ` · ${s.command}` : ""}`
    : `${s.windows} window${s.windows === 1 ? "" : "s"}${s.attached > 0 ? " · attached" : ""}`;
  detail.appendChild(meta);

  if (s.branch) {
    const branch = document.createElement("span");
    branch.className = s.dirty ? "branch dirty" : "branch";
    branch.innerHTML = icon("branch");
    branch.append(s.dirty ? `${s.branch}*` : s.branch);
    branch.title = s.dirty ? `${s.branch} · uncommitted changes` : s.branch;
    detail.appendChild(branch);
  }

  // What a session is serving is the one thing about it you cannot read off
  // the terminal without going looking, so it earns a chip of its own.
  if (s.ports?.length) {
    const ports = document.createElement("span");
    ports.className = "ports";
    ports.innerHTML = icon("plug");
    const shown = s.ports.slice(0, 2).map((p) => `:${p}`).join(" ");
    ports.append(s.ports.length > 2 ? `${shown} +${s.ports.length - 2}` : shown);
    ports.title = `listening on ${s.ports.map((p) => `:${p}`).join(", ")}`;
    detail.appendChild(ports);
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
    detail.appendChild(badge);
  }
  copy.append(name, detail);
  li.title = `${s.key} · ${s.windows} window${s.windows === 1 ? "" : "s"} · created ${new Date(s.created).toLocaleString()}${s.attached > 0 ? " · attached" : ""}` +
    (s.path ? `\n${s.path}` : "");

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

  const actions = document.createElement("span");
  actions.className = "session-actions";
  actions.append(rename, kill);
  li.append(grip, copy, actions);
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
  } else if (r.state === "off") {
    li.classList.add("off");
    state.textContent = "off";
    li.title = `${r.name} · off — :remote on ${r.name} to reconnect`;
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
        ?.closest("#sessions li:not(.section-head):not(.remote-head):not(.remote-session)");
      if (over && over !== li) {
        const r = over.getBoundingClientRect();
        ul.insertBefore(li, ev.clientY < r.top + r.height / 2 ? over : over.nextSibling);
      }
    };
    grip.addEventListener("pointermove", move);
    grip.addEventListener("pointerup", () => {
      grip.removeEventListener("pointermove", move);
      li.classList.remove("dragging");
      order = [...ul.querySelectorAll("li:not(.section-head):not(.remote-head):not(.remote-session)")].map((n) => n.dataset.name);
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

$("#new-session").addEventListener("click", () => openPalette("new"));

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
  { name: ":diff",    hint: "git diff of session cwd", run: () => { closePalette(); openDiffView(); } },
  { name: ":mush",    hint: "run an agent task here",  run: () => setPalMode("mush") },
  { name: ":runs",    hint: "open a mush run",         run: () => setPalMode("runs") },
  { name: ":md",      hint: "preview a markdown file", run: () => openMdPicker() },
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
  if (pal.mode === "remote") return remoteSuggest().items.map((it) => ({ kind: "suggest", ...it }));
  if (pal.mode === "mdpick")
    return (pal.files || []).filter(fuzzy(q)).map((f) => ({ kind: "file", name: f }));
  if (pal.mode === "pick")
    return allSessions().filter((s) => fuzzy(q)(s.key))
      .map((s) => ({ kind: "session", name: s.key, s }));
  if (pal.mode === "runs")
    return allRuns().filter((r) => fuzzy(q)(`${r.host} ${runLabel(r)} ${r.state} ${r.project}`))
      .map((r) => ({ kind: "run", name: runLabel(r), r }));
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
    } else if (it.kind === "command" || it.kind === "suggest") {
      label.textContent = it.name;
      label.className = "cmd";
      right.textContent = it.hint;
    } else if (it.kind === "file") {
      label.textContent = it.name;
      right.textContent = "markdown";
    } else if (it.kind === "run") {
      const r = it.r;
      label.textContent = `${{ live: "◐", wait: "✋", ok: "✓", bad: "✕" }[runState(r)]} ${it.name}`;
      right.textContent = [r.host, (r.project || "").split("/").filter(Boolean).pop(), r.state.replace("_", " ")].filter(Boolean).join(" · ");
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
  updateGhost();
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
  } else if (it.kind === "suggest") {
    palComplete(it);
  } else if (it.kind === "file") {
    const key = pal.arg;
    closePalette();
    viewerPane().openView({ type: "md", key, path: it.name });
  } else if (it.kind === "run") {
    closePalette();
    viewerPane().openMush(it.r.host, it.r);
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

// --- viewers ---

// Unified-diff text → DOM: file headers, hunk markers, +/- line coloring.
// Text nodes only, so diff content can't inject markup.
function renderDiff(root, text) {
  const frag = document.createDocumentFragment();
  const head = document.createElement("div");
  head.className = "d-root";
  head.textContent = text.trim() ? root : `${root} — working tree clean`;
  frag.appendChild(head);
  if (!text.trim()) return frag;
  for (const line of text.split("\n")) {
    if (line.startsWith("index ") || line.startsWith("+++") || line.startsWith("---")) continue;
    const div = document.createElement("div");
    if (line.startsWith("diff --git")) {
      div.className = "d-file";
      div.textContent = line.replace(/^diff --git a\/(.*) b\/.*$/, "$1");
    } else if (line.startsWith("@@")) {
      div.className = "d-hunk";
      div.textContent = line;
    } else {
      div.className = line.startsWith("+") ? "d-add" : line.startsWith("-") ? "d-del" : "d-ctx";
      div.textContent = line || " ";
    }
    frag.appendChild(div);
  }
  return frag;
}

// The viewer opens opposite the focused terminal: split first if needed.
function viewerPane() {
  const src = paneFor();
  if (panes.length === 1) addPane();
  const target = panes.find((p) => p !== src) || panes[panes.length - 1];
  setFocus(src);
  return target;
}

function openDiffView() {
  const key = paneFor()?.session || allSessions()[0]?.key;
  if (!key) { alert("attach a session first"); return; }
  viewerPane().openView({ type: "diff", key });
}

async function openMdPicker() {
  const key = paneFor()?.session || allSessions()[0]?.key;
  if (!key) { alert("attach a session first"); return; }
  let files;
  try { ({ files } = await api(keyApi(key, "/files"))); }
  catch (err) { alert(err.message); return; }
  if (!files.length) { alert("no markdown files in the session's directory"); return; }
  setPalMode("mdpick", null, key);
  pal.files = files;
  renderPalette();
}

// Stage-aware completion for ":remote": menu items for the token being
// typed, plus a ghost hint of the syntax still ahead. `words` holds only
// completed tokens — a trailing partial is popped off and used as the
// filter prefix.
function remoteSuggest() {
  const q = $("#pal-input").value;
  const words = q.split(/\s+/).filter(Boolean);
  const part = q.endsWith(" ") || !words.length ? "" : words.pop();
  const sugg = (list) => list.filter((it) => it.name.startsWith(part));
  const ghost = (t) => (part ? "" : t);
  const [verb] = words;
  if (!words.length)
    return {
      items: sugg([
        { name: "add", hint: "connect a new remote" },
        { name: "rm",  hint: "delete a remote" },
        { name: "off", hint: "disconnect, keep config" },
        { name: "on",  hint: "reconnect" },
      ]),
      ghost: ghost("add · rm · off · on"),
    };
  if (verb === "rm" || verb === "off" || verb === "on") {
    const pool = verb === "rm" ? remotes : remotes.filter((r) => (r.state === "off") === (verb === "on"));
    return {
      items: words.length === 1 ? sugg(pool.map((r) => ({ name: r.name, hint: r.state }))) : [],
      ghost: words.length === 1 ? ghost("<name>") : "",
    };
  }
  if (verb !== "add") return { items: [], ghost: ghost("add · rm · off · on") };
  if (words.length === 1) return { items: [], ghost: ghost("<name>") };
  if (words.length === 2)
    return {
      items: sugg([
        { name: "ssh", hint: "<host[:port]> [token]" },
        { name: "url", hint: "<url> [token]" },
      ]),
      ghost: ghost("ssh · url"),
    };
  if (words.length === 3)
    return { items: [], ghost: ghost(words[2] === "url" ? "<url> [token]" : "<host[:port]> [token]") };
  return { items: [], ghost: words.length === 4 ? ghost("[token]") : "" };
}

function remoteLineValid(line) {
  const w = line.split(/\s+/).filter(Boolean);
  return ["rm", "off", "on"].includes(w[0]) ? w.length === 2 :
    w[0] === "add" && (w[2] === "ssh" || w[2] === "url") && w.length >= 4 && w.length <= 5;
}

// ":remote ❯" one-liners: "add jack ssh jack", "add lab ssh lab.example:9000 TOKEN",
// "add juneau url http://juneau:8300 TOKEN", "rm jack", "off jack", "on jack".
async function runRemoteCommand(line) {
  const words = line.split(/\s+/).filter(Boolean);
  const [verb, name] = words;
  try {
    if (verb === "rm" && name && words.length === 2) {
      await api(`/api/remotes/${encodeURIComponent(name)}`, { method: "DELETE" });
    } else if ((verb === "off" || verb === "on") && name && words.length === 2) {
      await api(`/api/remotes/${encodeURIComponent(name)}`, {
        method: "PATCH",
        body: JSON.stringify({ off: verb === "off" }),
      });
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
      throw new Error("usage: add <name> ssh <host[:port]> [token] · add <name> url <url> [token] · rm <name> · off <name> · on <name>");
    }
  } catch (err) { alert(err.message); return; }
  closePalette();
  refreshSessions();
}

const PAL_MODES = {
  switch:  { prompt: "❯", ph: "session or :command", hint: "↑↓ move · tab complete · ⏎ open · : commands · esc close" },
  new:     { prompt: "new ❯", ph: "session name (host:name for a remote)", hint: "⏎ create · esc back" },
  pick:    { prompt: null, ph: "which session?", hint: "↑↓ move · ⏎ select · esc back" },
  input:   { prompt: null, ph: "new name", hint: "⏎ rename · esc back" },
  confirm: { prompt: null, ph: "", hint: "y kill · n / esc back" },
  remote:  { prompt: ":remote ❯", ph: "add · rm · off · on", hint: "tab complete · ⏎ run · esc back" },
  mdpick:  { prompt: ":md ❯", ph: "which file?", hint: "↑↓ move · ⏎ preview · esc back" },
  mush:    { prompt: ":mush ❯", ph: "[-m model] task (runs in the focused session's cwd)", hint: "⏎ run · esc back" },
  runs:    { prompt: ":runs ❯", ph: "which run?", hint: "↑↓ move · ⏎ open · esc back" },
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

// The ghost sits after the typed text (offset one cell for the block
// caret) and plays placeholder when the input is empty: the mode's ph in
// most modes, a stage-aware syntax hint in remote mode.
function updateGhost() {
  const inp = $("#pal-input");
  const g = $(".pal-ghost");
  g.textContent = pal.mode === "remote" ? remoteSuggest().ghost
    : inp.value ? "" : PAL_MODES[pal.mode]?.ph || "";
  g.style.left = `${inp.value.length + 1}ch`;
}

// Tab (or tap/⏎ on a suggestion): splice the selected name into the input
// in place of the partial token, without running anything.
function palComplete(it) {
  const inp = $("#pal-input");
  if (it.kind === "suggest") {
    const q = inp.value;
    const head = q.endsWith(" ") ? q : q.slice(0, q.lastIndexOf(" ") + 1);
    inp.value = head + it.name + " ";
  } else if (it.kind === "create") {
    return;
  } else {
    inp.value = it.name;
  }
  pal.index = 0;
  inp.focus();
  inp.setSelectionRange(inp.value.length, inp.value.length);
  renderPalette();
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
  if (e.key === "Tab") {
    e.preventDefault();
    if (items[pal.index]) palComplete(items[pal.index]);
  } else if (e.key === "ArrowDown" || e.ctrlKey && e.key === "n") {
    e.preventDefault(); pal.index++; renderPalette();
  } else if (e.key === "ArrowUp" || e.ctrlKey && e.key === "p") {
    e.preventDefault(); pal.index--; renderPalette();
  } else if (e.key === "Enter") {
    e.preventDefault();
    const name = $("#pal-input").value.trim();
    if (pal.mode === "new") { if (creatableKey(name)) await createSession(name); }
    else if (pal.mode === "input") {
      if (VALID_NAME.test(name)) { closePalette(); await renameSession(pal.arg, name); }
    } else if (pal.mode === "remote") {
      if (!remoteLineValid(name) && items[pal.index]) palComplete(items[pal.index]);
      else await runRemoteCommand(name);
    }
    else if (pal.mode === "mush") { await runMushCommand(name); }
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

// --- mush runs: muxdeck as a front-end of the mush agent protocol ---

function mushApi(host, rest = "") {
  if (!host) return `/api/mush/runs${rest}`;
  return `/api/remotes/${encodeURIComponent(host)}/proxy/mush/runs${rest}`;
}

async function runMushCommand(input) {
  if (!input) return;
  // "-m <model>" prefix overrides the engine's default model for this run.
  let task = input, model = "";
  const m = input.match(/^-m(?:odel)?\s+(\S+)\s+(.+)$/);
  if (m) { model = m[1]; task = m[2]; }
  const key = paneFor()?.session;
  if (!key) { alert("focus a session first — the run uses its working directory"); return; }
  const i = key.indexOf(":");
  const host = i < 0 ? "" : key.slice(0, i);
  const session = i < 0 ? key : key.slice(i + 1);
  try {
    const run = await api(mushApi(host), { method: "POST", body: JSON.stringify({ task, session, model }) });
    closePalette();
    viewerPane().openMush(host, run);
    refreshSessions();
  } catch (err) { alert(err.message); }
}

// mushRenderer builds the event-stream DOM incrementally. All event payloads
// land as text nodes — engine output is untrusted. send() carries protocol
// commands (approval answers) back over the run's websocket.
function mushRenderer(box, send) {
  const stream = document.createElement("div");
  stream.className = "m-stream";
  const approvals = document.createElement("div");
  approvals.className = "m-approvals";
  box.append(stream, approvals);

  let textBlock = null, thinkBlock = null, lastTool = null, prunedNote = null;
  const el = (tag, cls, text) => {
    const n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined) n.textContent = text;
    return n;
  };
  // Long runs grow the stream without bound (per-payload caps bound each
  // card, not the card count). Keep the DOM to the recent tail; the full
  // history stays in the daemon's replay buffer.
  const MAX_STREAM_NODES = 400;
  const push = (n) => {
    const follow = box.scrollTop + box.clientHeight >= box.scrollHeight - 40;
    stream.appendChild(n);
    if (stream.childElementCount > MAX_STREAM_NODES) {
      if (!prunedNote) {
        prunedNote = el("div", "m-audit", "(older output pruned)");
        stream.prepend(prunedNote);
      }
      while (stream.childElementCount > MAX_STREAM_NODES) stream.children[1].remove();
    }
    if (follow) box.scrollTop = box.scrollHeight;
  };
  const breakBlocks = () => { textBlock = null; thinkBlock = null; };
  const fmtArgs = (args) => {
    let s = JSON.stringify(args ?? {}, null, 1);
    if (s.length > 2048) s = s.slice(0, 2048) + " …";
    return s;
  };

  function render(env) {
    const d = env.data || {};
    switch (env.type) {
      case "run_queued":
        break;
      case "run_started":
        push(el("div", "m-task", d.task || ""));
        break;
      case "checkpoint":
      case "run_artifact":
        break;
      case "run_state_changed":
        push(el("div", "m-audit", `state · ${(d.state || "").replace("_", " ")}`));
        break;
      case "approval_resolved": {
        // The decision landed (ours, or someone answering elsewhere — the
        // CLI, GitHub, another viewer): retire any card still up.
        if (approvals.childElementCount) {
          approvals.innerHTML = "";
          push(el("div", "m-audit", `${d.approved ? "approved" : "denied"}${d.reason ? " · " + d.reason : ""}`));
        }
        break;
      }
      case "step_started":
        breakBlocks(); lastTool = null;
        push(el("div", "m-step", `step ${d.step} · ${d.model || ""}`));
        break;
      case "thinking": {
        if (!thinkBlock) {
          const det = el("details", "m-think");
          det.appendChild(el("summary", null, "thinking"));
          thinkBlock = el("div");
          det.appendChild(thinkBlock);
          push(det);
        }
        thinkBlock.textContent += d.delta || "";
        break;
      }
      case "assistant_text":
        if (!textBlock) { textBlock = el("div", "m-text"); push(textBlock); }
        textBlock.textContent += d.delta || "";
        break;
      case "tool_called": {
        breakBlocks();
        const card = el("div", "m-tool");
        card.appendChild(el("div", "m-tool-name", d.name || "tool"));
        card.appendChild(el("pre", "m-tool-args", fmtArgs(d.args)));
        lastTool = card;
        push(card);
        break;
      }
      case "tool_result": {
        const home = lastTool || stream;
        const det = el("details", "m-result" + (d.is_error ? " err" : ""));
        det.appendChild(el("summary", null, d.is_error ? "result · error" : "result"));
        let content = d.content || "";
        if (content.length > 8192) content = content.slice(0, 8192) + "\n…";
        det.appendChild(el("pre", null, content));
        home.appendChild(det);
        break;
      }
      case "escalated":
        push(el("div", "m-escalated", `↑ escalated to ${d.to || "?"}`));
        break;
      case "approval_requested": {
        breakBlocks();
        const card = el("div", "m-approve" + (d.risk === "destructive" ? " danger" : ""));
        card.appendChild(el("div", "m-approve-head", `${d.risk || "write"} · ${d.name || "tool"}`));
        card.appendChild(el("pre", "m-tool-args", fmtArgs(d.args)));
        const row = el("div", "m-approve-row");
        const yes = el("button", "m-yes", "approve");
        const no = el("button", "m-no", "deny");
        const answer = (approved) => {
          send({ type: "approval_response", data: { approved } });
          card.remove();
          push(el("div", "m-audit", `${approved ? "approved" : "denied"} · ${d.name}`));
        };
        // A replayed journal may already carry the answer; the card only
        // stays up if no approval_resolved follows (the daemon replays in
        // order, so a stale card is retired within the same burst).
        yes.addEventListener("click", () => answer(true));
        no.addEventListener("click", () => answer(false));
        row.append(yes, no);
        card.appendChild(row);
        approvals.appendChild(card);
        break;
      }
      case "verdict_reached":
        breakBlocks();
        push(el("div", `m-verdict ${d.verdict}`, `verdict: ${d.verdict}${d.feedback ? " — " + d.feedback : ""}`));
        break;
      case "done":
        breakBlocks();
        approvals.innerHTML = "";
        push(el("div", "m-done", `done · ${d.steps ?? "?"} steps${d.text ? " — " + d.text : ""}`));
        break;
      case "_exit":
        approvals.innerHTML = "";
        if (!RUN_OK.has(d.state)) {
          push(el("div", "m-exit", `run ${(d.state || "").replace("_", " ")}`));
          if (d.error) push(el("pre", "m-exit-err", d.error.trim()));
        }
        break;
      case "_error":
        note(env.error || "command error");
        break;
      case "_trimmed":
        push(el("div", "m-audit", "(history trimmed)"));
        break;
      // unknown tags are ignored: forward-compatible with newer engines
    }
  }

  function note(text) { push(el("div", "m-note", text)); }
  return { render, note };
}

// mushRunHeader is the ledger row above the stream: source, model, cost,
// branch/PR, and the actions mush offers a finished or stuck run.
function mushRunHeader(host, run, pane) {
  const el = document.createElement("div");
  el.className = "m-head";
  const line = document.createElement("div");
  line.className = "m-head-line";
  const actions = document.createElement("div");
  actions.className = "m-head-actions";
  const err = document.createElement("div");
  err.className = "m-head-err";
  err.hidden = true;
  el.append(line, actions, err);

  const btn = (label, title, fn) => {
    const b = document.createElement("button");
    b.textContent = label;
    b.title = title;
    b.addEventListener("click", async () => {
      b.disabled = true;
      try { await fn(); } catch (err) { alert(err.message); }
      b.disabled = false;
    });
    return b;
  };
  const post = (rest) => api(mushApi(host, `/${encodeURIComponent(run.id)}${rest}`), { method: "POST" });

  function update(r) {
    run = r;
    const cost = r.cost_usd ? `$${r.cost_usd.toFixed(3)}` : "";
    const tokens = r.tokens_in || r.tokens_out ? `${fmtTokens(r.tokens_in)}/${fmtTokens(r.tokens_out)}` : "";
    line.textContent = [host, (r.project || "").split("/").filter(Boolean).pop(), r.source, r.model, r.provider,
      `${r.steps || 0} steps`, tokens, cost, runDuration(r), r.verdict && r.verdict !== "-" ? `verdict ${r.verdict}` : "",
      r.branch, r.local ? "engine: this daemon" : ""].filter(Boolean).join(" · ");
    actions.innerHTML = "";
    if (r.pr_url) {
      const a = document.createElement("a");
      a.href = r.pr_url; a.target = "_blank"; a.rel = "noopener"; a.textContent = "open PR";
      actions.appendChild(a);
    }
    const live = RUN_LIVE.has(r.state);
    if (live && r.local) actions.appendChild(btn("stop", "interrupt this run", () => api(mushApi(host, `/${encodeURIComponent(r.id)}`), { method: "DELETE" })));
    if (!live) {
      actions.appendChild(btn("retry", "start a fresh run with the same task (mush run --parent)", async () => {
        const child = await post("/retry");
        pane.openMush(host, child);
      }));
      if (r.state === "failed" || r.state === "interrupted" || r.state === "blocked") {
        actions.appendChild(btn("resume", "continue from the last checkpoint (mush resume)", async () => {
          await post("/resume");
          pane.msg("resumed — the new run appears in the list shortly");
          setTimeout(() => { pane.msg(""); refreshSessions(); }, 2500);
        }));
      }
    }
    err.textContent = r.error || "";
    err.hidden = !r.error;
  }
  update(run);
  return { el, update };
}

function fmtTokens(n) {
  n = n || 0;
  return n >= 1000 ? `${(n / 1000).toFixed(n >= 10000 ? 0 : 1)}k` : `${n}`;
}
