use std::sync::Mutex;

use tauri::{Manager, RunEvent, WebviewUrl, WebviewWindowBuilder};

// Standard muxdeck port. If a daemon (e.g. a launchd service) is already
// serving here the app attaches to it instead of spawning its own, so the
// service and the app can coexist — the sidecar is only a fallback.
#[cfg(desktop)]
const ADDR: &str = "127.0.0.1:8300";

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
fn daemon_up() -> bool {
    std::net::TcpStream::connect_timeout(
        &ADDR.parse().unwrap(),
        std::time::Duration::from_millis(300),
    )
    .is_ok()
}

// iOS can't exec a sidecar; the app is a pure client of remote daemons and
// the webview simply loads the configured server.
#[cfg(desktop)]
fn ensure_daemon() -> Option<std::process::Child> {
    if daemon_up() {
        return None;
    }
    let exe = std::env::current_exe().ok()?;
    let bin = exe.parent().expect("exe has parent dir").join("muxdeck");
    let child = std::process::Command::new(bin)
        .args(["-addr", ADDR])
        .spawn()
        .ok()?;
    for _ in 0..100 {
        if daemon_up() {
            break;
        }
        std::thread::sleep(std::time::Duration::from_millis(100));
    }
    Some(child)
}

#[cfg(not(desktop))]
fn ensure_daemon() -> Option<std::process::Child> {
    None
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
            app.manage(Sidecar(Mutex::new(ensure_daemon())));
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
                let url = format!("http://{ADDR}").parse().expect("valid url");
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
