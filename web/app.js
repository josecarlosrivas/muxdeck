"use strict";

const $ = (sel) => document.querySelector(sel);

const isMac = /Mac|iP(hone|ad|od)/.test(navigator.platform || navigator.userAgent);

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
    $("#switcher").hidden ? openSwitcher() : closeSwitcher();
  } else if (chord(e, "f") && paneFor()?.term) {
    e.preventDefault();
    paneFor().toggleFind(true);
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
          <button class="p-close" title="close pane">&#10005;</button>
        </span>
      </div>
      <div class="findbar" hidden>
        <input placeholder="find" spellcheck="false" autocomplete="off">
        <button class="f-prev" title="previous match">&lsaquo;</button>
        <button class="f-next" title="next match">&rsaquo;</button>
        <button class="f-close" title="close">&#10005;</button>
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
      theme: { background: "#000000" },
    });
    this.fit = new FitAddon.FitAddon();
    this.search = new SearchAddon.SearchAddon();
    this.term.loadAddon(this.fit);
    this.term.loadAddon(this.search);
    this.term.loadAddon(new WebLinksAddon.WebLinksAddon((e, uri) => window.open(uri, "_blank", "noopener")));
    this.term.parser.registerOscHandler(52, (data) => { osc52ToClipboard(data); return true; });
    this.term.onData((d) => this.send(applyCtrl(d)));
    this.term.attachCustomKeyEventHandler((e) => !(chord(e, "k") || chord(e, "f")));
    const mount = this.el.querySelector(".pane-term");
    mount.innerHTML = "";
    this.term.open(mount);
    // Clipboard writes need transient activation, so copy at gesture end
    // rather than on every selection change.
    mount.addEventListener("mouseup", () => copySelection(this.term));
    mount.addEventListener("touchend", () => copySelection(this.term));
    this.fit.fit();
    this.term.focus();
  }

  connect() {
    const name = this.session;
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    const ws = new WebSocket(`${proto}//${location.host}/api/sessions/${encodeURIComponent(name)}/attach`);
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
      const { enabled } = await api(`/api/sessions/${encodeURIComponent(this.session)}/mouse`);
      btn.hidden = false;
      btn.classList.toggle("armed", enabled);
    } catch { btn.hidden = true; }
  }

  async toggleMouse() {
    if (!this.session) return;
    const btn = this.el.querySelector(".p-mouse");
    const want = !btn.classList.contains("armed");
    try {
      await api(`/api/sessions/${encodeURIComponent(this.session)}/mouse`, {
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
  if (focusedPane === p) setFocus(panes[0]);
  updateHash();
  refreshSessions();
}

$("#split").addEventListener("click", () => {
  if (panes.length === 1) addPane();
  else closePane(panes[panes.length - 1]);
});

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
const seenActivity = new Map(); // name -> last activity we consider seen

function orderedSessions() {
  const idx = new Map(order.map((n, i) => [n, i]));
  return [...lastSessions].sort(
    (a, b) => (idx.has(a.name) ? idx.get(a.name) : 1e9) - (idx.has(b.name) ? idx.get(b.name) : 1e9)
  );
}

async function refreshSessions() {
  let sessions;
  try {
    sessions = await api("/api/sessions");
  } catch {
    return;
  }
  lastSessions = sessions;
  const attached = new Set(panes.map((p) => p.session).filter(Boolean));
  for (const s of sessions) {
    // Viewing a session counts as seeing its output; new sessions start seen.
    if (attached.has(s.name) || !seenActivity.has(s.name)) seenActivity.set(s.name, s.activity);
  }

  const ul = $("#sessions");
  ul.innerHTML = "";
  for (const s of orderedSessions()) {
    const li = document.createElement("li");
    li.dataset.name = s.name;
    li.classList.toggle("active", attached.has(s.name));

    const grip = document.createElement("span");
    grip.className = "grip";
    grip.textContent = "⋮⋮";
    grip.title = "drag to reorder";
    initDrag(li, grip);

    const name = document.createElement("span");
    name.className = "name";
    name.textContent = s.name;
    if (s.activity > seenActivity.get(s.name)) {
      const dot = document.createElement("span");
      dot.className = "dot";
      dot.title = "new output";
      name.append(" ", dot);
    }

    const meta = document.createElement("span");
    meta.className = "meta";
    meta.textContent = `${s.windows} win${s.windows === 1 ? "" : "s"}${s.attached > 0 ? " ●" : ""}`;
    li.title = `${s.windows} window${s.windows === 1 ? "" : "s"} · created ${new Date(s.created).toLocaleString()}${s.attached > 0 ? " · attached" : ""}`;

    const rename = document.createElement("button");
    rename.className = "rename";
    rename.textContent = "✎";
    rename.title = `rename ${s.name}`;
    rename.addEventListener("click", (e) => {
      e.stopPropagation();
      renameSession(s.name);
    });

    const kill = document.createElement("button");
    kill.className = "kill";
    kill.textContent = "✕";
    kill.title = `kill ${s.name}`;
    kill.addEventListener("click", async (e) => {
      e.stopPropagation();
      if (!confirm(`Kill session "${s.name}"?`)) return;
      try { await api(`/api/sessions/${encodeURIComponent(s.name)}`, { method: "DELETE" }); }
      catch (err) { alert(err.message); }
      for (const p of panes) if (p.session === s.name) p.detach();
      refreshSessions();
    });

    li.append(grip, name, meta, rename, kill);
    li.addEventListener("click", () => paneFor().attach(s.name));
    ul.appendChild(li);
  }
}

function initDrag(li, grip) {
  grip.addEventListener("pointerdown", (e) => {
    e.preventDefault();
    e.stopPropagation();
    grip.setPointerCapture(e.pointerId);
    li.classList.add("dragging");
    const ul = $("#sessions");
    const move = (ev) => {
      const over = document.elementFromPoint(ev.clientX, ev.clientY)?.closest("#sessions li");
      if (over && over !== li) {
        const r = over.getBoundingClientRect();
        ul.insertBefore(li, ev.clientY < r.top + r.height / 2 ? over : over.nextSibling);
      }
    };
    grip.addEventListener("pointermove", move);
    grip.addEventListener("pointerup", () => {
      grip.removeEventListener("pointermove", move);
      li.classList.remove("dragging");
      order = [...ul.querySelectorAll("li")].map((n) => n.dataset.name);
      saveOrder();
    }, { once: true });
  });
}

async function renameSession(oldName) {
  const name = prompt("rename session", oldName);
  if (!name || name === oldName) return;
  try {
    await api(`/api/sessions/${encodeURIComponent(oldName)}/rename`, {
      method: "POST",
      body: JSON.stringify({ name }),
    });
  } catch (err) {
    alert(err.message);
    return;
  }
  for (const p of panes) {
    if (p.session === oldName) {
      p.session = name;
      p.el.querySelector(".pane-title").textContent = name;
    }
  }
  const i = order.indexOf(oldName);
  if (i >= 0) { order[i] = name; saveOrder(); }
  if (seenActivity.has(oldName)) {
    seenActivity.set(name, seenActivity.get(oldName));
    seenActivity.delete(oldName);
  }
  updateHash();
  refreshSessions();
}

$("#refresh").addEventListener("click", refreshSessions);

$("#new-session").addEventListener("submit", async (e) => {
  e.preventDefault();
  const name = $("#new-name").value.trim();
  if (!name) return;
  try {
    await api("/api/sessions", { method: "POST", body: JSON.stringify({ name }) });
    $("#new-name").value = "";
    await refreshSessions();
    paneFor().attach(name);
  } catch (err) {
    alert(err.message);
  }
});

// --- quick switcher ---

let swIndex = 0;

function fuzzy(q) {
  q = q.toLowerCase();
  return (name) => {
    let i = 0;
    for (const c of name.toLowerCase()) if (c === q[i]) i++;
    return i >= q.length;
  };
}

function switcherMatches() {
  return orderedSessions().map((s) => s.name).filter(fuzzy($("#switcher input").value));
}

function renderSwitcher() {
  const ul = $("#switcher ul");
  const names = switcherMatches();
  swIndex = Math.max(0, Math.min(swIndex, names.length - 1));
  ul.innerHTML = "";
  names.forEach((n, i) => {
    const li = document.createElement("li");
    li.textContent = n;
    li.classList.toggle("selected", i === swIndex);
    li.addEventListener("click", () => pickSession(n));
    ul.appendChild(li);
  });
}

function openSwitcher() {
  swIndex = 0;
  $("#switcher").hidden = false;
  const inp = $("#switcher input");
  inp.value = "";
  renderSwitcher();
  inp.focus();
}

function closeSwitcher() {
  $("#switcher").hidden = true;
  paneFor()?.term?.focus();
}

function pickSession(name) {
  closeSwitcher();
  paneFor().attach(name);
}

$("#switcher").addEventListener("click", (e) => {
  if (e.target === $("#switcher")) closeSwitcher();
});

$("#switcher input").addEventListener("input", () => renderSwitcher());
$("#switcher input").addEventListener("keydown", (e) => {
  const names = switcherMatches();
  if (e.key === "Escape") closeSwitcher();
  else if (e.key === "ArrowDown") { e.preventDefault(); swIndex++; renderSwitcher(); }
  else if (e.key === "ArrowUp") { e.preventDefault(); swIndex--; renderSwitcher(); }
  else if (e.key === "Enter" && names[swIndex]) pickSession(names[swIndex]);
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
addPane();
initKeybar();
window.addEventListener("resize", () => panes.forEach((p) => p.sendResize()));
document.addEventListener("visibilitychange", () => {
  if (!document.hidden) panes.forEach((p) => p.reconnectNow());
});
refreshSessions().then(attachFromHash);
setInterval(() => { if (!document.hidden) refreshSessions(); }, 10000);
