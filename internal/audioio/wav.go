package audioio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

const (
	maxChunkSize     = 256 * 1024 * 1024
	maxDataChunkSize = 500 * 1024 * 1024
)

// Reader provides sequential access to PCM samples from a WAV stream.
type Reader struct {
	rc        io.ReadCloser
	remaining uint32
	format    eventbus.AudioFormat
}

// NewReader parses a WAV header and prepares to stream PCM samples.
func NewReader(rc io.ReadCloser) (*Reader, error) {
	if rc == nil {
		return nil, errors.New("wav: reader nil")
	}

	header := make([]byte, 12)
	if _, err := io.ReadFull(rc, header); err != nil {
		return nil, fmt.Errorf("wav: read header: %w", err)
	}
	if string(header[0:4]) != "RIFF" || string(header[8:12]) != "WAVE" {
		return nil, errors.New("wav: invalid header")
	}

	var (
		fmtParsed bool
		dataSize  uint32
		format    eventbus.AudioFormat
	)

	for {
		chunkHeader := make([]byte, 8)
		if _, err := io.ReadFull(rc, chunkHeader); err != nil {
			return nil, fmt.Errorf("wav: read chunk header: %w", err)
		}
		chunkID := string(chunkHeader[0:4])
		chunkSize := binary.LittleEndian.Uint32(chunkHeader[4:8])
		if chunkSize > maxChunkSize {
			return nil, fmt.Errorf("wav: chunk %s too large (%d bytes)", strings.TrimSpace(chunkID), chunkSize)
		}

		switch chunkID {
		case "fmt ":
			if chunkSize < 16 {
				return nil, errors.New("wav: invalid fmt chunk")
			}
			payload := make([]byte, chunkSize)
			if _, err := io.ReadFull(rc, payload); err != nil {
				return nil, fmt.Errorf("wav: read fmt chunk: %w", err)
			}
			audioFmt := binary.LittleEndian.Uint16(payload[0:2])
			if audioFmt != 1 { // PCM
				return nil, fmt.Errorf("wav: unsupported audio format %d", audioFmt)
			}
			channels := binary.LittleEndian.Uint16(payload[2:4])
			sampleRate := binary.LittleEndian.Uint32(payload[4:8])
			bitDepth := binary.LittleEndian.Uint16(payload[14:16])

			if channels == 0 || sampleRate == 0 || bitDepth == 0 {
				return nil, errors.New("wav: invalid format values")
			}
			format = eventbus.AudioFormat{
				Encoding:   eventbus.AudioEncodingPCM16,
				SampleRate: int(sampleRate),
				Channels:   int(channels),
				BitDepth:   int(bitDepth),
				// FrameDuration intentionally left as zero so server defaults apply.
			}
			fmtParsed = true

			// Skip any extension bytes beyond the standard 16.
			extra := int(chunkSize) - 16
			if extra > 0 {
				if _, err := io.CopyN(io.Discard, rc, int64(extra)); err != nil {
					return nil, fmt.Errorf("wav: skip fmt padding: %w", err)
				}
			}
		case "data":
			if chunkSize > maxDataChunkSize {
				return nil, fmt.Errorf("wav: data chunk too large (%d bytes)", chunkSize)
			}
			dataSize = chunkSize
			break
		default:
			skip := int64(chunkSize)
			if skip%2 == 1 {
				skip++
			}
			if _, err := io.CopyN(io.Discard, rc, skip); err != nil {
				return nil, fmt.Errorf("wav: skip chunk %s: %w", strings.TrimSpace(chunkID), err)
			}
		}

		if fmtParsed && dataSize > 0 {
			break
		}
	}

	if !fmtParsed {
		return nil, errors.New("wav: fmt chunk missing")
	}
	if dataSize == 0 {
		return nil, errors.New("wav: data chunk missing")
	}

	return &Reader{
		rc:        rc,
		remaining: dataSize,
		format:    format,
	}, nil
}

// Read forwards PCM samples while tracking remaining bytes.
func (r *Reader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	if uint32(len(p)) > r.remaining {
		p = p[:r.remaining]
	}

	n, err := r.rc.Read(p)
	if n > 0 {
		if uint32(n) > r.remaining {
			n = int(r.remaining)
		}
		r.remaining -= uint32(n)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return n, err
	}
	if r.remaining == 0 {
		return n, io.EOF
	}
	return n, nil
}

// Format returns the parsed audio format.
func (r *Reader) Format() eventbus.AudioFormat {
	return r.format
}

// Close releases the underlying reader.
func (r *Reader) Close() error {
	if r.rc == nil {
		return nil
	}
	err := r.rc.Close()
	r.rc = nil
	return err
}

// WriteSeekCloser combines writing, seeking and closing.
type WriteSeekCloser interface {
	io.WriteSeeker
	io.Closer
}

// Writer encodes PCM samples into a WAV container.
type Writer struct {
	w        WriteSeekCloser
	format   eventbus.AudioFormat
	dataSize uint32
	closed   bool
}

// NewWriter creates a WAV writer with the provided PCM characteristics.
func NewWriter(w WriteSeekCloser, format eventbus.AudioFormat) (*Writer, error) {
	if w == nil {
		return nil, errors.New("wav: writer nil")
	}
	if strings.TrimSpace(string(format.Encoding)) == "" {
		format.Encoding = eventbus.AudioEncodingPCM16
	}
	if format.SampleRate <= 0 || format.Channels <= 0 || format.BitDepth <= 0 {
		return nil, errors.New("wav: invalid format parameters")
	}

	writer := &Writer{
		w:      w,
		format: format,
	}
	if err := writer.writeHeader(); err != nil {
		return nil, err
	}
	return writer, nil
}

func (w *Writer) writeHeader() error {
	if _, err := w.w.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("wav: seek start: %w", err)
	}
	header := make([]byte, 44)
	copy(header[0:4], "RIFF")
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16) // PCM fmt chunk size
	binary.LittleEndian.PutUint16(header[20:22], 1)  // PCM format
	binary.LittleEndian.PutUint16(header[22:24], uint16(w.format.Channels))
	binary.LittleEndian.PutUint32(header[24:28], uint32(w.format.SampleRate))
	byteRate := w.format.SampleRate * w.format.Channels * w.format.BitDepth / 8
	blockAlign := w.format.Channels * w.format.BitDepth / 8
	binary.LittleEndian.PutUint32(header[28:32], uint32(byteRate))
	binary.LittleEndian.PutUint16(header[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(header[34:36], uint16(w.format.BitDepth))
	copy(header[36:40], "data")
	binary.LittleEndian.PutUint32(header[40:44], 0)

	if _, err := w.w.Write(header); err != nil {
		return fmt.Errorf("wav: write header: %w", err)
	}
	return nil
}

// Write appends PCM data to the WAV file.
func (w *Writer) Write(p []byte) (int, error) {
	if w.closed {
		return 0, errors.New("wav: writer closed")
	}
	if len(p) == 0 {
		return 0, nil
	}
	n, err := w.w.Write(p)
	if n > 0 {
		w.dataSize += uint32(n)
	}
	return n, err
}

// Close finalises the WAV header and closes the underlying writer.
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true

	chunkSize := w.dataSize + 36
	if _, err := w.w.Seek(4, io.SeekStart); err != nil {
		return fmt.Errorf("wav: seek chunk size: %w", err)
	}
	if err := binary.Write(w.w, binary.LittleEndian, chunkSize); err != nil {
		return fmt.Errorf("wav: write chunk size: %w", err)
	}
	if _, err := w.w.Seek(40, io.SeekStart); err != nil {
		return fmt.Errorf("wav: seek data size: %w", err)
	}
	if err := binary.Write(w.w, binary.LittleEndian, w.dataSize); err != nil {
		return fmt.Errorf("wav: write data size: %w", err)
	}
	if _, err := w.w.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("wav: seek end: %w", err)
	}
	return w.w.Close()
}

// Duration estimates playback time based on accumulated samples.
func (w *Writer) Duration() time.Duration {
	sampleRate := int64(w.format.SampleRate)
	channels := int64(w.format.Channels)
	bitDepth := int64(w.format.BitDepth)

	if sampleRate <= 0 || channels <= 0 || bitDepth <= 0 {
		return 0
	}

	bytesPerSecond := sampleRate * channels * bitDepth / 8
	if bytesPerSecond <= 0 || bytesPerSecond > 1<<30 {
		return 0
	}
	return time.Duration(float64(w.dataSize) / float64(bytesPerSecond) * float64(time.Second))
}
