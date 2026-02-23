package constants

import "time"

// Shared duration vocabulary used by timeouts, polling and retry checks.
// Keep these centralized to simplify system-wide timing tuning.
const (
	Duration5Milliseconds   = 5 * time.Millisecond
	Duration10Milliseconds  = 10 * time.Millisecond
	Duration25Milliseconds  = 25 * time.Millisecond
	Duration50Milliseconds  = 50 * time.Millisecond
	Duration100Milliseconds = 100 * time.Millisecond
	Duration200Milliseconds = 200 * time.Millisecond
	Duration250Milliseconds = 250 * time.Millisecond
	Duration500Milliseconds = 500 * time.Millisecond

	Duration1Second    = 1 * time.Second
	Duration2Seconds   = 2 * time.Second
	Duration3Seconds   = 3 * time.Second
	Duration5Seconds   = 5 * time.Second
	Duration10Seconds  = 10 * time.Second
	Duration15Seconds  = 15 * time.Second
	Duration30Seconds  = 30 * time.Second
	Duration60Seconds  = 60 * time.Second
	Duration120Seconds = 120 * time.Second

	Duration2Minutes  = 2 * time.Minute
	Duration5Minutes  = 5 * time.Minute
	Duration10Minutes = 10 * time.Minute
	Duration30Minutes = 30 * time.Minute
)

// Domain-level timeout constants.
const (
	NAPAdapterConnectTimeout           = Duration10Seconds
	NAPAdapterRequestTimeout           = Duration30Seconds
	NAPAdapterCapabilitiesProbeTimeout = Duration3Seconds

	AdapterGracefulShutdownTimeout = Duration15Seconds

	GRPCClientUnixDialTimeout     = Duration5Seconds
	GRPCClientMinConnectTimeout   = Duration5Seconds
	GRPCClientVersionCheckTimeout = Duration2Seconds
	GRPCClientConfigLoadTimeout   = Duration5Seconds
)
