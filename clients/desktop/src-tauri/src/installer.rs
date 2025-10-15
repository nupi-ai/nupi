use std::env;
use std::fs;
use std::io::Write;
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};

// Install to user's home directory - no sudo required!
fn get_install_dir() -> PathBuf {
    let home = env::var("HOME").unwrap_or_else(|_| ".".to_string());
    PathBuf::from(home).join(".nupi").join("bin")
}

/// Check if binaries are installed
pub fn binaries_installed() -> bool {
    let install_dir = get_install_dir();
    let nupi_path = install_dir.join("nupi");
    let nupid_path = install_dir.join("nupid");

    nupi_path.exists() && nupid_path.exists()
}

/// Get the path to the sidecar binaries bundled with the app
pub fn get_sidecar_paths(app_handle: &tauri::AppHandle) -> Result<(PathBuf, PathBuf), String> {
    use tauri_plugin_shell::ShellExt;

    // Use Tauri's sidecar resolution which knows the correct paths
    let nupi_sidecar_cmd = app_handle
        .shell()
        .sidecar("nupi")
        .map_err(|e| format!("Failed to resolve nupi sidecar: {}", e))?;

    let nupid_sidecar_cmd = app_handle
        .shell()
        .sidecar("nupid")
        .map_err(|e| format!("Failed to resolve nupid sidecar: {}", e))?;

    // Convert to std::process::Command to get the path
    let nupi_std_cmd: std::process::Command = nupi_sidecar_cmd.into();
    let nupid_std_cmd: std::process::Command = nupid_sidecar_cmd.into();

    // Extract the program path from the command
    let nupi_path = nupi_std_cmd.get_program();
    let nupid_path = nupid_std_cmd.get_program();

    let nupi_pathbuf = PathBuf::from(nupi_path);
    let nupid_pathbuf = PathBuf::from(nupid_path);

    // Verify sidecars exist
    if !nupi_pathbuf.exists() {
        return Err(format!("Sidecar not found: {:?}", nupi_pathbuf));
    }
    if !nupid_pathbuf.exists() {
        return Err(format!("Sidecar not found: {:?}", nupid_pathbuf));
    }

    Ok((nupi_pathbuf, nupid_pathbuf))
}

/// Install binaries from sidecars to user's bin directory
/// No sudo required - installs to ~/.nupi/bin/
pub async fn ensure_binaries_installed(app_handle: &tauri::AppHandle) -> Result<String, String> {
    // Check if already installed
    if binaries_installed() {
        return Ok("Binaries already installed".to_string());
    }

    // Get sidecar paths
    let (nupi_sidecar, nupid_sidecar) = get_sidecar_paths(app_handle)?;

    let install_dir = get_install_dir();
    let nupi_dest = install_dir.join("nupi");
    let nupid_dest = install_dir.join("nupid");

    // Install directly - no sudo needed for user's home directory
    install_binaries(&nupi_sidecar, &nupid_sidecar, &nupi_dest, &nupid_dest)?;

    // Add to PATH automatically
    add_to_path()?;

    Ok(format!(
        "Binaries installed to: {}\nPATH configured automatically",
        install_dir.display()
    ))
}

/// Add ~/.nupi/bin to PATH in shell configuration files
fn add_to_path() -> Result<(), String> {
    let home = env::var("HOME").map_err(|_| "HOME not set")?;
    let shell_configs = vec![
        PathBuf::from(&home).join(".bashrc"),
        PathBuf::from(&home).join(".zshrc"),
        PathBuf::from(&home).join(".bash_profile"),
    ];

    let path_line = "export PATH=\"$HOME/.nupi/bin:$PATH\"";
    let mut updated = false;

    for config_path in shell_configs {
        if !config_path.exists() {
            continue;
        }

        // Read existing content
        let content = fs::read_to_string(&config_path)
            .map_err(|e| format!("Failed to read {:?}: {}", config_path, e))?;

        // Check if already added
        if content.contains(".nupi/bin") {
            continue;
        }

        // Append to file
        let mut file = fs::OpenOptions::new()
            .append(true)
            .open(&config_path)
            .map_err(|e| format!("Failed to open {:?}: {}", config_path, e))?;

        writeln!(file, "\n# Nupi")
            .map_err(|e| format!("Failed to write to {:?}: {}", config_path, e))?;
        writeln!(file, "{}", path_line)
            .map_err(|e| format!("Failed to write to {:?}: {}", config_path, e))?;

        updated = true;
        println!("Added to PATH in {:?}", config_path);
    }

    if updated {
        Ok(())
    } else {
        // No shell config found or already configured
        Ok(())
    }
}

fn install_binaries(
    nupi_src: &Path,
    nupid_src: &Path,
    nupi_dest: &Path,
    nupid_dest: &Path,
) -> Result<(), String> {
    // Create install directory if needed
    if let Some(parent) = nupi_dest.parent() {
        fs::create_dir_all(parent).map_err(|e| format!("Failed to create directory: {}", e))?;
    }

    // Copy binaries
    fs::copy(nupi_src, nupi_dest).map_err(|e| format!("Failed to copy nupi: {}", e))?;
    fs::copy(nupid_src, nupid_dest).map_err(|e| format!("Failed to copy nupid: {}", e))?;

    // Set executable permissions
    let perms = fs::Permissions::from_mode(0o755);
    fs::set_permissions(nupi_dest, perms.clone())
        .map_err(|e| format!("Failed to set permissions on nupi: {}", e))?;
    fs::set_permissions(nupid_dest, perms)
        .map_err(|e| format!("Failed to set permissions on nupid: {}", e))?;

    Ok(())
}
