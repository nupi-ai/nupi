package toolhandlers

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/nupi-ai/nupi/internal/jsruntime"
	"github.com/nupi-ai/nupi/internal/plugins/manifest"
)

// ToolDetectedEvent is emitted when a tool is first detected.
type ToolDetectedEvent struct {
	SessionID string
	Tool      string
	Timestamp time.Time
}

// ToolChangedEvent is emitted when the detected tool changes.
type ToolChangedEvent struct {
	SessionID    string
	PreviousTool string
	NewTool      string
	Timestamp    time.Time
}

// DetectionResult holds the result of a plugin detection attempt.
type DetectionResult struct {
	plugin   *JSPlugin
	filename string
	detected bool
	error    error
}

// JSRuntimeFunc is a function that returns the current JS runtime.
// Using a function allows the handler to always get the current runtime,
// even after a supervised restart.
type JSRuntimeFunc func() *jsruntime.Runtime

// ToolHandler manages tool detection for a session.
type ToolHandler struct {
	sessionID   string
	pluginDir   string
	indexPath   string
	runtimeFunc JSRuntimeFunc // Function to get current runtime (preferred)
	jsRuntime   *jsruntime.Runtime // Static runtime (for backward compat)

	index            PluginIndex
	plugins          map[string]*JSPlugin
	candidates       []string
	buffer           *RingBuffer
	allPluginsLoaded bool

	detectedTool   string
	detectedIcon   string
	throttleMode   bool
	continuousMode bool // Keeps detecting after initial tool
	lastCheck      time.Time
	lastToolChange time.Time // Rate limit tool_changed publications (min 5s)
	startTime      time.Time

	eventChan  chan ToolDetectedEvent
	changeChan chan ToolChangedEvent
	closeOnce  sync.Once
	mu         sync.RWMutex
}

// HandlerOption configures a ToolHandler.
type HandlerOption func(*ToolHandler)

// WithContinuousMode enables continuous tool detection after initial detection.
func WithContinuousMode(enabled bool) HandlerOption {
	return func(d *ToolHandler) {
		d.continuousMode = enabled
	}
}

// WithJSRuntime sets a static JS runtime for plugin execution.
// Deprecated: Use WithJSRuntimeFunc for dynamic runtime that survives restarts.
func WithJSRuntime(rt *jsruntime.Runtime) HandlerOption {
	return func(d *ToolHandler) {
		d.jsRuntime = rt
	}
}

// WithJSRuntimeFunc sets a function to get the current JS runtime.
// This is preferred over WithJSRuntime as it allows the handler to
// get the current runtime even after a supervised restart.
func WithJSRuntimeFunc(fn JSRuntimeFunc) HandlerOption {
	return func(d *ToolHandler) {
		d.runtimeFunc = fn
	}
}

// NewToolHandler creates a new handler for a session.
func NewToolHandler(sessionID, pluginDir string, opts ...HandlerOption) *ToolHandler {
	d := &ToolHandler{
		sessionID:  sessionID,
		pluginDir:  pluginDir,
		indexPath:  filepath.Join(pluginDir, "handlers_index.json"),
		plugins:    make(map[string]*JSPlugin),
		buffer:     NewRingBuffer(),
		eventChan:  make(chan ToolDetectedEvent, 1),
		changeChan: make(chan ToolChangedEvent, 8),
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// getRuntime returns the current JS runtime, preferring runtimeFunc over static jsRuntime.
func (d *ToolHandler) getRuntime() *jsruntime.Runtime {
	if d.runtimeFunc != nil {
		return d.runtimeFunc()
	}
	return d.jsRuntime
}

// Initialize loads the plugin index.
func (d *ToolHandler) Initialize() error {
	index, err := LoadIndex(d.indexPath)
	if err != nil {
		return fmt.Errorf("failed to load index: %w", err)
	}
	d.index = index
	return nil
}

// OnSessionStart is called when a new session starts.
func (d *ToolHandler) OnSessionStart(command string, args []string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	baseCommand := filepath.Base(command)

	candidates, exists := d.index[baseCommand]

	if !exists || len(candidates) == 0 {
		candidates, exists = d.index[command]
	}

	if !exists || len(candidates) == 0 {
		log.Printf("[Handler] No plugins found for command: %s (base: %s) - will fallback to all plugins", command, baseCommand)
	}

	log.Printf("[Handler] Found %d candidate plugins for command '%s': %v",
		len(candidates), command, candidates)

	rt := d.getRuntime()
	for _, pluginFile := range candidates {
		pluginPath := filepath.Join(d.pluginDir, pluginFile)
		// Use jsruntime for loading when available
		var plugin *JSPlugin
		var err error
		if rt != nil {
			plugin, err = LoadPluginWithRuntime(context.Background(), rt, pluginPath)
		} else {
			plugin, err = LoadPlugin(pluginPath)
		}
		if err != nil {
			log.Printf("[Handler] Failed to load plugin %s: %v", pluginFile, err)
			continue
		}
		d.plugins[pluginFile] = plugin
		d.candidates = append(d.candidates, pluginFile)
	}

	if len(d.plugins) == 0 {
		log.Printf("[Handler] No plugins loaded from command, will try all on first output")
	}

	d.startTime = time.Now()
	d.lastCheck = time.Now()

	return nil
}

// OnOutput is called when new output is received from the PTY.
func (d *ToolHandler) OnOutput(data []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// If already detected and not in continuous mode, skip processing
	if d.detectedTool != "" && !d.continuousMode {
		return
	}

	d.buffer.Append(data)

	now := time.Now()

	// Enter throttle mode after 30s of operation
	if !d.throttleMode && now.Sub(d.startTime) > 30*time.Second {
		d.throttleMode = true
		log.Printf("[Handler] Entering throttle mode after 30s")
	}

	// Debounce: always wait at least 200ms between detection checks
	// This prevents excessive detection attempts on rapid output bursts
	const debounceInterval = 200 * time.Millisecond
	if now.Sub(d.lastCheck) < debounceInterval {
		return
	}

	// In throttle mode: check every 5s (both initial and continuous detection per architecture 4.3.1)
	throttleInterval := 5 * time.Second

	if d.throttleMode && now.Sub(d.lastCheck) < throttleInterval {
		return
	}

	d.lastCheck = now
	output := d.buffer.String()

	if len(d.plugins) == 0 && !d.allPluginsLoaded {
		log.Printf("[Handler] Loading all plugins as fallback")
		d.loadAllPlugins()
	}

	results := d.runParallelDetection(d.plugins, output)

	// Rate limit tool_changed publications to prevent spamming bus/UI
	const toolChangeMinInterval = 5 * time.Second

	for _, result := range results {
		if result.detected {
			previousTool := d.detectedTool

			// Check if this is a tool change
			if previousTool != "" && previousTool != result.plugin.Name {
				// Enforce minimum 5s between tool_changed publications
				if !d.lastToolChange.IsZero() && now.Sub(d.lastToolChange) < toolChangeMinInterval {
					log.Printf("[Handler] Skipping tool change %s -> %s (rate limited, last change %v ago)",
						previousTool, result.plugin.Name, now.Sub(d.lastToolChange))
					return
				}

				d.detectedTool = result.plugin.Name
				d.detectedIcon = result.plugin.Icon
				d.lastToolChange = now
				log.Printf("[Handler] Tool changed for session %s: %s -> %s",
					d.sessionID, previousTool, result.plugin.Name)

				select {
				case d.changeChan <- ToolChangedEvent{
					SessionID:    d.sessionID,
					PreviousTool: previousTool,
					NewTool:      result.plugin.Name,
					Timestamp:    time.Now(),
				}:
				default:
					log.Printf("[Handler] Failed to emit change event (channel full)")
				}
				return
			}

			// First detection
			if previousTool == "" {
				d.detectedTool = result.plugin.Name
				d.detectedIcon = result.plugin.Icon
				log.Printf("[Handler] Tool detected for session %s: %s", d.sessionID, result.plugin.Name)

				select {
				case d.eventChan <- ToolDetectedEvent{
					SessionID: d.sessionID,
					Tool:      result.plugin.Name,
					Timestamp: time.Now(),
				}:
				default:
					log.Printf("[Handler] Failed to emit detection event (channel full)")
				}
				return
			}

			// Same tool detected again, no event needed
			return
		}
	}

	allFalse := true
	for _, result := range results {
		if result.error != nil {
			allFalse = false
			break
		}
	}

	if allFalse && len(d.candidates) > 0 && !d.allPluginsLoaded {
		log.Printf("[Handler] All candidates returned false, trying ALL plugins as fallback")
		d.loadAllPlugins()

		results = d.runParallelDetection(d.plugins, output)
		for _, result := range results {
			if result.detected {
				previousTool := d.detectedTool

				if previousTool != "" && previousTool != result.plugin.Name {
					// Enforce minimum 5s between tool_changed publications
					if !d.lastToolChange.IsZero() && now.Sub(d.lastToolChange) < toolChangeMinInterval {
						log.Printf("[Handler] Skipping tool change (fallback) %s -> %s (rate limited)",
							previousTool, result.plugin.Name)
						return
					}

					d.detectedTool = result.plugin.Name
					d.detectedIcon = result.plugin.Icon
					d.lastToolChange = now
					log.Printf("[Handler] Tool changed (fallback) for session %s: %s -> %s",
						d.sessionID, previousTool, result.plugin.Name)

					select {
					case d.changeChan <- ToolChangedEvent{
						SessionID:    d.sessionID,
						PreviousTool: previousTool,
						NewTool:      result.plugin.Name,
						Timestamp:    time.Now(),
					}:
					default:
						log.Printf("[Handler] Failed to emit change event (channel full)")
					}
					return
				}

				if previousTool == "" {
					d.detectedTool = result.plugin.Name
					d.detectedIcon = result.plugin.Icon
					log.Printf("[Handler] Tool detected (fallback) for session %s: %s", d.sessionID, result.plugin.Name)

					select {
					case d.eventChan <- ToolDetectedEvent{
						SessionID: d.sessionID,
						Tool:      result.plugin.Name,
						Timestamp: time.Now(),
					}:
					default:
						log.Printf("[Handler] Failed to emit detection event (channel full)")
					}
					return
				}

				return
			}
		}
	}
}

func (d *ToolHandler) runParallelDetection(plugins map[string]*JSPlugin, output string) []DetectionResult {
	if len(plugins) == 0 {
		return nil
	}

	rt := d.getRuntime()
	if rt == nil {
		// Runtime not available - this can happen during startup or after a crash.
		// Detection will be retried on next output.
		log.Printf("[Handler] JS runtime not available, skipping detection for session %s", d.sessionID)
		return nil
	}

	results := make(chan DetectionResult, len(plugins))
	// Use a timeout context for detection operations
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for filename, plugin := range plugins {
		go func(p *JSPlugin, f string) {
			detected, err := p.Detect(ctx, rt, output)
			results <- DetectionResult{
				plugin:   p,
				filename: f,
				detected: detected,
				error:    err,
			}
		}(plugin, filename)
	}

	var allResults []DetectionResult
	timeout := time.After(2 * time.Second)

	for i := 0; i < len(plugins); i++ {
		select {
		case result := <-results:
			if result.error != nil {
				log.Printf("[Handler] Plugin %s error: %v", result.filename, result.error)
			}
			if result.detected {
				return []DetectionResult{result}
			}
			allResults = append(allResults, result)

		case <-timeout:
			log.Printf("[Handler] Collection timeout after 2s, got %d/%d results", i, len(plugins))
			return allResults
		}
	}

	return allResults
}

func (d *ToolHandler) loadAllPlugins() {
	manifests, warnings := manifest.DiscoverWithWarnings(d.pluginDir)
	for _, w := range warnings {
		log.Printf("[Handler] skipped plugin in %s: %v", w.Dir, w.Err)
	}

	rt := d.getRuntime()
	for _, mf := range manifests {
		if mf.Type != manifest.PluginTypeToolHandler {
			continue
		}

		mainPath, err := mf.MainPath()
		if err != nil {
			log.Printf("[Handler] Failed to evaluate manifest %s: %v", mf.Dir, err)
			continue
		}

		relPath, err := mf.RelativeMainPath(d.pluginDir)
		if err != nil {
			relPath = mainPath
		}

		if _, exists := d.plugins[relPath]; exists {
			continue
		}

		// Use jsruntime for loading when available
		var plugin *JSPlugin
		if rt != nil {
			plugin, err = LoadPluginWithRuntime(context.Background(), rt, mainPath)
		} else {
			plugin, err = LoadPlugin(mainPath)
		}
		if err != nil {
			log.Printf("[Handler] Failed to load plugin %s: %v", relPath, err)
			continue
		}
		d.plugins[relPath] = plugin
	}

	d.allPluginsLoaded = true
	log.Printf("[Handler] Loaded %d plugins total", len(d.plugins))
}

// StopDetection stops the detection process and closes the event channel.
func (d *ToolHandler) StopDetection() {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.eventChan == nil && d.changeChan == nil {
		return
	}

	d.closeOnce.Do(func() {
		if d.eventChan != nil {
			close(d.eventChan)
		}
		if d.changeChan != nil {
			close(d.changeChan)
		}
		log.Printf("[Handler] Detection stopped, channels closed")
	})
}

// GetDetectedTool returns the detected tool name, if any.
func (d *ToolHandler) GetDetectedTool() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.detectedTool
}

// GetDetectedIcon returns the detected tool's icon filename, if any.
func (d *ToolHandler) GetDetectedIcon() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.detectedIcon
}

// EventChannel returns the channel for initial tool detection events.
func (d *ToolHandler) EventChannel() <-chan ToolDetectedEvent {
	return d.eventChan
}

// ChangeChannel returns the channel for tool change events.
func (d *ToolHandler) ChangeChannel() <-chan ToolChangedEvent {
	return d.changeChan
}

// IsContinuousMode returns whether continuous detection is enabled.
func (d *ToolHandler) IsContinuousMode() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.continuousMode
}

// EnableContinuousMode enables or disables continuous detection at runtime.
func (d *ToolHandler) EnableContinuousMode(enabled bool) {
	d.mu.Lock()
	d.continuousMode = enabled
	d.mu.Unlock()
	if enabled {
		log.Printf("[Handler] Continuous mode enabled for session %s", d.sessionID)
	} else {
		log.Printf("[Handler] Continuous mode disabled for session %s", d.sessionID)
	}
}
