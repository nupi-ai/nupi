import { useState, useEffect } from 'react';
import { AsciinemaPlayer } from './AsciinemaPlayer';

interface Recording {
  session_id: string;
  filename: string;
  command: string;
  args?: string[];
  work_dir?: string;
  start_time: string;
  duration: number;
  rows: number;
  cols: number;
  title: string;
  tool?: string;
  recording_path: string;
}

interface HistoryProps {
  daemonPort: number;
}

export function History({ daemonPort }: HistoryProps) {
  const [recordings, setRecordings] = useState<Recording[]>([]);
  const [selectedRecording, setSelectedRecording] = useState<Recording | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    loadRecordings();
  }, [daemonPort]);

  async function loadRecordings() {
    try {
      setLoading(true);
      setError(null);
      console.log('[History] Loading recordings from port:', daemonPort);
      const url = `http://127.0.0.1:${daemonPort}/recordings`;
      console.log('[History] Fetching:', url);
      const response = await fetch(url);
      console.log('[History] Response status:', response.status);
      if (!response.ok) {
        const text = await response.text();
        console.error('[History] Error response:', text);
        throw new Error(`Failed to load recordings: ${response.statusText}`);
      }
      const data = await response.json();
      console.log('[History] Loaded recordings:', data);
      setRecordings(data || []);
    } catch (err) {
      console.error('[History] Failed to load recordings:', err);
      setError(err instanceof Error ? err.message : 'Failed to load recordings');
    } finally {
      setLoading(false);
    }
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
      <div style={{ display: 'flex', flexDirection: 'column', height: '100%', width: '100%' }}>
        {/* Header */}
        <div style={{
          padding: '12px 16px',
          borderBottom: '1px solid #333',
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'space-between',
          backgroundColor: '#1a1a1a'
        }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: '12px' }}>
            <button
              onClick={() => setSelectedRecording(null)}
              style={{
                padding: '6px 12px',
                backgroundColor: '#333',
                border: 'none',
                borderRadius: '4px',
                color: '#fff',
                cursor: 'pointer',
                fontSize: '14px'
              }}
            >
              ← Back
            </button>
            <div style={{ display: 'flex', alignItems: 'center', gap: '8px' }}>
              <h2 style={{ margin: 0, fontSize: '16px', fontFamily: 'monospace' }}>
                {selectedRecording.command}
              </h2>
              {selectedRecording.args && selectedRecording.args.length > 0 && (
                <span style={{ fontSize: '15px', color: '#999', fontFamily: 'monospace' }}>
                  {selectedRecording.args.join(' ')}
                </span>
              )}
            </div>
          </div>
          <div style={{ fontSize: '13px', color: '#999' }}>
            {formatDate(selectedRecording.start_time)} · {formatDuration(selectedRecording.duration)}
          </div>
        </div>

        {/* Player */}
        <div style={{ flex: 1, padding: '16px', backgroundColor: '#0d1117', overflow: 'hidden', display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
          <AsciinemaPlayer
            src={`http://127.0.0.1:${daemonPort}/recordings/${selectedRecording.session_id}`}
            autoPlay={true}
            loop={false}
            speed={1}
            idleTimeLimit={2}
            theme="monokai"
            fit="both"
            controls={true}
          />
        </div>
      </div>
    );
  }

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100%', width: '100%' }}>
      {/* Header */}
      <div style={{
        padding: '12px 16px',
        borderBottom: '1px solid #333',
        backgroundColor: '#1a1a1a'
      }}>
        <h2 style={{ margin: 0, fontSize: '16px' }}>Session History</h2>
      </div>

      {/* Content */}
      <div style={{ flex: 1, overflow: 'auto', padding: '16px' }}>
        {loading && (
          <div style={{ textAlign: 'center', padding: '32px', color: '#999' }}>
            Loading recordings...
          </div>
        )}

        {error && (
          <div style={{ textAlign: 'center', padding: '32px', color: '#f87171' }}>
            {error}
          </div>
        )}

        {!loading && !error && recordings.length === 0 && (
          <div style={{ textAlign: 'center', padding: '32px', color: '#999' }}>
            No recordings found. Start a new session to create recordings.
          </div>
        )}

        {!loading && !error && recordings.length > 0 && (
          <div style={{ display: 'flex', flexDirection: 'column', gap: '8px' }}>
            {recordings.map((recording) => (
              <div
                key={recording.session_id}
                onClick={() => setSelectedRecording(recording)}
                style={{
                  padding: '12px 16px',
                  backgroundColor: '#1a1a1a',
                  border: '1px solid #333',
                  borderRadius: '6px',
                  cursor: 'pointer',
                  transition: 'all 0.2s',
                }}
                onMouseEnter={(e) => {
                  e.currentTarget.style.backgroundColor = '#252525';
                  e.currentTarget.style.borderColor = '#444';
                }}
                onMouseLeave={(e) => {
                  e.currentTarget.style.backgroundColor = '#1a1a1a';
                  e.currentTarget.style.borderColor = '#333';
                }}
              >
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'start' }}>
                  <div style={{ flex: 1 }}>
                    <div style={{ display: 'flex', alignItems: 'center', gap: '8px', marginBottom: '4px' }}>
                      <span style={{ fontSize: '14px', fontWeight: 500, fontFamily: 'monospace' }}>
                        {recording.command}
                      </span>
                      {recording.args && recording.args.length > 0 && (
                        <span style={{ fontSize: '13px', color: '#999', fontFamily: 'monospace' }}>
                          {recording.args.join(' ')}
                        </span>
                      )}
                      {recording.tool && (
                        <span style={{
                          fontSize: '11px',
                          padding: '2px 6px',
                          backgroundColor: '#333',
                          borderRadius: '3px',
                          color: '#4ade80'
                        }}>
                          {recording.tool}
                        </span>
                      )}
                    </div>
                    <div style={{ fontSize: '12px', color: '#666' }}>
                      Session ID: {recording.session_id}
                    </div>
                  </div>
                  <div style={{ textAlign: 'right' }}>
                    <div style={{ fontSize: '13px', color: '#999' }}>
                      {formatDate(recording.start_time)}
                    </div>
                    <div style={{ fontSize: '12px', color: '#666', marginTop: '2px' }}>
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
