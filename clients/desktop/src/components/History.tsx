import { useState, useEffect } from 'react';
import { invoke } from '@tauri-apps/api/core';
import { AsciinemaPlayer } from './AsciinemaPlayer';
import * as styles from './historyStyles';

interface Recording {
  session_id: string;
  filename: string;
  command: string;
  args: string[];
  work_dir?: string;
  start_time?: string;
  duration: number;
  rows: number;
  cols: number;
  title: string;
  tool?: string;
  recording_path: string;
}

export function History() {
  const [recordings, setRecordings] = useState<Recording[]>([]);
  const [selectedRecording, setSelectedRecording] = useState<Recording | null>(null);
  const [castData, setCastData] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    loadRecordings();
  }, []);

  async function loadRecordings() {
    try {
      setLoading(true);
      setError(null);
      const data = await invoke<Recording[]>('list_recordings', {});
      setRecordings(data || []);
    } catch (err) {
      console.error('[History] Failed to load recordings:', err);
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setLoading(false);
    }
  }

  async function loadCastData(sessionId: string) {
    try {
      setCastData(null);
      const data = await invoke<string>('get_recording', { sessionId });
      setCastData(data);
    } catch (err) {
      console.error('[History] Failed to load cast data:', err);
      setError(err instanceof Error ? err.message : String(err));
    }
  }

  function selectRecording(recording: Recording) {
    setSelectedRecording(recording);
    loadCastData(recording.session_id);
  }

  function formatDuration(seconds: number): string {
    const mins = Math.floor(seconds / 60);
    const secs = Math.floor(seconds % 60);
    return `${mins}:${secs.toString().padStart(2, '0')}`;
  }

  function formatDate(dateStr: string): string {
    const date = new Date(dateStr);
    return date.toLocaleString();
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
            {selectedRecording.start_time ? formatDate(selectedRecording.start_time) : 'Unknown'} · {formatDuration(selectedRecording.duration)}
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
                style={styles.recordingCard}
                onMouseEnter={(e) => {
                  styles.highlightRecordingCard(e.currentTarget);
                }}
                onMouseLeave={(e) => {
                  styles.resetRecordingCard(e.currentTarget);
                }}
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
                      {recording.start_time ? formatDate(recording.start_time) : 'Unknown'}
                    </div>
                    <div style={styles.recordingDuration}>
                      Duration: {formatDuration(recording.duration)}
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
