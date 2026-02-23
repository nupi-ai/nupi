use std::time::Duration;

pub const STALE_OPERATION_TIMEOUT: Duration = Duration::from_secs(3600);
pub const DAEMON_READINESS_RETRY_DELAY: Duration = Duration::from_millis(500);

pub const AUDIO_STREAM_REQUEST_TIMEOUT: Duration = Duration::from_secs(600);
pub const GRPC_CHANNEL_REQUEST_TIMEOUT: Duration = Duration::from_secs(10);
pub const GRPC_CHANNEL_CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
