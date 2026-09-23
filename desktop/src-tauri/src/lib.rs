use std::sync::Mutex;

use tauri::{Manager, RunEvent, WebviewUrl, WebviewWindowBuilder};

// Where a local daemon may already be serving: MUXDECK_ADDR (the daemon's
// own override) when set, then the standard port and the one above it,
// where a hand-installed service often lands. A daemon found there (e.g.
// a launchd service) is attached to instead of doubled — two daemons on
// one machine share the relay credential and fight over the tunnel — so
// the sidecar is only a fallback, spawned on the standard port.
#[cfg(desktop)]
const ADDRS: [&str; 2] = ["127.0.0.1:8300", "127.0.0.1:8301"];

struct Sidecar(Mutex<Option<std::process::Child>>);

// Cloud sign-in on desktop: the deck's ":cloud signin" sends the account
// page to the system browser, and the page comes back through the
// muxdeck:// scheme with a device token. The link is handed to the deck
// (which posts it to the daemon) rather than to the daemon directly, so
// the deck's own auth and refresh apply. A link that lands before the
// deck has loaded waits here for the page-load hook.
#[cfg(desktop)]
struct PendingLinks {
    urls: Mutex<Vec<String>>,
    loaded: std::sync::atomic::AtomicBool,
}

#[cfg(desktop)]
fn push_link(app: &tauri::AppHandle, url: String) {
    let Some(state) = app.try_state::<PendingLinks>() else { return };
    state.urls.lock().unwrap().push(url);
    if state.loaded.load(std::sync::atomic::Ordering::SeqCst) {
        deliver_links(app);
    }
}

#[cfg(desktop)]
fn deliver_links(app: &tauri::AppHandle) {
    let Some(state) = app.try_state::<PendingLinks>() else { return };
    let Some(win) = app.get_webview_window("main") else { return };
    let urls: Vec<String> = state.urls.lock().unwrap().drain(..).collect();
    for u in urls {
        let arg = serde_json::to_string(&u).unwrap_or_default();
        let _ = win.eval(&format!("window.muxdeckDeepLink && window.muxdeckDeepLink({arg})"));
    }
}

#[cfg(desktop)]
fn daemon_up(addr: &str) -> bool {
    let Ok(sock) = addr.parse() else { return false };
    std::net::TcpStream::connect_timeout(&sock, std::time::Duration::from_millis(300)).is_ok()
}

#[cfg(desktop)]
fn find_daemon() -> Option<String> {
    let env = std::env::var("MUXDECK_ADDR").ok().filter(|a| !a.is_empty());
    env.into_iter()
        .chain(ADDRS.iter().map(|a| a.to_string()))
        .find(|a| daemon_up(a))
}

// iOS can't exec a sidecar; the app is a pure client of remote daemons and
// the webview simply loads the configured server. On desktop this yields
// the address the app attaches to and the sidecar it spawned, if any.
#[cfg(desktop)]
fn ensure_daemon() -> (String, Option<std::process::Child>) {
    if let Some(addr) = find_daemon() {
        return (addr, None);
    }
    let addr = ADDRS[0].to_string();
    let child = std::env::current_exe().ok().and_then(|exe| {
        let bin = exe.parent().expect("exe has parent dir").join("muxdeck");
        std::process::Command::new(bin).args(["-addr", addr.as_str()]).spawn().ok()
    });
    if child.is_some() {
        for _ in 0..100 {
            if daemon_up(&addr) {
                break;
            }
            std::thread::sleep(std::time::Duration::from_millis(100));
        }
    }
    (addr, child)
}

// Launch-time update check: install silently, then offer a restart. Declining
// is safe — the swapped .app applies on the next launch anyway. Before
// restarting, kill our own sidecar so the relaunched app spawns the updated
// daemon instead of attaching to the old one (an external daemon, e.g. a
// launchd service, is never ours to kill and keeps working as before).
#[cfg(desktop)]
fn check_for_updates(app: tauri::AppHandle) {
    use tauri_plugin_dialog::{DialogExt, MessageDialogButtons};
    use tauri_plugin_updater::UpdaterExt;
    tauri::async_runtime::spawn(async move {
        let Ok(updater) = app.updater() else { return };
        let Ok(Some(update)) = updater.check().await else { return };
        if update.download_and_install(|_, _| {}, || {}).await.is_err() {
            return;
        }
        let handle = app.clone();
        app.dialog()
            .message(format!(
                "muxdeck {} has been installed and will run next launch.",
                update.version
            ))
            .title("Update ready")
            .buttons(MessageDialogButtons::OkCancelCustom(
                "Restart".into(),
                "Later".into(),
            ))
            .show(move |restart| {
                if restart {
                    if let Some(state) = handle.try_state::<Sidecar>() {
                        if let Some(child) = state.0.lock().unwrap().as_mut() {
                            let _ = child.kill();
                            let _ = child.wait();
                        }
                    }
                    handle.restart();
                }
            });
    });
}

// First-launch offer, made only when the app had to spawn its own daemon:
// run muxdeck as a login service instead, so terminals stay reachable from
// phones and the cloud with the app closed and Keep Awake works without it
// open. Yes: the sidecar is stopped, `muxdeck service install` points the
// service at the daemon inside this bundle (so it follows app updates),
// the claim code is shown, and the window reloads onto the service. No:
// remembered, and the same install stays one shell command away.
#[cfg(desktop)]
fn offer_service(app: tauri::AppHandle) {
    use tauri_plugin_dialog::{DialogExt, MessageDialogButtons};
    if !(cfg!(target_os = "macos") || cfg!(target_os = "linux")) {
        return;
    }
    let Some(home) = std::env::var_os("HOME") else { return };
    let home = std::path::PathBuf::from(home);
    let unit = if cfg!(target_os = "macos") {
        home.join("Library/LaunchAgents/com.muxdeck.agent.plist")
    } else {
        home.join(".config/systemd/user/muxdeck.service")
    };
    let declined = app
        .path()
        .config_dir()
        .ok()
        .map(|d| d.join("muxdeck").join("service-declined"));
    if unit.exists() || declined.as_ref().is_some_and(|p| p.exists()) {
        return;
    }
    let handle = app.clone();
    app.dialog()
        .message(
            "Your terminals stay reachable from your phone and muxdeck cloud while the app is closed, \
             and Keep Awake works without it open. muxdeck installs itself as a login service for \
             your user — no admin password needed.",
        )
        .title("Run muxdeck in the background?")
        .buttons(MessageDialogButtons::OkCancelCustom(
            "Run in background".into(),
            "Not now".into(),
        ))
        .show(move |yes| {
            if !yes {
                if let Some(p) = declined {
                    if let Some(dir) = p.parent() {
                        let _ = std::fs::create_dir_all(dir);
                    }
                    let _ = std::fs::write(p, b"");
                }
                return;
            }
            std::thread::spawn(move || install_service(handle));
        });
}

#[cfg(desktop)]
fn install_service(app: tauri::AppHandle) {
    use tauri_plugin_dialog::{DialogExt, MessageDialogButtons};
    use tauri_plugin_opener::OpenerExt;
    // The service takes the standard port; the sidecar holding it is ours to stop.
    if let Some(state) = app.try_state::<Sidecar>() {
        if let Some(mut child) = state.0.lock().unwrap().take() {
            let _ = child.kill();
            let _ = child.wait();
        }
    }
    let bin = std::env::current_exe()
        .ok()
        .and_then(|e| e.parent().map(|d| d.join("muxdeck")));
    let out = bin.and_then(|b| {
        std::process::Command::new(b)
            .args(["service", "install", "-json"])
            .output()
            .ok()
    });
    let mut failed = false;
    let mut url: Option<String> = None;
    let (title, text) = match out {
        Some(o) if o.status.success() => {
            let v: serde_json::Value = serde_json::from_slice(&o.stdout).unwrap_or_default();
            match v.get("claim") {
                Some(c) => {
                    url = c["url"].as_str().map(|s| s.to_string());
                    (
                        "muxdeck is running in the background",
                        format!(
                            "Claim code: {}   (expires in {} minutes)\n\nEnter it under Daemons on your account page and this machine appears in your other muxdeck apps.",
                            c["code"].as_str().unwrap_or("?"),
                            c["expires_in"].as_i64().unwrap_or(0) / 60
                        ),
                    )
                }
                None => ("muxdeck is running in the background", "Installed as a login service.".to_string()),
            }
        }
        Some(o) => {
            failed = true;
            ("Could not install the service", String::from_utf8_lossy(&o.stderr).trim().to_string())
        }
        None => {
            failed = true;
            ("Could not install the service", "the daemon binary next to the app could not be run".to_string())
        }
    };
    if failed {
        // Back onto a sidecar of our own; the window's address is the same.
        if let Some(state) = app.try_state::<Sidecar>() {
            let (_, child) = ensure_daemon();
            *state.0.lock().unwrap() = child;
        }
    }
    let has_url = url.is_some();
    let handle = app.clone();
    let mut dialog = app.dialog().message(text).title(title);
    if has_url {
        dialog = dialog.buttons(MessageDialogButtons::OkCancelCustom(
            "Open account page".into(),
            "Later".into(),
        ));
    }
    dialog.show(move |open| {
        if open {
            if let Some(u) = url {
                let _ = handle.opener().open_url(u, None::<&str>);
            }
        }
    });
    if let Some(win) = app.get_webview_window("main") {
        let _ = win.eval("location.reload()");
    }
}

#[cfg(not(desktop))]
const HOME_BUTTON_JS: &str = r#"(function () {
  if (window.top !== window) return;
  if (location.protocol === "tauri:" || location.hostname === "tauri.localhost") return;
  addEventListener("DOMContentLoaded", function () {
    var b = document.createElement("button");
    b.textContent = "⌂";
    b.title = "servers";
    b.style.cssText = "position:fixed;right:10px;bottom:10px;width:34px;height:34px;border-radius:17px;background:rgba(18,22,27,0.75);border:1px solid #232b33;color:#6b7681;font-size:15px;line-height:1;z-index:2147483647;-webkit-backdrop-filter:blur(6px);backdrop-filter:blur(6px)";
    b.addEventListener("click", function () { location.href = "tauri://localhost/index.html"; });
    document.body.appendChild(b);
  });
})();"#;

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    let builder = tauri::Builder::default()
        .plugin(tauri_plugin_notification::init())
        .plugin(tauri_plugin_deep_link::init())
        .plugin(tauri_plugin_opener::init());
    #[cfg(desktop)]
    let builder = builder
        .plugin(tauri_plugin_updater::Builder::new().build())
        .plugin(tauri_plugin_dialog::init());
    builder
        .setup(|app| {
            #[cfg(desktop)]
            let (addr, sidecar) = ensure_daemon();
            #[cfg(desktop)]
            let spawned = sidecar.is_some();
            #[cfg(not(desktop))]
            let sidecar = None;
            app.manage(Sidecar(Mutex::new(sidecar)));
            // Desktop rides the local daemon's own UI; mobile has no daemon
            // and loads the bundled server picker instead, which iframes the
            // chosen remote. A fixed window size on iOS letterboxes the
            // webview, so size only on desktop.
            #[cfg(desktop)]
            {
                use tauri::webview::PageLoadEvent;
                use tauri_plugin_deep_link::DeepLinkExt;
                app.manage(PendingLinks {
                    urls: Mutex::new(Vec::new()),
                    loaded: std::sync::atomic::AtomicBool::new(false),
                });
                let url = format!("http://{addr}").parse().expect("valid url");
                WebviewWindowBuilder::new(app, "main", WebviewUrl::External(url))
                    .title("muxdeck")
                    .inner_size(1280.0, 820.0)
                    .on_page_load(|win, payload| {
                        let app = win.app_handle().clone();
                        let Some(state) = app.try_state::<PendingLinks>() else { return };
                        let finished = matches!(payload.event(), PageLoadEvent::Finished);
                        state.loaded.store(finished, std::sync::atomic::Ordering::SeqCst);
                        if finished {
                            deliver_links(&app);
                        }
                    })
                    .build()?;
                let handle = app.handle().clone();
                app.deep_link().on_open_url(move |event| {
                    for u in event.urls() {
                        push_link(&handle, u.to_string());
                    }
                });
                // The link that launched the app, when it was not running.
                if let Ok(Some(urls)) = app.deep_link().get_current() {
                    for u in urls {
                        push_link(app.handle(), u.to_string());
                    }
                }
            }
            // Remote pages (the deck reached through the relay) get a
            // floating home button injected by the shell — the picker used
            // to host one, but a top-level page can't be overlaid by it.
            #[cfg(not(desktop))]
            WebviewWindowBuilder::new(app, "main", WebviewUrl::App("index.html".into()))
                .initialization_script(HOME_BUTTON_JS)
                .build()?;
            #[cfg(desktop)]
            check_for_updates(app.handle().clone());
            #[cfg(desktop)]
            if spawned {
                offer_service(app.handle().clone());
            }
            Ok(())
        })
        .build(tauri::generate_context!())
        .expect("error while building muxdeck desktop app")
        .run(|app, event| {
            if let RunEvent::Exit = event {
                if let Some(state) = app.try_state::<Sidecar>() {
                    if let Some(child) = state.0.lock().unwrap().as_mut() {
                        let _ = child.kill();
                        let _ = child.wait();
                    }
                }
            }
        });
}
