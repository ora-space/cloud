package controlgrpc

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
)

// signalService serves the Watch stream: at-most-once hints from the in-process ControlHub to the
// lease holder. The stream proves the caller's epoch once when it opens; a holder that later loses
// the lease still receives hints but fails every fenced write, so nothing depends on cutting it.
type signalService struct {
	controlpb.UnimplementedControlSignalServiceServer
	store *core.Store
}

func (s *signalService) Watch(req *controlpb.WatchRequest, stream controlpb.ControlSignalService_WatchServer) error {
	ctx := stream.Context()
	if _, e := s.store.Control(ctx, &core.ControlRequest{Action: "lease_check", Body: core.Object{"epoch": req.GetEpoch()}, Service: principal(ctx)}); e != nil {
		return toStatus(e)
	}
	signals, cancel, ok := s.store.Signals.Subscribe()
	if !ok {
		return status.Error(codes.Unavailable, "draining")
	}
	defer cancel()
	// Headers mark the moment the subscription exists: a caller that waits for them before acting
	// on its side cannot miss a signal published in between.
	if e := stream.SendHeader(nil); e != nil {
		return e
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case signal, open := <-signals:
			if !open {
				// Drain closed the hub: a clean end is the last thing the holder needs to see.
				return nil
			}
			if e := stream.Send(response(signal)); e != nil {
				return e
			}
		}
	}
}

func response(signal core.ControlSignal) *controlpb.WatchResponse {
	switch signal.Kind {
	case core.SignalDrain:
		return &controlpb.WatchResponse{Signal: &controlpb.WatchResponse_Drain{Drain: &controlpb.Drain{}}}
	case core.SignalWorkAvailable:
		out := &controlpb.WorkAvailable{}
		if signal.OperationID != "" {
			out.OperationId = &signal.OperationID
		}
		return &controlpb.WatchResponse{Signal: &controlpb.WatchResponse_WorkAvailable{WorkAvailable: out}}
	case core.SignalOperationAvailable:
		return &controlpb.WatchResponse{Signal: &controlpb.WatchResponse_OperationAvailable{OperationAvailable: &controlpb.OperationAvailable{OperationId: signal.OperationID}}}
	default:
		return &controlpb.WatchResponse{Signal: &controlpb.WatchResponse_NodeAssignment{NodeAssignment: &controlpb.NodeAssignment{NodeId: signal.NodeID}}}
	}
}
