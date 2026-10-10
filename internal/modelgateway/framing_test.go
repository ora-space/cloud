package modelgateway

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSSEBOMAndAllNewlineFormatsKeepSuccessAndSuppressErrors(t *testing.T) {
	secret := randomSecret(t)
	for _, ending := range []string{"\n", "\r\n", "\r"} {
		for _, bom := range []string{"", "\uFEFF"} {
			good := bom + "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}" + ending + ending + "data: [DONE]" + ending + ending
			w := httptest.NewRecorder()
			if err := forwardEvents(w, strings.NewReader(good), "openai-completions", newResponseIO(context.Background(), w)); err != nil {
				t.Fatal(err)
			}
			if w.Body.String() != good {
				t.Fatal("valid SSE framing bytes changed")
			}
			bad := bom + "data: {\"error\":{\"message\":\"diagnostic " + secret + "\"}}" + ending + ending
			w = httptest.NewRecorder()
			if err := forwardEvents(w, strings.NewReader(bad), "openai-completions", newResponseIO(context.Background(), w)); err == nil {
				t.Fatal("framed error escaped classification")
			}
			if strings.Contains(w.Body.String(), secret) || strings.Contains(w.Body.String(), "diagnostic") {
				t.Fatal("framed provider key leaked")
			}
		}
	}
}

func TestCancellationBeforeWriterCannotRestoreFutureDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	w := &blockedWriter{entered: make(chan struct{}), deadline: make(chan struct{})}
	guard := newResponseIO(ctx, w)
	cancel()
	guard.interrupt()
	if err := guard.prepareWrite(); err == nil {
		t.Fatal("cancelled writer accepted a future deadline")
	}
	if err := forwardEvents(w, strings.NewReader("data: {}\n\n"), "openai-completions", guard); err == nil {
		t.Fatal("cancelled stream entered writer")
	}
	select {
	case <-w.entered:
		t.Fatal("post-cancellation writer entered")
	default:
	}
}
