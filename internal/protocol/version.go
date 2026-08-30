package protocol

import "time"

// Version is the current wire protocol version this build declares (in an
// agent's "hello") and prefers (in a server's "hello_ack"). New agent and
// server builds always speak the newest Version.
//
// Upgrade process for a future version bump (Section 2.2, Section 19):
//  1. Introduce the new version's wire changes additively -- new envelope
//     fields, new BinaryFrameType constants, new optional payload fields
//     -- so an implementation that only understands the previous version
//     can still parse messages it receives (ignoring fields it doesn't
//     know) as long as the *previous* version's fields are still present
//     or optional.
//  2. Bump Version to the new value, but leave every prior version in
//     SupportedVersions. A server or agent built at this point still
//     accepts hellos declaring an older version, and should behave
//     compatibly with that older peer for the lifetime of that
//     connection (no version-specific fields the older peer wouldn't
//     understand).
//  3. Once every deployed agent has been confirmed upgraded (e.g. via the
//     auto-update mechanism, or an operator-driven fleet rollout), an
//     operator may choose to drop the oldest entry from
//     SupportedVersions in a subsequent release, at which point an agent
//     still declaring that version receives VersionMismatchEnvelope and
//     the connection is closed with WS close code 1002
//     (websocket.CloseProtocolError) -- a clear, immediate error rather
//     than a silent failure or partial handshake.
//
// Version "2" adds the FrameInputAck BinaryFrameType for the input_key /
// input_mouse_click / input_mouse_move / input_type tools' action
// acknowledgment stream; nothing about the envelope or existing frame
// types changed, so it remains wire-compatible with "1" and both are kept
// in SupportedVersions.
const Version = "2"

// SupportedVersions lists every protocol version this build accepts, in
// preference order (current version first). Do not remove an entry until
// no supported agent build still declares it -- see the upgrade process
// documented on Version above.
var SupportedVersions = []string{Version, "1"}

// IsSupportedVersion reports whether v is a protocol version this build
// accepts.
func IsSupportedVersion(v string) bool {
	for _, s := range SupportedVersions {
		if s == v {
			return true
		}
	}
	return false
}

// VersionMismatchEnvelope builds the error Envelope sent to an agent (or
// client) whose requested protocolVersion is not supported by this server.
// Per Section 2.2, the connection is then closed with WS code 1002.
func VersionMismatchEnvelope(requested string) Envelope {
	return Envelope{
		Type: MsgError,
		Ts:   time.Now().UTC(),
		Payload: ErrorPayload{
			Code: "version_mismatch",
			Message: "unsupported protocol version " + requested +
				"; server supports: " + joinVersions(SupportedVersions),
		},
	}
}

func joinVersions(vs []string) string {
	out := ""
	for i, v := range vs {
		if i > 0 {
			out += ", "
		}
		out += v
	}
	return out
}
