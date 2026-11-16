package link

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// maxFrame bounds one JSON frame. Bulk data is a byte stream, never a frame.
const maxFrame = 1 << 20

var errFrameTooBig = errors.New("link: frame too big")

// requestFrame is frame 1 of every stream, written by the opener.
type requestFrame struct {
	Op   string          `json:"op"`
	Body json.RawMessage `json:"body,omitempty"`
}

// replyFrame is frame 2, written by the side that serves the op. Exactly one of Err and Body is
// meaningful. After an Err the stream is closed.
type replyFrame struct {
	Err  *Error          `json:"err,omitempty"`
	Body json.RawMessage `json:"body,omitempty"`
}

// writeFrame writes a 4-byte big-endian length and the JSON of v in one Write, so a frame is never
// interleaved or split on the session.
func writeFrame(w io.Writer, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("link: encode frame: %w", err)
	}
	if len(payload) > maxFrame {
		return errFrameTooBig
	}
	buf := make([]byte, 4, 4+len(payload))
	binary.BigEndian.PutUint32(buf, uint32(len(payload)))
	if _, err := w.Write(append(buf, payload...)); err != nil {
		return fmt.Errorf("link: write frame: %w", err)
	}
	return nil
}

// readFrame reads exactly one frame and not a byte more, so the raw bytes that follow on a stream
// op are still in r. strict rejects unknown fields: requests are strict (a field the peer ignores
// could silently weaken an op), replies are lenient.
func readFrame(r io.Reader, v any, strict bool) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return fmt.Errorf("link: read frame header: %w", err)
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFrame {
		return errFrameTooBig
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return fmt.Errorf("link: read frame: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("link: decode frame: %w", err)
	}
	return nil
}
