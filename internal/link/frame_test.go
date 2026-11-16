package link

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestFrameLeavesTrailingBytes(t *testing.T) {
	var buf bytes.Buffer
	if err := writeFrame(&buf, requestFrame{Op: "a.b"}); err != nil {
		t.Fatal(err)
	}
	buf.WriteString("raw bytes follow")
	var rq requestFrame
	if err := readFrame(&buf, &rq, true); err != nil || rq.Op != "a.b" {
		t.Fatalf("%v %v", rq, err)
	}
	if buf.String() != "raw bytes follow" {
		t.Fatalf("readFrame consumed raw bytes: %q", buf.String())
	}
}

func TestFrameStrictForRequestsLenientForReplies(t *testing.T) {
	frame := append([]byte{0, 0, 0, 16}, `{"op":"a","x":1}`...)
	var rq requestFrame
	if err := readFrame(bytes.NewReader(frame), &rq, true); err == nil {
		t.Fatal("a request with an unknown field must be refused")
	}
	var lenient requestFrame
	if err := readFrame(bytes.NewReader(frame), &lenient, false); err != nil || lenient.Op != "a" {
		t.Fatalf("a reply with an unknown field must be accepted: %v %v", lenient, err)
	}
}

func TestFrameBounds(t *testing.T) {
	var rq requestFrame
	if err := readFrame(bytes.NewReader([]byte{0xff, 0, 0, 0}), &rq, true); !errors.Is(err, errFrameTooBig) {
		t.Fatalf("oversize read: %v", err)
	}
	big := replyFrame{Body: []byte(`"` + strings.Repeat("x", maxFrame) + `"`)}
	if err := writeFrame(io.Discard, big); !errors.Is(err, errFrameTooBig) {
		t.Fatalf("oversize write: %v", err)
	}
	if err := readFrame(bytes.NewReader([]byte{0, 0, 0, 9, '{'}), &rq, true); err == nil {
		t.Fatal("a truncated frame must fail")
	}
	if err := readFrame(bytes.NewReader([]byte{0, 0, 0, 3, 'n', 'o', 'p'}), &rq, true); err == nil {
		t.Fatal("a frame that is not JSON must fail")
	}
}
