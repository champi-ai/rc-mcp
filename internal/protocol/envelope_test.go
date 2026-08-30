package protocol

import (
	"encoding/json"
	"testing"
	"time"
)

// roundTrip marshals an Envelope to JSON, unmarshals it into a fresh
// Envelope, then re-marshals the raw payload into dst to check field
// fidelity against the original payload value.
func roundTrip(t *testing.T, env Envelope, dst any) Envelope {
	t.Helper()

	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Envelope
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Type != env.Type {
		t.Errorf("type: got %q want %q", got.Type, env.Type)
	}
	if got.ID != env.ID {
		t.Errorf("id: got %q want %q", got.ID, env.ID)
	}
	if got.ProtocolVersion != env.ProtocolVersion {
		t.Errorf("protocolVersion: got %q want %q", got.ProtocolVersion, env.ProtocolVersion)
	}
	if !got.Ts.Equal(env.Ts) {
		t.Errorf("ts: got %v want %v", got.Ts, env.Ts)
	}

	if got.Payload != nil {
		payloadRaw, err := json.Marshal(got.Payload)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		if err := json.Unmarshal(payloadRaw, dst); err != nil {
			t.Fatalf("unmarshal payload into dst: %v", err)
		}
	}

	return got
}

func TestEnvelopeRoundTrip_AllMessageTypes(t *testing.T) {
	ts := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	percent := 42.5

	cases := []struct {
		name    string
		env     Envelope
		payload any
	}{
		{
			name: "hello",
			env: Envelope{
				Type: MsgHello, ID: "corr-1", ProtocolVersion: Version, Ts: ts,
				Payload: HelloPayload{DeviceToken: "tok", Hostname: "host1", Capabilities: []string{"shell", "sysinfo"}},
			},
			payload: &HelloPayload{},
		},
		{
			name: "hello_ack",
			env: Envelope{
				Type: MsgHelloAck, ID: "corr-2", ProtocolVersion: Version, Ts: ts,
				Payload: HelloAckPayload{DeviceID: "dev-1", Resume: true},
			},
			payload: &HelloAckPayload{},
		},
		{
			name: "pair_request",
			env: Envelope{
				Type: MsgPairRequest, ID: "corr-3", Ts: ts,
				Payload: PairRequestPayload{Hostname: "host2"},
			},
			payload: &PairRequestPayload{},
		},
		{
			name: "pair_code",
			env: Envelope{
				Type: MsgPairCode, ID: "corr-4", Ts: ts,
				Payload: PairCodePayload{Code: "ABCD-1234", ExpiresAt: ts.Add(5 * time.Minute)},
			},
			payload: &PairCodePayload{},
		},
		{
			name: "pair_approved",
			env: Envelope{
				Type: MsgPairApproved, ID: "corr-5", Ts: ts,
				Payload: PairApprovedPayload{DeviceID: "dev-1", DeviceToken: "secret-token"},
			},
			payload: &PairApprovedPayload{},
		},
		{
			name: "dispatch",
			env: Envelope{
				Type: MsgDispatch, ID: "corr-6", Ts: ts,
				Payload: DispatchPayload{Tool: "shell_exec", RequestID: "req-1", SessionID: "sess-1", Input: map[string]any{"cmd": "ls"}},
			},
			payload: &DispatchPayload{},
		},
		{
			name: "result",
			env: Envelope{
				Type: MsgResult, ID: "corr-7", Ts: ts,
				Payload: ResultPayload{Tool: "shell_exec", Output: map[string]any{"exitCode": float64(0)}, IsError: false},
			},
			payload: &ResultPayload{},
		},
		{
			name: "progress",
			env: Envelope{
				Type: MsgProgress, ID: "corr-8", Ts: ts,
				Payload: ProgressPayload{Tool: "shell_exec", Percent: &percent, Message: "running"},
			},
			payload: &ProgressPayload{},
		},
		{
			name: "error",
			env: Envelope{
				Type: MsgError, ID: "corr-9", Ts: ts,
				Payload: ErrorPayload{Code: "auth_failed", Message: "bad token"},
			},
			payload: &ErrorPayload{},
		},
		{
			name: "cancel",
			env: Envelope{
				Type: MsgCancel, ID: "corr-10", Ts: ts,
				Payload: CancelPayload{Reason: "client_cancelled"},
			},
			payload: &CancelPayload{},
		},
		{
			name: "ping",
			env: Envelope{
				Type: MsgPing, Ts: ts,
			},
			payload: nil,
		},
		{
			name: "pong",
			env: Envelope{
				Type: MsgPong, Ts: ts,
			},
			payload: nil,
		},
		{
			name: "close",
			env: Envelope{
				Type: MsgClose, ID: "corr-11", Ts: ts,
				Payload: ClosePayload{Reason: "server_shutdown"},
			},
			payload: &ClosePayload{},
		},
	}

	if len(cases) != 13 {
		t.Fatalf("expected all 13 message types to be covered, got %d", len(cases))
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.payload == nil {
				roundTrip(t, tc.env, &struct{}{})
				return
			}
			roundTrip(t, tc.env, tc.payload)

			origRaw, err := json.Marshal(tc.env.Payload)
			if err != nil {
				t.Fatalf("marshal original payload: %v", err)
			}
			gotRaw, err := json.Marshal(tc.payload)
			if err != nil {
				t.Fatalf("marshal round-tripped payload: %v", err)
			}

			var origMap, gotMap map[string]any
			if err := json.Unmarshal(origRaw, &origMap); err != nil {
				t.Fatalf("unmarshal original: %v", err)
			}
			if err := json.Unmarshal(gotRaw, &gotMap); err != nil {
				t.Fatalf("unmarshal round-tripped: %v", err)
			}

			for k, v := range origMap {
				gv, ok := gotMap[k]
				if !ok {
					t.Errorf("field %q missing after round trip", k)
					continue
				}
				if fmtVal(v) != fmtVal(gv) {
					t.Errorf("field %q: got %v want %v", k, gv, v)
				}
			}
		})
	}
}

func fmtVal(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestEnvelope_MessageTypeConstants(t *testing.T) {
	all := []MessageType{
		MsgHello, MsgHelloAck, MsgPairRequest, MsgPairCode, MsgPairApproved,
		MsgDispatch, MsgResult, MsgProgress, MsgError, MsgCancel, MsgPing,
		MsgPong, MsgClose,
	}
	if len(all) != 13 {
		t.Fatalf("expected 13 message types, got %d", len(all))
	}
	seen := map[MessageType]bool{}
	for _, m := range all {
		if seen[m] {
			t.Errorf("duplicate message type %q", m)
		}
		seen[m] = true
	}
}
