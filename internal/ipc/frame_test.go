package ipc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestRoundTripAllKinds(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)

	payloads := []struct {
		kind Kind
		data []byte
	}{
		{KindRequest, []byte(`{"op":"service.list"}`)},
		{KindResponse, []byte(`{"ok":true}`)},
		{KindData, []byte{0x1b, '[', '2', 'J', 0x00, 0xff}}, // raw bytes, including NUL and 0xff
		{KindEvent, []byte(`{"service":"web"}`)},
	}
	for _, p := range payloads {
		if err := w.WriteFrame(p.kind, p.data); err != nil {
			t.Fatalf("WriteFrame(%s): %v", p.kind, err)
		}
	}

	r := NewReader(&buf)
	for _, want := range payloads {
		k, got, err := r.ReadFrame()
		if err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
		if k != want.kind {
			t.Errorf("kind = %s, want %s", k, want.kind)
		}
		if !bytes.Equal(got, want.data) {
			t.Errorf("payload = %v, want %v", got, want.data)
		}
	}
	if _, _, err := r.ReadFrame(); !errors.Is(err, io.EOF) {
		t.Errorf("after the last frame = %v, want io.EOF", err)
	}
}

// Raw bytes must survive exactly. A pty stream is not text and anything that "helpfully" mangles
// it shows up as a corrupted screen.
func TestDataFramesAreByteExact(t *testing.T) {
	payload := make([]byte, 256)
	for i := range payload {
		payload[i] = byte(i)
	}
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.WriteFrame(KindData, payload); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	k, got, err := NewReader(&buf).ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if k != KindData {
		t.Fatalf("kind = %s, want data", k)
	}
	if !bytes.Equal(got, payload) {
		t.Error("a data frame did not survive the round trip byte for byte")
	}
}

func TestEmptyPayloadIsFine(t *testing.T) {
	var buf bytes.Buffer
	if err := NewWriter(&buf).WriteFrame(KindData, nil); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	k, got, err := NewReader(&buf).ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if k != KindData || len(got) != 0 {
		t.Errorf("kind=%s len=%d, want data and 0", k, len(got))
	}
}

func TestJSONRoundTrip(t *testing.T) {
	type msg struct {
		Op   string `json:"op"`
		N    int    `json:"n"`
		Name string `json:"name,omitempty"`
	}
	var buf bytes.Buffer
	if err := NewWriter(&buf).WriteJSON(KindRequest, msg{Op: "service.start", N: 7, Name: "web"}); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	var got msg
	if err := NewReader(&buf).ReadJSON(KindRequest, &got); err != nil {
		t.Fatalf("ReadJSON: %v", err)
	}
	if got.Op != "service.start" || got.N != 7 || got.Name != "web" {
		t.Errorf("got %+v, want the same message back", got)
	}
}

// The reason for JSON: a newer peer adding a field must not break an older one.
func TestUnknownJSONFieldsAreTolerated(t *testing.T) {
	var buf bytes.Buffer
	newer := map[string]any{"op": "service.start", "name": "web", "fancy_new_field": []int{1, 2, 3}}
	if err := NewWriter(&buf).WriteJSON(KindRequest, newer); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	var older struct {
		Op   string `json:"op"`
		Name string `json:"name"`
	}
	if err := NewReader(&buf).ReadJSON(KindRequest, &older); err != nil {
		t.Fatalf("an older peer could not read a newer message: %v", err)
	}
	if older.Op != "service.start" || older.Name != "web" {
		t.Errorf("got %+v, want the fields it knows about", older)
	}
}

func TestReadJSONRejectsTheWrongKind(t *testing.T) {
	var buf bytes.Buffer
	if err := NewWriter(&buf).WriteJSON(KindEvent, map[string]string{"a": "b"}); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	var v map[string]string
	err := NewReader(&buf).ReadJSON(KindRequest, &v)
	if err == nil {
		t.Fatal("reading an event as a request was accepted")
	}
	if !strings.Contains(err.Error(), "event") || !strings.Contains(err.Error(), "request") {
		t.Errorf("error should name both kinds, got: %v", err)
	}
}

// A peer announcing an enormous frame must be refused, not allocated for.
func TestOversizedFrameIsRefusedWithoutAllocating(t *testing.T) {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], 3<<30) // 3 GiB
	_, _, err := NewReader(bytes.NewReader(hdr[:])).ReadFrame()
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
	// The message must carry the numbers, or the reader has to go and find the limit in the
	// source.
	if !strings.Contains(err.Error(), "3221225472") {
		t.Errorf("error does not say what was announced: %v", err)
	}
}

func TestWritingOversizedFrameIsRefused(t *testing.T) {
	var buf bytes.Buffer
	err := NewWriter(&buf).WriteFrame(KindData, make([]byte, MaxFrameSize+1))
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("err = %v, want ErrFrameTooLarge", err)
	}
	if buf.Len() != 0 {
		t.Errorf("a refused frame still wrote %d bytes to the stream", buf.Len())
	}
}

func TestZeroLengthFrameIsRejected(t *testing.T) {
	var hdr [4]byte // length 0
	_, _, err := NewReader(bytes.NewReader(hdr[:])).ReadFrame()
	if !errors.Is(err, ErrEmptyFrame) {
		t.Errorf("err = %v, want ErrEmptyFrame", err)
	}
}

// A frame kind we do not understand is an error, not something to skip quietly. A daemon silently
// discarding frames is how a version mismatch becomes "it just does nothing sometimes".
func TestUnknownKindIsReportedNotSkipped(t *testing.T) {
	var buf bytes.Buffer
	// Handcraft a frame with kind 99.
	buf.Write([]byte{0, 0, 0, 4, 99, 'a', 'b', 'c'})
	_, _, err := NewReader(&buf).ReadFrame()
	if !errors.Is(err, ErrUnknownKind) {
		t.Errorf("err = %v, want ErrUnknownKind", err)
	}
	if !strings.Contains(err.Error(), "99") {
		t.Errorf("error does not say which kind: %v", err)
	}
}

func TestWritingUnknownKindIsRefused(t *testing.T) {
	var buf bytes.Buffer
	if err := NewWriter(&buf).WriteFrame(Kind(42), []byte("x")); !errors.Is(err, ErrUnknownKind) {
		t.Errorf("err = %v, want ErrUnknownKind", err)
	}
}

// A stream that stops mid-frame must say so, not look like a clean end.
func TestTruncatedStreamsAreDistinguishedFromCleanEOF(t *testing.T) {
	var full bytes.Buffer
	if err := NewWriter(&full).WriteFrame(KindData, []byte("hello world")); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	complete := full.Bytes()

	t.Run("truncated header", func(t *testing.T) {
		_, _, err := NewReader(bytes.NewReader(complete[:2])).ReadFrame()
		if errors.Is(err, io.EOF) {
			t.Fatal("a truncated header looked like a clean end of stream")
		}
		if !strings.Contains(err.Error(), "header") {
			t.Errorf("error should mention the header: %v", err)
		}
	})

	t.Run("truncated body", func(t *testing.T) {
		_, _, err := NewReader(bytes.NewReader(complete[:len(complete)-3])).ReadFrame()
		if errors.Is(err, io.EOF) {
			t.Fatal("a truncated body looked like a clean end of stream")
		}
		if !strings.Contains(err.Error(), "body") {
			t.Errorf("error should mention the body: %v", err)
		}
	})
}

// Frames arriving in small pieces - which is what a socket does - must reassemble.
func TestFramesSurviveADribblingReader(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	for i := 0; i < 5; i++ {
		if err := w.WriteFrame(KindData, bytes.Repeat([]byte{byte('a' + i)}, 100)); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}
	}

	r := NewReader(&dribble{data: buf.Bytes()})
	for i := 0; i < 5; i++ {
		k, got, err := r.ReadFrame()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if k != KindData || len(got) != 100 || got[0] != byte('a'+i) {
			t.Errorf("frame %d = kind %s, %d bytes starting %q", i, k, len(got), got[:1])
		}
	}
}

// dribble hands over one byte at a time, the way a slow socket does.
type dribble struct {
	data []byte
	pos  int
}

func (d *dribble) Read(p []byte) (int, error) {
	if d.pos >= len(d.data) {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = d.data[d.pos]
	d.pos++
	return 1, nil
}

// The pty pump and the response path share a connection. Two interleaved frames would corrupt the
// stream past recovery, so the writer must serialise them.
func TestConcurrentWritersDoNotInterleave(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)

	const writers, each = 8, 50
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			payload := bytes.Repeat([]byte{byte('A' + i)}, 200)
			for j := 0; j < each; j++ {
				if err := w.WriteFrame(KindData, payload); err != nil {
					t.Errorf("WriteFrame: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()

	// Every frame must come back whole, with a single repeated byte throughout.
	r := NewReader(&buf)
	count := 0
	for {
		k, got, err := r.ReadFrame()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("frame %d: %v", count, err)
		}
		if k != KindData || len(got) != 200 {
			t.Fatalf("frame %d is %s with %d bytes: frames interleaved", count, k, len(got))
		}
		for _, c := range got {
			if c != got[0] {
				t.Fatalf("frame %d has mixed contents: frames interleaved", count)
			}
		}
		count++
	}
	if count != writers*each {
		t.Errorf("read %d frames, want %d", count, writers*each)
	}
}

func TestKindStringsAreReadable(t *testing.T) {
	for k, want := range map[Kind]string{
		KindRequest:  "request",
		KindResponse: "response",
		KindData:     "data",
		KindEvent:    "event",
	} {
		if got := k.String(); got != want {
			t.Errorf("Kind(%d).String() = %q, want %q", uint8(k), got, want)
		}
	}
	if got := Kind(77).String(); !strings.Contains(got, "77") {
		t.Errorf("an unknown kind should still print its number, got %q", got)
	}
}
