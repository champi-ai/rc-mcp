package protocol

import (
	"bytes"
	"testing"
)

func TestBinaryHeaderRoundTrip(t *testing.T) {
	frameTypes := []BinaryFrameType{
		FrameShellStdout, FrameShellStdin, FrameScreenshotPNG, FrameFileContent, FrameInputAck,
	}

	for _, ft := range frameTypes {
		h := BinaryHeader{
			CorrelationPrefix: [4]byte{0xDE, 0xAD, 0xBE, 0xEF},
			StreamSeq:         123456,
			FrameType:         ft,
		}

		buf := make([]byte, BinaryHeaderSize+4)
		copy(buf[BinaryHeaderSize:], []byte{1, 2, 3, 4}) // payload
		EncodeBinaryHeader(buf, h)

		got := DecodeBinaryHeader(buf)
		if got != h {
			t.Errorf("frame type %v: round trip mismatch: got %+v want %+v", ft, got, h)
		}

		// Payload bytes after the header must be untouched.
		if !bytes.Equal(buf[BinaryHeaderSize:], []byte{1, 2, 3, 4}) {
			t.Errorf("frame type %v: payload bytes corrupted: %v", ft, buf[BinaryHeaderSize:])
		}
	}
}

func TestBinaryHeaderSize(t *testing.T) {
	if BinaryHeaderSize != 9 {
		t.Fatalf("BinaryHeaderSize = %d, want 9", BinaryHeaderSize)
	}
}

func TestBinaryFrameTypeValues(t *testing.T) {
	cases := map[BinaryFrameType]byte{
		FrameShellStdout:   0x01,
		FrameShellStdin:    0x02,
		FrameScreenshotPNG: 0x03,
		FrameFileContent:   0x04,
		FrameInputAck:      0x05,
	}
	for ft, want := range cases {
		if byte(ft) != want {
			t.Errorf("frame type %v: got byte %#x want %#x", ft, byte(ft), want)
		}
	}
}

func TestCorrelationPrefix(t *testing.T) {
	prefix, err := CorrelationPrefix("de0adbee-e29b-41d4-a716-446655440000")
	if err != nil {
		t.Fatalf("CorrelationPrefix: %v", err)
	}
	want := [4]byte{0xde, 0x0a, 0xdb, 0xee}
	if prefix != want {
		t.Errorf("prefix = %x, want %x", prefix, want)
	}
}

func TestCorrelationPrefix_TooShort(t *testing.T) {
	if _, err := CorrelationPrefix("abc"); err == nil {
		t.Fatal("want error for too-short correlation id")
	}
}

func TestDecodeBinaryHeader_ZeroSeq(t *testing.T) {
	h := BinaryHeader{
		CorrelationPrefix: [4]byte{0, 0, 0, 0},
		StreamSeq:         0,
		FrameType:         FrameShellStdout,
	}
	buf := make([]byte, BinaryHeaderSize)
	EncodeBinaryHeader(buf, h)
	got := DecodeBinaryHeader(buf)
	if got != h {
		t.Errorf("zero-value round trip mismatch: got %+v want %+v", got, h)
	}
}
