package modelgateway

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/wanglongan587/cloud/internal/core"
)

const attemptPersistenceTimeout = 3 * time.Second

var errAuthorizationWatch = errors.New("model authorization observation failed")

// modelAttempt belongs to one HTTP handler, so observation needs no concurrent mutable state.
// The grant identifies the trusted run; neither client correlation headers nor provider data can
// supply ownership. An attempt is a gateway dispatch, not an inferred logical request or retry.
type modelAttempt struct {
	id       string
	started  time.Time
	observer usageObserver
	result   core.ModelAttemptFinish
	now      func() time.Time
}

func (s *Service) beginAttempt(ctx context.Context, grant *core.ModelGrant, streamed bool) (*modelAttempt, error) {
	id := uuid.NewString()
	persist, cancel := context.WithTimeout(ctx, attemptPersistenceTimeout)
	defer cancel()
	if err := s.options.Store.BeginModelAttempt(persist, grant, id, streamed); err != nil {
		return nil, err
	}
	return &modelAttempt{
		id: id, started: s.options.Now(), now: s.options.Now,
		observer: usageObserver{protocol: grant.Protocol},
		result:   core.ModelAttemptFinish{Outcome: "incomplete", ErrorCode: "model_attempt_incomplete"},
	}, nil
}

func (a *modelAttempt) elapsed() int64 { return max(0, a.now().Sub(a.started).Milliseconds()) }

func (a *modelAttempt) output(observed bool) {
	if observed && a.result.FirstOutputMS == nil {
		elapsed := a.elapsed()
		a.result.FirstOutputMS = &elapsed
	}
}

func (a *modelAttempt) observeEvent(event []byte) { a.output(a.observer.observeEvent(event)) }

func (a *modelAttempt) end(outcome, code string) {
	a.result.Outcome, a.result.ErrorCode = outcome, code
}

func (a *modelAttempt) usage() core.ModelAttemptUsage {
	u := a.observer.usage
	availability := "unavailable"
	if u.input != nil || u.output != nil || u.total != nil || u.cacheRead != nil || u.cacheWrite != nil || u.reasoning != nil {
		availability = "partial"
		if a.observer.terminal && a.observer.finalUsage && !u.invalid && u.input != nil && u.output != nil {
			availability = "complete"
		}
	}
	semantics := "includes_cache"
	if a.observer.protocol == "anthropic-messages" {
		semantics = "excludes_cache"
	}
	return core.ModelAttemptUsage{
		InputTokens: u.input, OutputTokens: u.output, TotalTokens: u.total,
		CacheReadTokens: u.cacheRead, CacheWriteTokens: u.cacheWrite, ReasoningTokens: u.reasoning,
		InputSemantics: semantics, Availability: availability,
	}
}

// finish uses a fresh, bounded context after request cancellation. Retries reuse the exact same
// immutable result and identity: an ambiguous commit can never add an extra attempt or sum usage.
// If all writes fail, the durable started row deliberately remains unresolved for operators.
func (s *Service) finishAttempt(ctx context.Context, a *modelAttempt) {
	a.result.TerminalObserved = a.observer.terminal
	a.result.Usage = a.usage()
	cause := context.Cause(ctx)
	var fault *core.Fault
	switch {
	case errors.Is(cause, context.Canceled):
		a.end("cancelled", "model_request_cancelled")
	case errors.Is(cause, context.DeadlineExceeded):
		a.end("cancelled", "model_request_timeout")
	case errors.As(cause, &fault):
		if fault.Status == 401 || fault.Status == 403 {
			a.end("cancelled", "model_authorization_revoked")
		} else {
			a.end("interrupted", "model_authorization_unavailable")
		}
	case errors.Is(cause, errAuthorizationWatch):
		a.end("interrupted", "model_authorization_unavailable")
	}
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), attemptPersistenceTimeout)
	defer cancel()
	for range 3 {
		if err := s.options.Store.FinishModelAttempt(persist, a.id, &a.result); err == nil {
			return
		}
		if persist.Err() != nil {
			break
		}
	}
	s.options.Logger.Warn("model attempt finalization unavailable", zap.String("attempt_id", a.id))
}
