package logger

import "go.uber.org/zap"

// Values adapts a zap logger to the key/value logging shape some packages consume instead of zap's
// typed fields — `mirrorpublish.Logger` is the one in this repository.
//
// It exists so that one process emits one log stream. The Git Server is composed of packages written
// against two logging call shapes, and a command that logged half its lines to the deployment's
// configured destination and half to a second logger it opened itself would be one an operator has to
// read in two places and correlate by hand. zap's SugaredLogger already takes exactly the
// `msg, keysAndValues...` call shape, so what is adapted here is the field typing and not the format:
// the pairs reach the same encoder, level filter and destination as every other line.
type Values struct{ log *zap.SugaredLogger }

// NewValues wraps log. A nil logger discards, as every nil logger in this package does.
func NewValues(log *zap.Logger) Values {
	if log == nil {
		log = zap.NewNop()
	}
	// One frame is skipped so the recorded caller is the code that logged rather than this adapter;
	// the SugaredLogger already accounts for its own frame.
	return Values{log: log.WithOptions(zap.AddCallerSkip(1)).Sugar()}
}

// Info records one message with its key/value pairs.
func (v Values) Info(msg string, args ...any) { v.log.Infow(msg, args...) }

// Warn records one message with its key/value pairs.
func (v Values) Warn(msg string, args ...any) { v.log.Warnw(msg, args...) }
