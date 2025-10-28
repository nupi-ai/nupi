package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	apihttp "github.com/nupi-ai/nupi/internal/api/http"
	"github.com/nupi-ai/nupi/internal/config"
	"github.com/spf13/cobra"
)

func TestParseManifest_Empty(t *testing.T) {
	raw, err := parseManifest("")
	if err != nil {
		t.Fatalf("parseManifest returned error: %v", err)
	}
	if raw != nil {
		t.Fatalf("expected nil manifest for empty input")
	}
}

func TestParseManifest_JSON(t *testing.T) {
	input := `{"apiVersion":"v1","metadata":{"name":"test"}}`
	raw, err := parseManifest(input)
	if err != nil {
		t.Fatalf("parseManifest returned error: %v", err)
	}
	if string(raw) != input {
		t.Fatalf("expected JSON to remain unchanged, got %s", raw)
	}
}

func TestParseManifest_YAML(t *testing.T) {
	input := `
apiVersion: v1
metadata:
  name: test
spec:
  slot: stt
`
	raw, err := parseManifest(input)
	if err != nil {
		t.Fatalf("parseManifest returned error: %v", err)
	}

	var data map[string]interface{}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("unmarshal converted manifest: %v", err)
	}
	if data["apiVersion"] != "v1" {
		t.Fatalf("expected apiVersion to be preserved, got %v", data["apiVersion"])
	}
	spec, ok := data["spec"].(map[string]interface{})
	if !ok || spec["slot"] != "stt" {
		t.Fatalf("expected spec.slot=stt, got %v", data["spec"])
	}
}

func TestParseManifest_Invalid(t *testing.T) {
	_, err := parseManifest("{invalid json")
	if err == nil {
		t.Fatalf("expected error for invalid manifest")
	}
	if !strings.Contains(err.Error(), "invalid YAML") {
		t.Fatalf("expected YAML error message, got %v", err)
	}
}

func TestParseManifest_NonStringKeys(t *testing.T) {
	input := `
1: numeric
true: boolean
`
	raw, err := parseManifest(input)
	if err != nil {
		t.Fatalf("parseManifest returned error: %v", err)
	}

	var data map[string]interface{}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("unmarshal converted manifest: %v", err)
	}
	if data["1"] != "numeric" {
		t.Fatalf("expected numeric key to be preserved as \"1\", got %v", data["1"])
	}
	if data["true"] != "boolean" {
		t.Fatalf("expected boolean key to be preserved as \"true\", got %v", data["true"])
	}
}

func TestParseManifest_DeepNesting(t *testing.T) {
	var builder strings.Builder
	depth := 1100
	for i := 0; i < depth; i++ {
		builder.WriteString(strings.Repeat("  ", i))
		builder.WriteString("node")
		builder.WriteString(strconv.Itoa(i))
		builder.WriteString(":\n")
	}
	builder.WriteString(strings.Repeat("  ", depth))
	builder.WriteString("leaf: value\n")

	if _, err := parseManifest(builder.String()); err != nil {
		t.Fatalf("parseManifest returned error for deep nesting: %v", err)
	}
}

func TestParseManifest_DeepNestingCutoff(t *testing.T) {
	var builder strings.Builder
	depth := 1100
	for i := 0; i < depth; i++ {
		builder.WriteString(strings.Repeat("  ", i))
		builder.WriteString("node")
		builder.WriteString(strconv.Itoa(i))
		builder.WriteString(":\n")
	}
	builder.WriteString(strings.Repeat("  ", depth))
	builder.WriteString("leaf: value\n")

	raw, err := parseManifest(builder.String())
	if err != nil {
		t.Fatalf("parseManifest returned error: %v", err)
	}

	var data map[string]interface{}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("unmarshal converted manifest: %v", err)
	}

	if maxDepth := maxJSONDepth(data); maxDepth > 1050 {
		t.Fatalf("expected json depth to be bounded, got %d", maxDepth)
	}
}

func maxJSONDepth(value interface{}) int {
	return maxJSONDepthInternal(value, 1)
}

func maxJSONDepthInternal(value interface{}, depth int) int {
	max := depth
	switch v := value.(type) {
	case map[string]interface{}:
		for _, val := range v {
			if d := maxJSONDepthInternal(val, depth+1); d > max {
				max = d
			}
		}
	case []interface{}:
		for _, val := range v {
			if d := maxJSONDepthInternal(val, depth+1); d > max {
				max = d
			}
		}
	}
	return max
}

func TestLoadAdapterManifestFile(t *testing.T) {
	manifest := `apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: Local STT
  slug: local-stt
spec:
  slot: stt
  entrypoint:
    command: ./adapter
    args: ["--foo", "bar"]
    transport: grpc
`

	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "plugin.yaml")
	if err := os.WriteFile(tmpFile, []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	parsed, raw, err := loadAdapterManifestFile(tmpFile)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if parsed.Metadata.Slug != "local-stt" {
		t.Fatalf("unexpected slug %q", parsed.Metadata.Slug)
	}
	if parsed.Adapter == nil || parsed.Adapter.Slot != "stt" {
		t.Fatalf("unexpected adapter slot")
	}
	if parsed.Adapter.Entrypoint.Command != "./adapter" {
		t.Fatalf("unexpected command %q", parsed.Adapter.Entrypoint.Command)
	}
	if len(parsed.Adapter.Entrypoint.Args) != 2 {
		t.Fatalf("unexpected args %#v", parsed.Adapter.Entrypoint.Args)
	}
	if strings.TrimSpace(raw) == "" {
		t.Fatalf("expected raw manifest contents to be returned")
	}
}

func TestAdaptersInstallLocalRegistersAndCopiesBinary(t *testing.T) {
	var registerPayload apihttp.AdapterRegistrationRequest
	handlers := map[string]httpHandlerFunc{
		"/adapters/register": func(r *http.Request) *http.Response {
			if r.Method != http.MethodPost {
				t.Fatalf("unexpected method %s", r.Method)
			}
			if err := json.NewDecoder(r.Body).Decode(&registerPayload); err != nil {
				t.Fatalf("decode register payload: %v", err)
			}
			rec := httptest.NewRecorder()
			resp := apihttp.AdapterRegistrationResult{Adapter: apihttp.AdapterDescriptor{ID: registerPayload.AdapterID, Type: registerPayload.Type, Name: registerPayload.Name}}
			if err := json.NewEncoder(rec).Encode(resp); err != nil {
				t.Fatalf("encode register response: %v", err)
			}
			return rec.Result()
		},
		"/adapters/bind": func(r *http.Request) *http.Response {
			if r.Method != http.MethodPost {
				t.Fatalf("unexpected method %s", r.Method)
			}
			adapterID := registerPayload.AdapterID
			rec := httptest.NewRecorder()
			resp := apihttp.AdapterActionResult{
				Adapter: apihttp.AdapterEntry{
					Slot:      "stt",
					AdapterID: &adapterID,
				},
			}
			if err := json.NewEncoder(rec).Encode(resp); err != nil {
				t.Fatalf("encode bind response: %v", err)
			}
			return rec.Result()
		},
	}
	fixture := setupInstallLocalTest(t, `apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: Local STT
  slug: local-stt
spec:
  slot: stt
  entrypoint:
    command: ./adapter
    args: ["--variant", "base"]
    transport: grpc
`, "http://adapters.test", handlers)

	binaryPath := filepath.Join(t.TempDir(), "adapter-bin")
	if err := os.WriteFile(binaryPath, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}

	mustSetFlag(t, fixture.cmd, "binary", binaryPath)
	mustSetFlag(t, fixture.cmd, "copy-binary", "true")
	mustSetFlag(t, fixture.cmd, "endpoint-address", "127.0.0.1:50051")
	mustSetFlag(t, fixture.cmd, "slot", "stt")

	if err := adaptersInstallLocal(fixture.cmd, nil); err != nil {
		t.Fatalf("install-local failed: %v", err)
	}

	paths, err := config.EnsureInstanceDirs("")
	if err != nil {
		t.Fatalf("ensure instance dirs: %v", err)
	}
	expectedDir := filepath.Join(paths.Home, "plugins", sanitizeAdapterSlug("local-stt"), "bin")
	expectedFile := filepath.Join(expectedDir, filepath.Base(binaryPath))
	if _, err := os.Stat(expectedFile); err != nil {
		t.Fatalf("expected binary at %s: %v", expectedFile, err)
	}
	if registerPayload.Endpoint.Command != expectedFile {
		t.Fatalf("expected command %s, got %s", expectedFile, registerPayload.Endpoint.Command)
	}
}

func TestAdaptersInstallLocalBuildsBinary(t *testing.T) {
	sourceDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceDir, "go.mod"), []byte(`module example.com/localadapter

go 1.21
`), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	adapterDir := filepath.Join(sourceDir, "cmd", "adapter")
	if err := os.MkdirAll(adapterDir, 0o755); err != nil {
		t.Fatalf("create adapter dir: %v", err)
	}
	mainSrc := `package main

import "fmt"

func main() { fmt.Println("ok") }
`
	if err := os.WriteFile(filepath.Join(adapterDir, "main.go"), []byte(mainSrc), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}

	var registerPayload apihttp.AdapterRegistrationRequest
	handlers := map[string]httpHandlerFunc{
		"/adapters/register": func(r *http.Request) *http.Response {
			if r.Method != http.MethodPost {
				t.Fatalf("unexpected method %s", r.Method)
			}
			if err := json.NewDecoder(r.Body).Decode(&registerPayload); err != nil {
				t.Fatalf("decode register payload: %v", err)
			}
			rec := httptest.NewRecorder()
			resp := apihttp.AdapterRegistrationResult{Adapter: apihttp.AdapterDescriptor{ID: registerPayload.AdapterID, Type: registerPayload.Type, Name: registerPayload.Name}}
			if err := json.NewEncoder(rec).Encode(resp); err != nil {
				t.Fatalf("encode register response: %v", err)
			}
			return rec.Result()
		},
	}
	fixture := setupInstallLocalTest(t, `apiVersion: nap.nupi.ai/v1alpha1
kind: Plugin
type: adapter
metadata:
  name: Local STT
  slug: local-stt
spec:
  slot: stt
  entrypoint:
    command: ./adapter
    transport: grpc
`, "http://adapters.test", handlers)

	mustSetFlag(t, fixture.cmd, "build", "true")
	mustSetFlag(t, fixture.cmd, "adapter-dir", sourceDir)
	mustSetFlag(t, fixture.cmd, "endpoint-address", "127.0.0.1:50051")

	if err := adaptersInstallLocal(fixture.cmd, nil); err != nil {
		t.Fatalf("install-local build failed: %v", err)
	}

	expectedBinary := filepath.Join(sourceDir, "dist", "local-stt")
	if _, err := os.Stat(expectedBinary); err != nil {
		t.Fatalf("expected built binary at %s: %v", expectedBinary, err)
	}
	if registerPayload.Endpoint.Command != expectedBinary {
		t.Fatalf("expected command %s, got %s", expectedBinary, registerPayload.Endpoint.Command)
	}
}

func TestSanitizeAdapterSlug(t *testing.T) {
	cases := map[string]string{
		"Local STT":         "local-stt",
		"UPPER_case--Slug":  "upper_case--slug",
		"   spaced slug   ": "spaced-slug",
		"../dangerous/path": "dangerous-path",
		"@@@":               "adapter",
		"slug-with-extremely-long-name-that-should-be-trimmed-because-it-exceeds-sixty-four-characters": "slug-with-extremely-long-name-that-should-be-trimmed-because-it-",
	}

	for input, expected := range cases {
		if actual := sanitizeAdapterSlug(input); actual != expected {
			t.Fatalf("sanitizeAdapterSlug(%q) = %q, expected %q", input, actual, expected)
		}
	}
}

func TestCopyFile(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	srcPath := filepath.Join(srcDir, "source.txt")
	content := []byte("hello world")
	if err := os.WriteFile(srcPath, content, 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	dstPath := filepath.Join(dstDir, "copied.txt")
	if err := copyFile(srcPath, dstPath, 0o711); err != nil {
		t.Fatalf("copyFile error: %v", err)
	}

	data, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(data) != string(content) {
		t.Fatalf("expected copied contents %q, got %q", content, data)
	}

	info, err := os.Stat(dstPath)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if info.Mode().Perm() != 0o711 {
		t.Fatalf("expected permissions 0711, got %v", info.Mode().Perm())
	}
}

func TestCopyFile_SameSourceAndDestination(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "file.txt")
	if err := os.WriteFile(path, []byte("content"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	err := copyFile(path, path, 0o644)
	if err == nil || !strings.Contains(err.Error(), "source and destination are the same") {
		t.Fatalf("expected same-path error, got %v", err)
	}
}

func TestCopyFile_DestinationExists(t *testing.T) {
	tmpDir := t.TempDir()
	src := filepath.Join(tmpDir, "src.txt")
	dst := filepath.Join(tmpDir, "dst.txt")

	if err := os.WriteFile(src, []byte("src"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := os.WriteFile(dst, []byte("dst"), 0o644); err != nil {
		t.Fatalf("write dst: %v", err)
	}

	err := copyFile(src, dst, 0o644)
	if err == nil || !strings.Contains(err.Error(), "destination already exists") {
		t.Fatalf("expected destination exists error, got %v", err)
	}
}

func TestCopyFile_SourceIsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated privileges on Windows")
	}

	tmpDir := t.TempDir()
	target := filepath.Join(tmpDir, "target.txt")
	link := filepath.Join(tmpDir, "link.txt")
	dst := filepath.Join(tmpDir, "dst.txt")

	if err := os.WriteFile(target, []byte("content"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	err := copyFile(link, dst, 0o644)
	if err == nil || !strings.Contains(err.Error(), "source is a symlink") {
		t.Fatalf("expected symlink error, got %v", err)
	}
}

func TestCopyFile_SourceMissing(t *testing.T) {
	tmpDir := t.TempDir()
	dst := filepath.Join(tmpDir, "dst.txt")

	err := copyFile(filepath.Join(tmpDir, "missing.txt"), dst, 0o644)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected not exist error, got %v", err)
	}
}

type httpHandlerFunc func(*http.Request) *http.Response

type mockTransport struct {
	t        *testing.T
	handlers map[string]httpHandlerFunc
}

type installLocalTestFixture struct {
	cmd *cobra.Command
}

func setupInstallLocalTest(t *testing.T, manifest string, baseURL string, handlers map[string]httpHandlerFunc) installLocalTestFixture {
	t.Helper()

	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	setHTTPMockTransport(t, handlers)
	t.Setenv("NUPI_BASE_URL", baseURL)

	manifestPath := filepath.Join(t.TempDir(), "plugin.yaml")
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	cmd := newInstallLocalCommand()
	mustSetFlag(t, cmd, "manifest-file", manifestPath)
	return installLocalTestFixture{cmd: cmd}
}

func newInstallLocalCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "install-local"}
	cmd.Flags().String("manifest-file", "", "")
	cmd.Flags().String("id", "", "")
	cmd.Flags().String("binary", "", "")
	cmd.Flags().Bool("copy-binary", false, "")
	cmd.Flags().Bool("build", false, "")
	cmd.Flags().String("adapter-dir", "", "")
	cmd.Flags().String("endpoint-address", "", "")
	cmd.Flags().StringArray("endpoint-arg", nil, "")
	cmd.Flags().StringArray("endpoint-env", nil, "")
	cmd.Flags().String("slot", "", "")
	cmd.Flags().String("config", "", "")
	return cmd
}

func mustSetFlag(t *testing.T, cmd *cobra.Command, name, value string) {
	t.Helper()
	if err := cmd.Flags().Set(name, value); err != nil {
		t.Fatalf("set flag %s: %v", name, err)
	}
}

func (m *mockTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if handler, ok := m.handlers[req.URL.Path]; ok {
		return handler(req), nil
	}
	m.t.Fatalf("unexpected request path: %s", req.URL.Path)
	panic("unreachable")
}

func setHTTPMockTransport(t *testing.T, handlers map[string]httpHandlerFunc) {
	t.Helper()
	original := http.DefaultTransport
	http.DefaultTransport = &mockTransport{t: t, handlers: handlers}
	t.Cleanup(func() { http.DefaultTransport = original })
}
