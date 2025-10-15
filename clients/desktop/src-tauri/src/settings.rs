use serde::{Deserialize, Serialize};
use std::fs;
use std::io::{self, Write};
use std::path::PathBuf;

const SETTINGS_DIR: &str = ".nupi/desktop";
const SETTINGS_FILE: &str = "settings.json";

#[derive(Default, Clone, Serialize, Deserialize, Debug)]
pub struct ClientSettings {
    pub base_url: Option<String>,
    pub api_token: Option<String>,
}

fn settings_path() -> PathBuf {
    let mut dir = dirs::home_dir().unwrap_or_else(|| PathBuf::from("."));
    dir.push(SETTINGS_DIR);
    dir.push(SETTINGS_FILE);
    dir
}

pub fn load() -> ClientSettings {
    let path = settings_path();
    match fs::read_to_string(&path) {
        Ok(contents) => serde_json::from_str(&contents).unwrap_or_default(),
        Err(_) => ClientSettings::default(),
    }
}

pub fn save(settings: &ClientSettings) -> io::Result<()> {
    let path = settings_path();
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)?;
    }

    let payload = serde_json::to_vec_pretty(settings).unwrap_or_else(|_| b"{}".to_vec());
    let mut file = fs::File::create(path)?;
    file.write_all(&payload)
}
