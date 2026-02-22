// Suppress result_large_err: GrpcClientError contains tonic::Status which is large
// but boxing it would make the API less ergonomic with no real benefit.
#![allow(clippy::result_large_err)]

use std::{
    collections::HashMap,
    env, fs,
    path::PathBuf,
    sync::{
        Arc, Once,
        atomic::{AtomicBool, AtomicU64, Ordering},
    },
    time::Duration,
};

static TLS_INSECURE_WARN: Once = Once::new();

use dirs::home_dir;
use hound::{SampleFormat, WavSpec, WavWriter};
use rusqlite::{Connection, OpenFlags, params};
use serde::{Deserialize, Serialize};
use tokio::sync::watch;
use tonic::transport::{Channel, ClientTlsConfig, Endpoint};

use crate::proto::nupi_api;
use crate::settings;

const INSTANCE_NAME: &str = "default";
const PROFILE_NAME: &str = "default";
const MAX_AUDIO_FILE_SIZE: u64 = 500 * 1024 * 1024;

/// App version injected at compile time from Cargo.toml.
pub const APP_VERSION: &str = env!("CARGO_PKG_VERSION");
const DEFAULT_CAPTURE_STREAM_ID: &str = "mic";
const DEFAULT_PLAYBACK_STREAM_ID: &str = "tts";
const SLOT_STT: &str = "stt";
const SLOT_TTS: &str = "tts";
const CANCELLED_MESSAGE: &str = "Voice stream cancelled by user";

const MAX_METADATA_ENTRIES: usize = 32;
const MAX_METADATA_KEY_CHARS: usize = 64;
const MAX_METADATA_VALUE_CHARS: usize = 512;
const MAX_METADATA_TOTAL_BYTES: usize = 4096;

const AUDIO_CHUNK_SIZE: usize = 4096;

// ---------------------------------------------------------------------------
// Error
// ---------------------------------------------------------------------------

#[derive(Debug)]
pub enum GrpcClientError {
    MissingHomeDir,
    Config(String),
    Io(std::io::Error),
    Sql(rusqlite::Error),
    Grpc(tonic::Status),
    Transport(tonic::transport::Error),
    Audio(String),
    VoiceNotReady {
        message: String,
        diagnostics: Vec<VoiceDiagnosticSummary>,
    },
}

impl std::fmt::Display for GrpcClientError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            GrpcClientError::MissingHomeDir => {
                write!(f, "Unable to determine user home directory")
            }
            GrpcClientError::Config(msg) => write!(f, "{msg}"),
            GrpcClientError::Io(err) => write!(f, "{err}"),
            GrpcClientError::Sql(err) => write!(f, "{err}"),
            GrpcClientError::Grpc(status) => write!(f, "gRPC error: {status}"),
            GrpcClientError::Transport(err) => write!(f, "gRPC transport error: {err}"),
            GrpcClientError::Audio(err) => write!(f, "{err}"),
            GrpcClientError::VoiceNotReady {
                message,
                diagnostics,
            } => {
                if diagnostics.is_empty() {
                    write!(f, "{message}")
                } else {
                    let joined = diagnostics
                        .iter()
                        .map(|diag| format!("[{}] {}", diag.slot, diag.message))
                        .collect::<Vec<_>>()
                        .join("; ");
                    write!(f, "{message} ({joined})")
                }
            }
        }
    }
}

impl std::error::Error for GrpcClientError {}

impl From<std::io::Error> for GrpcClientError {
    fn from(value: std::io::Error) -> Self {
        GrpcClientError::Io(value)
    }
}

impl From<rusqlite::Error> for GrpcClientError {
    fn from(value: rusqlite::Error) -> Self {
        GrpcClientError::Sql(value)
    }
}

impl From<tonic::Status> for GrpcClientError {
    fn from(value: tonic::Status) -> Self {
        GrpcClientError::Grpc(value)
    }
}

impl From<tonic::transport::Error> for GrpcClientError {
    fn from(value: tonic::transport::Error) -> Self {
        GrpcClientError::Transport(value)
    }
}

impl From<hound::Error> for GrpcClientError {
    fn from(value: hound::Error) -> Self {
        GrpcClientError::Audio(value.to_string())
    }
}

// ---------------------------------------------------------------------------
// DTO types (unchanged public surface for the Tauri frontend)
// ---------------------------------------------------------------------------

#[allow(dead_code)]
#[derive(Debug)]
pub struct DaemonStatus {
    pub version: Option<String>,
    pub sessions_count: Option<u64>,
    pub port: u16,
    pub grpc_port: Option<u16>,
    pub binding: Option<String>,
    pub grpc_binding: Option<String>,
    pub auth_required: Option<bool>,
}

impl From<nupi_api::DaemonStatusResponse> for DaemonStatus {
    fn from(r: nupi_api::DaemonStatusResponse) -> Self {
        Self {
            version: if r.version.is_empty() {
                None
            } else {
                Some(r.version)
            },
            sessions_count: Some(r.sessions_count as u64),
            port: r.port as u16,
            grpc_port: if r.grpc_port == 0 {
                None
            } else {
                Some(r.grpc_port as u16)
            },
            binding: if r.binding.is_empty() {
                None
            } else {
                Some(r.binding)
            },
            grpc_binding: if r.grpc_binding.is_empty() {
                None
            } else {
                Some(r.grpc_binding)
            },
            auth_required: Some(r.auth_required),
        }
    }
}

#[derive(Debug, Clone, Serialize)]
pub struct TokenEntry {
    pub id: String,
    pub name: Option<String>,
    pub role: String,
    pub masked_token: Option<String>,
    pub created_at: Option<String>,
}

impl From<nupi_api::TokenInfo> for TokenEntry {
    fn from(t: nupi_api::TokenInfo) -> Self {
        Self {
            id: t.id,
            name: if t.name.is_empty() {
                None
            } else {
                Some(t.name)
            },
            role: t.role,
            masked_token: if t.masked_value.is_empty() {
                None
            } else {
                Some(t.masked_value)
            },
            created_at: t.created_at.map(format_timestamp),
        }
    }
}

#[derive(Debug, Clone, Serialize)]
pub struct LanguageEntry {
    pub iso1: String,
    pub bcp47: String,
    pub english_name: String,
    pub native_name: String,
}

impl From<nupi_api::LanguageInfo> for LanguageEntry {
    fn from(l: nupi_api::LanguageInfo) -> Self {
        Self {
            iso1: l.iso1,
            bcp47: l.bcp47,
            english_name: l.english_name,
            native_name: l.native_name,
        }
    }
}

#[derive(Debug, Clone, Serialize)]
pub struct CreatedToken {
    pub token: String,
    pub id: String,
    pub name: Option<String>,
    pub role: String,
}

#[derive(Debug, Clone, Serialize)]
pub struct PairingEntry {
    pub code: String,
    pub name: Option<String>,
    pub role: String,
    pub expires_at: Option<String>,
}

impl From<nupi_api::PairingInfo> for PairingEntry {
    fn from(p: nupi_api::PairingInfo) -> Self {
        Self {
            code: p.code,
            name: if p.name.is_empty() {
                None
            } else {
                Some(p.name)
            },
            role: p.role,
            expires_at: p.expires_at.map(format_timestamp),
        }
    }
}

#[derive(Debug, Clone, Serialize)]
pub struct CreatedPairing {
    pub pair_code: String,
    pub name: Option<String>,
    pub role: String,
    pub expires_at: Option<String>,
}

impl From<nupi_api::CreatePairingResponse> for CreatedPairing {
    fn from(r: nupi_api::CreatePairingResponse) -> Self {
        Self {
            pair_code: r.code,
            name: if r.name.is_empty() {
                None
            } else {
                Some(r.name)
            },
            role: r.role,
            expires_at: r.expires_at.map(format_timestamp),
        }
    }
}

#[derive(Debug, Clone, Serialize)]
pub struct ClaimedToken {
    pub token: String,
    pub name: Option<String>,
    pub role: String,
    pub created_at: Option<String>,
}

impl From<nupi_api::ClaimPairingResponse> for ClaimedToken {
    fn from(r: nupi_api::ClaimPairingResponse) -> Self {
        Self {
            token: r.token,
            name: if r.name.is_empty() {
                None
            } else {
                Some(r.name)
            },
            role: r.role,
            created_at: r.created_at.map(format_timestamp),
        }
    }
}

#[derive(Debug, Clone)]
pub struct VoiceStreamRequest {
    pub session_id: String,
    pub stream_id: Option<String>,
    pub input_path: PathBuf,
    pub playback_output: Option<PathBuf>,
    pub disable_playback: bool,
    pub metadata: HashMap<String, String>,
}

#[derive(Debug, Serialize)]
pub struct VoiceStreamSummary {
    pub session_id: String,
    pub stream_id: String,
    pub bytes_uploaded: u64,
    pub ingress_source: String,
    pub playback_chunks: u64,
    pub playback_bytes: u64,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub playback_path: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub playback_error: Option<String>,
}

#[derive(Debug, Serialize)]
pub struct VoiceInterruptSummary {
    pub session_id: String,
    pub stream_id: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub reason: Option<String>,
    #[serde(skip_serializing_if = "std::collections::HashMap::is_empty")]
    pub metadata: HashMap<String, String>,
}

#[derive(Debug, Clone, Serialize)]
pub struct AudioFormatSummary {
    pub encoding: String,
    pub sample_rate: u32,
    pub channels: u16,
    pub bit_depth: u16,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub frame_ms: Option<u32>,
}

#[derive(Debug, Clone, Serialize)]
pub struct AudioCapabilitySummary {
    pub stream_id: String,
    pub format: AudioFormatSummary,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub metadata: Option<HashMap<String, String>>,
}

impl From<nupi_api::AudioCapability> for AudioCapabilitySummary {
    fn from(c: nupi_api::AudioCapability) -> Self {
        let fmt = c.format.unwrap_or_default();
        Self {
            stream_id: c.stream_id,
            format: AudioFormatSummary {
                encoding: if fmt.encoding.is_empty() {
                    "pcm_s16le".to_string()
                } else {
                    fmt.encoding
                },
                sample_rate: fmt.sample_rate,
                channels: fmt.channels as u16,
                bit_depth: fmt.bit_depth as u16,
                frame_ms: if fmt.frame_duration_ms == 0 {
                    None
                } else {
                    Some(fmt.frame_duration_ms)
                },
            },
            metadata: if c.metadata.is_empty() {
                None
            } else {
                Some(c.metadata)
            },
        }
    }
}

#[derive(Debug, Clone, Serialize)]
pub struct VoiceDiagnosticSummary {
    pub slot: String,
    pub code: String,
    pub message: String,
}

#[derive(Debug, Clone, Serialize)]
pub struct AudioCapabilitiesSummary {
    pub capture: Vec<AudioCapabilitySummary>,
    pub playback: Vec<AudioCapabilitySummary>,
    pub capture_enabled: bool,
    pub playback_enabled: bool,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub diagnostics: Vec<VoiceDiagnosticSummary>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub message: Option<String>,
}

// ---------------------------------------------------------------------------
// Session DTO
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, Serialize)]
pub struct SessionInfo {
    pub id: String,
    pub command: String,
    pub args: Vec<String>,
    pub status: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub pid: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub start_time: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub exit_code: Option<i32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub work_dir: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub tool: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub tool_icon: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub tool_icon_data: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub mode: Option<String>,
}

impl From<nupi_api::Session> for SessionInfo {
    fn from(s: nupi_api::Session) -> Self {
        Self {
            id: s.id,
            command: s.command,
            args: s.args,
            status: s.status,
            pid: if s.pid == 0 { None } else { Some(s.pid as u32) },
            start_time: s.start_time.map(format_timestamp),
            exit_code: s.exit_code,
            work_dir: if s.work_dir.is_empty() {
                None
            } else {
                Some(s.work_dir)
            },
            tool: if s.tool.is_empty() {
                None
            } else {
                Some(s.tool)
            },
            tool_icon: if s.tool_icon.is_empty() {
                None
            } else {
                Some(s.tool_icon)
            },
            tool_icon_data: if s.tool_icon_data.is_empty() {
                None
            } else {
                Some(s.tool_icon_data)
            },
            mode: if s.mode.is_empty() {
                None
            } else {
                Some(s.mode)
            },
        }
    }
}

// ---------------------------------------------------------------------------
// Recording DTO
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, Serialize)]
pub struct RecordingInfo {
    pub session_id: String,
    pub filename: String,
    pub command: String,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub args: Vec<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub work_dir: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub start_time: Option<String>,
    pub duration: f64,
    pub rows: u32,
    pub cols: u32,
    pub title: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub tool: Option<String>,
    pub recording_path: String,
}

impl From<nupi_api::Recording> for RecordingInfo {
    fn from(r: nupi_api::Recording) -> Self {
        Self {
            session_id: r.session_id,
            filename: r.filename,
            command: r.command,
            args: r.args,
            work_dir: if r.work_dir.is_empty() {
                None
            } else {
                Some(r.work_dir)
            },
            start_time: r.start_time.map(format_timestamp),
            duration: r.duration_sec,
            rows: r.rows,
            cols: r.cols,
            title: r.title,
            tool: if r.tool.is_empty() {
                None
            } else {
                Some(r.tool)
            },
            recording_path: r.recording_path,
        }
    }
}

// ---------------------------------------------------------------------------
// Timestamp helper
// ---------------------------------------------------------------------------

fn format_timestamp(ts: prost_types::Timestamp) -> String {
    let secs = ts.seconds;
    let nanos = ts.nanos.max(0) as u32;

    // Convert epoch seconds to date/time components.
    const SECS_PER_DAY: i64 = 86400;
    let day_secs = secs.rem_euclid(SECS_PER_DAY);
    let days = (secs - day_secs) / SECS_PER_DAY + 719_468;

    let era = if days >= 0 { days } else { days - 146_096 } / 146_097;
    let doe = (days - era * 146_097) as u32;
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146_096) / 365;
    let y = yoe as i64 + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let m = if mp < 10 { mp + 3 } else { mp - 9 };
    let year = if m <= 2 { y + 1 } else { y };

    let hour = day_secs / 3600;
    let minute = (day_secs % 3600) / 60;
    let second = day_secs % 60;

    // Suppress sub-second part when nanos == 0 for cleaner output.
    if nanos == 0 {
        format!("{year:04}-{m:02}-{d:02}T{hour:02}:{minute:02}:{second:02}Z")
    } else {
        // Trim trailing zeros from the fractional part.
        let frac = format!("{nanos:09}");
        let trimmed = frac.trim_end_matches('0');
        format!("{year:04}-{m:02}-{d:02}T{hour:02}:{minute:02}:{second:02}.{trimmed}Z")
    }
}

// ---------------------------------------------------------------------------
// Metadata validation (mirrored from rest_client.rs)
// ---------------------------------------------------------------------------

fn normalize_metadata_map(
    metadata: HashMap<String, String>,
    client_label: &str,
) -> HashMap<String, String> {
    let mut normalized = HashMap::new();
    for (key, value) in metadata.into_iter() {
        let trimmed_key = key.trim();
        if trimmed_key.is_empty() {
            continue;
        }
        normalized.insert(trimmed_key.to_string(), value.trim().to_string());
    }
    if normalized.len() < MAX_METADATA_ENTRIES {
        normalized
            .entry("client".to_string())
            .or_insert_with(|| client_label.to_string());
    }
    normalized
}

fn validate_metadata_map(metadata: &HashMap<String, String>) -> Result<(), GrpcClientError> {
    if metadata.len() > MAX_METADATA_ENTRIES {
        return Err(GrpcClientError::Config(format!(
            "metadata has {} entries, max {}",
            metadata.len(),
            MAX_METADATA_ENTRIES
        )));
    }

    let mut total_bytes: usize = 0;
    for (key, value) in metadata {
        if key.chars().count() > MAX_METADATA_KEY_CHARS {
            return Err(GrpcClientError::Config(format!(
                "metadata key {key:?} exceeds {MAX_METADATA_KEY_CHARS} characters",
            )));
        }
        if value.chars().count() > MAX_METADATA_VALUE_CHARS {
            return Err(GrpcClientError::Config(format!(
                "metadata value for key {key:?} exceeds {MAX_METADATA_VALUE_CHARS} characters",
            )));
        }
        total_bytes += key.len() + value.len();
    }

    if total_bytes > MAX_METADATA_TOTAL_BYTES {
        return Err(GrpcClientError::Config(format!(
            "metadata total size exceeds {MAX_METADATA_TOTAL_BYTES} bytes",
        )));
    }

    Ok(())
}

fn default_capture_format_summary() -> AudioFormatSummary {
    AudioFormatSummary {
        encoding: "pcm_s16le".to_string(),
        sample_rate: 16_000,
        channels: 1,
        bit_depth: 16,
        frame_ms: Some(20),
    }
}

/// Picks the most relevant diagnostic message for a logical slot.
fn primary_diagnostic_message(
    diags: &[VoiceDiagnosticSummary],
    slot: &str,
    fallback: &str,
) -> String {
    for diag in diags {
        if diag.slot.eq_ignore_ascii_case(slot) {
            return diag.message.clone();
        }
    }
    diags
        .first()
        .map(|diag| diag.message.clone())
        .unwrap_or_else(|| fallback.to_string())
}

// ---------------------------------------------------------------------------
// Transport snapshot from SQLite
// ---------------------------------------------------------------------------

#[derive(Debug, Clone)]
struct TransportSnapshot {
    grpc_port: i32,
    grpc_binding: String,
    tls_cert_path: Option<String>,
    tls_key_path: Option<String>,
    tokens: Vec<String>,
}

#[derive(Debug, Clone, Deserialize)]
#[allow(dead_code)]
struct BootstrapTLS {
    #[serde(default)]
    insecure: Option<bool>,
    #[serde(default)]
    ca_cert_path: Option<String>,
    #[serde(default)]
    server_name: Option<String>,
}

#[derive(Debug, Clone, Deserialize)]
struct BootstrapConfig {
    #[serde(default)]
    base_url: Option<String>,
    #[serde(default)]
    api_token: Option<String>,
    #[serde(default)]
    tls: Option<BootstrapTLS>,
}

// ---------------------------------------------------------------------------
// GrpcClient
// ---------------------------------------------------------------------------

#[derive(Clone)]
pub struct GrpcClient {
    channel: Channel,
    token: Option<String>,
    language: Option<String>,
}

impl GrpcClient {
    pub fn channel(&self) -> Channel {
        self.channel.clone()
    }

    pub fn auth_request<T>(&self, msg: T) -> tonic::Request<T> {
        let mut req = tonic::Request::new(msg);
        if let Some(token) = &self.token {
            match format!("Bearer {token}").parse() {
                Ok(val) => {
                    req.metadata_mut().insert("authorization", val);
                }
                Err(_) => {
                    eprintln!("[nupi-desktop] WARNING: auth token contains invalid characters for gRPC metadata — request will be sent without authentication");
                }
            }
        }
        if let Some(lang) = &self.language {
            match lang.parse() {
                Ok(val) => {
                    req.metadata_mut().insert("nupi-language", val);
                }
                Err(_) => {
                    eprintln!("[nupi-desktop] WARNING: language preference contains invalid characters for gRPC metadata — request will be sent without language header");
                }
            }
        }
        req
    }

    fn require_token(&self) -> Result<String, GrpcClientError> {
        if let Some(token) = self
            .token
            .as_ref()
            .map(|t| t.trim().to_string())
            .filter(|t| !t.is_empty())
        {
            return Ok(token);
        }

        if let Ok(env_token) = env::var("NUPI_API_TOKEN") {
            let trimmed = env_token.trim();
            if !trimmed.is_empty() {
                return Ok(trimmed.to_string());
            }
        }

        Err(GrpcClientError::Config(
            "API token is required for this operation".into(),
        ))
    }

    // -----------------------------------------------------------------------
    // Discovery
    // -----------------------------------------------------------------------

    pub async fn discover() -> Result<Self, GrpcClientError> {
        let persisted = settings::load();

        let language = persisted_language(&persisted);

        // 1. NUPI_BASE_URL env → explicit endpoint
        if let Ok(raw) = env::var("NUPI_BASE_URL") {
            if !raw.trim().is_empty() {
                return Self::from_explicit(raw.trim(), persisted.api_token.clone(), None, language).await;
            }
        }

        // 2. Persisted settings
        if let Some(base) = persisted.base_url.as_ref() {
            if !base.trim().is_empty() {
                return Self::from_explicit(base.trim(), persisted.api_token.clone(), None, language).await;
            }
        }

        // 3. Bootstrap file
        if let Some(bootstrap) = load_bootstrap_config()? {
            if let Some(base) = bootstrap
                .base_url
                .as_ref()
                .map(|v| v.trim())
                .filter(|v| !v.is_empty())
            {
                let fallback_token = bootstrap
                    .api_token
                    .as_ref()
                    .map(|v| v.trim().to_string())
                    .filter(|v| !v.is_empty());

                return Self::from_explicit(base, fallback_token, bootstrap.tls.as_ref(), language.clone()).await;
            }
        }

        // 4. SQLite config store → gRPC port
        let snapshot = load_transport_snapshot()?;

        if snapshot.grpc_port <= 0 {
            return Err(GrpcClientError::Config(
                "Daemon gRPC port is not configured. Start the daemon once or run quickstart."
                    .into(),
            ));
        }

        let tls_enabled = snapshot
            .tls_cert_path
            .as_ref()
            .map(|p| !p.trim().is_empty())
            .unwrap_or_default()
            && snapshot
                .tls_key_path
                .as_ref()
                .map(|p| !p.trim().is_empty())
                .unwrap_or_default();

        let mut host = determine_host(snapshot.grpc_binding.as_str(), tls_enabled);
        if let Ok(override_host) = env::var("NUPI_DAEMON_HOST") {
            if !override_host.trim().is_empty() {
                host = override_host.trim().to_string();
            }
        }

        let scheme = if tls_enabled { "https" } else { "http" };
        let uri = format!("{scheme}://{host}:{}", snapshot.grpc_port);

        let insecure = env::var("NUPI_TLS_INSECURE")
            .ok()
            .map(|v| v == "1")
            .unwrap_or(false);

        let channel = build_channel(&uri, tls_enabled, insecure, snapshot.tls_cert_path.as_deref())
            .await?;

        let mut token = resolve_token(snapshot.tokens);
        if token.is_none() {
            token = persisted
                .api_token
                .map(|v| v.trim().to_string())
                .filter(|s| !s.is_empty());
        }

        Ok(Self { channel, token, language })
    }

    async fn from_explicit(
        raw: &str,
        default_token: Option<String>,
        tls_overrides: Option<&BootstrapTLS>,
        language: Option<String>,
    ) -> Result<Self, GrpcClientError> {
        let mut url_str = raw.to_string();
        if !url_str.contains("://") {
            url_str = format!("https://{url_str}");
        }

        let url = url::Url::parse(&url_str)
            .map_err(|err| GrpcClientError::Config(format!("Invalid NUPI_BASE_URL ({err})")))?;

        if url.host_str().is_none() {
            return Err(GrpcClientError::Config(
                "NUPI_BASE_URL must include host".into(),
            ));
        }

        let tls_enabled = url.scheme() == "https";

        let insecure_env = env::var("NUPI_TLS_INSECURE")
            .ok()
            .map(|v| v == "1")
            .unwrap_or(false);
        let insecure_override = tls_overrides.and_then(|tls| tls.insecure).unwrap_or(false);
        let insecure = insecure_env || insecure_override;

        let ca_path = if !insecure {
            let ca_from_env = env::var("NUPI_TLS_CA_CERT")
                .ok()
                .map(|v| v.trim().to_string())
                .filter(|v| !v.is_empty());

            ca_from_env.or_else(|| {
                tls_overrides
                    .and_then(|tls| tls.ca_cert_path.as_ref())
                    .map(|v| v.trim().to_string())
                    .filter(|v| !v.is_empty())
            })
        } else {
            None
        };

        let channel =
            build_channel(url.as_str(), tls_enabled, insecure, ca_path.as_deref()).await?;

        let env_token = env::var("NUPI_API_TOKEN")
            .ok()
            .map(|t| t.trim().to_string())
            .filter(|t| !t.is_empty());
        let fallback_token = default_token
            .map(|v| v.trim().to_string())
            .filter(|v| !v.is_empty());
        let token = env_token.or(fallback_token);

        Ok(Self { channel, token, language })
    }

    // -----------------------------------------------------------------------
    // Simple RPC methods
    // -----------------------------------------------------------------------

    pub async fn daemon_status(&self) -> Result<DaemonStatus, GrpcClientError> {
        let mut client =
            nupi_api::daemon_service_client::DaemonServiceClient::new(self.channel.clone());
        let response = client
            .status(self.auth_request(nupi_api::DaemonStatusRequest {}))
            .await?;
        Ok(DaemonStatus::from(response.into_inner()))
    }

    pub async fn is_reachable(&self) -> bool {
        match self.daemon_status().await {
            Ok(_) => true,
            Err(GrpcClientError::Grpc(ref s))
                if s.code() == tonic::Code::Unauthenticated =>
            {
                true
            }
            Err(_) => false,
        }
    }

    pub async fn list_languages(&self) -> Result<Vec<LanguageEntry>, GrpcClientError> {
        let mut client =
            nupi_api::daemon_service_client::DaemonServiceClient::new(self.channel.clone());
        let response = client
            .list_languages(self.auth_request(nupi_api::ListLanguagesRequest {}))
            .await?;
        Ok(response
            .into_inner()
            .languages
            .into_iter()
            .map(LanguageEntry::from)
            .collect())
    }

    pub async fn shutdown_daemon(&self) -> Result<(), GrpcClientError> {
        let mut client =
            nupi_api::daemon_service_client::DaemonServiceClient::new(self.channel.clone());
        client
            .shutdown(self.auth_request(nupi_api::ShutdownRequest {}))
            .await?;
        Ok(())
    }

    pub async fn list_tokens(&self) -> Result<Vec<TokenEntry>, GrpcClientError> {
        let _ = self.require_token()?;
        let mut client =
            nupi_api::auth_service_client::AuthServiceClient::new(self.channel.clone());
        let response = client
            .list_tokens(self.auth_request(nupi_api::ListTokensRequest {}))
            .await?;
        Ok(response
            .into_inner()
            .tokens
            .into_iter()
            .map(TokenEntry::from)
            .collect())
    }

    pub async fn create_token(
        &self,
        name: Option<String>,
        role: Option<String>,
    ) -> Result<CreatedToken, GrpcClientError> {
        let _ = self.require_token()?;
        let mut client =
            nupi_api::auth_service_client::AuthServiceClient::new(self.channel.clone());
        let response = client
            .create_token(self.auth_request(nupi_api::CreateTokenRequest {
                name: name.unwrap_or_default(),
                role: role.unwrap_or_else(|| "admin".into()),
            }))
            .await?;
        let inner = response.into_inner();
        let info = inner.info.unwrap_or_default();
        Ok(CreatedToken {
            token: inner.token,
            id: info.id,
            name: if info.name.is_empty() {
                None
            } else {
                Some(info.name)
            },
            role: info.role,
        })
    }

    pub async fn delete_token(
        &self,
        id: Option<String>,
        token_value: Option<String>,
    ) -> Result<(), GrpcClientError> {
        let _ = self.require_token()?;

        let id_str = id
            .and_then(|v| {
                let t = v.trim().to_string();
                if t.is_empty() { None } else { Some(t) }
            })
            .unwrap_or_default();
        let token_str = token_value
            .and_then(|v| {
                let t = v.trim().to_string();
                if t.is_empty() { None } else { Some(t) }
            })
            .unwrap_or_default();

        if id_str.is_empty() && token_str.is_empty() {
            return Err(GrpcClientError::Config(
                "Provide token string or id to delete".into(),
            ));
        }

        let mut client =
            nupi_api::auth_service_client::AuthServiceClient::new(self.channel.clone());
        client
            .delete_token(self.auth_request(nupi_api::DeleteTokenRequest {
                id: id_str,
                token: token_str,
            }))
            .await?;
        Ok(())
    }

    pub async fn list_pairings(&self) -> Result<Vec<PairingEntry>, GrpcClientError> {
        let _ = self.require_token()?;
        let mut client =
            nupi_api::auth_service_client::AuthServiceClient::new(self.channel.clone());
        let response = client
            .list_pairings(self.auth_request(nupi_api::ListPairingsRequest {}))
            .await?;
        Ok(response
            .into_inner()
            .pairings
            .into_iter()
            .map(PairingEntry::from)
            .collect())
    }

    pub async fn create_pairing(
        &self,
        name: Option<String>,
        role: Option<String>,
        expires_in: Option<u32>,
    ) -> Result<CreatedPairing, GrpcClientError> {
        let _ = self.require_token()?;
        let mut client =
            nupi_api::auth_service_client::AuthServiceClient::new(self.channel.clone());
        let response = client
            .create_pairing(self.auth_request(nupi_api::CreatePairingRequest {
                name: name.unwrap_or_default(),
                role: role.unwrap_or_else(|| "read-only".into()),
                expires_in_seconds: expires_in.unwrap_or(300).min(i32::MAX as u32) as i32,
            }))
            .await?;
        Ok(CreatedPairing::from(response.into_inner()))
    }

    // -----------------------------------------------------------------------
    // Audio capabilities
    // -----------------------------------------------------------------------

    pub async fn audio_capabilities(
        &self,
        session_id: Option<&str>,
    ) -> Result<AudioCapabilitiesSummary, GrpcClientError> {
        let session = session_id
            .and_then(|v| {
                let t = v.trim();
                if t.is_empty() { None } else { Some(t.to_string()) }
            })
            .unwrap_or_default();

        let mut client =
            nupi_api::audio_service_client::AudioServiceClient::new(self.channel.clone());
        let result = client
            .get_audio_capabilities(
                self.auth_request(nupi_api::GetAudioCapabilitiesRequest {
                    session_id: session,
                }),
            )
            .await;

        match result {
            Ok(response) => {
                let inner = response.into_inner();
                let capture: Vec<AudioCapabilitySummary> =
                    inner.capture.into_iter().map(AudioCapabilitySummary::from).collect();
                let playback: Vec<AudioCapabilitySummary> =
                    inner.playback.into_iter().map(AudioCapabilitySummary::from).collect();

                let capture_enabled = capture.iter().any(|c| {
                    c.metadata
                        .as_ref()
                        .and_then(|m| m.get("ready"))
                        .map(|v| v == "true")
                        .unwrap_or(false)
                });
                let playback_enabled = playback.iter().any(|c| {
                    c.metadata
                        .as_ref()
                        .and_then(|m| m.get("ready"))
                        .map(|v| v == "true")
                        .unwrap_or(false)
                });

                let mut diagnostics = Vec::new();
                for cap in capture.iter().chain(playback.iter()) {
                    if let Some(meta) = &cap.metadata {
                        if let Some(diag_str) = meta.get("diagnostics") {
                            if !diag_str.trim().is_empty() {
                                diagnostics.push(VoiceDiagnosticSummary {
                                    slot: cap.stream_id.clone(),
                                    code: "info".to_string(),
                                    message: diag_str.clone(),
                                });
                            }
                        }
                    }
                }

                Ok(AudioCapabilitiesSummary {
                    capture,
                    playback,
                    capture_enabled,
                    playback_enabled,
                    diagnostics,
                    message: None,
                })
            }
            Err(status) if status.code() == tonic::Code::FailedPrecondition => {
                Ok(AudioCapabilitiesSummary {
                    capture: Vec::new(),
                    playback: Vec::new(),
                    capture_enabled: false,
                    playback_enabled: false,
                    diagnostics: Vec::new(),
                    message: Some(status.message().to_string()),
                })
            }
            Err(status) => Err(GrpcClientError::Grpc(status)),
        }
    }

    // -----------------------------------------------------------------------
    // Audio streaming — ingress (voice_stream_from_file)
    // -----------------------------------------------------------------------

    pub async fn voice_stream_from_file(
        &self,
        req: VoiceStreamRequest,
        abort: Option<watch::Receiver<bool>>,
    ) -> Result<VoiceStreamSummary, GrpcClientError> {
        let VoiceStreamRequest {
            session_id,
            stream_id,
            input_path,
            playback_output,
            disable_playback,
            metadata,
        } = req;

        let trimmed_session = session_id.trim();
        if trimmed_session.is_empty() {
            return Err(GrpcClientError::Config("session_id is required".into()));
        }

        let stream_id_value = stream_id
            .as_ref()
            .map(|v| v.trim())
            .filter(|v| !v.is_empty())
            .map(|v| v.to_string())
            .unwrap_or_else(|| DEFAULT_CAPTURE_STREAM_ID.to_string());

        let ingress_source = input_path.display().to_string();

        let file_meta = tokio::fs::metadata(&input_path).await?;
        if file_meta.len() > MAX_AUDIO_FILE_SIZE {
            return Err(GrpcClientError::Config(format!(
                "audio file too large: {} bytes (max {})",
                file_meta.len(),
                MAX_AUDIO_FILE_SIZE
            )));
        }

        let extension = input_path
            .extension()
            .and_then(|ext| ext.to_str())
            .map(|ext| ext.to_ascii_lowercase())
            .unwrap_or_default();
        if extension != "wav" {
            return Err(GrpcClientError::Config(
                "Only WAV files are currently supported for voice streaming".into(),
            ));
        }

        let metadata_norm = normalize_metadata_map(metadata, "desktop");
        validate_metadata_map(&metadata_norm)?;
        let subscribe_playback = !disable_playback || playback_output.is_some();

        let capabilities = self.audio_capabilities(Some(trimmed_session)).await?;

        if !capabilities.capture_enabled {
            let message = capabilities.message.clone().unwrap_or_else(|| {
                primary_diagnostic_message(
                    &capabilities.diagnostics,
                    SLOT_STT,
                    "Voice capture is disabled",
                )
            });
            return Err(GrpcClientError::VoiceNotReady {
                message,
                diagnostics: capabilities.diagnostics.clone(),
            });
        }
        if subscribe_playback && !capabilities.playback_enabled {
            let message = capabilities.message.clone().unwrap_or_else(|| {
                primary_diagnostic_message(
                    &capabilities.diagnostics,
                    SLOT_TTS,
                    "Voice playback is disabled",
                )
            });
            return Err(GrpcClientError::VoiceNotReady {
                message,
                diagnostics: capabilities.diagnostics.clone(),
            });
        }

        let cancel_flag = Arc::new(AtomicBool::new(false));
        let bytes_uploaded = Arc::new(AtomicU64::new(0));

        // Read WAV file and stream chunks via gRPC client-streaming
        let path_clone = input_path.clone();
        let cancel_flag_reader = Arc::clone(&cancel_flag);
        let bytes_counter = Arc::clone(&bytes_uploaded);
        let session_for_stream = trimmed_session.to_string();
        let stream_id_for_stream = stream_id_value.clone();
        let metadata_for_stream = metadata_norm.clone();

        let (tx, rx) = tokio::sync::mpsc::channel::<nupi_api::StreamAudioInRequest>(16);

        // Spawn WAV reader task
        let reader_task = tokio::task::spawn_blocking(move || -> Result<(), GrpcClientError> {
            if cancel_flag_reader.load(Ordering::SeqCst) {
                return Err(GrpcClientError::Audio(CANCELLED_MESSAGE.to_string()));
            }

            let mut reader = hound::WavReader::open(&path_clone)?;
            let spec = reader.spec();
            if spec.sample_format != SampleFormat::Int || spec.bits_per_sample != 16 {
                return Err(GrpcClientError::Config(
                    "Only 16-bit PCM WAV files are supported for voice streaming".to_string(),
                ));
            }

            let format = nupi_api::AudioFormat {
                encoding: "pcm_s16le".to_string(),
                sample_rate: spec.sample_rate,
                channels: spec.channels as u32,
                bit_depth: spec.bits_per_sample as u32,
                frame_duration_ms: 0,
            };

            // First chunk with format and metadata
            let mut pcm_buffer = Vec::with_capacity(AUDIO_CHUNK_SIZE);
            let mut sequence: u64 = 0;
            let mut is_first = true;

            for sample in reader.samples::<i16>() {
                if cancel_flag_reader.load(Ordering::SeqCst) {
                    return Err(GrpcClientError::Audio(CANCELLED_MESSAGE.to_string()));
                }
                let sample = sample.map_err(|e| GrpcClientError::Audio(e.to_string()))?;
                pcm_buffer.extend_from_slice(&sample.to_le_bytes());

                if pcm_buffer.len() >= AUDIO_CHUNK_SIZE {
                    bytes_counter.fetch_add(pcm_buffer.len() as u64, Ordering::SeqCst);
                    let chunk_data = std::mem::replace(
                        &mut pcm_buffer,
                        Vec::with_capacity(AUDIO_CHUNK_SIZE),
                    );
                    let req = nupi_api::StreamAudioInRequest {
                        session_id: session_for_stream.clone(),
                        stream_id: stream_id_for_stream.clone(),
                        format: if is_first { Some(format.clone()) } else { None },
                        chunk: Some(nupi_api::AudioChunk {
                            data: chunk_data,
                            sequence,
                            duration_ms: 0,
                            first: is_first,
                            last: false,
                            metadata: if is_first {
                                metadata_for_stream.clone()
                            } else {
                                HashMap::new()
                            },
                        }),
                    };
                    is_first = false;
                    sequence += 1;

                    if tx.blocking_send(req).is_err() {
                        return Ok(()); // receiver dropped
                    }
                }
            }

            // Send final chunk
            bytes_counter.fetch_add(pcm_buffer.len() as u64, Ordering::SeqCst);
            let req = nupi_api::StreamAudioInRequest {
                session_id: session_for_stream,
                stream_id: stream_id_for_stream,
                format: if is_first { Some(format) } else { None },
                chunk: Some(nupi_api::AudioChunk {
                    data: pcm_buffer,
                    sequence,
                    duration_ms: 0,
                    first: is_first,
                    last: true,
                    metadata: if is_first {
                        metadata_for_stream
                    } else {
                        HashMap::new()
                    },
                }),
            };
            let _ = tx.blocking_send(req);

            Ok(())
        });

        // Send the stream to gRPC
        let stream = tokio_stream::wrappers::ReceiverStream::new(rx);
        let mut grpc_request = self.auth_request(stream);
        grpc_request.set_timeout(Duration::from_secs(600));

        let mut client =
            nupi_api::audio_service_client::AudioServiceClient::new(self.channel.clone());

        let send_result = if let Some(mut cancel_signal) = abort.clone() {
            let cancel_flag_select = Arc::clone(&cancel_flag);
            let mut send_future = Box::pin(client.stream_audio_in(grpc_request));
            loop {
                tokio::select! {
                    res = &mut send_future => break res.map(|_| ()).map_err(GrpcClientError::from),
                    _ = cancel_signal.changed() => {
                        if *cancel_signal.borrow() {
                            cancel_flag_select.store(true, Ordering::SeqCst);
                            let _ = reader_task.await;
                            return Err(GrpcClientError::Audio(CANCELLED_MESSAGE.into()));
                        }
                    }
                }
            }
        } else {
            client
                .stream_audio_in(grpc_request)
                .await
                .map(|_| ())
                .map_err(GrpcClientError::from)
        };

        // Wait for reader task to complete
        let reader_result = reader_task
            .await
            .map_err(|err| GrpcClientError::Audio(err.to_string()))?;
        reader_result?;

        send_result?;

        if cancel_flag.load(Ordering::SeqCst) {
            return Err(GrpcClientError::Audio(CANCELLED_MESSAGE.into()));
        }

        let uploaded = bytes_uploaded.load(Ordering::SeqCst);

        // Handle playback capture
        self.finalize_voice_stream(
            trimmed_session,
            stream_id_value,
            ingress_source,
            playback_output,
            subscribe_playback,
            abort,
            cancel_flag,
            uploaded,
        )
        .await
    }

    #[allow(clippy::too_many_arguments)]
    async fn finalize_voice_stream(
        &self,
        session_id: &str,
        stream_id: String,
        ingress_source: String,
        playback_output: Option<PathBuf>,
        subscribe_playback: bool,
        mut abort: Option<watch::Receiver<bool>>,
        cancel_flag: Arc<AtomicBool>,
        bytes_uploaded: u64,
    ) -> Result<VoiceStreamSummary, GrpcClientError> {
        let mut playback_error: Option<String> = None;
        let playback_metrics = if subscribe_playback {
            if cancel_flag.load(Ordering::SeqCst) {
                return Err(GrpcClientError::Audio(CANCELLED_MESSAGE.into()));
            }
            match abort.take() {
                Some(mut cancel_signal) => {
                    let cancel_flag_select = Arc::clone(&cancel_flag);
                    let mut capture_future = Box::pin(self.capture_playback(
                        session_id,
                        &stream_id,
                        playback_output.clone(),
                    ));
                    let result = loop {
                        tokio::select! {
                            res = &mut capture_future => break res,
                            _ = cancel_signal.changed() => {
                                if *cancel_signal.borrow() {
                                    cancel_flag_select.store(true, Ordering::SeqCst);
                                    return Err(GrpcClientError::Audio(CANCELLED_MESSAGE.into()));
                                }
                            }
                        }
                    };
                    match result {
                        Ok(metrics) => metrics,
                        Err(err) => {
                            playback_error = Some(err.to_string());
                            PlaybackMetrics {
                                chunks: 0,
                                bytes: 0,
                                saved_path: None,
                            }
                        }
                    }
                }
                None => match self
                    .capture_playback(session_id, &stream_id, playback_output.clone())
                    .await
                {
                    Ok(metrics) => metrics,
                    Err(err) => {
                        playback_error = Some(err.to_string());
                        PlaybackMetrics {
                            chunks: 0,
                            bytes: 0,
                            saved_path: None,
                        }
                    }
                },
            }
        } else {
            PlaybackMetrics {
                chunks: 0,
                bytes: 0,
                saved_path: None,
            }
        };

        if cancel_flag.load(Ordering::SeqCst) {
            return Err(GrpcClientError::Audio(CANCELLED_MESSAGE.into()));
        }

        Ok(VoiceStreamSummary {
            session_id: session_id.to_string(),
            stream_id,
            bytes_uploaded,
            ingress_source,
            playback_chunks: playback_metrics.chunks,
            playback_bytes: playback_metrics.bytes,
            playback_path: playback_metrics.saved_path,
            playback_error,
        })
    }

    // -----------------------------------------------------------------------
    // Audio streaming — egress (capture_playback)
    // -----------------------------------------------------------------------

    async fn capture_playback(
        &self,
        session_id: &str,
        stream_id: &str,
        playback_output: Option<PathBuf>,
    ) -> Result<PlaybackMetrics, GrpcClientError> {
        let mut client =
            nupi_api::audio_service_client::AudioServiceClient::new(self.channel.clone());

        let request = self.auth_request(nupi_api::StreamAudioOutRequest {
            session_id: session_id.to_string(),
            stream_id: if stream_id.is_empty() {
                DEFAULT_PLAYBACK_STREAM_ID.to_string()
            } else {
                stream_id.to_string()
            },
        });

        let response = client.stream_audio_out(request).await?;
        let mut stream = response.into_inner();

        let mut chunks = 0u64;
        let mut bytes = 0u64;
        let mut format_summary: Option<AudioFormatSummary> = None;
        let mut wav_writer: Option<WavWriter<std::io::BufWriter<std::fs::File>>> = None;
        let mut playback_path = playback_output;

        while let Some(msg) = stream
            .message()
            .await
            .map_err(GrpcClientError::Grpc)?
        {
            // Extract format from first message
            if format_summary.is_none() {
                if let Some(fmt) = &msg.format {
                    format_summary = Some(AudioFormatSummary {
                        encoding: if fmt.encoding.is_empty() {
                            "pcm_s16le".to_string()
                        } else {
                            fmt.encoding.clone()
                        },
                        sample_rate: fmt.sample_rate,
                        channels: fmt.channels as u16,
                        bit_depth: fmt.bit_depth as u16,
                        frame_ms: if fmt.frame_duration_ms == 0 {
                            None
                        } else {
                            Some(fmt.frame_duration_ms)
                        },
                    });
                }
            }

            // Initialize WAV writer if output path specified
            if playback_path.is_some() && wav_writer.is_none() {
                let summary = format_summary
                    .clone()
                    .unwrap_or_else(default_capture_format_summary);
                if format_summary.is_none() {
                    format_summary = Some(summary.clone());
                }
                let spec = WavSpec {
                    channels: summary.channels,
                    sample_rate: summary.sample_rate,
                    bits_per_sample: summary.bit_depth,
                    sample_format: SampleFormat::Int,
                };
                wav_writer = Some(WavWriter::create(playback_path.as_ref().unwrap(), spec)?);
            }

            if let Some(chunk) = &msg.chunk {
                let data = &chunk.data;
                let data_len = data.len() as u64;
                let projected = bytes + data_len;
                if projected > MAX_AUDIO_FILE_SIZE {
                    return Err(GrpcClientError::Audio(format!(
                        "Playback exceeded {MAX_AUDIO_FILE_SIZE} bytes limit",
                    )));
                }
                bytes = projected;

                if let Some(writer) = wav_writer.as_mut() {
                    if data.len() % 2 != 0 {
                        return Err(GrpcClientError::Audio(
                            "playback PCM payload has odd length".into(),
                        ));
                    }
                    for sample_bytes in data.chunks_exact(2) {
                        let sample = i16::from_le_bytes([sample_bytes[0], sample_bytes[1]]);
                        writer.write_sample(sample)?;
                    }
                }

                if !data.is_empty() {
                    chunks += 1;
                }

                if chunk.last {
                    break;
                }
            }
        }

        let mut saved_path = None;
        if let Some(writer) = wav_writer {
            writer.finalize()?;
            if let Some(path) = playback_path.take() {
                saved_path = Some(path.to_string_lossy().to_string());
            }
        }

        Ok(PlaybackMetrics {
            chunks,
            bytes,
            saved_path,
        })
    }

    // -----------------------------------------------------------------------
    // Voice interrupt
    // -----------------------------------------------------------------------

    pub async fn voice_interrupt(
        &self,
        session_id: &str,
        stream_id: Option<&str>,
        reason: Option<&str>,
        metadata: HashMap<String, String>,
    ) -> Result<VoiceInterruptSummary, GrpcClientError> {
        let trimmed_session = session_id.trim();
        if trimmed_session.is_empty() {
            return Err(GrpcClientError::Config("session_id is required".into()));
        }

        let stream_value = stream_id
            .and_then(|v| {
                let trimmed = v.trim();
                if trimmed.is_empty() {
                    None
                } else {
                    Some(trimmed.to_string())
                }
            })
            .unwrap_or_else(|| DEFAULT_PLAYBACK_STREAM_ID.to_string());

        let reason_value = reason.and_then(|v| {
            let trimmed = v.trim();
            if trimmed.is_empty() {
                None
            } else {
                Some(trimmed.to_string())
            }
        });

        let metadata_norm = normalize_metadata_map(metadata, "desktop");
        validate_metadata_map(&metadata_norm)?;

        let capabilities = self.audio_capabilities(Some(trimmed_session)).await?;
        if !capabilities.playback_enabled {
            let message = capabilities.message.clone().unwrap_or_else(|| {
                primary_diagnostic_message(
                    &capabilities.diagnostics,
                    SLOT_TTS,
                    "Voice playback is disabled",
                )
            });
            return Err(GrpcClientError::VoiceNotReady {
                message,
                diagnostics: capabilities.diagnostics.clone(),
            });
        }

        let mut client =
            nupi_api::audio_service_client::AudioServiceClient::new(self.channel.clone());
        client
            .interrupt_tts(self.auth_request(nupi_api::InterruptTtsRequest {
                session_id: trimmed_session.to_string(),
                stream_id: stream_value.clone(),
                reason: reason_value.clone().unwrap_or_default(),
            }))
            .await?;

        Ok(VoiceInterruptSummary {
            session_id: trimmed_session.to_string(),
            stream_id: stream_value,
            reason: reason_value,
            metadata: metadata_norm,
        })
    }
    // -----------------------------------------------------------------------
    // Session management
    // -----------------------------------------------------------------------

    pub async fn list_sessions(&self) -> Result<Vec<SessionInfo>, GrpcClientError> {
        let mut client =
            nupi_api::sessions_service_client::SessionsServiceClient::new(self.channel.clone());
        let response = client
            .list_sessions(self.auth_request(nupi_api::ListSessionsRequest {}))
            .await?;
        Ok(response
            .into_inner()
            .sessions
            .into_iter()
            .map(SessionInfo::from)
            .collect())
    }

    pub async fn get_session(&self, session_id: &str) -> Result<SessionInfo, GrpcClientError> {
        let mut client =
            nupi_api::sessions_service_client::SessionsServiceClient::new(self.channel.clone());
        let response = client
            .get_session(self.auth_request(nupi_api::GetSessionRequest {
                session_id: session_id.to_string(),
            }))
            .await?;
        let session = response
            .into_inner()
            .session
            .ok_or_else(|| GrpcClientError::Config("Server returned empty session".into()))?;
        Ok(SessionInfo::from(session))
    }

    #[allow(clippy::too_many_arguments)]
    pub async fn create_session(
        &self,
        command: &str,
        args: Vec<String>,
        cols: u32,
        rows: u32,
        work_dir: Option<String>,
        env: Vec<String>,
        detached: bool,
        inspect: bool,
    ) -> Result<SessionInfo, GrpcClientError> {
        let mut client =
            nupi_api::sessions_service_client::SessionsServiceClient::new(self.channel.clone());
        let response = client
            .create_session(self.auth_request(nupi_api::CreateSessionRequest {
                command: command.to_string(),
                args,
                working_dir: work_dir.unwrap_or_default(),
                env,
                rows,
                cols,
                detached,
                inspect,
            }))
            .await?;
        let session = response
            .into_inner()
            .session
            .ok_or_else(|| GrpcClientError::Config("Server returned empty session".into()))?;
        Ok(SessionInfo::from(session))
    }

    pub async fn kill_session(&self, session_id: &str) -> Result<(), GrpcClientError> {
        let mut client =
            nupi_api::sessions_service_client::SessionsServiceClient::new(self.channel.clone());
        client
            .kill_session(self.auth_request(nupi_api::KillSessionRequest {
                session_id: session_id.to_string(),
            }))
            .await?;
        Ok(())
    }

    // -----------------------------------------------------------------------
    // Recordings
    // -----------------------------------------------------------------------

    pub async fn list_recordings(
        &self,
        session_id: Option<&str>,
    ) -> Result<Vec<RecordingInfo>, GrpcClientError> {
        let mut client =
            nupi_api::recordings_service_client::RecordingsServiceClient::new(self.channel.clone());
        let response = client
            .list_recordings(self.auth_request(nupi_api::ListRecordingsRequest {
                session_id: session_id.unwrap_or_default().to_string(),
            }))
            .await?;
        Ok(response
            .into_inner()
            .recordings
            .into_iter()
            .map(RecordingInfo::from)
            .collect())
    }

    pub async fn get_recording(
        &self,
        session_id: &str,
    ) -> Result<String, GrpcClientError> {
        const MAX_RECORDING_SIZE: usize = 100 * 1024 * 1024; // 100 MiB

        let mut client =
            nupi_api::recordings_service_client::RecordingsServiceClient::new(self.channel.clone());
        let response = client
            .get_recording(self.auth_request(nupi_api::GetRecordingRequest {
                session_id: session_id.to_string(),
            }))
            .await?;
        let mut stream = response.into_inner();
        let mut data = Vec::new();
        while let Some(chunk) = stream.message().await? {
            if data.len() + chunk.data.len() > MAX_RECORDING_SIZE {
                return Err(GrpcClientError::Config(format!(
                    "recording exceeds {MAX_RECORDING_SIZE} byte limit"
                )));
            }
            data.extend_from_slice(&chunk.data);
        }
        String::from_utf8(data)
            .map_err(|e| GrpcClientError::Config(format!("recording is not valid UTF-8: {e}")))
    }
}

// ---------------------------------------------------------------------------
// Playback metrics (internal)
// ---------------------------------------------------------------------------

#[derive(Debug)]
struct PlaybackMetrics {
    chunks: u64,
    bytes: u64,
    saved_path: Option<String>,
}

// ---------------------------------------------------------------------------
// Standalone claim_pairing_grpc()
// ---------------------------------------------------------------------------

pub async fn claim_pairing_grpc(
    base_url: &str,
    code: &str,
    name: Option<String>,
    insecure: bool,
) -> Result<ClaimedToken, GrpcClientError> {
    let mut url_str = base_url.trim_end_matches('/').to_string();
    if !url_str.contains("://") {
        url_str = format!("https://{url_str}");
    }

    let tls_enabled = url_str.starts_with("https://");

    let channel = build_channel(&url_str, tls_enabled, insecure, None).await?;

    let mut client = nupi_api::auth_service_client::AuthServiceClient::new(channel);
    let response = client
        .claim_pairing(tonic::Request::new(nupi_api::ClaimPairingRequest {
            code: code.to_string(),
            client_name: name.unwrap_or_default(),
        }))
        .await?;

    Ok(ClaimedToken::from(response.into_inner()))
}

// ---------------------------------------------------------------------------
// Channel builder
// ---------------------------------------------------------------------------

async fn build_channel(
    uri: &str,
    tls_enabled: bool,
    insecure: bool,
    ca_cert_path: Option<&str>,
) -> Result<Channel, GrpcClientError> {
    // When insecure mode is requested, downgrade to plaintext HTTP/2.
    // tonic 0.12 doesn't expose `rustls_client_config()` on ClientTlsConfig,
    // so we bypass TLS entirely for dev/insecure scenarios.
    let effective_uri = if tls_enabled && insecure {
        TLS_INSECURE_WARN.call_once(|| {
            eprintln!("[TLS] WARNING: TLS verification disabled — downgrading to plaintext gRPC. Do NOT use in production.");
        });
        uri.replacen("https://", "http://", 1)
    } else {
        uri.to_string()
    };

    let endpoint = Endpoint::from_shared(effective_uri)
        .map_err(|e| GrpcClientError::Config(format!("Invalid endpoint URI: {e}")))?
        .timeout(Duration::from_secs(10))
        .connect_timeout(Duration::from_secs(5));

    let endpoint = if tls_enabled && !insecure {
        if let Some(ca_path) = ca_cert_path {
            let pem = fs::read(ca_path)?;
            let ca = tonic::transport::Certificate::from_pem(pem);
            let tls = ClientTlsConfig::new().ca_certificate(ca);
            endpoint.tls_config(tls)?
        } else {
            let tls = ClientTlsConfig::new();
            endpoint.tls_config(tls)?
        }
    } else {
        endpoint
    };

    Ok(endpoint.connect_lazy())
}

// ---------------------------------------------------------------------------
// SQLite helpers (same as rest_client.rs)
// ---------------------------------------------------------------------------

fn load_transport_snapshot() -> Result<TransportSnapshot, GrpcClientError> {
    let home = home_dir().ok_or(GrpcClientError::MissingHomeDir)?;

    let db_path = home
        .join(".nupi")
        .join("instances")
        .join(INSTANCE_NAME)
        .join("config.db");

    if !db_path.exists() {
        return Err(GrpcClientError::Config(format!(
            "Config store not found at {}",
            db_path.display()
        )));
    }

    let conn = Connection::open_with_flags(
        db_path,
        OpenFlags::SQLITE_OPEN_READ_ONLY | OpenFlags::SQLITE_OPEN_URI,
    )?;

    apply_pragmas(&conn)?;

    let settings = load_settings(&conn)?;

    let grpc_port = settings
        .get("transport.grpc_port")
        .and_then(|v| v.parse::<i32>().ok())
        .unwrap_or_default();

    let grpc_binding = settings
        .get("transport.grpc_binding")
        .cloned()
        .unwrap_or_else(|| {
            settings
                .get("transport.binding")
                .cloned()
                .unwrap_or_else(|| "loopback".to_string())
        });

    let tls_cert_path = settings
        .get("transport.tls_cert_path")
        .filter(|s| !s.trim().is_empty())
        .cloned();

    let tls_key_path = settings
        .get("transport.tls_key_path")
        .filter(|s| !s.trim().is_empty())
        .cloned();

    let tokens = load_tokens(&conn)?;

    Ok(TransportSnapshot {
        grpc_port,
        grpc_binding,
        tls_cert_path,
        tls_key_path,
        tokens,
    })
}

fn apply_pragmas(conn: &Connection) -> Result<(), GrpcClientError> {
    conn.pragma_update(None, "busy_timeout", 5000)?;
    conn.pragma_update(None, "foreign_keys", 1)?;
    Ok(())
}

fn load_settings(conn: &Connection) -> Result<HashMap<String, String>, GrpcClientError> {
    let mut stmt = conn.prepare(
        "SELECT key, value FROM settings WHERE instance_name = ?1 AND profile_name = ?2",
    )?;

    let mut rows = stmt.query(params![INSTANCE_NAME, PROFILE_NAME])?;
    let mut map = HashMap::new();

    while let Some(row) = rows.next()? {
        let key: String = row.get(0)?;
        let value: String = row.get(1)?;
        map.insert(key, value);
    }

    Ok(map)
}

fn load_tokens(conn: &Connection) -> Result<Vec<String>, GrpcClientError> {
    let mut stmt = conn.prepare(
        "SELECT value FROM security_settings WHERE instance_name = ?1 AND profile_name = ?2 AND key = ?3",
    )?;

    let mut rows = stmt.query(params![INSTANCE_NAME, PROFILE_NAME, "auth.http_tokens"])?;
    if let Some(row) = rows.next()? {
        let payload: String = row.get(0)?;
        Ok(parse_tokens(payload))
    } else {
        Ok(vec![])
    }
}

fn parse_tokens(raw: String) -> Vec<String> {
    let trimmed = raw.trim();
    if trimmed.is_empty() {
        return vec![];
    }

    if let Ok(simple) = serde_json::from_str::<Vec<String>>(trimmed) {
        return sanitize_tokens(simple);
    }

    if let Ok(structured) = serde_json::from_str::<Vec<serde_json::Value>>(trimmed) {
        let collected = structured
            .into_iter()
            .filter_map(|v| {
                v.get("token")
                    .and_then(|t| t.as_str())
                    .map(str::to_owned)
            })
            .collect::<Vec<_>>();
        return sanitize_tokens(collected);
    }

    vec![]
}

fn sanitize_tokens(tokens: Vec<String>) -> Vec<String> {
    let mut seen = std::collections::HashSet::new();
    let mut out = Vec::new();

    for token in tokens {
        let trimmed = token.trim().to_string();
        if trimmed.is_empty() || seen.contains(&trimmed) {
            continue;
        }
        seen.insert(trimmed.clone());
        out.push(trimmed);
    }
    out
}

fn resolve_token(mut tokens: Vec<String>) -> Option<String> {
    if let Ok(env_token) = env::var("NUPI_API_TOKEN") {
        let trimmed = env_token.trim().to_string();
        if !trimmed.is_empty() {
            return Some(trimmed);
        }
    }
    if tokens.is_empty() {
        None
    } else {
        tokens.sort();
        Some(tokens[0].clone())
    }
}

fn persisted_language(settings: &settings::ClientSettings) -> Option<String> {
    settings
        .language
        .as_ref()
        .map(|v| v.trim().to_lowercase())
        .filter(|v| !v.is_empty())
}

fn load_bootstrap_config() -> Result<Option<BootstrapConfig>, GrpcClientError> {
    let mut path = home_dir().ok_or(GrpcClientError::MissingHomeDir)?;
    path.push(".nupi");
    path.push("bootstrap.json");

    match fs::read_to_string(&path) {
        Ok(contents) => {
            let cfg: BootstrapConfig = serde_json::from_str(&contents)
                .map_err(|e| GrpcClientError::Config(format!("Invalid bootstrap.json: {e}")))?;
            Ok(Some(cfg))
        }
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => Ok(None),
        Err(err) => Err(GrpcClientError::Io(err)),
    }
}

fn determine_host(binding: &str, tls_enabled: bool) -> String {
    fn loopback_host(tls_enabled: bool) -> String {
        if tls_enabled {
            "localhost".to_string()
        } else {
            "127.0.0.1".to_string()
        }
    }

    match binding.trim().to_lowercase().as_str() {
        "" | "loopback" | "0.0.0.0" | "::" | "lan" | "public" => loopback_host(tls_enabled),
        other => other.to_string(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn validate_metadata_map_accepts_valid() {
        let mut m = HashMap::new();
        m.insert("key1".to_string(), "value1".to_string());
        m.insert("client".to_string(), "desktop".to_string());
        assert!(validate_metadata_map(&m).is_ok());
    }

    #[test]
    fn validate_metadata_map_accepts_empty() {
        let m = HashMap::new();
        assert!(validate_metadata_map(&m).is_ok());
    }

    #[test]
    fn validate_metadata_map_rejects_too_many_entries() {
        let mut m = HashMap::new();
        for i in 0..=MAX_METADATA_ENTRIES {
            m.insert(format!("k{i}"), "v".to_string());
        }
        let err = validate_metadata_map(&m).unwrap_err();
        assert!(err.to_string().contains("entries"));
    }

    #[test]
    fn validate_metadata_map_rejects_long_key() {
        let mut m = HashMap::new();
        let long_key: String = "a".repeat(MAX_METADATA_KEY_CHARS + 1);
        m.insert(long_key, "v".to_string());
        let err = validate_metadata_map(&m).unwrap_err();
        assert!(err.to_string().contains("key"));
    }

    #[test]
    fn validate_metadata_map_rejects_long_value() {
        let mut m = HashMap::new();
        let long_val: String = "b".repeat(MAX_METADATA_VALUE_CHARS + 1);
        m.insert("k".to_string(), long_val);
        let err = validate_metadata_map(&m).unwrap_err();
        assert!(err.to_string().contains("value"));
    }

    #[test]
    fn validate_metadata_map_rejects_total_bytes_exceeded() {
        let mut m = HashMap::new();
        for i in 0..21 {
            m.insert(format!("{i:02}"), "x".repeat(200));
        }
        let err = validate_metadata_map(&m).unwrap_err();
        assert!(err.to_string().contains("total size"));
    }

    #[test]
    fn validate_metadata_map_key_limit_counts_chars_not_bytes() {
        let mut m = HashMap::new();
        let emoji_key: String = "\u{1F600}".repeat(MAX_METADATA_KEY_CHARS);
        m.insert(emoji_key, "v".to_string());
        assert!(validate_metadata_map(&m).is_ok());

        let mut m2 = HashMap::new();
        let long_emoji_key: String = "\u{1F600}".repeat(MAX_METADATA_KEY_CHARS + 1);
        m2.insert(long_emoji_key, "v".to_string());
        assert!(validate_metadata_map(&m2).is_err());
    }

    #[test]
    fn normalize_metadata_map_trims_and_injects_client() {
        let mut m = HashMap::new();
        m.insert("  foo  ".to_string(), "  bar  ".to_string());
        m.insert("".to_string(), "ignored".to_string());
        let result = normalize_metadata_map(m, "test");
        assert_eq!(result.get("foo"), Some(&"bar".to_string()));
        assert_eq!(result.get("client"), Some(&"test".to_string()));
        assert!(!result.contains_key(""));
        assert!(!result.contains_key("  foo  "));
    }

    #[test]
    fn normalize_metadata_map_skips_client_at_max_entries() {
        let mut m = HashMap::new();
        for i in 0..MAX_METADATA_ENTRIES {
            m.insert(format!("k{i}"), "v".to_string());
        }
        let result = normalize_metadata_map(m, "desktop");
        assert_eq!(result.len(), MAX_METADATA_ENTRIES);
        assert!(!result.contains_key("client"));
        assert!(validate_metadata_map(&result).is_ok());
    }

    #[test]
    fn normalize_metadata_map_preserves_existing_client() {
        let mut m = HashMap::new();
        for i in 0..MAX_METADATA_ENTRIES - 1 {
            m.insert(format!("k{i}"), "v".to_string());
        }
        m.insert("client".to_string(), "custom".to_string());
        let result = normalize_metadata_map(m, "desktop");
        assert_eq!(result.len(), MAX_METADATA_ENTRIES);
        assert_eq!(result.get("client"), Some(&"custom".to_string()));
        assert!(validate_metadata_map(&result).is_ok());
    }

    #[test]
    fn format_timestamp_epoch_zero() {
        let ts = prost_types::Timestamp {
            seconds: 0,
            nanos: 0,
        };
        assert_eq!(format_timestamp(ts), "1970-01-01T00:00:00Z");
    }

    #[test]
    fn format_timestamp_with_nanos() {
        let ts = prost_types::Timestamp {
            seconds: 1_700_000_000,
            nanos: 123_000_000,
        };
        let result = format_timestamp(ts);
        assert!(result.starts_with("2023-"));
        assert!(result.contains(".123"));
        assert!(result.ends_with('Z'));
    }

    #[test]
    fn format_timestamp_negative_nanos_clamped() {
        let ts = prost_types::Timestamp {
            seconds: 0,
            nanos: -1,
        };
        // Negative nanos should be clamped to 0
        assert_eq!(format_timestamp(ts), "1970-01-01T00:00:00Z");
    }

    // -----------------------------------------------------------------------
    // parse_tokens / sanitize_tokens / resolve_token
    // -----------------------------------------------------------------------

    #[test]
    fn parse_tokens_empty_string() {
        assert!(parse_tokens("".to_string()).is_empty());
        assert!(parse_tokens("   ".to_string()).is_empty());
    }

    #[test]
    fn parse_tokens_simple_array() {
        let tokens = parse_tokens(r#"["tok1","tok2","tok3"]"#.to_string());
        assert_eq!(tokens, vec!["tok1", "tok2", "tok3"]);
    }

    #[test]
    fn parse_tokens_structured_array() {
        let tokens =
            parse_tokens(r#"[{"token":"abc"},{"token":"def"},{"other":"skip"}]"#.to_string());
        assert_eq!(tokens, vec!["abc", "def"]);
    }

    #[test]
    fn parse_tokens_deduplicates_and_trims() {
        let tokens = parse_tokens(r#"["  tok1  "," tok1","tok2"]"#.to_string());
        assert_eq!(tokens, vec!["tok1", "tok2"]);
    }

    #[test]
    fn parse_tokens_invalid_json() {
        assert!(parse_tokens("not json".to_string()).is_empty());
    }

    #[test]
    fn sanitize_tokens_removes_empty_and_dupes() {
        let input = vec![
            "a".to_string(),
            "  ".to_string(),
            "b".to_string(),
            "a".to_string(),
            "".to_string(),
        ];
        let result = sanitize_tokens(input);
        assert_eq!(result, vec!["a", "b"]);
    }

    #[test]
    fn resolve_token_prefers_env() {
        let tokens = vec!["stored_token".to_string()];
        let result = resolve_token(tokens);
        // If NUPI_API_TOKEN env is set, it takes priority; otherwise stored token is used.
        let env_token = env::var("NUPI_API_TOKEN")
            .ok()
            .map(|v| v.trim().to_string())
            .filter(|v| !v.is_empty());
        match env_token {
            Some(expected) => assert_eq!(result, Some(expected)),
            None => assert_eq!(result, Some("stored_token".to_string())),
        }
    }

    #[test]
    fn resolve_token_empty_returns_none_without_env() {
        let result = resolve_token(vec![]);
        // If NUPI_API_TOKEN env is set, it returns that; otherwise None.
        let env_token = env::var("NUPI_API_TOKEN")
            .ok()
            .map(|v| v.trim().to_string())
            .filter(|v| !v.is_empty());
        match env_token {
            Some(expected) => assert_eq!(result, Some(expected)),
            None => assert_eq!(result, None),
        }
    }

    #[test]
    fn resolve_token_sorts_and_picks_first() {
        let tokens = vec!["charlie".to_string(), "alpha".to_string(), "bravo".to_string()];
        let result = resolve_token(tokens);
        // After sorting: alpha, bravo, charlie → picks "alpha" (unless env overrides).
        let env_token = env::var("NUPI_API_TOKEN")
            .ok()
            .map(|v| v.trim().to_string())
            .filter(|v| !v.is_empty());
        match env_token {
            Some(expected) => assert_eq!(result, Some(expected)),
            None => assert_eq!(result, Some("alpha".to_string())),
        }
    }

    // -----------------------------------------------------------------------
    // determine_host
    // -----------------------------------------------------------------------

    #[test]
    fn determine_host_loopback_no_tls() {
        assert_eq!(determine_host("loopback", false), "127.0.0.1");
        assert_eq!(determine_host("", false), "127.0.0.1");
    }

    #[test]
    fn determine_host_loopback_with_tls() {
        assert_eq!(determine_host("loopback", true), "localhost");
        assert_eq!(determine_host("", true), "localhost");
    }

    #[test]
    fn determine_host_wildcard_bindings() {
        assert_eq!(determine_host("0.0.0.0", false), "127.0.0.1");
        assert_eq!(determine_host("::", false), "127.0.0.1");
        assert_eq!(determine_host("lan", true), "localhost");
        assert_eq!(determine_host("public", true), "localhost");
    }

    #[test]
    fn determine_host_custom_binding() {
        assert_eq!(determine_host("192.168.1.100", false), "192.168.1.100");
        assert_eq!(determine_host("my-host.local", true), "my-host.local");
    }

    #[test]
    fn determine_host_trims_whitespace() {
        assert_eq!(determine_host("  loopback  ", false), "127.0.0.1");
        assert_eq!(determine_host("  192.168.1.1  ", false), "192.168.1.1");
    }
}
