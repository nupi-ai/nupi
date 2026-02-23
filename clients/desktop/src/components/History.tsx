import { useState, useEffect } from 'react';
import { api, toErrorMessage, type Recording } from '../api';
import { mergeStyles } from '../utils/mergeStyles';
import { AsciinemaPlayer } from './AsciinemaPlayer';
import * as styles from './historyStyles';
import { formatDateTimeLocal, formatDurationSeconds } from '@nupi/shared/time';

export function History() {
  const [recordings, setRecordings] = useState<Recording[]>([]);
  const [selectedRecording, setSelectedRecording] = useState<Recording | null>(null);
  const [castData, setCastData] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [hoveredRecordingId, setHoveredRecordingId] = useState<string | null>(null);

  useEffect(() => {
    loadRecordings();
  }, []);

  async function loadRecordings() {
    try {
      setLoading(true);
      setError(null);
      const data = await api.recordings.list();
      setRecordings(data || []);
    } catch (err) {
      console.error('[History] Failed to load recordings:', err);
      setError(toErrorMessage(err));
    } finally {
      setLoading(false);
    }
  }

  async function loadCastData(sessionId: string) {
    try {
      setCastData(null);
      const data = await api.recordings.get(sessionId);
      setCastData(data);
    } catch (err) {
      console.error('[History] Failed to load cast data:', err);
      setError(toErrorMessage(err));
    }
  }

  function selectRecording(recording: Recording) {
    setSelectedRecording(recording);
    loadCastData(recording.session_id);
  }

  if (selectedRecording) {
    return (
      <div style={styles.root}>
        {/* Header */}
        <div style={styles.selectedHeader}>
          <div style={styles.headerLeadingSection}>
            <button
              onClick={() => { setSelectedRecording(null); setCastData(null); }}
              style={styles.backButton}
            >
              ← Back
            </button>
            <div style={styles.commandRow}>
              <h2 style={styles.commandTitle}>
                {selectedRecording.command}
              </h2>
              {selectedRecording.args && selectedRecording.args.length > 0 && (
                <span style={styles.commandArguments}>
                  {selectedRecording.args.join(' ')}
                </span>
              )}
            </div>
          </div>
          <div style={styles.headerMetadata}>
            {selectedRecording.start_time ? formatDateTimeLocal(selectedRecording.start_time) : 'Unknown'} · {formatDurationSeconds(selectedRecording.duration)}
          </div>
        </div>

        {/* Player */}
        <div style={styles.playerContainer}>
          {castData ? (
            <AsciinemaPlayer
              data={castData}
              autoPlay={true}
              loop={false}
              speed={1}
              idleTimeLimit={2}
              theme="monokai"
              fit="both"
              controls={true}
            />
          ) : (
            <div style={styles.loadingText}>Loading recording...</div>
          )}
        </div>
      </div>
    );
  }

  return (
    <div style={styles.root}>
      {/* Header */}
      <div style={styles.listHeader}>
        <h2 style={styles.listTitle}>Session History</h2>
      </div>

      {/* Content */}
      <div style={styles.content}>
        {loading && (
          <div style={styles.centeredMessage}>
            Loading recordings...
          </div>
        )}

        {error && (
          <div style={styles.centeredErrorMessage}>
            {error}
          </div>
        )}

        {!loading && !error && recordings.length === 0 && (
          <div style={styles.centeredMessage}>
            No recordings found. Start a new session to create recordings.
          </div>
        )}

        {!loading && !error && recordings.length > 0 && (
          <div style={styles.recordingList}>
            {recordings.map((recording) => (
              <div
                key={recording.session_id}
                onClick={() => selectRecording(recording)}
                style={mergeStyles(
                  styles.recordingCard,
                  hoveredRecordingId === recording.session_id && styles.recordingCardHover,
                )}
                onMouseEnter={() => setHoveredRecordingId(recording.session_id)}
                onMouseLeave={() => setHoveredRecordingId(null)}
              >
                <div style={styles.recordingRow}>
                  <div style={styles.recordingMainColumn}>
                    <div style={styles.recordingHeadingRow}>
                      <span style={styles.recordingCommand}>
                        {recording.command}
                      </span>
                      {recording.args && recording.args.length > 0 && (
                        <span style={styles.recordingArgs}>
                          {recording.args.join(' ')}
                        </span>
                      )}
                      {recording.tool && (
                        <span style={styles.recordingToolBadge}>
                          {recording.tool}
                        </span>
                      )}
                    </div>
                    <div style={styles.recordingSessionId}>
                      Session ID: {recording.session_id}
                    </div>
                  </div>
                  <div style={styles.recordingMetaColumn}>
                    <div style={styles.recordingStartTime}>
                      {recording.start_time ? formatDateTimeLocal(recording.start_time) : 'Unknown'}
                    </div>
                    <div style={styles.recordingDuration}>
                      Duration: {formatDurationSeconds(recording.duration)}
                    </div>
                  </div>
                </div>
              </div>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}
