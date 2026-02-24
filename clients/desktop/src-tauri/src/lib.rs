use grpc_client::{
    ClaimedToken, CreatedPairing, CreatedToken, GrpcClient, GrpcClientError, LanguageEntry,
    PairingEntry, RecordingInfo, SessionInfo, TokenEntry, VoiceStreamRequest, claim_pairing_grpc,
};
use once_cell::sync::Lazy;
use parking_lot::Mutex;
use serde::Serialize;
use serde_json::{Value, json, to_value};
use std::{collections::HashMap, env, path::PathBuf, process::Stdio, time::Instant};
use tauri::{
    Emitter, Manager,
    image::Image,
    menu::{Menu, MenuItem, PredefinedMenuItem},
    tray::TrayIconBuilder,
};
use tauri_plugin_shell::ShellExt;
use tokio::sync::watch;

mod grpc_client;
mod installer;
mod proto;
mod settings;
mod timeouts;

use proto::nupi_api;
use settings::ClientSettings;
use timeouts::{DAEMON_READINESS_RETRY_DELAY, STALE_OPERATION_TIMEOUT};

// ---------------------------------------------------------------------------
// Session stream management
// ---------------------------------------------------------------------------

struct SessionStream {
    sender: tokio::sync::mpsc::Sender<nupi_api::AttachSessionRequest>,
    task_handle: tokio::task::JoinHandle<()>,
}

struct AppState {
    daemon_pid: Mutex<Option<u32>>,
    daemon_running: Mutex<bool>,
    active_streams: tokio::sync::Mutex<HashMap<String, SessionStream>>,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct CommandError {
    code: &'static str,
    message: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    details: Option<Value>,
}

type CommandResult<T> = Result<T, CommandError>;

impl CommandError {
    fn new(code: &'static str, message: impl Into<String>) -> Self {
        Self {
            code,
            message: message.into(),
            details: None,
        }
    }

    fn invalid_argument(message: impl Into<String>) -> Self {
        Self::new("INVALID_ARGUMENT", message)
    }

    fn internal(message: impl Into<String>) -> Self {
        Self::new("INTERNAL", message)
    }
}

impl std::fmt::Display for CommandError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}: {}", self.code, self.message)
    }
}

impl std::error::Error for CommandError {}

impl From<GrpcClientError> for CommandError {
    fn from(value: GrpcClientError) -> Self {
        match value {
            GrpcClientError::MissingHomeDir => {
                Self::new("CONFIG_ERROR", "Unable to determine user home directory")
            }
            GrpcClientError::Config(msg) => Self::new("CONFIG_ERROR", msg),
            GrpcClientError::Io(err) => {
                if err.kind() == std::io::ErrorKind::NotFound {
                    Self::new("NOT_FOUND", err.to_string())
                } else {
                    Self::new("IO_ERROR", err.to_string())
                }
            }
            GrpcClientError::Sql(err) => Self::new("DATABASE_ERROR", err.to_string()),
            GrpcClientError::Grpc(status) => {
                Self::new(grpc_code_to_command_code(status.code()), status.to_string())
            }
            GrpcClientError::Transport(err) => Self::new("UNAVAILABLE", err.to_string()),
            GrpcClientError::Audio(err) => Self::new("AUDIO_ERROR", err),
            GrpcClientError::VoiceNotReady { message } => Self {
                code: "FAILED_PRECONDITION",
                message,
                details: None,
            },
        }
    }
}

fn grpc_code_to_command_code(code: tonic::Code) -> &'static str {
    match code {
        tonic::Code::InvalidArgument => "INVALID_ARGUMENT",
        tonic::Code::NotFound => "NOT_FOUND",
        tonic::Code::AlreadyExists => "CONFLICT",
        tonic::Code::PermissionDenied => "FORBIDDEN",
        tonic::Code::Unauthenticated => "UNAUTHORIZED",
        tonic::Code::FailedPrecondition => "FAILED_PRECONDITION",
        tonic::Code::DeadlineExceeded => "TIMEOUT",
        tonic::Code::Unavailable => "UNAVAILABLE",
        tonic::Code::ResourceExhausted => "RATE_LIMITED",
        _ => "GRPC_ERROR",
    }
}

const CANCELLED_MESSAGE: &str = "Voice stream cancelled by user";
/// Tauri event name for version mismatch between app and daemon.
/// Keep in sync with: clients/desktop/src/App.tsx (listen call).
const EVENT_VERSION_MISMATCH: &str = "version-mismatch";

struct VoiceOperation {
    sender: watch::Sender<bool>,
    started: Instant,
}

static ACTIVE_VOICE_STREAMS: Lazy<Mutex<HashMap<String, VoiceOperation>>> =
    Lazy::new(|| Mutex::new(HashMap::new()));

fn cleanup_stale_operations(registry: &mut HashMap<String, VoiceOperation>) {
    registry.retain(|_, op| op.started.elapsed() < STALE_OPERATION_TIMEOUT);
}

fn register_voice_operation(operation_id: &str) -> CommandResult<(String, watch::Receiver<bool>)> {
    let trimmed = require_not_empty(operation_id, "operation_id")?;

    let mut registry = ACTIVE_VOICE_STREAMS.lock();
    cleanup_stale_operations(&mut registry);
    if registry.contains_key(trimmed) {
        return Err(CommandError::new(
            "CONFLICT",
            "a voice upload is already active for this identifier",
        ));
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
    ACTIVE_VOICE_STREAMS.lock().remove(operation_id);
}

fn cancel_voice_operation(operation_id: &str) -> CommandResult<bool> {
    let trimmed = require_not_empty(operation_id, "operation_id")?;
    let mut registry = ACTIVE_VOICE_STREAMS.lock();
    cleanup_stale_operations(&mut registry);
    if let Some(operation) = registry.remove(trimmed) {
        let _ = operation.sender.send(true);
        Ok(true)
    } else {
        Ok(false)
    }
}

fn stringify_error<E: std::fmt::Display>(err: E) -> CommandError {
    CommandError::internal(err.to_string())
}

async fn discover_client() -> CommandResult<GrpcClient> {
    GrpcClient::discover().await.map_err(CommandError::from)
}

async fn with_client<F, Fut, T>(f: F) -> CommandResult<T>
where
    F: FnOnce(GrpcClient) -> Fut,
    Fut: std::future::Future<Output = Result<T, GrpcClientError>>,
{
    let client = discover_client().await?;
    f(client).await.map_err(CommandError::from)
}

fn require_not_empty<'a>(val: &'a str, field_name: &str) -> CommandResult<&'a str> {
    let trimmed = val.trim();
    if trimmed.is_empty() {
        Err(CommandError::invalid_argument(format!(
            "{field_name} is required"
        )))
    } else {
        Ok(trimmed)
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
) -> CommandResult<String> {
    if let Ok(client) = GrpcClient::discover().await {
        if client.is_reachable().await {
            *state.daemon_pid.lock() = None;
            *state.daemon_running.lock() = true;
            return Ok("Daemon already running".to_string());
        }
    }

    // Use installed nupid from ~/.nupi/bin/ instead of sidecar
    // This way the daemon uses the same binaries as CLI
    let home = env::var("HOME").map_err(|_| CommandError::new("CONFIG_ERROR", "HOME not set"))?;
    let nupid_path = std::path::PathBuf::from(&home).join(".nupi/bin/nupid");

    if !nupid_path.exists() {
        return Err(CommandError::new(
            "NOT_FOUND",
            format!(
                "nupid not found at {}. Please install binaries first.",
                nupid_path.display()
            ),
        ));
    }

    let mut std_command = std::process::Command::new(nupid_path);
    std_command.stdin(Stdio::null());
    std_command.stdout(Stdio::null());
    std_command.stderr(Stdio::null());

    match std_command.spawn() {
        Ok(child) => {
            *state.daemon_pid.lock() = Some(child.id());
            *state.daemon_running.lock() = true;
            // Drop the child handle to allow the daemon to outlive the app.
            Ok("Daemon started".to_string())
        }
        Err(e) => Err(CommandError::internal(format!(
            "Failed to start daemon: {}",
            e
        ))),
    }
}

#[tauri::command]
async fn stop_daemon(
    app: tauri::AppHandle,
    state: tauri::State<'_, AppState>,
) -> CommandResult<String> {
    if let Ok(client) = GrpcClient::discover().await {
        match client.shutdown_daemon().await {
            Ok(()) => {
                *state.daemon_pid.lock() = None;
                *state.daemon_running.lock() = false;
                let payload = json!({
                    "success": true,
                    "message": "Shutdown request sent to daemon",
                    "method": "api"
                });
                return Ok(payload.to_string());
            }
            Err(GrpcClientError::Grpc(ref s))
                if s.code() == tonic::Code::Unimplemented || s.code() == tonic::Code::NotFound =>
            {
                // Fallback handled below
            }
            Err(GrpcClientError::Grpc(ref s)) if s.code() == tonic::Code::Unauthenticated => {
                return Err(CommandError::new(
                    "UNAUTHORIZED",
                    "Daemon rejected shutdown request (unauthorized)",
                ));
            }
            Err(err) => {
                println!("[nupi-desktop] API shutdown failed, falling back: {err}");
            }
        }
    }

    let shell = app.shell();

    match shell
        .sidecar("nupi")
        .map_err(stringify_error)?
        .args(["daemon", "stop", "--json"])
        .output()
        .await
    {
        Ok(output) => {
            if output.status.success() {
                *state.daemon_pid.lock() = None;
                *state.daemon_running.lock() = false;

                let stdout = String::from_utf8_lossy(&output.stdout);
                Ok(stdout.to_string())
            } else {
                let stderr = String::from_utf8_lossy(&output.stderr);
                Err(CommandError::internal(format!(
                    "Failed to stop daemon: {}",
                    stderr
                )))
            }
        }
        Err(e) => Err(CommandError::internal(format!(
            "Failed to execute nupi: {}",
            e
        ))),
    }
}

#[tauri::command]
async fn get_client_settings() -> CommandResult<ClientSettings> {
    Ok(settings::load())
}

#[tauri::command]
async fn set_base_url(base_url: Option<String>) -> CommandResult<ClientSettings> {
    let trimmed = base_url.and_then(|value| {
        let t = value.trim().to_string();
        if t.is_empty() { None } else { Some(t) }
    });
    let val = trimmed.clone();
    let settings = settings::modify(|s| {
        s.base_url = val;
    })
    .map_err(stringify_error)?;
    match trimmed {
        Some(ref value) => unsafe { env::set_var("NUPI_BASE_URL", value) },
        None => unsafe { env::remove_var("NUPI_BASE_URL") },
    }
    Ok(settings)
}

#[tauri::command]
async fn set_api_token(token: Option<String>) -> CommandResult<ClientSettings> {
    let cleaned = token.and_then(|value| {
        let t = value.trim().to_string();
        if t.is_empty() { None } else { Some(t) }
    });
    let val = cleaned.clone();
    let settings = settings::modify(|s| {
        s.api_token = val;
    })
    .map_err(stringify_error)?;
    match cleaned {
        Some(ref value) => unsafe { env::set_var("NUPI_API_TOKEN", value) },
        None => unsafe { env::remove_var("NUPI_API_TOKEN") },
    }
    Ok(settings)
}

#[tauri::command]
async fn get_language() -> CommandResult<Option<String>> {
    Ok(settings::load().language.and_then(|v| {
        let t = v.trim().to_lowercase();
        if t.is_empty() { None } else { Some(t) }
    }))
}

#[tauri::command]
async fn set_language(language: Option<String>) -> CommandResult<ClientSettings> {
    let cleaned = match language {
        Some(v) => {
            let t = v.trim().to_string();
            if t.is_empty() {
                None
            } else if t.chars().count() > 35 {
                return Err(CommandError::invalid_argument(
                    "language value too long (max 35 characters)",
                ));
            } else {
                Some(t.to_lowercase())
            }
        }
        None => None,
    };
    let val = cleaned.clone();
    let settings = settings::modify(|s| {
        s.language = val;
    })
    .map_err(stringify_error)?;
    Ok(settings)
}

#[tauri::command]
async fn list_languages() -> CommandResult<Vec<LanguageEntry>> {
    with_client(|client| async move { client.list_languages().await }).await
}

#[tauri::command]
async fn list_api_tokens() -> CommandResult<Vec<TokenEntry>> {
    with_client(|client| async move { client.list_tokens().await }).await
}

#[tauri::command]
async fn create_api_token(
    name: Option<String>,
    role: Option<String>,
) -> CommandResult<CreatedToken> {
    let created =
        with_client(|client| async move { client.create_token(name, role).await }).await?;
    let token_value = created.token.clone();
    settings::modify(|s| {
        s.api_token = Some(token_value);
    })
    .map_err(stringify_error)?;
    unsafe { env::set_var("NUPI_API_TOKEN", created.token.as_str()) };
    Ok(created)
}

#[tauri::command]
async fn delete_api_token(id: Option<String>, token: Option<String>) -> CommandResult<()> {
    let delete_id = id.clone();
    let delete_token = token.clone();
    with_client(|client| async move { client.delete_token(delete_id, delete_token).await }).await?;

    if let Some(candidate) = token {
        settings::modify(|s| {
            if s.api_token.as_deref() == Some(candidate.as_str()) {
                s.api_token = None;
            }
        })
        .map_err(stringify_error)?;
        // Remove env var if it was cleared
        let current = settings::load();
        if current.api_token.is_none() {
            unsafe { env::remove_var("NUPI_API_TOKEN") };
        }
    }
    Ok(())
}

#[tauri::command]
async fn list_pairings() -> CommandResult<Vec<PairingEntry>> {
    with_client(|client| async move { client.list_pairings().await }).await
}

#[tauri::command]
async fn create_pairing(
    name: Option<String>,
    role: Option<String>,
    expires_in_seconds: Option<u32>,
) -> CommandResult<CreatedPairing> {
    with_client(|client| async move { client.create_pairing(name, role, expires_in_seconds).await })
        .await
}

#[tauri::command]
async fn claim_pairing(
    base_url: String,
    code: String,
    name: Option<String>,
    insecure: bool,
) -> CommandResult<ClaimedToken> {
    let trimmed_base = base_url.trim().to_string();
    let trimmed_code = code.trim().to_string();
    if trimmed_base.is_empty() || trimmed_code.is_empty() {
        return Err(CommandError::invalid_argument(
            "Base URL and pairing code are required",
        ));
    }

    let claimed = claim_pairing_grpc(&trimmed_base, &trimmed_code, name, insecure)
        .await
        .map_err(CommandError::from)?;

    let url_val = trimmed_base.clone();
    let token_val = claimed.token.clone();
    settings::modify(|s| {
        s.base_url = Some(url_val);
        s.api_token = Some(token_val);
    })
    .map_err(stringify_error)?;
    unsafe {
        env::set_var("NUPI_BASE_URL", trimmed_base);
        env::set_var("NUPI_API_TOKEN", claimed.token.as_str());
    }

    Ok(claimed)
}

// ---------------------------------------------------------------------------
// Session streaming commands (Task 1: AC #2)
// ---------------------------------------------------------------------------

fn session_event_type_to_wire(event_type: nupi_api::SessionEventType) -> &'static str {
    match event_type {
        nupi_api::SessionEventType::Unspecified => "unknown",
        nupi_api::SessionEventType::Created => "session_created",
        nupi_api::SessionEventType::Killed => "session_killed",
        nupi_api::SessionEventType::StatusChanged => "session_status_changed",
        nupi_api::SessionEventType::ModeChanged => "session_mode_changed",
        nupi_api::SessionEventType::ToolDetected => "tool_detected",
        nupi_api::SessionEventType::ResizeInstruction => "resize_instruction",
    }
}

fn forward_session_event(app: &tauri::AppHandle, session_id: &str, event: nupi_api::SessionEvent) {
    let event_type = session_event_type_to_wire(event.r#type());

    let payload = json!({
        "event_type": event_type,
        "session_id": event.session_id,
        "data": event.data,
    });

    // Emit session-specific event (for the attached terminal)
    app.emit(&format!("session_event:{session_id}"), &payload)
        .ok();
    // Emit global event (for session list updates)
    app.emit("session_event", &payload).ok();
}

#[tauri::command]
async fn attach_session(
    app: tauri::AppHandle,
    state: tauri::State<'_, AppState>,
    session_id: String,
) -> CommandResult<()> {
    let trimmed = require_not_empty(&session_id, "session_id")?.to_string();

    // If already attached, detach first
    {
        let mut streams = state.active_streams.lock().await;
        if let Some(old) = streams.remove(&trimmed) {
            let _ = old
                .sender
                .send(nupi_api::AttachSessionRequest {
                    payload: Some(nupi_api::attach_session_request::Payload::Control(
                        "detach".to_string(),
                    )),
                })
                .await;
            old.task_handle.abort();
        }
    }

    let client = discover_client().await?;

    let (tx, rx) = tokio::sync::mpsc::channel::<nupi_api::AttachSessionRequest>(64);

    // Buffer init message before opening the stream
    tx.send(nupi_api::AttachSessionRequest {
        payload: Some(nupi_api::attach_session_request::Payload::Init(
            nupi_api::AttachInit {
                session_id: trimmed.clone(),
                include_history: true,
            },
        )),
    })
    .await
    .map_err(stringify_error)?;

    // Open bidirectional gRPC stream
    let stream = tokio_stream::wrappers::ReceiverStream::new(rx);
    let request = client.auth_request(stream);

    let mut sessions_client =
        nupi_api::sessions_service_client::SessionsServiceClient::new(client.channel());
    let response = sessions_client
        .attach_session(request)
        .await
        .map_err(stringify_error)?;
    let mut resp_stream = response.into_inner();

    // Spawn background task to forward output/events to frontend
    let sid = trimmed.clone();
    let app_clone = app.clone();

    let task_handle = tokio::spawn(async move {
        while let Ok(Some(msg)) = resp_stream.message().await {
            match msg.payload {
                Some(nupi_api::attach_session_response::Payload::Output(bytes)) => {
                    let text = String::from_utf8_lossy(&bytes);
                    app_clone
                        .emit(&format!("session_output:{sid}"), text.as_ref())
                        .ok();
                }
                Some(nupi_api::attach_session_response::Payload::Event(event)) => {
                    forward_session_event(&app_clone, &sid, event);
                }
                None => {}
            }
        }

        // Stream ended — notify frontend and clean up
        app_clone
            .emit(&format!("session_disconnected:{sid}"), ())
            .ok();
        let state = app_clone.state::<AppState>();
        state.active_streams.lock().await.remove(&sid);
    });

    state.active_streams.lock().await.insert(
        trimmed,
        SessionStream {
            sender: tx,
            task_handle,
        },
    );

    Ok(())
}

#[tauri::command]
async fn send_input(
    state: tauri::State<'_, AppState>,
    session_id: String,
    input: Vec<u8>,
) -> CommandResult<()> {
    let trimmed = require_not_empty(&session_id, "session_id")?.to_string();
    if input.len() > 64 * 1024 {
        return Err(CommandError::invalid_argument("input exceeds 64 KiB limit"));
    }

    let streams = state.active_streams.lock().await;
    let stream = streams
        .get(&trimmed)
        .ok_or_else(|| CommandError::new("NOT_FOUND", "Not attached to this session"))?;

    stream
        .sender
        .send(nupi_api::AttachSessionRequest {
            payload: Some(nupi_api::attach_session_request::Payload::Input(input)),
        })
        .await
        .map_err(stringify_error)?;

    Ok(())
}

#[tauri::command]
async fn resize_session(
    state: tauri::State<'_, AppState>,
    session_id: String,
    cols: u32,
    rows: u32,
) -> CommandResult<()> {
    let trimmed = require_not_empty(&session_id, "session_id")?.to_string();
    if cols == 0 || cols > 500 || rows == 0 || rows > 500 {
        return Err(CommandError::invalid_argument(
            "cols and rows must be between 1 and 500",
        ));
    }

    let streams = state.active_streams.lock().await;
    let stream = streams
        .get(&trimmed)
        .ok_or_else(|| CommandError::new("NOT_FOUND", "Not attached to this session"))?;

    stream
        .sender
        .send(nupi_api::AttachSessionRequest {
            payload: Some(nupi_api::attach_session_request::Payload::Resize(
                nupi_api::ResizeRequest {
                    cols,
                    rows,
                    source: "desktop".to_string(),
                    metadata: HashMap::new(),
                },
            )),
        })
        .await
        .map_err(stringify_error)?;

    Ok(())
}

#[tauri::command]
async fn detach_session(
    state: tauri::State<'_, AppState>,
    session_id: String,
) -> CommandResult<()> {
    let trimmed = require_not_empty(&session_id, "session_id")?.to_string();

    let mut streams = state.active_streams.lock().await;
    if let Some(stream) = streams.remove(&trimmed) {
        // Send detach control message, then drop the sender
        let _ = stream
            .sender
            .send(nupi_api::AttachSessionRequest {
                payload: Some(nupi_api::attach_session_request::Payload::Control(
                    "detach".to_string(),
                )),
            })
            .await;
        stream.task_handle.abort();
    }

    Ok(())
}

// ---------------------------------------------------------------------------
// Session management commands (Task 2: AC #6)
// ---------------------------------------------------------------------------

#[tauri::command]
async fn list_sessions() -> CommandResult<Vec<SessionInfo>> {
    with_client(|client| async move { client.list_sessions().await }).await
}

#[tauri::command]
async fn get_session(session_id: String) -> CommandResult<SessionInfo> {
    let trimmed = require_not_empty(&session_id, "session_id")?;
    with_client(|client| async move { client.get_session(trimmed).await }).await
}

#[tauri::command]
async fn create_session(
    command: String,
    args: Option<Vec<String>>,
    cols: Option<u32>,
    rows: Option<u32>,
    work_dir: Option<String>,
    detached: Option<bool>,
) -> CommandResult<SessionInfo> {
    let trimmed = require_not_empty(&command, "command")?;
    with_client(|client| async move {
        client
            .create_session(
                trimmed,
                args.unwrap_or_default(),
                cols.unwrap_or(80),
                rows.unwrap_or(24),
                work_dir,
                Vec::new(),
                detached.unwrap_or(false),
                false,
            )
            .await
    })
    .await
}

#[tauri::command]
async fn kill_session(session_id: String) -> CommandResult<()> {
    let trimmed = require_not_empty(&session_id, "session_id")?;
    with_client(|client| async move { client.kill_session(trimmed).await }).await
}

// ---------------------------------------------------------------------------
// Recording commands
// ---------------------------------------------------------------------------

#[tauri::command]
async fn list_recordings(session_id: Option<String>) -> CommandResult<Vec<RecordingInfo>> {
    let filter = session_id
        .as_ref()
        .map(|v| v.trim())
        .filter(|v| !v.is_empty());
    with_client(|client| async move { client.list_recordings(filter).await }).await
}

#[tauri::command]
async fn get_recording(session_id: String) -> CommandResult<String> {
    let trimmed = require_not_empty(&session_id, "session_id")?;
    with_client(|client| async move { client.get_recording(trimmed).await }).await
}

// ---------------------------------------------------------------------------
// Voice / audio commands
// ---------------------------------------------------------------------------

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
) -> CommandResult<Value> {
    let trimmed_session = require_not_empty(&session_id, "session_id")?;
    let trimmed_input = require_not_empty(&input_path, "input_path")?;

    let client = discover_client().await?;

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
            Err(GrpcClientError::Audio(msg)) if msg.trim().eq_ignore_ascii_case(CANCELLED_MESSAGE)
        );
        if !cancelled {
            complete_voice_operation(&op);
        }
    }

    result
        .map_err(CommandError::from)
        .and_then(|summary| to_value(summary).map_err(stringify_error))
}

#[tauri::command]
async fn voice_cancel_stream(_app: tauri::AppHandle, operation_id: String) -> CommandResult<bool> {
    cancel_voice_operation(&operation_id)
}

#[tauri::command]
async fn voice_interrupt_command(
    _app: tauri::AppHandle,
    session_id: String,
    stream_id: Option<String>,
    reason: Option<String>,
    metadata: Option<HashMap<String, String>>,
) -> CommandResult<Value> {
    let trimmed_session = require_not_empty(&session_id, "session_id")?;

    let summary = with_client(|client| async move {
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
    })
    .await?;

    to_value(summary).map_err(stringify_error)
}

#[tauri::command]
async fn voice_status_command(
    _app: tauri::AppHandle,
    session_id: Option<String>,
) -> CommandResult<Value> {
    let filter = session_id
        .as_ref()
        .map(|value| value.trim())
        .filter(|value| !value.is_empty());

    let caps = with_client(|client| async move { client.audio_capabilities(filter).await }).await?;
    to_value(caps).map_err(stringify_error)
}

#[tauri::command]
async fn daemon_status(_app: tauri::AppHandle) -> CommandResult<bool> {
    match GrpcClient::discover().await {
        Ok(client) => Ok(client.is_reachable().await),
        Err(err) => {
            println!("[nupi-desktop] daemon_status probe failed: {err}");
            Ok(false)
        }
    }
}

#[tauri::command]
async fn check_binaries_installed(_app: tauri::AppHandle) -> CommandResult<bool> {
    Ok(installer::binaries_installed())
}

#[tauri::command]
async fn install_binaries(app: tauri::AppHandle) -> CommandResult<String> {
    installer::ensure_binaries_installed(&app)
        .await
        .map_err(stringify_error)
}

/// Strips the `v` prefix and any trailing git-describe suffix (`-N-gHASH`)
/// so that versions like "v0.3.0-5-gabcdef" and "0.3.0" compare as equal.
///
/// Assumes input is ASCII (version strings contain only digits, dots, hyphens,
/// and lowercase hex letters). Non-ASCII input is safe (no panic) but may
/// produce incorrect results.
fn normalize_version(v: &str) -> &str {
    let v = v.strip_prefix('v').unwrap_or(v);
    // Strip git-describe suffix: "-<digits>-g<hex>" at end of string.
    if let Some(g_pos) = v.rfind("-g") {
        let hash = &v[g_pos + 2..];
        if !hash.is_empty() && hash.chars().all(|c| c.is_ascii_hexdigit()) {
            if let Some(n_pos) = v[..g_pos].rfind('-') {
                let num = &v[n_pos + 1..g_pos];
                if !num.is_empty() && num.chars().all(|c| c.is_ascii_digit()) {
                    return &v[..n_pos];
                }
            }
        }
    }
    v
}

/// Returns true if app and daemon versions differ after normalization.
/// Returns false when either version is empty or "dev".
fn versions_mismatch(app_ver: &str, daemon_ver: &str) -> bool {
    if app_ver.is_empty() || daemon_ver.is_empty() {
        return false;
    }
    if app_ver == "dev" || daemon_ver == "dev" {
        return false;
    }
    // 0.0.0 is the Cargo fallback when no git tags exist (see Makefile CARGO_SEMVER).
    if app_ver == "0.0.0" || daemon_ver == "0.0.0" {
        return false;
    }
    normalize_version(app_ver) != normalize_version(daemon_ver)
}

/// Emit a `version-mismatch` event if the app and daemon versions differ.
/// Skips the check when either side reports "dev" (development builds).
fn emit_version_mismatch_if_needed(handle: &tauri::AppHandle, daemon_version: &Option<String>) {
    let app_ver = grpc_client::APP_VERSION;
    if let Some(dv) = daemon_version {
        if versions_mismatch(app_ver, dv) {
            eprintln!("[nupi-desktop] Version mismatch: app v{app_ver}, daemon v{dv}");
            let _ = handle.emit(
                EVENT_VERSION_MISMATCH,
                json!({
                    "app_version": app_ver,
                    "daemon_version": dv,
                }),
            );
        }
    }
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
        .plugin(tauri_plugin_dialog::init())
        .plugin(tauri_plugin_shell::init())
        .manage(AppState {
            daemon_pid: Mutex::new(None),
            daemon_running: Mutex::new(false),
            active_streams: tokio::sync::Mutex::new(HashMap::new()),
        })
        .invoke_handler(tauri::generate_handler![
            greet,
            start_daemon,
            stop_daemon,
            daemon_status,
            check_binaries_installed,
            install_binaries,
            get_client_settings,
            set_base_url,
            set_api_token,
            get_language,
            set_language,
            list_languages,
            list_api_tokens,
            create_api_token,
            delete_api_token,
            list_pairings,
            create_pairing,
            claim_pairing,
            // Session streaming (Task 1)
            attach_session,
            send_input,
            resize_session,
            detach_session,
            // Session management (Task 2)
            list_sessions,
            get_session,
            create_session,
            kill_session,
            // Recordings
            list_recordings,
            get_recording,
            // Voice / audio
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
                tokio::time::sleep(DAEMON_READINESS_RETRY_DELAY).await;

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

                // Probe daemon with a single client discovery + RPC
                let (is_running, daemon_version) = match GrpcClient::discover().await {
                    Ok(client) => match client.daemon_status().await {
                        Ok(status) => (true, status.version),
                        Err(GrpcClientError::Grpc(ref s))
                            if s.code() == tonic::Code::Unauthenticated =>
                        {
                            // FIXME: version mismatch toast (AC #5) cannot fire here
                            // because the daemon requires auth and we have no token yet.
                            // A post-authentication version check would be needed.
                            (true, None)
                        }
                        Err(_) => (false, None),
                    },
                    Err(err) => {
                        println!("[nupi-desktop] daemon probe failed: {err}");
                        (false, None)
                    }
                };

                if is_running {
                    let state = app_handle.state::<AppState>();
                    *state.daemon_running.lock() = true;
                    println!("Daemon already running");
                    update_tray_menu(&app_handle, true).ok();
                    let _ = app_handle.emit("daemon-started", ());

                    // Version check — reuse the version from the single RPC above
                    emit_version_mismatch_if_needed(&app_handle, &daemon_version);
                } else {
                    // Start daemon if not running
                    println!("Starting daemon...");
                    match start_daemon(app_handle.clone(), app_handle.state::<AppState>()).await
                    {
                        Ok(_) => {
                            println!("Daemon started successfully");
                            update_tray_menu(&app_handle, true).ok();
                            let _ = app_handle.emit("daemon-started", ());

                            // Post-start version check: wait for daemon readiness, then verify version
                            let ah = app_handle.clone();
                            tokio::spawn(async move {
                                let mut checked = false;
                                for _ in 0..10 {
                                    tokio::time::sleep(DAEMON_READINESS_RETRY_DELAY).await;
                                    if let Ok(client) = GrpcClient::discover().await {
                                        if let Ok(status) = client.daemon_status().await {
                                            emit_version_mismatch_if_needed(&ah, &status.version);
                                            checked = true;
                                            break;
                                        }
                                    }
                                }
                                if !checked {
                                    eprintln!("[nupi-desktop] Post-start version check failed after 10 retries");
                                }
                            });
                        }
                        Err(e) => {
                            eprintln!("Failed to start daemon: {}", e);
                            let _ = app_handle
                                .emit("daemon-error", format!("Failed to start daemon: {}", e));
                            update_tray_menu_with_status(
                                &app_handle,
                                "🔴 Nupi failed to start",
                            )
                            .ok();
                        }
                    }
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_versions_mismatch_same() {
        assert!(!versions_mismatch("0.3.0", "0.3.0"));
    }

    #[test]
    fn test_versions_mismatch_v_prefix_normalized() {
        assert!(!versions_mismatch("v0.3.0", "0.3.0"));
        assert!(!versions_mismatch("0.3.0", "v0.3.0"));
        assert!(!versions_mismatch("v0.3.0", "v0.3.0"));
    }

    #[test]
    fn test_versions_mismatch_dev_skipped() {
        assert!(!versions_mismatch("dev", "0.3.0"));
        assert!(!versions_mismatch("0.3.0", "dev"));
        assert!(!versions_mismatch("dev", "dev"));
    }

    #[test]
    fn test_versions_mismatch_different() {
        assert!(versions_mismatch("0.3.0", "0.2.0"));
        assert!(versions_mismatch("v0.3.0", "v0.2.0"));
    }

    #[test]
    fn test_versions_mismatch_empty_skipped() {
        assert!(!versions_mismatch("0.3.0", ""));
        assert!(!versions_mismatch("", "0.3.0"));
        assert!(!versions_mismatch("", ""));
    }

    #[test]
    fn test_versions_mismatch_zero_sentinel_skipped() {
        // 0.0.0 is the CARGO_SEMVER fallback when no git tags exist
        assert!(!versions_mismatch("0.0.0", "0.3.0"));
        assert!(!versions_mismatch("0.3.0", "0.0.0"));
        assert!(!versions_mismatch("0.0.0", "abcdef1"));
    }

    #[test]
    fn test_versions_mismatch_git_describe_suffix() {
        // Same base version with git-describe suffix → match
        assert!(!versions_mismatch("0.3.0-5-gabcdef", "0.3.0"));
        assert!(!versions_mismatch("0.3.0", "v0.3.0-10-g1234567"));
        assert!(!versions_mismatch("0.3.0-5-gabcdef", "0.3.0-10-g1234567"));
        // Different base version with suffix → mismatch
        assert!(versions_mismatch("0.3.0-5-gabcdef", "0.2.0"));
    }

    #[test]
    fn test_normalize_version() {
        assert_eq!(normalize_version("v0.3.0"), "0.3.0");
        assert_eq!(normalize_version("0.3.0-5-gabcdef"), "0.3.0");
        assert_eq!(normalize_version("v0.3.0-10-g1234567"), "0.3.0");
        assert_eq!(normalize_version("0.3.0"), "0.3.0");
        assert_eq!(normalize_version("dev"), "dev");
        // semver pre-release should NOT be stripped
        assert_eq!(normalize_version("0.3.0-rc1"), "0.3.0-rc1");
        // semver pre-release + git-describe suffix — strip suffix, keep pre-release
        assert_eq!(normalize_version("0.3.0-beta-5-gabcdef"), "0.3.0-beta");
    }

    #[test]
    fn test_session_event_type_to_wire_mapping() {
        assert_eq!(
            session_event_type_to_wire(nupi_api::SessionEventType::Created),
            "session_created"
        );
        assert_eq!(
            session_event_type_to_wire(nupi_api::SessionEventType::Killed),
            "session_killed"
        );
        assert_eq!(
            session_event_type_to_wire(nupi_api::SessionEventType::StatusChanged),
            "session_status_changed"
        );
        assert_eq!(
            session_event_type_to_wire(nupi_api::SessionEventType::ModeChanged),
            "session_mode_changed"
        );
        assert_eq!(
            session_event_type_to_wire(nupi_api::SessionEventType::ToolDetected),
            "tool_detected"
        );
        assert_eq!(
            session_event_type_to_wire(nupi_api::SessionEventType::ResizeInstruction),
            "resize_instruction"
        );
    }

    #[test]
    fn test_session_event_type_to_wire_unspecified() {
        assert_eq!(
            session_event_type_to_wire(nupi_api::SessionEventType::Unspecified),
            "unknown"
        );
    }
}
