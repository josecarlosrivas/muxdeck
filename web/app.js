"use strict";

const $ = (sel) => document.querySelector(sel);

let term = null;
let fitAddon = null;
let ws = null;
let activeSession = null;

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
    refreshSessions();
  } else {
    $("#login-error").hidden = false;
  }
});

// --- session list ---

async function refreshSessions() {
  let sessions;
  try {
    sessions = await api("/api/sessions");
  } catch {
    return;
  }
  const ul = $("#sessions");
  ul.innerHTML = "";
  for (const s of sessions) {
    const li = document.createElement("li");
    li.classList.toggle("active", s.name === activeSession);

    const name = document.createElement("span");
    name.className = "name";
    name.textContent = s.name;

    const meta = document.createElement("span");
    meta.className = "meta";
    meta.textContent = `${s.windows}w${s.attached > 0 ? " ●" : ""}`;

    const kill = document.createElement("button");
    kill.className = "kill";
    kill.textContent = "✕";
    kill.title = `kill ${s.name}`;
    kill.addEventListener("click", async (e) => {
      e.stopPropagation();
      if (!confirm(`Kill session "${s.name}"?`)) return;
      try { await api(`/api/sessions/${encodeURIComponent(s.name)}`, { method: "DELETE" }); }
      catch (err) { alert(err.message); }
      if (s.name === activeSession) detach();
      refreshSessions();
    });

    li.append(name, meta, kill);
    li.addEventListener("click", () => attach(s.name));
    ul.appendChild(li);
  }
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
    attach(name);
  } catch (err) {
    alert(err.message);
  }
});

// --- terminal attach ---

function detach() {
  if (ws) {
    ws.onclose = null;
    ws.close();
    ws = null;
  }
  if (term) {
    term.dispose();
    term = null;
    fitAddon = null;
  }
  activeSession = null;
  $("#terminal").hidden = true;
  $("#placeholder").hidden = false;
  $("#placeholder").textContent = "select or create a session";
}

function sendResize() {
  if (!ws || ws.readyState !== WebSocket.OPEN || !fitAddon) return;
  fitAddon.fit();
  ws.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }));
}

function attach(name) {
  detach();
  activeSession = name;
  $("#placeholder").hidden = true;
  $("#terminal").hidden = false;

  term = new Terminal({
    fontFamily: "SF Mono, Menlo, Monaco, monospace",
    fontSize: 13,
    cursorBlink: true,
    theme: { background: "#000000" },
  });
  fitAddon = new FitAddon.FitAddon();
  term.loadAddon(fitAddon);
  term.open($("#terminal"));
  fitAddon.fit();
  term.focus();

  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  ws = new WebSocket(`${proto}//${location.host}/api/sessions/${encodeURIComponent(name)}/attach`);
  ws.binaryType = "arraybuffer";

  ws.onopen = () => {
    sendResize();
    refreshSessions();
  };
  ws.onmessage = (e) => {
    if (e.data instanceof ArrayBuffer) {
      term.write(new Uint8Array(e.data));
    } else {
      try {
        const msg = JSON.parse(e.data);
        if (msg.type === "error") term.write(`\r\n[muxdeck] ${msg.data}\r\n`);
      } catch {}
    }
  };
  ws.onclose = () => {
    if (term) term.write("\r\n[muxdeck] disconnected\r\n");
    refreshSessions();
  };

  term.onData((data) => sendInput(applyCtrl(data)));

  new ResizeObserver(() => sendResize()).observe($("#term-wrap"));
}

function sendInput(data) {
  if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({ type: "input", data }));
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
  // the terminal while tapping bar keys.
  bar.addEventListener("pointerdown", (e) => {
    const btn = e.target.closest("button");
    if (!btn) return;
    e.preventDefault();
    if (btn.id === "key-ctrl") {
      setCtrl(!ctrlArmed);
    } else {
      sendInput(applyCtrl(btn.dataset.key));
    }
  });

  // Shrink the layout to the visual viewport so the terminal is not hidden
  // behind the software keyboard.
  if (window.visualViewport) {
    const vv = window.visualViewport;
    const adjust = () => {
      $("#app").style.height = `${vv.height}px`;
      sendResize();
    };
    vv.addEventListener("resize", adjust);
  }
}

// --- boot ---

if ("serviceWorker" in navigator) {
  navigator.serviceWorker.register("sw.js").catch(() => {});
}
initKeybar();
window.addEventListener("resize", sendResize);
refreshSessions();
setInterval(() => { if (!document.hidden) refreshSessions(); }, 10000);
