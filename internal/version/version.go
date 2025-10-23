package version

var version = "dev"

// String returns the build version for the current binary.
func String() string {
	return version
}
