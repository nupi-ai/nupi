use parking_lot::Mutex;
use serde::{Deserialize, Serialize};
use std::fs;
use std::io::{self, Write};
use std::path::PathBuf;

const SETTINGS_DIR: &str = ".nupi/desktop";
const SETTINGS_FILE: &str = "settings.json";

/// Guards concurrent load+modify+save cycles against TOCTOU races.
static SETTINGS_LOCK: Mutex<()> = Mutex::new(());

#[derive(Default, Clone, Serialize, Deserialize, Debug)]
pub struct ClientSettings {
    pub base_url: Option<String>,
    pub api_token: Option<String>,
    pub language: Option<String>,
}

fn settings_path() -> PathBuf {
    let mut dir = dirs::home_dir().unwrap_or_else(|| PathBuf::from("."));
    dir.push(SETTINGS_DIR);
    dir.push(SETTINGS_FILE);
    dir
}

/// Reads settings from disk. This does NOT acquire `SETTINGS_LOCK` — reads are
/// lock-free because `save()` uses atomic rename, so a concurrent `modify()`
/// can never produce a partially-written file. The worst case is a stale read
/// (seeing the old value during a concurrent write), which is acceptable for a
/// desktop app. Use `modify()` for read-modify-write operations.
pub fn load() -> ClientSettings {
    let path = settings_path();
    match fs::read_to_string(&path) {
        Ok(contents) => serde_json::from_str(&contents).unwrap_or_default(),
        Err(_) => ClientSettings::default(),
    }
}

/// Atomically loads settings, applies a mutation, saves, and returns the result.
/// Holds a lock to prevent concurrent read-modify-write races.
pub fn modify<F>(mutate: F) -> io::Result<ClientSettings>
where
    F: FnOnce(&mut ClientSettings),
{
    let _guard = SETTINGS_LOCK.lock();
    let mut settings = load();
    mutate(&mut settings);
    save(&settings)?;
    Ok(settings)
}

/// Writes settings to disk atomically. Prefer `modify()` for read-modify-write
/// operations to avoid TOCTOU races.
fn save(settings: &ClientSettings) -> io::Result<()> {
    let path = settings_path();
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)?;
    }

    let payload =
        serde_json::to_vec_pretty(settings).map_err(|e| io::Error::new(io::ErrorKind::Other, e))?;

    // Atomic write: write to a temporary file, then rename into place.
    // This prevents corruption if the process crashes mid-write.
    let tmp_path = path.with_extension("json.tmp");
    let mut file = fs::File::create(&tmp_path)?;
    file.write_all(&payload)?;
    file.sync_all()?;
    fs::rename(&tmp_path, &path)
}
