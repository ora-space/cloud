// Package controlgrpc serves the Controller-facing internal control contract (internal/controlpb)
// over gRPC. It is a translation layer only: every RPC names the calling Controller from its
// metadata, converts the request into a core.ControlRequest, runs it through the same Store.Control
// transaction as the JSON internal API, and maps the resulting Fault to a gRPC status. No business
// rule, cache or retry lives here.
package controlgrpc

import (
	"context"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
)

// HolderMetadata names the Controller on every call. Controllers are not authenticated at this
// stage: the value is the ControllerId Cloud records as lease and submission holder, nothing more.
const HolderMetadata = "x-ora-controller-id"

// maxHolder bounds the self-declared identity like any other untrusted identifier.
const maxHolder = 256

// keepaliveMinTime is the shortest client PING interval the listener accepts without counting a
// strike. grpc-go's default policy (5 minutes, no PINGs without an active stream) sends
// GOAWAY(too_many_pings) after three strikes, which would cut the Controller's 30-second keepalive on
// an idle Watch together with every unary call on that connection. 5 seconds sits below any sane
// client interval (grpc-go clients cannot go under 10 seconds) with room for jitter, yet still
// refuses a PING flood. The listener never PINGs on its own: TCP keepalive eventually closes a lost
// Controller's connection, and a bounded subscriber buffer keeps it from blocking anyone meanwhile.
const keepaliveMinTime = 5 * time.Second

type claimsKey struct{}

// New builds the gRPC server that names the calling Controller on every unary and stream call and
// accepts its keepalive PINGs, including while no stream is active so a Controller that has lost its
// Watch can still detect a dead connection before its next call. The caller owns the listener and
// the stop sequence.
func New(store *core.Store) *grpc.Server {
	server := grpc.NewServer(
		grpc.ChainUnaryInterceptor(unaryHolder),
		grpc.ChainStreamInterceptor(streamHolder),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: keepaliveMinTime, PermitWithoutStream: true}),
	)
	controlpb.RegisterControllerLeaseServiceServer(server, &leaseService{store: store})
	controlpb.RegisterExecutionServiceServer(server, &executionService{store: store})
	controlpb.RegisterControlSignalServiceServer(server, &signalService{store: store})
	controlpb.RegisterWorkspaceOperationServiceServer(server, &operationService{store: store})
	controlpb.RegisterNodeReportServiceServer(server, &nodeService{store: store})
	return server
}

// holder turns the declared ControllerId into the controller principal Store.Control expects, so
// lease ownership and submission replay keep their meaning without any credential.
func holder(ctx context.Context) (context.Context, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get(HolderMetadata)
	if len(values) != 1 || strings.TrimSpace(values[0]) == "" || len(values[0]) > maxHolder {
		return nil, status.Error(codes.InvalidArgument, HolderMetadata+" metadata is required")
	}
	claims := &core.Claims{Kind: "service", Role: "controller", RegisteredClaims: jwt.RegisteredClaims{Subject: values[0]}}
	return context.WithValue(ctx, claimsKey{}, claims), nil
}

func unaryHolder(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	ctx, e := holder(ctx)
	if e != nil {
		return nil, e
	}
	return handler(ctx, req)
}

func streamHolder(srv any, stream grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	ctx, e := holder(stream.Context())
	if e != nil {
		return e
	}
	return handler(srv, &authenticatedStream{ServerStream: stream, ctx: ctx})
}

// authenticatedStream carries the named principal to stream handlers through Context().
type authenticatedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *authenticatedStream) Context() context.Context { return s.ctx }

// principal returns the claims the interceptor built; handlers are only reachable through it.
func principal(ctx context.Context) *core.Claims {
	claims, _ := ctx.Value(claimsKey{}).(*core.Claims)
	return claims
}
