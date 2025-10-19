package audioio

import (
	"bytes"
	"io"
	"os"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/eventbus"
)

func TestWriterReaderRoundTrip(t *testing.T) {
	t.Parallel()

	samples := []byte{0x00, 0x10, 0xFF, 0x7F, 0x01, 0x00}
	format := eventbus.AudioFormat{
		Encoding:      eventbus.AudioEncodingPCM16,
		SampleRate:    16000,
		Channels:      1,
		BitDepth:      16,
		FrameDuration: 20 * time.Millisecond,
	}

	file, err := os.CreateTemp(t.TempDir(), "roundtrip-*.wav")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}

	writer, err := NewWriter(file, format)
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	if _, err := writer.Write(samples); err != nil {
		t.Fatalf("write samples: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	reader, err := os.Open(file.Name())
	if err != nil {
		t.Fatalf("open wav: %v", err)
	}
	defer reader.Close()

	wavReader, err := NewReader(reader)
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}
	defer wavReader.Close()

	gotFormat := wavReader.Format()
	if gotFormat.SampleRate != format.SampleRate || gotFormat.Channels != format.Channels || gotFormat.BitDepth != format.BitDepth {
		t.Fatalf("unexpected format: %+v", gotFormat)
	}

	data, err := io.ReadAll(wavReader)
	if err != nil && err != io.EOF {
		t.Fatalf("read data: %v", err)
	}
	if !bytes.Equal(samples, data) {
		t.Fatalf("unexpected payload: %v", data)
	}
}

func TestNewReaderRejectsInvalidHeader(t *testing.T) {
	t.Parallel()

	payload := []byte("not-a-wav")
	reader := io.NopCloser(bytes.NewReader(payload))
	if _, err := NewReader(reader); err == nil {
		t.Fatalf("expected error for invalid header")
	}
}
