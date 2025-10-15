use std::{collections::HashMap, env, fs, path::PathBuf, time::Duration};

use dirs::home_dir;
use reqwest::{Certificate, Client, StatusCode};
use rusqlite::{params, Connection, OpenFlags};
use serde::{Deserialize, Serialize};
use serde_json::{json, Map, Value};
use url::Url;

use crate::settings;

const INSTANCE_NAME: &str = "default";
const PROFILE_NAME: &str = "default";

#[derive(Debug)]
pub enum RestClientError {
    MissingHomeDir,
    Config(String),
    Io(std::io::Error),
    Sql(rusqlite::Error),
    Http(reqwest::Error),
    Status(StatusCode),
    Json(serde_json::Error),
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

pub struct RestClient {
    inner: Client,
    base_url: String,
    token: Option<String>,
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
    match binding.trim().to_lowercase().as_str() {
        "" | "loopback" => {
            if tls_enabled {
                "localhost".to_string()
            } else {
                "127.0.0.1".to_string()
            }
        }
        _ => {
            if tls_enabled {
                "localhost".to_string()
            } else {
                "127.0.0.1".to_string()
            }
        }
    }
}
