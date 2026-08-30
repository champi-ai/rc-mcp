package transport

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/CloudKeter/rc-mcp/internal/session"
)

// heartbeatInterval is how often the SSE writer sends a comment-only
// keepalive line while idle, so intermediaries (nginx) don't time out the
// connection.
const heartbeatInterval = 20 * time.Second

// serveSSE runs the SSE writer goroutine for a session's GET /mcp stream:
// it optionally replays events since Last-Event-ID, then drains
// sess.EventCh, assigning monotonically increasing id: fields and
// appending each event to the replay buffer as it is written. It blocks
// until the client disconnects or the session is cancelled.
func serveSSE(w http.ResponseWriter, r *http.Request, sess *session.Session) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	if raw := r.Header.Get("Last-Event-ID"); raw != "" {
		lastID, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			http.Error(w, "invalid Last-Event-ID", http.StatusBadRequest)
			return
		}
		events, replayable := sess.ReplayBuffer.Since(lastID)
		if !replayable {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		for _, ev := range events {
			writeSSEFrame(w, ev.ID, ev.Event, ev.Data)
		}
		flusher.Flush()
	} else {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
	}

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case ev, chOpen := <-sess.EventCh:
			if !chOpen {
				_, _ = fmt.Fprint(w, ": session closed\n\n")
				flusher.Flush()
				return
			}
			stored := sess.ReplayBuffer.Append(ev.Event, ev.Data)
			writeSSEFrame(w, stored.ID, stored.Event, stored.Data)
			flusher.Flush()
		case <-ticker.C:
			_, _ = fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		case <-sess.Ctx.Done():
			_, _ = fmt.Fprint(w, "; session expired\n\n")
			flusher.Flush()
			return
		case <-r.Context().Done():
			return
		}
	}
}

func writeSSEFrame(w http.ResponseWriter, id int64, event, data string) {
	if event != "" && event != "message" {
		_, _ = fmt.Fprintf(w, "event: %s\n", event)
	}
	_, _ = fmt.Fprintf(w, "id: %d\n", id)
	// SSE "data:" lines cannot contain literal newlines; split multi-line
	// payloads into multiple data: lines per the SSE spec. JSON-RPC bodies
	// are single-line (compact-encoded) in practice, but this is cheap
	// insurance.
	for _, line := range strings.Split(data, "\n") {
		_, _ = fmt.Fprintf(w, "data: %s\n", line)
	}
	_, _ = fmt.Fprint(w, "\n")
}
