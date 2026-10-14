package modelgateway

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// responseIO serializes deadline updates with cancellation. The mutex protects transitions,
// never the actual write, so cancellation can interrupt a blocked downstream.
type responseIO struct {
	mu          sync.Mutex
	ctx         context.Context
	controller  *http.ResponseController
	interrupted bool
}

// guardedWriter makes safe-fault JSON use the same cancellation gate as protocol writes.
// It never holds a lock while writing, so the watcher can force an in-progress write to fail.
type guardedWriter struct {
	http.ResponseWriter
	io *responseIO
}

func (w *guardedWriter) Write(data []byte) (int, error) {
	if err := w.io.prepareWrite(); err != nil {
		return 0, err
	}
	return w.ResponseWriter.Write(data)
}

func (w *guardedWriter) SetWriteDeadline(deadline time.Time) error {
	return w.io.setWriteDeadline(deadline)
}

func (w *guardedWriter) SetReadDeadline(deadline time.Time) error {
	return w.io.setReadDeadline(deadline)
}

func (w *guardedWriter) FlushError() error {
	if err := w.io.prepareWrite(); err != nil {
		return err
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func (io *responseIO) setWriteDeadline(deadline time.Time) error {
	io.mu.Lock()
	defer io.mu.Unlock()
	if err := io.ctx.Err(); err != nil {
		return err
	}
	if io.interrupted {
		return context.Canceled
	}
	return io.controller.SetWriteDeadline(deadline)
}

func (io *responseIO) setReadDeadline(deadline time.Time) error {
	io.mu.Lock()
	defer io.mu.Unlock()
	if err := io.ctx.Err(); err != nil {
		return err
	}
	if io.interrupted {
		return context.Canceled
	}
	return io.controller.SetReadDeadline(deadline)
}

func newResponseIO(ctx context.Context, w http.ResponseWriter) *responseIO {
	return &responseIO{ctx: ctx, controller: http.NewResponseController(w)}
}

func (io *responseIO) readDeadline() error {
	io.mu.Lock()
	defer io.mu.Unlock()
	if err := io.ctx.Err(); err != nil {
		return err
	}
	_ = io.controller.SetReadDeadline(time.Now().Add(30 * time.Second))
	return nil
}

func (io *responseIO) clearRead() {
	io.mu.Lock()
	defer io.mu.Unlock()
	if !io.interrupted && io.ctx.Err() == nil {
		_ = io.controller.SetReadDeadline(time.Time{})
	}
}

func (io *responseIO) prepareWrite() error {
	io.mu.Lock()
	defer io.mu.Unlock()
	if err := io.ctx.Err(); err != nil {
		return err
	}
	if io.interrupted {
		return context.Canceled
	}
	_ = io.controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return nil
}

func (io *responseIO) interrupt() {
	io.mu.Lock()
	defer io.mu.Unlock()
	io.interrupted = true
	_ = io.controller.SetReadDeadline(time.Now())
	_ = io.controller.SetWriteDeadline(time.Now())
}

func (io *responseIO) complete() {
	io.mu.Lock()
	defer io.mu.Unlock()
	if !io.interrupted {
		_ = io.controller.SetReadDeadline(time.Time{})
		// net/http clears this after finishRequest flushes buffered JSON and footer bytes.
		_ = io.controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
	}
}
