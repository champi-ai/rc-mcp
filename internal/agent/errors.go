package agent

import "errors"

var (
	errUnexpectedFirstMessage = errors.New("agent connection: expected hello or pair_request as first message")
	errVersionMismatch        = errors.New("agent connection: protocol version mismatch")
	errAuthFailed             = errors.New("agent connection: authentication failed")
	errPairingRejected        = errors.New("agent connection: pairing rejected")
	errPairingExpired         = errors.New("agent connection: pairing expired")
	errConnectionClosed       = errors.New("agent connection: closed while waiting for approval")
)
