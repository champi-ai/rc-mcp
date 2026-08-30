package protocol

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
)

// BinaryFrameType identifies the content of a binary WebSocket frame.
type BinaryFrameType byte

const (
	FrameShellStdout   BinaryFrameType = 0x01
	FrameShellStdin    BinaryFrameType = 0x02
	FrameScreenshotPNG BinaryFrameType = 0x03
	FrameFileContent   BinaryFrameType = 0x04
	// FrameInputAck carries a per-action acknowledgment for the input_key /
	// input_mouse_click / input_mouse_move / input_type tools (protocol
	// version "2", Section 19). Introduced additively: a "1"-speaking peer
	// never sends or expects this frame type.
	FrameInputAck BinaryFrameType = 0x05
)

// BinaryHeader is the fixed 9-byte header for binary WebSocket frames.
// Layout: [4 bytes correlation ID prefix] [4 bytes stream sequence] [1 byte frame type]
type BinaryHeader struct {
	CorrelationPrefix [4]byte         // first 4 bytes of the correlation UUID
	StreamSeq         uint32          // monotonically increasing per correlation ID
	FrameType         BinaryFrameType // content type
}

// BinaryHeaderSize is the fixed size of the binary frame header.
const BinaryHeaderSize = 9

// EncodeBinaryHeader writes the header into the first 9 bytes of buf.
// buf must be at least BinaryHeaderSize bytes long.
func EncodeBinaryHeader(buf []byte, h BinaryHeader) {
	copy(buf[0:4], h.CorrelationPrefix[:])
	binary.BigEndian.PutUint32(buf[4:8], h.StreamSeq)
	buf[8] = byte(h.FrameType)
}

// DecodeBinaryHeader reads the header from the first 9 bytes of buf.
// buf must be at least BinaryHeaderSize bytes long.
func DecodeBinaryHeader(buf []byte) BinaryHeader {
	var h BinaryHeader
	copy(h.CorrelationPrefix[:], buf[0:4])
	h.StreamSeq = binary.BigEndian.Uint32(buf[4:8])
	h.FrameType = BinaryFrameType(buf[8])
	return h
}

// CorrelationPrefix derives the 4-byte binary-frame correlation prefix from
// a dispatch's string correlation ID (a hyphenated UUIDv4), per Section 2.2:
// "the first 4 bytes of the UUID". It parses the UUID's raw 16 bytes and
// returns the first 4.
func CorrelationPrefix(id string) ([4]byte, error) {
	var out [4]byte
	raw := strings.ReplaceAll(id, "-", "")
	if len(raw) < 8 {
		return out, fmt.Errorf("protocol: correlation id %q too short to derive a prefix", id)
	}
	b, err := hex.DecodeString(raw[0:8])
	if err != nil {
		return out, fmt.Errorf("protocol: parse correlation id %q: %w", id, err)
	}
	copy(out[:], b)
	return out, nil
}
