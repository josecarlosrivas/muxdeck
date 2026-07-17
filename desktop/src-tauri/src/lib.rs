use std::net::TcpStream;
use std::sync::Mutex;
use std::time::Duration;

use tauri::{Manager, RunEvent, WebviewUrl, WebviewWindowBuilder};

// Standard muxdeck port. If a daemon (e.g. a launchd service) is already
// serving here the app attaches to it instead of spawning its own, so the
// service and the app can coexist — the sidecar is only a fallback.
const ADDR: &str = "127.0.0.1:8300";

struct Sidecar(Mutex<Option<std::process::Child>>);

fn daemon_up() -> bool {
    TcpStream::connect_timeout(&ADDR.parse().unwrap(), Duration::from_millis(300)).is_ok()
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
        std::thread::sleep(Duration::from_millis(100));
    }
    Some(child)
}

#[cfg(not(desktop))]
fn ensure_daemon() -> Option<std::process::Child> {
    None
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    tauri::Builder::default()
        .plugin(tauri_plugin_notification::init())
        .setup(|app| {
            app.manage(Sidecar(Mutex::new(ensure_daemon())));
            let url = format!("http://{ADDR}").parse().expect("valid url");
            WebviewWindowBuilder::new(app, "main", WebviewUrl::External(url))
                .title("muxdeck")
                .inner_size(1280.0, 820.0)
                .build()?;
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
