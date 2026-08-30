package transport

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/champi-ai/rc-mcp/internal/session"
)

func TestRequestElicitation_AcceptedFlow(t *testing.T) {
	sess := session.New(context.Background(), "sess-1", 10)

	resultCh := make(chan ElicitationResult, 1)
	go func() {
		resultCh <- RequestElicitation(context.Background(), sess, "Execute this command on device X?", json.RawMessage(`{"type":"object"}`), time.Second)
	}()

	var ev session.SSEEvent
	select {
	case ev = <-sess.EventCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for elicitation/create SSE event")
	}
	if !strings.Contains(ev.Data, `"method":"elicitation/create"`) {
		t.Fatalf("event data = %s, want elicitation/create", ev.Data)
	}

	var msg map[string]any
	if err := json.Unmarshal([]byte(ev.Data), &msg); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	id, _ := msg["id"].(string)
	if id == "" {
		t.Fatal("elicitation request missing id")
	}

	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result": map[string]any{
			"action":  "accept",
			"content": map[string]any{"confirm": true},
		},
	}
	raw, _ := json.Marshal(resp)
	if !sess.DeliverResponse(id, raw) {
		t.Fatal("DeliverResponse: no waiting handler found")
	}

	select {
	case result := <-resultCh:
		if result.Declined {
			t.Fatalf("result.Declined = true, want false: reason=%s", result.Reason)
		}
		var content map[string]any
		if err := json.Unmarshal(result.Content, &content); err != nil {
			t.Fatalf("unmarshal content: %v", err)
		}
		if content["confirm"] != true {
			t.Fatalf("content = %v, want confirm=true", content)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for RequestElicitation to return")
	}
}

func TestRequestElicitation_Declined(t *testing.T) {
	sess := session.New(context.Background(), "sess-1", 10)

	resultCh := make(chan ElicitationResult, 1)
	go func() {
		resultCh <- RequestElicitation(context.Background(), sess, "confirm?", nil, time.Second)
	}()

	ev := <-sess.EventCh
	var msg map[string]any
	if err := json.Unmarshal([]byte(ev.Data), &msg); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	id := msg["id"].(string)

	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  map[string]any{"action": "decline"},
	}
	raw, _ := json.Marshal(resp)
	if !sess.DeliverResponse(id, raw) {
		t.Fatal("DeliverResponse: no waiting handler found")
	}

	result := <-resultCh
	if !result.Declined || result.Reason != "declined" {
		t.Fatalf("result = %+v, want Declined=true Reason=declined", result)
	}
}

func TestRequestElicitation_Timeout(t *testing.T) {
	sess := session.New(context.Background(), "sess-1", 10)

	start := time.Now()
	result := RequestElicitation(context.Background(), sess, "confirm?", nil, 50*time.Millisecond)
	elapsed := time.Since(start)

	if !result.Declined || result.Reason != "elicitation_timeout" {
		t.Fatalf("result = %+v, want elicitation_timeout", result)
	}
	if elapsed < 50*time.Millisecond {
		t.Fatalf("returned after %v, want >= 50ms", elapsed)
	}
}

func TestRequestElicitation_ContextCancelled(t *testing.T) {
	sess := session.New(context.Background(), "sess-1", 10)
	ctx, cancel := context.WithCancel(context.Background())

	resultCh := make(chan ElicitationResult, 1)
	go func() {
		resultCh <- RequestElicitation(ctx, sess, "confirm?", nil, 5*time.Second)
	}()

	<-sess.EventCh // drain the elicitation/create event
	cancel()

	select {
	case result := <-resultCh:
		if !result.Declined || result.Reason != "cancelled" {
			t.Fatalf("result = %+v, want Declined=true Reason=cancelled", result)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for cancellation to unblock RequestElicitation")
	}
}

func TestRequestElicitation_DefaultTimeoutApplied(t *testing.T) {
	if DefaultElicitationTimeout != 120*time.Second {
		t.Fatalf("DefaultElicitationTimeout = %v, want 120s", DefaultElicitationTimeout)
	}
}
