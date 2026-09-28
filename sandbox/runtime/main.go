package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const outputLimit = 65536

type boundedBuffer struct {
	sync.Mutex
	data      bytes.Buffer
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	n := len(p)
	remaining := outputLimit - b.data.Len()
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	_, _ = b.data.Write(p)
	return n, nil
}
func (b *boundedBuffer) String() string { b.Lock(); defer b.Unlock(); return b.data.String() }

type request struct {
	Command []string `json:"command"`
	Cwd     string   `json:"cwd"`
	Timeout string   `json:"timeout"`
}
type response struct {
	Stdout     string  `json:"stdout"`
	Stderr     string  `json:"stderr"`
	ExitCode   int     `json:"exitCode"`
	Error      string  `json:"error,omitempty"`
	TimedOut   bool    `json:"timedOut"`
	Truncated  bool    `json:"truncated"`
	DurationMS float64 `json:"duration_ms"`
}

func run(ctx context.Context, r request) response {
	start := time.Now()
	out, errout := &boundedBuffer{}, &boundedBuffer{}
	timeout := 30 * time.Second
	if r.Timeout != "" {
		d, err := time.ParseDuration(r.Timeout)
		if err != nil || d <= 0 || d > 120*time.Second {
			return response{ExitCode: -1, Error: "timeout must be >0 and <=120s"}
		}
		timeout = d
	}
	if len(r.Command) == 0 {
		return response{ExitCode: -1, Error: "command is required"}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// This endpoint is the sandbox execution boundary: it intentionally accepts
	// an argv vector, never invokes a shell implicitly, and runs inside gVisor.
	cmd := exec.CommandContext(ctx, r.Command[0], r.Command[1:]...) // #nosec G204
	cmd.Dir = r.Cwd
	if cmd.Dir == "" {
		cmd.Dir = "/workspace"
	}
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/workspace", "LANG=C.UTF-8", "PYTHONDONTWRITEBYTECODE=1"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdout = out
	cmd.Stderr = errout
	err := cmd.Run()
	result := response{Stdout: out.String(), Stderr: errout.String(), Truncated: out.truncated || errout.truncated, DurationMS: float64(time.Since(start).Microseconds()) / 1000}
	if err != nil {
		result.ExitCode = -1
		result.Error = err.Error()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			result.ExitCode = ee.ExitCode()
		}
	}
	result.TimedOut = ctx.Err() == context.DeadlineExceeded
	return result
}

func process(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1024*1024)
	defer r.Body.Close()
	var input request
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&input); err != nil {
		http.Error(w, "invalid request", 400)
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		http.Error(w, "one JSON request required", 400)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(run(r.Context(), input))
}

func main() {
	if err := os.MkdirAll("/workspace", 0o755); err != nil {
		panic(err)
	}
	http.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	http.HandleFunc("POST /process", process)
	http.HandleFunc("GET /ora-node/v1", oraNodeHandler)
	server := &http.Server{Addr: ":80", ReadHeaderTimeout: 5 * time.Second}
	panic(server.ListenAndServe())
}
