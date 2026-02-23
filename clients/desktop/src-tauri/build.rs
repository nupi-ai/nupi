fn main() {
    // gRPC proto code generation via tonic-build.
    // Include path is scoped to api/grpc/v1/ to avoid rerun-if-changed on the entire repo.
    let proto_dir = std::path::Path::new("../../../api/grpc/v1");

    let mut protos: Vec<std::path::PathBuf> = std::fs::read_dir(proto_dir)
        .expect("Failed to read proto directory")
        .filter_map(|entry| entry.ok().map(|e| e.path()))
        .filter(|path| path.extension().and_then(|e| e.to_str()) == Some("proto"))
        .collect();
    protos.sort();

    tonic_build::configure()
        .build_server(false) // Desktop app only needs gRPC client stubs
        .compile_protos(&protos, &[proto_dir])
        .expect("Failed to compile proto files");

    tauri_build::build()
}
