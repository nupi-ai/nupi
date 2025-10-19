use once_cell::sync::Lazy;
use rest_client::{
    ClaimedToken, CreatedPairing, CreatedToken, PairingEntry, RestClient, RestClientError,
    TokenEntry, VoiceStreamRequest, claim_pairing_token,
};
use serde_json::{Value, json, to_value};
use std::{
    collections::HashMap,
    env,
    path::PathBuf,
    process::Stdio,
    sync::Mutex,
    time::{Duration, Instant},
};
use tauri::{
    Emitter, Manager,
    image::Image,
    menu::{Menu, MenuItem, PredefinedMenuItem},
    tray::TrayIconBuilder,
};
use tauri_plugin_shell::ShellExt;
use tokio::sync::watch;

mod installer;
mod rest_client;
mod settings;

use settings::ClientSettings;

struct AppState {
    daemon_pid: Mutex<Option<u32>>,
    daemon_running: Mutex<bool>,
}

const CANCELLED_MESSAGE: &str = "Voice stream cancelled by user";
const STALE_OPERATION_TIMEOUT: Duration = Duration::from_secs(3600);

struct VoiceOperation {
    sender: watch::Sender<bool>,
    started: Instant,
}

static ACTIVE_VOICE_STREAMS: Lazy<Mutex<HashMap<String, VoiceOperation>>> =
    Lazy::new(|| Mutex::new(HashMap::new()));

fn cleanup_stale_operations(registry: &mut HashMap<String, VoiceOperation>) {
    registry.retain(|_, op| op.started.elapsed() < STALE_OPERATION_TIMEOUT);
}

fn register_voice_operation(operation_id: &str) -> Result<(String, watch::Receiver<bool>), String> {
    let trimmed = operation_id.trim();
    if trimmed.is_empty() {
        return Err("operation_id is required".into());
    }

    let mut registry = ACTIVE_VOICE_STREAMS
        .lock()
        .map_err(|_| "voice operation registry poisoned".to_string())?;
    cleanup_stale_operations(&mut registry);
    if registry.contains_key(trimmed) {
        return Err("a voice upload is already active for this identifier".into());
    }
    let (sender, receiver) = watch::channel(false);
    registry.insert(
        trimmed.to_string(),
        VoiceOperation {
            sender,
            started: Instant::now(),
        },
    );
    Ok((trimmed.to_string(), receiver))
}

fn complete_voice_operation(operation_id: &str) {
    if let Ok(mut registry) = ACTIVE_VOICE_STREAMS.lock() {
        registry.remove(operation_id);
    }
}

fn cancel_voice_operation(operation_id: &str) -> Result<bool, String> {
    let trimmed = operation_id.trim();
    if trimmed.is_empty() {
        return Err("operation_id is required".into());
    }
    let mut registry = ACTIVE_VOICE_STREAMS
        .lock()
        .map_err(|_| "voice operation registry poisoned".to_string())?;
    cleanup_stale_operations(&mut registry);
    if let Some(operation) = registry.remove(trimmed) {
        let _ = operation.sender.send(true);
        Ok(true)
    } else {
        Ok(false)
    }
}

// Learn more about Tauri commands at https://tauri.app/develop/calling-rust/
#[tauri::command]
fn greet(name: &str) -> String {
    format!("Hello, {}! You've been greeted from Rust!", name)
}

#[tauri::command]
async fn start_daemon(
    _app: tauri::AppHandle,
    state: tauri::State<'_, AppState>,
) -> Result<String, String> {
    if let Ok(client) = RestClient::discover().await {
        if client.is_reachable().await {
            *state.daemon_pid.lock().unwrap() = None;
            *state.daemon_running.lock().unwrap() = true;
            return Ok("Daemon already running".to_string());
        }
    }

    // Use installed nupid from ~/.nupi/bin/ instead of sidecar
    // This way the daemon uses the same binaries as CLI
    let home = env::var("HOME").map_err(|_| "HOME not set".to_string())?;
    let nupid_path = std::path::PathBuf::from(&home).join(".nupi/bin/nupid");

    if !nupid_path.exists() {
        return Err(format!(
            "nupid not found at {}. Please install binaries first.",
            nupid_path.display()
        ));
    }

    let mut std_command = std::process::Command::new(nupid_path);
    std_command.stdin(Stdio::null());
    std_command.stdout(Stdio::null());
    std_command.stderr(Stdio::null());

    match std_command.spawn() {
        Ok(child) => {
            *state.daemon_pid.lock().unwrap() = Some(child.id());
            *state.daemon_running.lock().unwrap() = true;
            // Drop the child handle to allow the daemon to outlive the app.
            Ok("Daemon started".to_string())
        }
        Err(e) => Err(format!("Failed to start daemon: {}", e)),
    }
}

#[tauri::command]
async fn stop_daemon(
    app: tauri::AppHandle,
    state: tauri::State<'_, AppState>,
) -> Result<String, String> {
    if let Ok(client) = RestClient::discover().await {
        match client.shutdown_daemon().await {
            Ok(()) => {
                *state.daemon_pid.lock().unwrap() = None;
                *state.daemon_running.lock().unwrap() = false;
                let payload = json!({
                    "success": true,
                    "message": "Shutdown request sent to daemon",
                    "method": "api"
                });
                return Ok(payload.to_string());
            }
            Err(RestClientError::Status(code))
                if code == reqwest::StatusCode::NOT_IMPLEMENTED
                    || code == reqwest::StatusCode::NOT_FOUND =>
            {
                // Fallback handled below
            }
            Err(RestClientError::Status(reqwest::StatusCode::UNAUTHORIZED)) => {
                return Err("Daemon rejected shutdown request (unauthorized)".into());
            }
            Err(err) => {
                println!("[nupi-desktop] API shutdown failed, falling back: {err}");
            }
        }
    }

    let shell = app.shell();

    match shell
        .sidecar("nupi")
        .map_err(|e| e.to_string())?
        .args(["daemon", "stop", "--json"])
        .output()
        .await
    {
        Ok(output) => {
            if output.status.success() {
                *state.daemon_pid.lock().unwrap() = None;
                *state.daemon_running.lock().unwrap() = false;

                let stdout = String::from_utf8_lossy(&output.stdout);
                Ok(stdout.to_string())
            } else {
                let stderr = String::from_utf8_lossy(&output.stderr);
                Err(format!("Failed to stop daemon: {}", stderr))
            }
        }
        Err(e) => Err(format!("Failed to execute nupi: {}", e)),
    }
}

#[tauri::command]
async fn get_client_settings() -> Result<ClientSettings, String> {
    Ok(settings::load())
}

#[tauri::command]
async fn set_base_url(base_url: Option<String>) -> Result<ClientSettings, String> {
    let mut settings = settings::load();
    let trimmed = base_url.and_then(|value| {
        let t = value.trim().to_string();
        if t.is_empty() { None } else { Some(t) }
    });
    settings.base_url = trimmed.clone();
    settings::save(&settings).map_err(|e| e.to_string())?;
    match trimmed {
        Some(ref value) => unsafe { env::set_var("NUPI_BASE_URL", value) },
        None => unsafe { env::remove_var("NUPI_BASE_URL") },
    }
    Ok(settings)
}

#[tauri::command]
async fn set_api_token(token: Option<String>) -> Result<ClientSettings, String> {
    let mut settings = settings::load();
    let cleaned = token.and_then(|value| {
        let t = value.trim().to_string();
        if t.is_empty() { None } else { Some(t) }
    });
    settings.api_token = cleaned.clone();
    settings::save(&settings).map_err(|e| e.to_string())?;
    match cleaned {
        Some(ref value) => unsafe { env::set_var("NUPI_API_TOKEN", value) },
        None => unsafe { env::remove_var("NUPI_API_TOKEN") },
    }
    Ok(settings)
}

#[tauri::command]
async fn list_api_tokens() -> Result<Vec<TokenEntry>, String> {
    let client = RestClient::discover().await.map_err(|e| e.to_string())?;
    client.list_tokens().await.map_err(|e| e.to_string())
}

#[tauri::command]
async fn create_api_token(
    name: Option<String>,
    role: Option<String>,
) -> Result<CreatedToken, String> {
    let client = RestClient::discover().await.map_err(|e| e.to_string())?;
    let created = client
        .create_token(name, role)
        .await
        .map_err(|e| e.to_string())?;
    let mut settings = settings::load();
    settings.api_token = Some(created.token.clone());
    settings::save(&settings).map_err(|e| e.to_string())?;
    unsafe { env::set_var("NUPI_API_TOKEN", created.token.as_str()) };
    Ok(created)
}

#[tauri::command]
async fn delete_api_token(id: Option<String>, token: Option<String>) -> Result<(), String> {
    let client = RestClient::discover().await.map_err(|e| e.to_string())?;
    client
        .delete_token(id.clone(), token.clone())
        .await
        .map_err(|e| e.to_string())?;

    if let Some(candidate) = token {
        let mut settings = settings::load();
        if let Some(current) = settings.api_token.clone() {
            if current == candidate {
                settings.api_token = None;
                settings::save(&settings).map_err(|e| e.to_string())?;
                unsafe { env::remove_var("NUPI_API_TOKEN") };
            }
        }
    }
    Ok(())
}

#[tauri::command]
async fn list_pairings() -> Result<Vec<PairingEntry>, String> {
    let client = RestClient::discover().await.map_err(|e| e.to_string())?;
    client.list_pairings().await.map_err(|e| e.to_string())
}

#[tauri::command]
async fn create_pairing(
    name: Option<String>,
    role: Option<String>,
    expires_in_seconds: Option<u32>,
) -> Result<CreatedPairing, String> {
    let client = RestClient::discover().await.map_err(|e| e.to_string())?;
    client
        .create_pairing(name, role, expires_in_seconds)
        .await
        .map_err(|e| e.to_string())
}

#[tauri::command]
async fn claim_pairing(
    base_url: String,
    code: String,
    name: Option<String>,
    insecure: bool,
) -> Result<ClaimedToken, String> {
    let trimmed_base = base_url.trim().to_string();
    let trimmed_code = code.trim().to_string();
    if trimmed_base.is_empty() || trimmed_code.is_empty() {
        return Err("Base URL and pairing code are required".into());
    }

    let claimed = claim_pairing_token(&trimmed_base, &trimmed_code, name, insecure)
        .await
        .map_err(|e| e.to_string())?;

    let mut settings = settings::load();
    settings.base_url = Some(trimmed_base.clone());
    settings.api_token = Some(claimed.token.clone());
    settings::save(&settings).map_err(|e| e.to_string())?;
    unsafe {
        env::set_var("NUPI_BASE_URL", trimmed_base);
        env::set_var("NUPI_API_TOKEN", claimed.token.as_str());
    }

    Ok(claimed)
}

#[tauri::command]
async fn voice_stream_from_file(
    _app: tauri::AppHandle,
    session_id: String,
    stream_id: Option<String>,
    input_path: String,
    playback_output: Option<String>,
    disable_playback: Option<bool>,
    metadata: Option<HashMap<String, String>>,
    operation_id: Option<String>,
) -> Result<Value, String> {
    let trimmed_session = session_id.trim();
    if trimmed_session.is_empty() {
        return Err("session_id is required".into());
    }
    let trimmed_input = input_path.trim();
    if trimmed_input.is_empty() {
        return Err("input_path is required".into());
    }

    let client = RestClient::discover()
        .await
        .map_err(|err| err.to_string())?;

    let request = VoiceStreamRequest {
        session_id: trimmed_session.to_string(),
        stream_id: stream_id
            .as_ref()
            .map(|value| value.trim())
            .filter(|value| !value.is_empty())
            .map(|value| value.to_string()),
        input_path: PathBuf::from(trimmed_input),
        playback_output: playback_output.and_then(|path| {
            let trimmed = path.trim().to_string();
            if trimmed.is_empty() {
                None
            } else {
                Some(PathBuf::from(trimmed))
            }
        }),
        disable_playback: disable_playback.unwrap_or(true),
        metadata: metadata.unwrap_or_default(),
    };
    let mut registered_operation: Option<String> = None;
    let mut cancel_receiver: Option<watch::Receiver<bool>> = None;

    if let Some(op_id) = operation_id {
        let (key, receiver) = register_voice_operation(&op_id)?;
        registered_operation = Some(key);
        cancel_receiver = Some(receiver);
    }

    let result = client
        .voice_stream_from_file(request, cancel_receiver)
        .await;

    if let Some(op) = registered_operation {
        let cancelled = matches!(
            result.as_ref(),
            Err(RestClientError::Audio(msg)) if msg.trim().eq_ignore_ascii_case(CANCELLED_MESSAGE)
        );
        if !cancelled {
            complete_voice_operation(&op);
        }
    }

    result
        .map_err(|err| err.to_string())
        .and_then(|summary| to_value(summary).map_err(|err| err.to_string()))
}

#[tauri::command]
async fn voice_cancel_stream(_app: tauri::AppHandle, operation_id: String) -> Result<bool, String> {
    cancel_voice_operation(&operation_id)
}

#[tauri::command]
async fn voice_interrupt_command(
    _app: tauri::AppHandle,
    session_id: String,
    stream_id: Option<String>,
    reason: Option<String>,
    metadata: Option<HashMap<String, String>>,
) -> Result<Value, String> {
    let trimmed_session = session_id.trim();
    if trimmed_session.is_empty() {
        return Err("session_id is required".into());
    }

    let client = RestClient::discover()
        .await
        .map_err(|err| err.to_string())?;

    client
        .voice_interrupt(
            trimmed_session,
            stream_id
                .as_ref()
                .map(|value| value.trim())
                .filter(|value| !value.is_empty()),
            reason
                .as_ref()
                .map(|value| value.trim())
                .filter(|value| !value.is_empty()),
            metadata.unwrap_or_default(),
        )
        .await
        .map_err(|err| err.to_string())
        .and_then(|summary| to_value(summary).map_err(|err| err.to_string()))
}

#[tauri::command]
async fn voice_status_command(
    _app: tauri::AppHandle,
    session_id: Option<String>,
) -> Result<Value, String> {
    let client = RestClient::discover()
        .await
        .map_err(|err| err.to_string())?;

    let filter = session_id
        .as_ref()
        .map(|value| value.trim())
        .filter(|value| !value.is_empty());

    client
        .audio_capabilities(filter)
        .await
        .map_err(|err| err.to_string())
        .and_then(|caps| to_value(caps).map_err(|err| err.to_string()))
}

#[tauri::command]
async fn daemon_status(_app: tauri::AppHandle) -> Result<bool, String> {
    match RestClient::discover().await {
        Ok(client) => Ok(client.is_reachable().await),
        Err(err) => {
            println!("[nupi-desktop] daemon_status probe failed: {err}");
            Ok(false)
        }
    }
}

#[tauri::command]
async fn check_binaries_installed(_app: tauri::AppHandle) -> Result<bool, String> {
    Ok(installer::binaries_installed())
}

#[tauri::command]
async fn install_binaries(app: tauri::AppHandle) -> Result<String, String> {
    installer::ensure_binaries_installed(&app).await
}

#[tauri::command]
async fn get_daemon_port(_app: tauri::AppHandle) -> Result<u16, String> {
    let client = RestClient::discover()
        .await
        .map_err(|e| format!("Failed to discover daemon: {}", e))?;
    client
        .daemon_status()
        .await
        .map(|status| status.port as u16)
        .map_err(|e| format!("Failed to fetch daemon status: {}", e))
}

// Helper function to update tray menu with current daemon state
fn update_tray_menu(
    app: &tauri::AppHandle,
    daemon_running: bool,
) -> Result<(), Box<dyn std::error::Error>> {
    update_tray_menu_with_status(
        app,
        if daemon_running {
            "🟢 Nupi is running"
        } else {
            "🟡 Nupi is starting..."
        },
    )
}

// Helper function to update tray menu with custom status text
fn update_tray_menu_with_status(
    app: &tauri::AppHandle,
    status_text: &str,
) -> Result<(), Box<dyn std::error::Error>> {
    if let Some(tray) = app.tray_by_id("main-tray") {
        // Build new menu with updated text - Docker Desktop style
        let daemon_status = MenuItem::with_id(
            app,
            "daemon_status",
            status_text,
            false, // Not clickable, just shows status
            None::<&str>,
        )?;
        let separator1 = PredefinedMenuItem::separator(app)?;
        let show = MenuItem::with_id(app, "show", "Dashboard", true, None::<&str>)?;
        let separator2 = PredefinedMenuItem::separator(app)?;
        let quit = MenuItem::with_id(app, "quit", "Quit Nupi Desktop", true, None::<&str>)?;

        let menu = Menu::new(app)?;
        menu.append(&daemon_status)?;
        menu.append(&separator1)?;
        menu.append(&show)?;
        #[cfg(debug_assertions)]
        {
            let separator_dev = PredefinedMenuItem::separator(app)?;
            menu.append(&separator_dev)?;
            let devtools =
                MenuItem::with_id(app, "devtools", "Open Developer Tools", true, None::<&str>)?;
            menu.append(&devtools)?;
        }
        menu.append(&separator2)?;
        menu.append(&quit)?;

        // Replace the existing menu
        tray.set_menu(Some(menu))?;
    }
    Ok(())
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    let mut app = tauri::Builder::default()
        .plugin(tauri_plugin_opener::init())
        .plugin(tauri_plugin_shell::init())
        .manage(AppState {
            daemon_pid: Mutex::new(None),
            daemon_running: Mutex::new(false),
        })
        .invoke_handler(tauri::generate_handler![
            greet,
            start_daemon,
            stop_daemon,
            daemon_status,
            get_daemon_port,
            check_binaries_installed,
            install_binaries,
            get_client_settings,
            set_base_url,
            set_api_token,
            list_api_tokens,
            create_api_token,
            delete_api_token,
            list_pairings,
            create_pairing,
            claim_pairing,
            voice_stream_from_file,
            voice_cancel_stream,
            voice_interrupt_command,
            voice_status_command
        ])
        .setup(|app| {
            // Always start daemon with the app
            let app_handle = app.app_handle().clone();

            tauri::async_runtime::spawn(async move {
                // Small delay to ensure UI is ready
                std::thread::sleep(std::time::Duration::from_millis(500));

                // Check and install binaries if needed
                if !installer::binaries_installed() {
                    println!(
                        "Binaries not found, installing to ~/.nupi/bin/ and configuring PATH..."
                    );
                    match installer::ensure_binaries_installed(&app_handle).await {
                        Ok(msg) => {
                            println!("✓ {}", msg);
                            println!("✓ PATH configured in shell config files");
                            println!(
                                "  Restart your terminal or run: source ~/.bashrc (or ~/.zshrc)"
                            );
                            let _ = app_handle.emit("binaries-installed", msg);
                        }
                        Err(e) => {
                            eprintln!("Failed to install binaries: {}", e);
                            let _ = app_handle.emit(
                                "installation-error",
                                format!("Failed to install binaries: {}", e),
                            );
                            // Continue anyway - user might have installed manually
                        }
                    }
                } else {
                    println!("✓ Binaries already installed in ~/.nupi/bin/");
                }

                // Check if daemon is already running
                if let Ok(status) = daemon_status(app_handle.clone()).await {
                    if !status {
                        // Start daemon if not running
                        println!("Starting daemon...");
                        match start_daemon(app_handle.clone(), app_handle.state::<AppState>()).await
                        {
                            Ok(_) => {
                                println!("Daemon started successfully");
                                // Update menu to reflect running state
                                update_tray_menu(&app_handle, true).ok();
                                let _ = app_handle.emit("daemon-started", ());
                            }
                            Err(e) => {
                                eprintln!("Failed to start daemon: {}", e);
                                // Show error in UI and menu
                                let _ = app_handle
                                    .emit("daemon-error", format!("Failed to start daemon: {}", e));
                                update_tray_menu_with_status(
                                    &app_handle,
                                    "🔴 Nupi failed to start",
                                )
                                .ok();
                            }
                        }
                    } else {
                        // Update state if daemon is already running
                        let state = app_handle.state::<AppState>();
                        *state.daemon_running.lock().unwrap() = true;
                        println!("Daemon already running");
                        // Update menu to reflect running state
                        update_tray_menu(&app_handle, true).ok();
                        let _ = app_handle.emit("daemon-started", ());
                    }
                } else {
                    eprintln!("Failed to check daemon status");
                    let _ = app_handle.emit("daemon-error", "Failed to check daemon status");
                    update_tray_menu_with_status(&app_handle, "🔴 Nupi failed to start").ok();
                }
            });

            // Load tray icon from file
            let icon_bytes = include_bytes!("../icons/icon.png");
            let tray_icon = Image::from_bytes(icon_bytes).expect("Failed to load tray icon");

            // Create tray icon with proper ID to prevent duplicates
            let tray = TrayIconBuilder::with_id("main-tray")
                .icon(tray_icon)
                .icon_as_template(true)
                .show_menu_on_left_click(true)
                .on_menu_event(|app, event| {
                    match event.id.as_ref() {
                        "show" => {
                            if let Some(window) = app.get_webview_window("main") {
                                // Show dock icon when showing window
                                #[cfg(target_os = "macos")]
                                {
                                    let _ =
                                        app.set_activation_policy(tauri::ActivationPolicy::Regular);
                                }
                                let _ = window.show();
                                let _ = window.set_focus();
                            }
                        }
                        #[cfg(debug_assertions)]
                        "devtools" => {
                            if let Some(window) = app.get_webview_window("main") {
                                window.open_devtools();
                                let _ = window.set_focus();
                            }
                        }
                        "daemon_status" => {
                            // Status item - do nothing when clicked
                        }
                        "quit" => {
                            // Leave the daemon running in the background and just exit the desktop app
                            let app_handle = app.app_handle().clone();
                            tauri::async_runtime::spawn(async move {
                                app_handle.exit(0);
                            });
                        }
                        _ => {}
                    }
                })
                .build(app)?;

            // Build initial tray menu (will be updated after daemon starts)
            let daemon_status = MenuItem::with_id(
                app,
                "daemon_status",
                "⚪ Nupi is starting...",
                false,
                None::<&str>,
            )?;
            let separator1 = PredefinedMenuItem::separator(app)?;
            let show = MenuItem::with_id(app, "show", "Dashboard", true, None::<&str>)?;
            let separator2 = PredefinedMenuItem::separator(app)?;
            let quit = MenuItem::with_id(app, "quit", "Quit Nupi Desktop", true, None::<&str>)?;

            let menu = Menu::new(app)?;
            menu.append(&daemon_status)?;
            menu.append(&separator1)?;
            menu.append(&show)?;
            menu.append(&separator2)?;
            menu.append(&quit)?;

            tray.set_menu(Some(menu))?;
            tray.set_tooltip(Some("Nupi - AI CLI Tool Manager"))?;

            // Prevent showing main window on app launch
            if let Some(window) = app.get_webview_window("main") {
                let _ = window.hide();
            }

            // Ensure we start in accessory mode (no dock icon)
            #[cfg(target_os = "macos")]
            {
                let _ = app.set_activation_policy(tauri::ActivationPolicy::Accessory);
            }

            Ok(())
        })
        .on_window_event(|window, event| {
            // Handle window close to keep app running in tray
            if let tauri::WindowEvent::CloseRequested { api, .. } = event {
                // Prevent window from closing, just hide it
                api.prevent_close();
                let _ = window.hide();
                // Hide dock icon when hiding window
                #[cfg(target_os = "macos")]
                {
                    let _ = window
                        .app_handle()
                        .set_activation_policy(tauri::ActivationPolicy::Accessory);
                }
            }
        })
        .build(tauri::generate_context!())
        .expect("error while building tauri application");

    // Start with accessory mode (no dock icon) until window is shown
    #[cfg(target_os = "macos")]
    let _ = app.set_activation_policy(tauri::ActivationPolicy::Accessory);

    // Now run the application
    app.run(|_app_handle, _event| {})
}
