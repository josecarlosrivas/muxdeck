// End-to-end test for the command palette's :remote mode: stage-aware
// completions, ghost hints, Enter-completes-then-runs, and mobile-width
// containment. Spawns a sandboxed muxdeck (spare port, scratch remotes
// registry, loopback so no auth) and drives it with headless Chrome.
//
// Usage: MUXDECK_BIN=/path/to/muxdeck node palette.test.mjs
// (default binary path: ../../muxdeck, i.e. `go build -o muxdeck .` at the
// repo root)

import { spawn } from "node:child_process";
import { mkdtemp, writeFile, rm } from "node:fs/promises";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import puppeteer from "puppeteer";

const here = dirname(fileURLToPath(import.meta.url));
const bin = resolve(process.env.MUXDECK_BIN || join(here, "..", "..", "muxdeck"));

let passed = 0;
const failures = [];
function assert(name, cond, detail = "") {
  if (cond) {
    passed++;
    console.log(`  ok ${name}`);
  } else {
    failures.push(name);
    console.log(`FAIL ${name}${detail ? ` — ${detail}` : ""}`);
  }
}
function assertEq(name, got, want) {
  const g = JSON.stringify(got), w = JSON.stringify(want);
  assert(name, g === w, `got ${g}, want ${w}`);
}

function freePort() {
  return new Promise((res, rej) => {
    const srv = createServer();
    srv.listen(0, "127.0.0.1", () => {
      const { port } = srv.address();
      srv.close(() => res(port));
    });
    srv.on("error", rej);
  });
}

async function waitFor(fn, what, ms = 10000) {
  const deadline = Date.now() + ms;
  for (;;) {
    try {
      const v = await fn();
      if (v) return v;
    } catch {}
    if (Date.now() > deadline) throw new Error(`timeout waiting for ${what}`);
    await new Promise((r) => setTimeout(r, 100));
  }
}

// --- sandboxed server ---
// Two url-mode remotes point at a dead port (state "down"), one is soft-off
// (state "off"), so the state-filtered pools for on/off are distinguishable.
const work = await mkdtemp(join(tmpdir(), "muxdeck-palette-"));
const remotesPath = join(work, "remotes.json");
await writeFile(remotesPath, JSON.stringify([
  { name: "jack", mode: "url", url: "http://127.0.0.1:9" },
  { name: "lab", mode: "url", url: "http://127.0.0.1:9", off: true },
  { name: "juneau", mode: "url", url: "http://127.0.0.1:9" },
]));

const port = await freePort();
const base = `http://127.0.0.1:${port}`;
const server = spawn(bin, ["-addr", `127.0.0.1:${port}`], {
  env: { ...process.env, MUXDECK_REMOTES: remotesPath },
  stdio: ["ignore", "inherit", "inherit"],
});
server.on("error", (err) => {
  console.error(`failed to start ${bin}: ${err.message}`);
  process.exit(1);
});
await waitFor(() => fetch(base).then((r) => r.ok), "server ready");

const browser = await puppeteer.launch({ args: ["--no-sandbox"] });
const page = await browser.newPage();
const alerts = [];
page.on("dialog", (d) => { alerts.push(d.message()); d.dismiss(); });

// --- DOM helpers ---
const ghost = () => page.$eval(".pal-ghost", (el) => el.textContent);
const ghostLeft = () => page.$eval(".pal-ghost", (el) => el.style.left);
const names = () => page.$$eval(".pal-list li", (els) => els.map((li) => li.firstChild.textContent));
const hints = () => page.$$eval(".pal-list li .dim", (els) => els.map((el) => el.textContent));
const inputVal = () => page.$eval("#pal-input", (el) => el.value);
const modeText = () => page.$eval(".pal-mode", (el) => el.textContent);
const paletteHidden = () => page.$eval("#palette", (el) => el.hidden);
const clearInput = () => page.evaluate(() => {
  const inp = document.querySelector("#pal-input");
  inp.value = "";
  inp.dispatchEvent(new Event("input", { bubbles: true }));
});
const openRemoteMode = async () => {
  await page.evaluate(() => { openPalette("switch"); });
  await page.type("#pal-input", ":remote");
  await page.keyboard.press("Enter");
  await waitFor(async () => (await modeText()) === ":remote ❯", "remote mode");
};

try {
  await page.setViewport({ width: 1280, height: 800 });
  await page.goto(base, { waitUntil: "networkidle0" });
  // The remotes list must have landed before pool assertions mean anything.
  await page.waitForFunction(() => remotes.length === 3);

  // --- switch mode: ghost plays placeholder when empty ---
  await page.evaluate(() => { openPalette("switch"); });
  assertEq("switch mode: empty-input ghost is the placeholder", await ghost(), "session or :command");

  await page.type("#pal-input", ":remote");
  assert("switch mode: :command menu filters to :remote", (await names()).includes(":remote"));
  await page.keyboard.press("Enter");
  assertEq("enter on :remote enters remote mode", await modeText(), ":remote ❯");

  // --- stage 1: verb ---
  assertEq("verb stage: ghost lists the verbs", await ghost(), "add · rm · off · on");
  assertEq("verb stage: menu offers the verbs", await names(), ["add", "rm", "off", "on"]);

  await page.type("#pal-input", "a");
  assertEq("mid-token: menu narrows by prefix", await names(), ["add"]);
  assertEq("mid-token: ghost goes quiet", await ghost(), "");

  await page.keyboard.press("Tab");
  assertEq("tab completes the verb plus a space", await inputVal(), "add ");
  assertEq("add stage 2: ghost shows <name>", await ghost(), "<name>");
  assertEq("add stage 2: free-typed, no menu", await names(), []);

  // --- add: mode stage ---
  await page.type("#pal-input", "newbox ");
  assertEq("add stage 3: menu offers ssh/url", await names(), ["ssh", "url"]);
  assertEq("add stage 3: per-option arg syntax rides as hints", await hints(),
    ["<host[:port]> [token]", "<url> [token]"]);
  assertEq("add stage 3: ghost shows ssh · url", await ghost(), "ssh · url");

  await page.keyboard.press("Tab");
  assertEq("tab completes ssh", await inputVal(), "add newbox ssh ");
  assertEq("add stage 4: ghost shows host syntax", await ghost(), "<host[:port]> [token]");

  await page.type("#pal-input", "host:9000 ");
  assertEq("add stage 5: ghost shows optional token", await ghost(), "[token]");
  await page.type("#pal-input", "tok ");
  assertEq("complete line: ghost is empty", await ghost(), "");

  // --- ghost offset: one cell past the block caret ---
  assertEq("ghost sits at (len+1)ch", await ghostLeft(),
    `${(await inputVal()).length + 1}ch`);

  // --- rm/off/on pools ---
  await clearInput();
  await page.type("#pal-input", "rm ");
  assertEq("rm offers every remote", await names(), ["jack", "lab", "juneau"]);
  assertEq("rm hints carry remote state", await hints(), ["down", "off", "down"]);

  await clearInput();
  await page.type("#pal-input", "on ");
  assertEq("on offers only remotes that are off", await names(), ["lab"]);

  await clearInput();
  await page.type("#pal-input", "off ");
  assertEq("off offers only remotes that are up", await names(), ["jack", "juneau"]);

  // --- Enter completes while invalid, runs when valid (the phone path) ---
  await clearInput();
  await page.type("#pal-input", "rm");
  await page.keyboard.press("Enter");
  assertEq("enter on an invalid line completes instead of running", await inputVal(), "rm ");
  assert("palette stays open after completion", !(await paletteHidden()));

  await page.type("#pal-input", "jack");
  await page.keyboard.press("Enter");
  await waitFor(paletteHidden, "palette to close after rm runs");
  assert("enter on a valid line runs it and closes the palette", await paletteHidden());
  const after = await fetch(`${base}/api/remotes`).then((r) => r.json());
  assertEq("the rm really deleted the remote", after.map((r) => r.name).sort(), ["juneau", "lab"]);

  // --- escape backs out to switch mode ---
  await openRemoteMode();
  await page.keyboard.press("Escape");
  assertEq("escape backs out to switch mode", await modeText(), "❯");
  await page.keyboard.press("Escape");
  assert("escape again closes the palette", await paletteHidden());

  // --- mobile width: the ghost stays inside the input field ---
  await page.setViewport({ width: 375, height: 667 });
  await openRemoteMode();
  const [g, f] = await page.evaluate(() => {
    const g = document.querySelector(".pal-ghost").getBoundingClientRect();
    const f = document.querySelector(".pal-field").getBoundingClientRect();
    return [g.toJSON(), f.toJSON()];
  });
  assert("375px: longest ghost fits inside the field", g.right <= f.right + 0.5,
    `ghost right ${g.right}, field right ${f.right}`);
  assert("375px: ghost is visible", g.width > 0);

  assertEq("no stray alert dialogs", alerts, []);
} finally {
  await browser.close();
  server.kill();
  await rm(work, { recursive: true, force: true });
}

console.log(`\n${passed}/${passed + failures.length} assertions passed`);
if (failures.length) {
  console.error(`failed: ${failures.join("; ")}`);
  process.exit(1);
}
