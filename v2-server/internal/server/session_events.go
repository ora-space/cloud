package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
)

// chatEvent is the data of one session event message, for a conversation and a workflow node
// session alike: both are sessions of the shared execution service.
type chatEvent struct {
	Sequence   uint64                     `json:"sequence"`
	OccurredAt time.Time                  `json:"occurredAt"`
	Kind       execution.EventKind        `json:"kind"`
	TurnID     string                     `json:"turnId"`
	History    *execution.HistorySnapshot `json:"history,omitempty"`
	TextDelta  string                     `json:"textDelta,omitempty"`
	Result     *chatResult                `json:"result,omitempty"`
	Prompt     string                     `json:"prompt,omitempty"`
}

// turnPrompt reads the text a turn of the session was asked.
type turnPrompt func(ctx context.Context, sessionID, turnID string) (string, error)

// relaySessionEvents writes a session's events from stream as Server-Sent Events, and closes the
// stream. Each event's id is its session sequence, so a reconnecting EventSource resumes after the
// last one it saw. A stream that ends with the session sends an "end" event; a failure sends
// "failure" with a code instead of looking like a clean end. what and id name the stream in logs.
func relaySessionEvents(w http.ResponseWriter, r *http.Request, stream execution.Stream, prompt turnPrompt, what, id string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		_ = stream.Close()
		internal(w, what+" events", errors.New("response writer cannot flush"))
		return
	}
	ctx := r.Context()

	type next struct {
		event execution.Event
		err   error
	}
	events := make(chan next)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			ev, e := stream.Next(ctx)
			select {
			case events <- next{ev, e}:
			case <-ctx.Done():
				return
			}
			if e != nil {
				return
			}
		}
	}()
	// Close unblocks Next; wait so the goroutine never outlives the request.
	defer wg.Wait()
	defer func() { _ = stream.Close() }()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	tick := time.NewTicker(heartbeat)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			_, _ = io.WriteString(w, ": ping\n\n")
		case n := <-events:
			switch {
			case errors.Is(n.err, io.EOF):
				_, _ = io.WriteString(w, "event: end\ndata: {}\n\n")
				flusher.Flush()
				return
			case n.err != nil:
				if ctx.Err() == nil {
					slog.Error(what+" events", what, id, "error", n.err)
					writeEvent(w, "failure", "", map[string]string{"code": eventFailure(n.err)})
					flusher.Flush()
				}
				return
			}
			ev := n.event
			out := chatEvent{Sequence: ev.Sequence, OccurredAt: ev.OccurredAt, Kind: ev.Kind, TurnID: ev.Ref.TurnID, History: ev.History, TextDelta: ev.TextDelta}
			if ev.Result != nil {
				res := resultOf(*ev.Result)
				out.Result = &res
			}
			if ev.Kind == execution.EventTurnEnded {
				// The prompt only lets a page show a turn no history snapshot holds; without it the
				// event is still the turn's authoritative result, so the stream goes on.
				if text, e := prompt(ctx, ev.Ref.SessionID, ev.Ref.TurnID); e == nil {
					out.Prompt = text
				} else if ctx.Err() == nil {
					slog.Warn(what+" event prompt", what, id, "turn", ev.Ref.TurnID, "error", e)
				}
			}
			writeEvent(w, "", strconv.FormatUint(ev.Sequence, 10), out)
		}
		flusher.Flush()
	}
}
