package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// stopWithin bounds how long the test lets a stopping process take before it calls the shutdown a
// failure. It is deliberately shorter than `shutdownGrace`: a process that ends its jobs and then waits
// for them returns in the time it takes to remove a directory, and a process that only waits for them
// runs to the full grace period — which is the behavior this case exists to tell apart.
const stopWithin = 5 * time.Second

// TestAStoppingProcessEndsItsInFlightJobsAndWaitsForThem is the executor half of the material's
// lifetime, and it is about the one outcome no deferred destruction can cover by itself.
//
// Every job destroys the material it staged in a `defer`, so success, a Git failure, a timeout, a
// cancellation and a panic all reach the same cleanup. What none of those covers is the process being
// asked to stop while a job is running: `http.Server.Shutdown` stops accepting work and then waits for
// the connections it already has, and a job is not a connection. A shutdown that only waits leaves the
// handler running past the grace period, returns with it still running, and takes with it the only
// process that would have removed the token it staged — the next start's sweep is what repairs that,
// and only because a start can.
//
// The job here stands in for a real one in the two ways that matter to this boundary: it runs under the
// process's context, and it does deferred work after it learns to stop. The case asserts that stopping
// the process cancels it, that the process does not return before that work is done, and that it
// returns without the shutdown deadline having expired — the answer a process that only waited for it
// could not give.
func TestAStoppingProcessEndsItsInFlightJobsAndWaitsForThem(t *testing.T) {
	started := make(chan struct{})
	finished := make(chan struct{})
	jobContext, endJobs := context.WithCancel(context.Background())
	defer endJobs()
	server := &http.Server{
		Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			close(started)
			<-r.Context().Done()
			// A real job removes its material here. The channel is what makes "the process waited"
			// observable rather than inferred from a duration.
			close(finished)
		}),
		BaseContext: func(net.Listener) context.Context { return jobContext },
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
	})

	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- serveUntilStopped(server, listener, stop, endJobs) }()
	// The job is dispatched and left in flight. Its answer is not what is being read — a job that is
	// cancelled answers nothing — so the request's own outcome is deliberately not asserted on.
	go func() {
		request, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
			"http://"+listener.Addr().String()+"/v1/clone-executions", http.NoBody)
		if err != nil {
			return
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}()
	select {
	case <-started:
	case <-time.After(stopWithin):
		t.Fatal("the job was never dispatched")
	}

	close(stop)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the stopping process reported %v, want a clean stop", err)
		}
	case <-time.After(stopWithin):
		t.Fatalf("the process did not stop within %s: the job in flight was never ended", stopWithin)
	}
	select {
	case <-finished:
	default:
		t.Fatal("the process returned before the job it had in flight finished")
	}
	if err := jobContext.Err(); err == nil {
		t.Fatal("the job's context was never cancelled")
	}
}
