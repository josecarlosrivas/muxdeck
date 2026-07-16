#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

use std::net::TcpStream;
use std::process::{Child, Command};
use std::sync::Mutex;
use std::time::Duration;

use tauri::{Manager, RunEvent, WebviewUrl, WebviewWindowBuilder};

// Standard muxdeck port. If a daemon (e.g. a launchd service) is already
// serving here the app attaches to it instead of spawning its own, so the
// service and the app can coexist — the sidecar is only a fallback.
const ADDR: &str = "127.0.0.1:8300";

struct Sidecar(Mutex<Option<Child>>);

fn daemon_up() -> bool {
    TcpStream::connect_timeout(&ADDR.parse().unwrap(), Duration::from_millis(300)).is_ok()
}

fn main() {
    tauri::Builder::default()
        .plugin(tauri_plugin_notification::init())
        .setup(|app| {
            let mut child: Option<Child> = None;
            if !daemon_up() {
                let exe = std::env::current_exe()?;
                let bin = exe.parent().expect("exe has parent dir").join("muxdeck");
                child = Some(Command::new(bin).args(["-addr", ADDR]).spawn()?);
                for _ in 0..100 {
                    if daemon_up() {
                        break;
                    }
                    std::thread::sleep(Duration::from_millis(100));
                }
            }
            app.manage(Sidecar(Mutex::new(child)));
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
