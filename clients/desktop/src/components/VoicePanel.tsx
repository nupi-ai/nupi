import { useAudioFilePicker } from "../hooks/useAudioFilePicker";
import { useVoiceSessionConfig } from "../hooks/useVoiceSessionConfig";
import { useVoiceStream } from "../hooks/useVoiceStream";
import { mergeStyles } from "../utils/mergeStyles";
import { LanguageSelector } from "./LanguageSelector";
import * as styles from "./voicePanelStyles";

function stringify(value: unknown): string {
  if (value === null || value === undefined) {
    return "";
  }
  try {
    return JSON.stringify(value, null, 2);
  } catch {
    return String(value);
  }
}

export function VoicePanel() {
  const { sessionId, setSessionId, streamId, setStreamId, metadata } =
    useVoiceSessionConfig();

  const {
    inputPath,
    outputPath,
    disablePlayback,
    setDisablePlayback,
    pickInput,
    pickOutput,
    clearOutput,
  } = useAudioFilePicker();

  const {
    status,
    result,
    isBusy,
    playbackError,
    activeOperationId,
    isCancelling,
    capabilities,
    performUpload,
    performInterrupt,
    cancelUpload,
    fetchCapabilities,
  } = useVoiceStream({
    sessionId,
    streamId,
    inputPath,
    outputPath,
    disablePlayback,
    metadata,
  });

  return (
    <div style={styles.root}>
      <section style={styles.section}>
        <h2 style={styles.sectionHeading}>Voice Streaming (Preview)</h2>
        <p style={styles.descriptionText}>
          This panel provides an early bridge between the desktop app and the new
          voice pipeline. You can attach WAV recordings to an active session,
          forward manual interruptions, and inspect the capabilities reported by
          the daemon. Microphone capture and real-time playback will land in a
          subsequent iteration.
        </p>
        <div style={styles.formGrid}>
          <label style={styles.fieldLabel}>
            <span style={styles.labelText}>Session ID</span>
            <input
              value={sessionId}
              onChange={(event) => setSessionId(event.target.value)}
              placeholder="e.g. sess-123"
              style={styles.textInput}
            />
          </label>
          <label style={styles.fieldLabel}>
            <span style={styles.labelText}>Stream ID (optional)</span>
            <input
              value={streamId}
              onChange={(event) => setStreamId(event.target.value)}
              placeholder="mic"
              style={styles.textInput}
            />
          </label>
        </div>

        <div style={styles.buttonRow}>
          <button onClick={pickInput} style={styles.primaryButton}>
            {inputPath ? "Change Input File" : "Select Input File"}
          </button>
          <button onClick={pickOutput} style={styles.secondaryButton}>
            {outputPath ? "Change Output Path" : "Save Playback to WAV"}
          </button>
          {outputPath && (
            <button onClick={clearOutput} style={styles.dangerOutlineButton}>
              Clear Output
            </button>
          )}
          <label style={styles.checkboxLabel}>
            <input
              type="checkbox"
              checked={disablePlayback}
              onChange={(event) => setDisablePlayback(event.target.checked)}
              disabled={!!outputPath}
            />
            Skip playback subscription
          </label>
        </div>

        <div style={styles.actionButtonRow}>
          <button
            onClick={performUpload}
            disabled={isBusy}
            style={mergeStyles(styles.successButton, styles.busyCursor(isBusy))}
          >
            {isBusy ? "Streaming\u2026" : "Send Audio"}
          </button>
          <button
            onClick={performInterrupt}
            disabled={isBusy}
            style={mergeStyles(styles.warningButton, styles.busyCursor(isBusy))}
          >
            Interrupt TTS
          </button>
          <button
            onClick={fetchCapabilities}
            disabled={isBusy}
            style={mergeStyles(styles.indigoButton, styles.busyCursor(isBusy))}
          >
            Refresh Capabilities
          </button>
          {isBusy && activeOperationId && (
            <button
              onClick={cancelUpload}
              disabled={isCancelling}
              style={mergeStyles(styles.dangerButton, styles.busyCursor(isCancelling))}
            >
              {isCancelling ? "Cancelling\u2026" : "Cancel Upload"}
            </button>
          )}
        </div>

        <p style={styles.statusText}>Status: {status}</p>
        {inputPath && (
          <p style={styles.infoText}>
            Using audio file: <code>{inputPath}</code>
          </p>
        )}
        {outputPath && (
          <p style={styles.infoText}>
            Playback will be saved to: <code>{outputPath}</code>
          </p>
        )}
        {playbackError && (
          <p style={styles.warningText}>
            Playback warning: {playbackError}
          </p>
        )}
      </section>

      <section style={styles.section}>
        <h3 style={styles.sectionHeading}>Capabilities</h3>
        <pre style={styles.capabilitiesPre}>
{stringify(capabilities) || "Use \u201cRefresh Capabilities\u201d to fetch the latest capture/playback configuration."}
        </pre>
      </section>

      <section style={styles.section}>
        <h3 style={styles.sectionHeading}>Last Command Result</h3>
        <pre style={styles.resultPre}>
{stringify(result) || "No command executed yet. Run a voice action to see detailed JSON output."}
        </pre>
      </section>

      <LanguageSelector />
    </div>
  );
}
