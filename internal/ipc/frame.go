// Package ipc is the wire between a gozellij client and the fabric daemon.
//
// # Why this shape
//
// DESIGN.md leaned towards protobuf, for the reason Zellij uses it: a client and a daemon can be
// different versions, and the protocol has to survive that. Length-prefixed JSON gets the same
// property - unknown fields are ignored, new optional fields are free - without a codegen step, a
// generated-file-is-committed rule, or a protoc in the build. For Phase 1's control traffic
// (add a service, list services, tell me the status) the encoding cost is irrelevant, and being
// able to read a socket dump with your eyes is worth a great deal while the protocol is young.
//
// The exception is pty bytes. Those are high volume and base64 in JSON would be both slower and
// larger, so a frame carries a kind byte and data frames hold raw bytes. One framing, two
// payloads: control is JSON, streams are bytes.
//
//	+--------+------+------------------+
//	| uint32 | kind |     payload      |
//	| length | byte |  length-1 bytes  |
//	+--------+------+------------------+
//
// Length covers the kind byte plus the payload, so a frame is never zero-length and a peer that
// sends one is telling us something is wrong.
package ipc

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Kind says what a frame carries.
type Kind uint8

const (
	// KindRequest is a JSON Request from client to daemon.
	KindRequest Kind = 1
	// KindResponse is a JSON Response from daemon to client.
	KindResponse Kind = 2
	// KindData is raw stream bytes: pty output going out, keystrokes coming in.
	KindData Kind = 3
	// KindEvent is a JSON Event the daemon sends unprompted, e.g. a service changing state.
	KindEvent Kind = 4
)

func (k Kind) String() string {
	switch k {
	case KindRequest:
		return "request"
	case KindResponse:
		return "response"
	case KindData:
		return "data"
	case KindEvent:
		return "event"
	default:
		return fmt.Sprintf("Kind(%d)", uint8(k))
	}
}

// Valid reports whether k is a kind this version understands.
func (k Kind) Valid() bool {
	switch k {
	case KindRequest, KindResponse, KindData, KindEvent:
		return true
	default:
		return false
	}
}

// MaxFrameSize caps what one frame may claim to be.
//
// Without this, a peer that is buggy, wedged, or hostile can send a four byte header claiming four
// gigabytes and we would dutifully try to allocate it. The limit is generous enough for a full
// screen repaint of a very large terminal and far below anything that hurts.
const MaxFrameSize = 8 << 20 // 8 MiB

// ErrFrameTooLarge is returned when a peer announces a frame beyond MaxFrameSize.
var ErrFrameTooLarge = errors.New("frame exceeds the maximum size")

// ErrEmptyFrame is returned for a frame with no kind byte.
var ErrEmptyFrame = errors.New("frame is empty (no kind byte)")

// ErrUnknownKind is returned for a frame whose kind this version does not know.
//
// Deliberately an error rather than a silent skip. A daemon quietly discarding frames it does not
// understand is how a version mismatch turns into "it just does nothing sometimes", which is the
// failure mode this project exists to avoid. The caller can decide to tolerate it; it cannot
// decide that if it is never told.
var ErrUnknownKind = errors.New("unknown frame kind")

// Writer writes frames to a stream. It is safe for concurrent use: the pty pump and the response
// path both write to the same connection, and two interleaved frames would corrupt the stream
// beyond recovery.
type Writer struct {
	mu  sync.Mutex
	w   io.Writer
	buf []byte
}

// NewWriter wraps w.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w, buf: make([]byte, 0, 4096)}
}

// WriteFrame writes one frame.
func (fw *Writer) WriteFrame(k Kind, payload []byte) error {
	if !k.Valid() {
		return fmt.Errorf("%w: %d", ErrUnknownKind, uint8(k))
	}
	n := len(payload) + 1
	if n > MaxFrameSize {
		return fmt.Errorf("%w: %d bytes (max %d)", ErrFrameTooLarge, n, MaxFrameSize)
	}

	fw.mu.Lock()
	defer fw.mu.Unlock()

	// One write, so a concurrent writer cannot interleave with a partially written frame.
	fw.buf = fw.buf[:0]
	fw.buf = binary.BigEndian.AppendUint32(fw.buf, uint32(n))
	fw.buf = append(fw.buf, byte(k))
	fw.buf = append(fw.buf, payload...)

	if _, err := fw.w.Write(fw.buf); err != nil {
		return fmt.Errorf("writing %s frame: %w", k, err)
	}
	return nil
}

// WriteJSON marshals v and writes it as a frame of kind k.
func (fw *Writer) WriteJSON(k Kind, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encoding %s frame: %w", k, err)
	}
	return fw.WriteFrame(k, b)
}

// Reader reads frames from a stream. Unlike Writer it is not safe for concurrent use; one
// goroutine owns the read side of a connection.
type Reader struct {
	r      io.Reader
	header [4]byte
	buf    []byte
}

// NewReader wraps r.
func NewReader(r io.Reader) *Reader {
	return &Reader{r: r}
}

// ReadFrame reads the next frame. The returned slice is only valid until the next call: copy it if
// you need to keep it. At a clean end of stream it returns io.EOF.
func (fr *Reader) ReadFrame() (Kind, []byte, error) {
	if _, err := io.ReadFull(fr.r, fr.header[:]); err != nil {
		// A clean EOF on the header boundary is the peer hanging up, which is normal and is
		// passed through unwrapped so callers can test it with errors.Is. A partial header is
		// a truncated stream and is not.
		if errors.Is(err, io.EOF) {
			return 0, nil, io.EOF
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, nil, fmt.Errorf("truncated frame header: %w", err)
		}
		return 0, nil, err
	}

	n := binary.BigEndian.Uint32(fr.header[:])
	if n == 0 {
		return 0, nil, ErrEmptyFrame
	}
	if n > MaxFrameSize {
		// Say the number. "Frame too large" without it sends the reader to the source to
		// find out what the limit is.
		return 0, nil, fmt.Errorf("%w: peer announced %d bytes (max %d)", ErrFrameTooLarge, n, MaxFrameSize)
	}

	if cap(fr.buf) < int(n) {
		fr.buf = make([]byte, n)
	}
	fr.buf = fr.buf[:n]
	if _, err := io.ReadFull(fr.r, fr.buf); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, nil, fmt.Errorf("truncated frame body: wanted %d bytes: %w", n, err)
		}
		return 0, nil, err
	}

	k := Kind(fr.buf[0])
	if !k.Valid() {
		return k, nil, fmt.Errorf("%w: %d", ErrUnknownKind, uint8(k))
	}
	return k, fr.buf[1:], nil
}

// ReadJSON reads the next frame and decodes its payload into v, checking the kind.
func (fr *Reader) ReadJSON(want Kind, v any) error {
	k, payload, err := fr.ReadFrame()
	if err != nil {
		return err
	}
	if k != want {
		return fmt.Errorf("expected a %s frame, got %s", want, k)
	}
	if err := json.Unmarshal(payload, v); err != nil {
		return fmt.Errorf("decoding %s frame: %w", k, err)
	}
	return nil
}
