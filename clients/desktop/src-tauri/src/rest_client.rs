use std::{
    collections::HashMap,
    env, fs,
    path::PathBuf,
    sync::{
        Arc,
        atomic::{AtomicBool, AtomicU64, Ordering},
    },
    time::Duration,
};

use base64::{Engine as _, engine::general_purpose::STANDARD as BASE64_STANDARD};
use bytes::Bytes;
use dirs::home_dir;
use futures_util::StreamExt as FuturesStreamExt;
use hound::{SampleFormat, WavSpec, WavWriter};
use reqwest::{Certificate, Client, StatusCode};
use rusqlite::{Connection, OpenFlags, params};
use serde::{Deserialize, Serialize};
use serde_json::{Map, Value, json};
use tokio::{
    fs as tokio_fs,
    io::{AsyncBufReadExt, BufReader},
    sync::{mpsc, oneshot, watch},
    task,
};
use tokio_stream::{StreamExt as TokioStreamExt, wrappers::ReceiverStream};
use tokio_util::io::StreamReader;
use url::Url;

use crate::settings;

const INSTANCE_NAME: &str = "default";
const PROFILE_NAME: &str = "default";
const MAX_AUDIO_FILE_SIZE: u64 = 500 * 1024 * 1024;
const DEFAULT_CAPTURE_STREAM_ID: &str = "mic";
const DEFAULT_PLAYBACK_STREAM_ID: &str = "tts";
const SLOT_STT: &str = "stt";
const SLOT_TTS: &str = "tts";
const CANCELLED_MESSAGE: &str = "Voice stream cancelled by user";

#[derive(Debug)]
pub enum RestClientError {
    MissingHomeDir,
    Config(String),
    Io(std::io::Error),
    Sql(rusqlite::Error),
    Http(reqwest::Error),
    Status(StatusCode),
    Json(serde_json::Error),
    Audio(String),
    VoiceNotReady {
        message: String,
        diagnostics: Vec<VoiceDiagnosticSummary>,
    },
}

impl std::fmt::Display for RestClientError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            RestClientError::MissingHomeDir => write!(f, "Unable to determine user home directory"),
            RestClientError::Config(msg) => write!(f, "{msg}"),
            RestClientError::Io(err) => write!(f, "{err}"),
            RestClientError::Sql(err) => write!(f, "{err}"),
            RestClientError::Http(err) => write!(f, "{err}"),
            RestClientError::Status(code) => write!(f, "HTTP request failed with status {code}"),
            RestClientError::Json(err) => write!(f, "{err}"),
            RestClientError::Audio(err) => write!(f, "{err}"),
            RestClientError::VoiceNotReady {
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
                    write!(f, "{} ({})", message, joined)
                }
            }
        }
    }
}

impl std::error::Error for RestClientError {}

impl From<std::io::Error> for RestClientError {
    fn from(value: std::io::Error) -> Self {
        RestClientError::Io(value)
    }
}

impl From<rusqlite::Error> for RestClientError {
    fn from(value: rusqlite::Error) -> Self {
        RestClientError::Sql(value)
    }
}

impl From<reqwest::Error> for RestClientError {
    fn from(value: reqwest::Error) -> Self {
        RestClientError::Http(value)
    }
}

impl From<serde_json::Error> for RestClientError {
    fn from(value: serde_json::Error) -> Self {
        RestClientError::Json(value)
    }
}

impl From<hound::Error> for RestClientError {
    fn from(value: hound::Error) -> Self {
        RestClientError::Audio(value.to_string())
    }
}

impl From<base64::DecodeError> for RestClientError {
    fn from(value: base64::DecodeError) -> Self {
        RestClientError::Audio(value.to_string())
    }
}

#[derive(Debug, Clone)]
struct TransportSnapshot {
    port: i32,
    binding: String,
    tls_cert_path: Option<String>,
    tls_key_path: Option<String>,
    tokens: Vec<String>,
}

#[allow(dead_code)]
#[derive(Debug, Deserialize)]
pub struct DaemonStatus {
    pub version: Option<String>,
    #[serde(default)]
    pub sessions_count: Option<u64>,
    #[serde(default)]
    pub port: u16,
    #[serde(default)]
    pub grpc_port: Option<u16>,
    #[serde(default)]
    pub binding: Option<String>,
    #[serde(default)]
    pub grpc_binding: Option<String>,
    #[serde(default)]
    pub auth_required: Option<bool>,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct TokenEntry {
    pub id: String,
    #[serde(default)]
    pub name: Option<String>,
    pub role: String,
    #[serde(default)]
    pub masked_token: Option<String>,
    #[serde(default)]
    pub created_at: Option<String>,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct TokenResponse {
    pub tokens: Vec<TokenEntry>,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct CreatedToken {
    pub token: String,
    pub id: String,
    #[serde(default)]
    pub name: Option<String>,
    pub role: String,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct PairingEntry {
    pub code: String,
    #[serde(default)]
    pub name: Option<String>,
    pub role: String,
    #[serde(default)]
    pub expires_at: Option<String>,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct PairingList {
    pub pairings: Vec<PairingEntry>,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct CreatedPairing {
    pub pair_code: String,
    #[serde(default)]
    pub name: Option<String>,
    pub role: String,
    #[serde(default)]
    pub expires_at: Option<String>,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct ClaimedToken {
    pub token: String,
    #[serde(default)]
    pub name: Option<String>,
    pub role: String,
    #[serde(default)]
    pub created_at: Option<String>,
}

#[derive(Clone)]
pub struct RestClient {
    inner: Client,
    base_url: String,
    token: Option<String>,
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
    #[serde(skip_serializing_if = "std::collections::HashMap::is_empty", default)]
    pub metadata: HashMap<String, String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AudioFormatSummary {
    pub encoding: String,
    pub sample_rate: u32,
    pub channels: u16,
    pub bit_depth: u16,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub frame_ms: Option<u32>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AudioCapabilitySummary {
    pub stream_id: String,
    pub format: AudioFormatSummary,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub metadata: Option<HashMap<String, String>>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct VoiceDiagnosticSummary {
    pub slot: String,
    pub code: String,
    pub message: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AudioCapabilitiesSummary {
    pub capture: Vec<AudioCapabilitySummary>,
    pub playback: Vec<AudioCapabilitySummary>,
    #[serde(default)]
    pub capture_enabled: bool,
    #[serde(default)]
    pub playback_enabled: bool,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub diagnostics: Vec<VoiceDiagnosticSummary>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub message: Option<String>,
}

#[derive(Debug, Deserialize)]
struct VoiceErrorPayload {
    #[serde(default)]
    error: Option<String>,
    #[serde(default)]
    diagnostics: Vec<VoiceDiagnosticSummary>,
    #[serde(default)]
    capture_ready: Option<bool>,
    #[serde(default)]
    playback_ready: Option<bool>,
}

/// Picks the most relevant diagnostic message for a logical slot, falling back
/// to the first diagnostic when slot-specific entries are unavailable.
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

#[derive(Debug)]
struct PlaybackMetrics {
    chunks: u64,
    bytes: u64,
    saved_path: Option<String>,
}

#[derive(Debug, Deserialize)]
#[allow(dead_code)]
struct AudioEgressHttpChunk {
    #[serde(default)]
    format: Option<AudioFormatPayload>,
    #[serde(default)]
    sequence: u64,
    #[serde(default)]
    duration_ms: Option<u32>,
    #[serde(rename = "final", default)]
    final_flag: bool,
    #[serde(default)]
    data: Option<String>,
    #[serde(default)]
    metadata: Option<HashMap<String, String>>,
    #[serde(default)]
    error: Option<String>,
}

#[derive(Debug, Deserialize)]
struct AudioFormatPayload {
    #[serde(default)]
    encoding: Option<String>,
    #[serde(default)]
    sample_rate: Option<u32>,
    #[serde(default)]
    channels: Option<u16>,
    #[serde(default)]
    bit_depth: Option<u16>,
    #[serde(default, rename = "frame_ms")]
    frame_ms: Option<u32>,
    #[serde(default, rename = "frame_duration_ms")]
    frame_duration_ms: Option<u32>,
}

impl AudioFormatPayload {
    fn to_summary(&self) -> AudioFormatSummary {
        AudioFormatSummary {
            encoding: self
                .encoding
                .clone()
                .unwrap_or_else(|| "pcm_s16le".to_string()),
            sample_rate: self.sample_rate.unwrap_or(16_000),
            channels: self.channels.unwrap_or(1),
            bit_depth: self.bit_depth.unwrap_or(16),
            frame_ms: self.frame_ms.or(self.frame_duration_ms),
        }
    }
}

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
    normalized
        .entry("client".to_string())
        .or_insert_with(|| client_label.to_string());
    normalized
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

struct WavUpload {
    format: AudioFormatSummary,
    body: reqwest::Body,
    bytes: Arc<AtomicU64>,
    task: task::JoinHandle<Result<(), RestClientError>>,
}

async fn prepare_wav_upload(
    path: &PathBuf,
    cancel_flag: Arc<AtomicBool>,
) -> Result<WavUpload, RestClientError> {
    let path_clone = path.clone();
    let (tx, rx) = mpsc::channel::<Result<Vec<u8>, RestClientError>>(8);
    let (format_tx, format_rx) = oneshot::channel::<Result<AudioFormatSummary, RestClientError>>();
    let bytes_counter = Arc::new(AtomicU64::new(0));
    let counter_clone = Arc::clone(&bytes_counter);

    let cancel_flag_writer = Arc::clone(&cancel_flag);
    let task = task::spawn_blocking(move || -> Result<(), RestClientError> {
        if cancel_flag_writer.load(Ordering::SeqCst) {
            return Err(RestClientError::Audio(CANCELLED_MESSAGE.to_string()));
        }

        let mut reader = match hound::WavReader::open(&path_clone) {
            Ok(reader) => reader,
            Err(err) => {
                let message = err.to_string();
                let _ = format_tx.send(Err(RestClientError::Audio(message.clone())));
                return Err(RestClientError::Audio(message));
            }
        };

        if cancel_flag_writer.load(Ordering::SeqCst) {
            return Err(RestClientError::Audio(CANCELLED_MESSAGE.to_string()));
        }

        let spec = reader.spec();
        if spec.sample_format != SampleFormat::Int || spec.bits_per_sample != 16 {
            let message = "Only 16-bit PCM WAV files are supported for voice streaming".to_string();
            let _ = format_tx.send(Err(RestClientError::Config(message.clone())));
            return Err(RestClientError::Config(message));
        }

        if cancel_flag_writer.load(Ordering::SeqCst) {
            return Err(RestClientError::Audio(CANCELLED_MESSAGE.to_string()));
        }

        let format = AudioFormatSummary {
            encoding: "pcm_s16le".to_string(),
            sample_rate: spec.sample_rate,
            channels: spec.channels,
            bit_depth: spec.bits_per_sample,
            frame_ms: None,
        };

        if format_tx.send(Ok(format)).is_err() {
            return Ok(());
        }

        let mut buffer = Vec::with_capacity(64 * 1024);
        for sample in reader.samples::<i16>() {
            if cancel_flag_writer.load(Ordering::SeqCst) {
                let _ =
                    tx.blocking_send(Err(RestClientError::Audio(CANCELLED_MESSAGE.to_string())));
                return Err(RestClientError::Audio(CANCELLED_MESSAGE.to_string()));
            }
            let sample = match sample {
                Ok(value) => value,
                Err(err) => {
                    let message = err.to_string();
                    let _ = tx.blocking_send(Err(RestClientError::Audio(message.clone())));
                    return Err(RestClientError::Audio(message));
                }
            };
            buffer.extend_from_slice(&sample.to_le_bytes());
            if buffer.len() >= 64 * 1024 {
                counter_clone.fetch_add(buffer.len() as u64, Ordering::SeqCst);
                if tx.blocking_send(Ok(buffer)).is_err() {
                    return Ok(());
                }
                buffer = Vec::with_capacity(64 * 1024);
            }
        }

        if !buffer.is_empty() {
            counter_clone.fetch_add(buffer.len() as u64, Ordering::SeqCst);
            let _ = tx.blocking_send(Ok(buffer));
        }

        Ok(())
    });

    let format = match format_rx.await {
        Ok(result) => result?,
        Err(_) => {
            let join_result = task
                .await
                .map_err(|err| RestClientError::Audio(err.to_string()))?;
            return match join_result {
                Ok(()) => Err(RestClientError::Audio("Failed to parse WAV header".into())),
                Err(err) => Err(err),
            };
        }
    };

    let stream = TokioStreamExt::map(ReceiverStream::new(rx), |chunk| chunk.map(Bytes::from));
    let body = reqwest::Body::wrap_stream(stream);

    Ok(WavUpload {
        format,
        body,
        bytes: bytes_counter,
        task,
    })
}

async fn wait_for_cancel_signal(
    signal: &mut watch::Receiver<bool>,
    flag: &Arc<AtomicBool>,
) -> bool {
    if *signal.borrow() {
        flag.store(true, Ordering::SeqCst);
        return true;
    }
    while signal.changed().await.is_ok() {
        if *signal.borrow() {
            flag.store(true, Ordering::SeqCst);
            return true;
        }
    }
    false
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

impl RestClient {
    fn require_token(&self) -> Result<String, RestClientError> {
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

        Err(RestClientError::Config(
            "API token is required for this operation".into(),
        ))
    }

    pub async fn discover() -> Result<Self, RestClientError> {
        let persisted = settings::load();

        if let Ok(raw) = env::var("NUPI_BASE_URL") {
            if !raw.trim().is_empty() {
                return Self::from_explicit(raw.trim(), persisted.api_token.clone(), None);
            }
        }

        if let Some(base) = persisted.base_url.as_ref() {
            if !base.trim().is_empty() {
                return Self::from_explicit(base.trim(), persisted.api_token.clone(), None);
            }
        }

        if let Some(bootstrap) = load_bootstrap_config()? {
            if let Some(base) = bootstrap
                .base_url
                .as_ref()
                .map(|value| value.trim())
                .filter(|value| !value.is_empty())
            {
                let fallback_token = bootstrap
                    .api_token
                    .as_ref()
                    .map(|value| value.trim().to_string())
                    .filter(|value| !value.is_empty());

                return Self::from_explicit(base, fallback_token, bootstrap.tls.as_ref());
            }
        }

        let snapshot = load_transport_snapshot()?;

        if snapshot.port <= 0 {
            return Err(RestClientError::Config(
                "Daemon HTTP port is not configured. Start the daemon once or run quickstart."
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

        let mut host = determine_host(snapshot.binding.as_str(), tls_enabled);
        if let Ok(override_host) = env::var("NUPI_DAEMON_HOST") {
            if !override_host.trim().is_empty() {
                host = override_host.trim().to_string();
            }
        }

        let scheme = if tls_enabled { "https" } else { "http" };
        let base_url = format!("{scheme}://{host}:{}", snapshot.port);

        let mut builder = Client::builder().timeout(Duration::from_secs(10));

        let insecure = env::var("NUPI_TLS_INSECURE")
            .ok()
            .map(|v| v == "1")
            .unwrap_or(false);

        if insecure {
            builder = builder.danger_accept_invalid_certs(true);
        } else if tls_enabled {
            let cert_path =
                PathBuf::from(snapshot.tls_cert_path.as_ref().ok_or_else(|| {
                    RestClientError::Config("TLS certificate path not set".into())
                })?);
            let pem = fs::read(cert_path)?;
            let cert = Certificate::from_pem(&pem)?;
            builder = builder.add_root_certificate(cert);
        }

        let inner = builder.build()?;
        let mut token = resolve_token(snapshot.tokens);
        if token.is_none() {
            token = persisted
                .api_token
                .map(|value| value.trim().to_string())
                .filter(|s| !s.is_empty());
        }

        Ok(Self {
            inner,
            base_url: base_url.trim_end_matches('/').to_string(),
            token,
        })
    }

    fn from_explicit(
        raw: &str,
        default_token: Option<String>,
        tls_overrides: Option<&BootstrapTLS>,
    ) -> Result<Self, RestClientError> {
        let mut url_str = raw.to_string();
        if !url_str.contains("://") {
            url_str = format!("https://{url_str}");
        }

        let url = Url::parse(&url_str)
            .map_err(|err| RestClientError::Config(format!("Invalid NUPI_BASE_URL ({err})")))?;

        if url.host_str().is_none() {
            return Err(RestClientError::Config(
                "NUPI_BASE_URL must include host".into(),
            ));
        }

        let mut builder = Client::builder().timeout(Duration::from_secs(10));

        let insecure_env = env::var("NUPI_TLS_INSECURE")
            .ok()
            .map(|v| v == "1")
            .unwrap_or(false);

        let insecure_override = tls_overrides.and_then(|tls| tls.insecure).unwrap_or(false);

        let insecure = insecure_env || insecure_override;

        if insecure {
            builder = builder
                .danger_accept_invalid_certs(true)
                .danger_accept_invalid_hostnames(true);
        } else {
            let ca_from_env = env::var("NUPI_TLS_CA_CERT")
                .ok()
                .map(|v| v.trim().to_string())
                .filter(|v| !v.is_empty());

            let ca_path = ca_from_env.or_else(|| {
                tls_overrides
                    .and_then(|tls| tls.ca_cert_path.as_ref())
                    .map(|value| value.trim().to_string())
                    .filter(|value| !value.is_empty())
            });

            if let Some(path) = ca_path {
                let pem = fs::read(&path)?;
                let cert = Certificate::from_pem(&pem)?;
                builder = builder.add_root_certificate(cert);
            }
        }

        let inner = builder.build()?;
        let env_token = env::var("NUPI_API_TOKEN")
            .ok()
            .map(|t| t.trim().to_string())
            .filter(|t| !t.is_empty());
        let fallback_token = default_token
            .map(|value| value.trim().to_string())
            .filter(|value| !value.is_empty());
        let token = env_token.or(fallback_token);

        Ok(Self {
            inner,
            base_url: url.as_str().trim_end_matches('/').to_string(),
            token,
        })
    }

    pub async fn daemon_status(&self) -> Result<DaemonStatus, RestClientError> {
        let url = format!("{}/daemon/status", self.base_url);
        let mut request = self.inner.get(url);
        if let Some(token) = &self.token {
            request = request.bearer_auth(token);
        }

        let response = request.send().await?;

        if response.status().is_success() {
            let status = response.json::<DaemonStatus>().await?;
            Ok(status)
        } else if response.status() == StatusCode::UNAUTHORIZED {
            Err(RestClientError::Status(StatusCode::UNAUTHORIZED))
        } else {
            Err(RestClientError::Status(response.status()))
        }
    }

    pub async fn is_reachable(&self) -> bool {
        match self.daemon_status().await {
            Ok(_) => true,
            Err(RestClientError::Status(StatusCode::UNAUTHORIZED)) => true,
            Err(_) => false,
        }
    }

    pub async fn shutdown_daemon(&self) -> Result<(), RestClientError> {
        let url = format!("{}/daemon/shutdown", self.base_url);
        let mut request = self.inner.post(url);
        if let Some(token) = &self.token {
            request = request.bearer_auth(token);
        }

        let response = request.send().await?;
        match response.status() {
            StatusCode::ACCEPTED => Ok(()),
            status => Err(RestClientError::Status(status)),
        }
    }

    pub async fn list_tokens(&self) -> Result<Vec<TokenEntry>, RestClientError> {
        let token = self.require_token()?;
        let url = format!("{}/auth/tokens", self.base_url);
        let response = self.inner.get(url).bearer_auth(token).send().await?;
        if !response.status().is_success() {
            return Err(RestClientError::Status(response.status()));
        }
        let payload = response.json::<TokenResponse>().await?;
        Ok(payload.tokens)
    }

    pub async fn create_token(
        &self,
        name: Option<String>,
        role: Option<String>,
    ) -> Result<CreatedToken, RestClientError> {
        let token = self.require_token()?;
        let url = format!("{}/auth/tokens", self.base_url);
        let payload = json!({
            "name": name.unwrap_or_default(),
            "role": role.unwrap_or_else(|| "admin".into()),
        });
        let response = self
            .inner
            .post(url)
            .bearer_auth(token)
            .json(&payload)
            .send()
            .await?;
        if !response.status().is_success() {
            return Err(RestClientError::Status(response.status()));
        }
        Ok(response.json::<CreatedToken>().await?)
    }

    pub async fn delete_token(
        &self,
        id: Option<String>,
        token_value: Option<String>,
    ) -> Result<(), RestClientError> {
        let token = self.require_token()?;
        let url = format!("{}/auth/tokens", self.base_url);
        let mut payload = Map::new();
        if let Some(value) = id.and_then(|v| {
            let trimmed = v.trim().to_string();
            if trimmed.is_empty() {
                None
            } else {
                Some(trimmed)
            }
        }) {
            payload.insert("id".to_string(), Value::String(value));
        }
        if let Some(value) = token_value.and_then(|v| {
            let trimmed = v.trim().to_string();
            if trimmed.is_empty() {
                None
            } else {
                Some(trimmed)
            }
        }) {
            payload.insert("token".to_string(), Value::String(value));
        }
        if payload.is_empty() {
            return Err(RestClientError::Config(
                "Provide token string or id to delete".into(),
            ));
        }
        let response = self
            .inner
            .delete(url)
            .bearer_auth(token)
            .json(&Value::Object(payload))
            .send()
            .await?;
        if response.status().is_success() || response.status() == StatusCode::NO_CONTENT {
            Ok(())
        } else {
            Err(RestClientError::Status(response.status()))
        }
    }

    pub async fn list_pairings(&self) -> Result<Vec<PairingEntry>, RestClientError> {
        let token = self.require_token()?;
        let url = format!("{}/auth/pairings", self.base_url);
        let response = self.inner.get(url).bearer_auth(token).send().await?;
        if !response.status().is_success() {
            return Err(RestClientError::Status(response.status()));
        }
        let payload = response.json::<PairingList>().await?;
        Ok(payload.pairings)
    }

    pub async fn create_pairing(
        &self,
        name: Option<String>,
        role: Option<String>,
        expires_in: Option<u32>,
    ) -> Result<CreatedPairing, RestClientError> {
        let token = self.require_token()?;
        let url = format!("{}/auth/pairings", self.base_url);
        let payload = json!({
            "name": name.unwrap_or_default(),
            "role": role.unwrap_or_else(|| "read-only".into()),
            "expires_in_seconds": expires_in.unwrap_or(300),
        });
        let response = self
            .inner
            .post(url)
            .bearer_auth(token)
            .json(&payload)
            .send()
            .await?;
        if !response.status().is_success() {
            return Err(RestClientError::Status(response.status()));
        }
        Ok(response.json::<CreatedPairing>().await?)
    }
    pub async fn voice_stream_from_file(
        &self,
        req: VoiceStreamRequest,
        abort: Option<watch::Receiver<bool>>,
    ) -> Result<VoiceStreamSummary, RestClientError> {
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
            return Err(RestClientError::Config("session_id is required".into()));
        }

        let stream_id_value = stream_id
            .as_ref()
            .map(|value| value.trim())
            .filter(|value| !value.is_empty())
            .map(|value| value.to_string())
            .unwrap_or_else(|| DEFAULT_CAPTURE_STREAM_ID.to_string());

        let ingress_source = input_path.display().to_string();

        let file_meta = tokio_fs::metadata(&input_path).await?;
        if file_meta.len() > MAX_AUDIO_FILE_SIZE {
            return Err(RestClientError::Config(format!(
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
            return Err(RestClientError::Config(
                "Only WAV files are currently supported for voice streaming".into(),
            ));
        }

        let metadata_norm = normalize_metadata_map(metadata, "desktop");
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
            return Err(RestClientError::VoiceNotReady {
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
            return Err(RestClientError::VoiceNotReady {
                message,
                diagnostics: capabilities.diagnostics.clone(),
            });
        }

        let cancel_flag = Arc::new(AtomicBool::new(false));

        let WavUpload {
            format,
            body,
            bytes: upload_bytes,
            task: upload_task,
        } = prepare_wav_upload(&input_path, Arc::clone(&cancel_flag)).await?;

        let mut url = Url::parse(&format!("{}/audio/ingress", self.base_url))
            .map_err(|err| RestClientError::Config(format!("Invalid base URL: {err}")))?;
        {
            let mut pairs = url.query_pairs_mut();
            pairs.append_pair("session_id", trimmed_session);
            pairs.append_pair("stream_id", &stream_id_value);
            pairs.append_pair("encoding", &format.encoding);
            pairs.append_pair("sample_rate", &format.sample_rate.to_string());
            pairs.append_pair("channels", &format.channels.to_string());
            pairs.append_pair("bit_depth", &format.bit_depth.to_string());
            if let Some(frame_ms) = format.frame_ms {
                pairs.append_pair("frame_ms", &frame_ms.to_string());
            }
            if !metadata_norm.is_empty() {
                pairs.append_pair("metadata", &serde_json::to_string(&metadata_norm)?);
            }
        }

        let mut request = self.inner.post(url).timeout(Duration::from_secs(600));
        if let Some(token) = &self.token {
            request = request.bearer_auth(token);
        }
        let request = request
            .header("Content-Type", "application/octet-stream")
            .body(body);

        let mut send_future = Box::pin(request.send());
        let response = if let Some(mut cancel_signal) = abort.clone() {
            loop {
                tokio::select! {
                    res = &mut send_future => break res?,
                    cancelled = wait_for_cancel_signal(&mut cancel_signal, &cancel_flag) => {
                        if cancelled {
                            let _ = upload_task.await;
                            return Err(RestClientError::Audio(CANCELLED_MESSAGE.into()));
                        } else {
                            break send_future.await?;
                        }
                    }
                }
            }
        } else {
            send_future.await?
        };
        if response.status() != StatusCode::NO_CONTENT {
            return Err(RestClientError::Status(response.status()));
        }

        let upload_status = upload_task
            .await
            .map_err(|err| RestClientError::Audio(err.to_string()))?;
        if let Err(err) = upload_status {
            return Err(err);
        }

        if cancel_flag.load(Ordering::SeqCst) {
            return Err(RestClientError::Audio(CANCELLED_MESSAGE.into()));
        }

        let bytes_uploaded = upload_bytes.load(Ordering::SeqCst);

        self.finalize_voice_stream(
            trimmed_session,
            stream_id_value,
            ingress_source,
            playback_output,
            subscribe_playback,
            abort,
            cancel_flag,
            bytes_uploaded,
        )
        .await
    }

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
    ) -> Result<VoiceStreamSummary, RestClientError> {
        let mut playback_error: Option<String> = None;
        let playback_metrics = if subscribe_playback {
            if cancel_flag.load(Ordering::SeqCst) {
                return Err(RestClientError::Audio(CANCELLED_MESSAGE.into()));
            }
            match abort.take() {
                Some(mut cancel_signal) => {
                    let mut capture_future = Box::pin(self.capture_playback(
                        session_id,
                        &stream_id,
                        playback_output.clone(),
                    ));
                    let result = loop {
                        tokio::select! {
                            res = &mut capture_future => break res,
                            cancelled = wait_for_cancel_signal(&mut cancel_signal, &cancel_flag) => {
                                if cancelled {
                                    return Err(RestClientError::Audio(CANCELLED_MESSAGE.into()));
                                } else {
                                    break capture_future.await;
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
            return Err(RestClientError::Audio(CANCELLED_MESSAGE.into()));
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

    pub async fn voice_interrupt(
        &self,
        session_id: &str,
        stream_id: Option<&str>,
        reason: Option<&str>,
        metadata: HashMap<String, String>,
    ) -> Result<VoiceInterruptSummary, RestClientError> {
        let trimmed_session = session_id.trim();
        if trimmed_session.is_empty() {
            return Err(RestClientError::Config("session_id is required".into()));
        }

        let stream_value = stream_id
            .and_then(|value| {
                let trimmed = value.trim();
                if trimmed.is_empty() {
                    None
                } else {
                    Some(trimmed.to_string())
                }
            })
            .unwrap_or_else(|| DEFAULT_PLAYBACK_STREAM_ID.to_string());

        let reason_value = reason.and_then(|value| {
            let trimmed = value.trim();
            if trimmed.is_empty() {
                None
            } else {
                Some(trimmed.to_string())
            }
        });

        let metadata_norm = normalize_metadata_map(metadata, "desktop");

        let capabilities = self.audio_capabilities(Some(trimmed_session)).await?;
        if !capabilities.playback_enabled {
            let message = capabilities.message.clone().unwrap_or_else(|| {
                primary_diagnostic_message(
                    &capabilities.diagnostics,
                    SLOT_TTS,
                    "Voice playback is disabled",
                )
            });
            return Err(RestClientError::VoiceNotReady {
                message,
                diagnostics: capabilities.diagnostics.clone(),
            });
        }

        let mut body = json!({
            "session_id": trimmed_session,
        });
        if !stream_value.is_empty() {
            body["stream_id"] = stream_value.clone().into();
        }
        if let Some(reason) = reason_value.as_ref() {
            body["reason"] = reason.clone().into();
        }
        if !metadata_norm.is_empty() {
            body["metadata"] = serde_json::to_value(&metadata_norm)?;
        }

        let mut request = self
            .inner
            .post(format!("{}/audio/interrupt", self.base_url));
        if let Some(token) = &self.token {
            request = request.bearer_auth(token);
        }
        let response = request.json(&body).send().await?;
        if response.status() != StatusCode::NO_CONTENT {
            return Err(RestClientError::Status(response.status()));
        }

        Ok(VoiceInterruptSummary {
            session_id: trimmed_session.to_string(),
            stream_id: stream_value,
            reason: reason_value,
            metadata: metadata_norm,
        })
    }

    pub async fn audio_capabilities(
        &self,
        session_id: Option<&str>,
    ) -> Result<AudioCapabilitiesSummary, RestClientError> {
        let mut url = Url::parse(&format!("{}/audio/capabilities", self.base_url))
            .map_err(|err| RestClientError::Config(format!("Invalid base URL: {err}")))?;
        if let Some(session) = session_id.and_then(|value| {
            let trimmed = value.trim();
            if trimmed.is_empty() {
                None
            } else {
                Some(trimmed.to_string())
            }
        }) {
            url.query_pairs_mut().append_pair("session_id", &session);
        }

        let mut request = self.inner.get(url).timeout(Duration::from_secs(10));
        if let Some(token) = &self.token {
            request = request.bearer_auth(token);
        }
        let response = request.send().await?;
        let status = response.status();
        if status == StatusCode::PRECONDITION_FAILED {
            let payload = response.json::<VoiceErrorPayload>().await?;
            return Ok(AudioCapabilitiesSummary {
                capture: Vec::new(),
                playback: Vec::new(),
                capture_enabled: payload.capture_ready.unwrap_or(false),
                playback_enabled: payload.playback_ready.unwrap_or(false),
                diagnostics: payload.diagnostics,
                message: payload.error,
            });
        }
        if !status.is_success() {
            return Err(RestClientError::Status(status));
        }
        response
            .json::<AudioCapabilitiesSummary>()
            .await
            .map_err(Into::into)
    }

    async fn capture_playback(
        &self,
        session_id: &str,
        stream_id: &str,
        playback_output: Option<PathBuf>,
    ) -> Result<PlaybackMetrics, RestClientError> {
        let mut url = Url::parse(&format!("{}/audio/egress", self.base_url))
            .map_err(|err| RestClientError::Config(format!("Invalid base URL: {err}")))?;
        {
            let mut pairs = url.query_pairs_mut();
            pairs.append_pair("session_id", session_id);
            if !stream_id.is_empty() {
                pairs.append_pair("stream_id", stream_id);
            }
        }

        let mut request = self.inner.get(url).timeout(Duration::from_secs(600));
        if let Some(token) = &self.token {
            request = request.bearer_auth(token);
        }
        let response = request.send().await?;
        if !response.status().is_success() {
            return Err(RestClientError::Status(response.status()));
        }

        let byte_stream = FuturesStreamExt::map(response.bytes_stream(), |result| {
            result.map_err(|err| std::io::Error::new(std::io::ErrorKind::Other, err))
        });
        let mut reader = BufReader::new(StreamReader::new(byte_stream));

        let mut buffer = String::new();
        let mut chunks = 0u64;
        let mut bytes = 0u64;
        let mut format_summary: Option<AudioFormatSummary> = None;
        let mut wav_writer: Option<WavWriter<std::io::BufWriter<std::fs::File>>> = None;
        let mut playback_path = playback_output;

        loop {
            buffer.clear();
            let read = reader.read_line(&mut buffer).await?;
            if read == 0 {
                break;
            }
            let line = buffer.trim();
            if line.is_empty() {
                continue;
            }

            let chunk: AudioEgressHttpChunk = serde_json::from_str(line)?;
            if let Some(err) = chunk.error.as_ref() {
                if !err.trim().is_empty() {
                    return Err(RestClientError::Audio(err.clone()));
                }
            }

            if format_summary.is_none() {
                if let Some(payload) = chunk.format.as_ref() {
                    format_summary = Some(payload.to_summary());
                }
            }

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

            if let Some(ref data_b64) = chunk.data {
                let decoded = BASE64_STANDARD.decode(data_b64)?;
                let decoded_len = decoded.len() as u64;
                let projected = bytes + decoded_len;
                if projected > MAX_AUDIO_FILE_SIZE {
                    return Err(RestClientError::Audio(format!(
                        "Playback exceeded {} bytes limit",
                        MAX_AUDIO_FILE_SIZE
                    )));
                }
                bytes = projected;
                if let Some(writer) = wav_writer.as_mut() {
                    if decoded.len() % 2 != 0 {
                        return Err(RestClientError::Audio(
                            "playback PCM payload has odd length".into(),
                        ));
                    }
                    for sample_bytes in decoded.chunks_exact(2) {
                        let sample = i16::from_le_bytes([sample_bytes[0], sample_bytes[1]]);
                        writer.write_sample(sample)?;
                    }
                }
                if !decoded.is_empty() {
                    chunks += 1;
                }
            }

            if chunk.final_flag {
                break;
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
}

pub async fn claim_pairing_token(
    base_url: &str,
    code: &str,
    name: Option<String>,
    insecure: bool,
) -> Result<ClaimedToken, RestClientError> {
    let mut builder = Client::builder().timeout(Duration::from_secs(10));
    if insecure {
        builder = builder
            .danger_accept_invalid_certs(true)
            .danger_accept_invalid_hostnames(true);
    }

    let client = builder.build()?;
    let url = format!("{}/auth/pair", base_url.trim_end_matches('/'));
    let payload = json!({
        "code": code,
        "name": name.unwrap_or_default(),
    });

    let response = client.post(url).json(&payload).send().await?;
    if !response.status().is_success() {
        return Err(RestClientError::Status(response.status()));
    }

    Ok(response.json::<ClaimedToken>().await?)
}

fn load_transport_snapshot() -> Result<TransportSnapshot, RestClientError> {
    let home = home_dir().ok_or(RestClientError::MissingHomeDir)?;

    let db_path = home
        .join(".nupi")
        .join("instances")
        .join(INSTANCE_NAME)
        .join("config.db");

    if !db_path.exists() {
        return Err(RestClientError::Config(format!(
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
    let port = settings
        .get("transport.http_port")
        .and_then(|value| value.parse::<i32>().ok())
        .unwrap_or_default();

    let binding = settings
        .get("transport.binding")
        .cloned()
        .unwrap_or_else(|| "loopback".to_string());

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
        port,
        binding,
        tls_cert_path,
        tls_key_path,
        tokens,
    })
}

fn apply_pragmas(conn: &Connection) -> Result<(), RestClientError> {
    conn.pragma_update(None, "busy_timeout", &5000)?;
    conn.pragma_update(None, "foreign_keys", &1)?;
    Ok(())
}

fn load_settings(conn: &Connection) -> Result<HashMap<String, String>, RestClientError> {
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

fn load_tokens(conn: &Connection) -> Result<Vec<String>, RestClientError> {
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
            .filter_map(|value| {
                value
                    .get("token")
                    .and_then(|v| v.as_str())
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

fn load_bootstrap_config() -> Result<Option<BootstrapConfig>, RestClientError> {
    let mut path = home_dir().ok_or(RestClientError::MissingHomeDir)?;
    path.push(".nupi");
    path.push("bootstrap.json");

    match fs::read_to_string(&path) {
        Ok(contents) => {
            let cfg: BootstrapConfig =
                serde_json::from_str(&contents).map_err(RestClientError::Json)?;
            Ok(Some(cfg))
        }
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => Ok(None),
        Err(err) => Err(RestClientError::Io(err)),
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
