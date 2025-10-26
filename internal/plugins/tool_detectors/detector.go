package tooldetectors

import (
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/nupi-ai/nupi/internal/plugins/manifest"
)

// ToolDetectedEvent is emitted when a tool is detected.
type ToolDetectedEvent struct {
	SessionID string
	Tool      string
	Timestamp time.Time
}

// DetectionResult holds the result of a plugin detection attempt.
type DetectionResult struct {
	plugin   *JSPlugin
	filename string
	detected bool
	error    error
}

// ToolDetector manages tool detection for a session.
type ToolDetector struct {
	sessionID string
	pluginDir string
	indexPath string

	index            PluginIndex
	plugins          map[string]*JSPlugin
	candidates       []string
	buffer           *RingBuffer
	allPluginsLoaded bool

	detectedTool string
	detectedIcon string
	throttleMode bool
	lastCheck    time.Time
	startTime    time.Time

	eventChan chan ToolDetectedEvent
	closeOnce sync.Once
	mu        sync.RWMutex
}

// NewToolDetector creates a new detector for a session.
func NewToolDetector(sessionID, pluginDir string) *ToolDetector {
	return &ToolDetector{
		sessionID: sessionID,
		pluginDir: pluginDir,
		indexPath: filepath.Join(pluginDir, "detectors_index.json"),
		plugins:   make(map[string]*JSPlugin),
		buffer:    NewRingBuffer(),
		eventChan: make(chan ToolDetectedEvent, 1),
	}
}

// Initialize loads the plugin index.
func (d *ToolDetector) Initialize() error {
	index, err := LoadIndex(d.indexPath)
	if err != nil {
		return fmt.Errorf("failed to load index: %w", err)
	}
	d.index = index
	return nil
}

// OnSessionStart is called when a new session starts.
func (d *ToolDetector) OnSessionStart(command string, args []string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	baseCommand := filepath.Base(command)

	candidates, exists := d.index[baseCommand]

	if !exists || len(candidates) == 0 {
		candidates, exists = d.index[command]
	}

	if !exists || len(candidates) == 0 {
		log.Printf("[Detector] No plugins found for command: %s (base: %s) - will fallback to all plugins", command, baseCommand)
	}

	log.Printf("[Detector] Found %d candidate plugins for command '%s': %v",
		len(candidates), command, candidates)

	for _, pluginFile := range candidates {
		pluginPath := filepath.Join(d.pluginDir, pluginFile)
		plugin, err := LoadPlugin(pluginPath)
		if err != nil {
			log.Printf("[Detector] Failed to load plugin %s: %v", pluginFile, err)
			continue
		}
		d.plugins[pluginFile] = plugin
		d.candidates = append(d.candidates, pluginFile)
	}

	if len(d.plugins) == 0 {
		log.Printf("[Detector] No plugins loaded from command, will try all on first output")
	}

	d.startTime = time.Now()
	d.lastCheck = time.Now()

	return nil
}

// OnOutput is called when new output is received from the PTY.
func (d *ToolDetector) OnOutput(data []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.detectedTool != "" {
		return
	}

	d.buffer.Append(data)

	now := time.Now()

	if !d.throttleMode && now.Sub(d.startTime) > 30*time.Second {
		d.throttleMode = true
		log.Printf("[Detector] Entering throttle mode after 30s")
	}

	if d.throttleMode && now.Sub(d.lastCheck) < 5*time.Second {
		return
	}

	d.lastCheck = now
	output := d.buffer.String()

	if len(d.plugins) == 0 {
		log.Printf("[Detector] Loading all plugins as fallback")
		d.loadAllPlugins()
	}

	results := d.runParallelDetection(d.plugins, output)

	for _, result := range results {
		if result.detected {
			d.detectedTool = result.plugin.Name
			d.detectedIcon = result.plugin.Icon
			log.Printf("[Detector] Tool detected for session %s: %s", d.sessionID, result.plugin.Name)

			select {
			case d.eventChan <- ToolDetectedEvent{
				SessionID: d.sessionID,
				Tool:      result.plugin.Name,
				Timestamp: time.Now(),
			}:
			default:
				log.Printf("[Detector] Failed to emit detection event (channel full)")
			}
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
		log.Printf("[Detector] All candidates returned false, trying ALL plugins as fallback")
		d.loadAllPlugins()

		results = d.runParallelDetection(d.plugins, output)
		for _, result := range results {
			if result.detected {
				d.detectedTool = result.plugin.Name
				d.detectedIcon = result.plugin.Icon
				log.Printf("[Detector] Tool detected (fallback) for session %s: %s", d.sessionID, result.plugin.Name)

				select {
				case d.eventChan <- ToolDetectedEvent{
					SessionID: d.sessionID,
					Tool:      result.plugin.Name,
					Timestamp: time.Now(),
				}:
				default:
					log.Printf("[Detector] Failed to emit detection event (channel full)")
				}
				return
			}
		}
	}
}

func (d *ToolDetector) runParallelDetection(plugins map[string]*JSPlugin, output string) []DetectionResult {
	if len(plugins) == 0 {
		return nil
	}

	results := make(chan DetectionResult, len(plugins))

	for filename, plugin := range plugins {
		go func(p *JSPlugin, f string) {
			detected, err := p.Detect(output)
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
				log.Printf("[Detector] Plugin %s error: %v", result.filename, result.error)
			}
			if result.detected {
				return []DetectionResult{result}
			}
			allResults = append(allResults, result)

		case <-timeout:
			log.Printf("[Detector] Collection timeout after 2s, got %d/%d results", i, len(plugins))
			return allResults
		}
	}

	return allResults
}

func (d *ToolDetector) loadAllPlugins() {
	manifests, err := manifest.Discover(d.pluginDir)
	if err != nil {
		log.Printf("[Detector] Failed to discover plugins: %v", err)
		return
	}

	for _, mf := range manifests {
		if mf.Type != manifest.PluginTypeToolDetector {
			continue
		}

		mainPath, err := mf.MainPath()
		if err != nil {
			log.Printf("[Detector] Failed to evaluate manifest %s: %v", mf.Dir, err)
			continue
		}

		relPath, err := mf.RelativeMainPath(d.pluginDir)
		if err != nil {
			relPath = mainPath
		}

		if _, exists := d.plugins[relPath]; exists {
			continue
		}

		plugin, err := LoadPlugin(mainPath)
		if err != nil {
			log.Printf("[Detector] Failed to load plugin %s: %v", relPath, err)
			continue
		}
		d.plugins[relPath] = plugin
	}

	d.allPluginsLoaded = true
	log.Printf("[Detector] Loaded %d plugins total", len(d.plugins))
}

// StopDetection stops the detection process and closes the event channel.
func (d *ToolDetector) StopDetection() {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.eventChan == nil {
		return
	}

	d.closeOnce.Do(func() {
		close(d.eventChan)
		log.Printf("[Detector] Detection stopped, event channel closed")
	})
}

// GetDetectedTool returns the detected tool name, if any.
func (d *ToolDetector) GetDetectedTool() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.detectedTool
}

// GetDetectedIcon returns the detected tool's icon filename, if any.
func (d *ToolDetector) GetDetectedIcon() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.detectedIcon
}

// EventChannel returns the channel for tool detection events.
func (d *ToolDetector) EventChannel() <-chan ToolDetectedEvent {
	return d.eventChan
}
