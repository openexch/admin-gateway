// SPDX-License-Identifier: Apache-2.0

// Package agenthub is the control-plane side of the gateway↔agentd protocol
// (docs/AGENT-ARCHITECTURE.md Horizon B): a gRPC server agents dial into,
// plus RemoteAgent — the agent.ProcessAgent implementation that forwards
// every verb over a live session. The gateway keeps using the in-process
// LocalAgent until a topology binds hosts to remote agents (milestone 4);
// in milestone 3 the hub only serves the loopback parity tests and the
// opt-in ADMIN_AGENT_LISTEN wiring.
package agenthub

import (
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/match/admin-gateway/agentwire"
	"github.com/match/admin-gateway/logging"
)

const (
	// ProtoVersion is bumped on incompatible envelope changes; the hub
	// rejects agents with a different major.
	ProtoVersion = 1

	artifactChunkBytes = 256 * 1024
)

type Hub struct {
	agentwire.UnimplementedControlPlaneServer

	token string // static bearer token (M3); empty = no auth (loopback tests)
	log   *slog.Logger

	mu       sync.Mutex
	sessions map[string]*session
	events   map[string]*hostEvents

	transfers *transferTable
}

func NewHub(token string) *Hub {
	return &Hub{
		token:     token,
		log:       logging.Component("agenthub"),
		sessions:  make(map[string]*session),
		events:    make(map[string]*hostEvents),
		transfers: newTransferTable(),
	}
}

// ServerOptions returns the interceptor chain enforcing the shared token on
// every RPC (Session and FetchArtifact alike).
func (h *Hub) ServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.UnaryInterceptor(h.unaryAuth),
		grpc.StreamInterceptor(h.streamAuth),
	}
}

func (h *Hub) authorized(ctx context.Context) bool {
	if h.token == "" {
		return true
	}
	md, _ := metadata.FromIncomingContext(ctx)
	for _, v := range md.Get("authorization") {
		if len(v) > 7 && subtle.ConstantTimeCompare([]byte(v[7:]), []byte(h.token)) == 1 {
			return true
		}
	}
	return false
}

func (h *Hub) unaryAuth(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	if !h.authorized(ctx) {
		return nil, status.Error(codes.Unauthenticated, "bad or missing agent token")
	}
	return handler(ctx, req)
}

func (h *Hub) streamAuth(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if !h.authorized(ss.Context()) {
		return status.Error(codes.Unauthenticated, "bad or missing agent token")
	}
	return handler(srv, ss)
}

// Session implements the persistent agent connection: Hello handshake,
// registration (newest session displaces an older one for the same host —
// agent restarts must win), then the receive loop dispatching results,
// events and snapshots until the stream ends.
func (h *Hub) Session(stream agentwire.ControlPlane_SessionServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return status.Error(codes.InvalidArgument, "first message must be Hello")
	}
	if hello.ProtoVersion != ProtoVersion {
		stream.Send(&agentwire.ControlMessage{Msg: &agentwire.ControlMessage_HelloAck{
			HelloAck: &agentwire.HelloAck{Accepted: false,
				Reason: fmt.Sprintf("proto version %d not supported (want %d)", hello.ProtoVersion, ProtoVersion)},
		}})
		return status.Error(codes.FailedPrecondition, "proto version mismatch")
	}
	if hello.HostId == "" {
		return status.Error(codes.InvalidArgument, "empty host_id")
	}

	s := newSession(hello.HostId, stream)

	h.mu.Lock()
	if old, ok := h.sessions[hello.HostId]; ok {
		// Newest wins: a restarted agent must displace its stale session.
		old.close()
		h.log.Warn("displacing stale agent session", "host", hello.HostId)
	}
	h.sessions[hello.HostId] = s
	if _, ok := h.events[hello.HostId]; !ok {
		h.events[hello.HostId] = &hostEvents{}
	}
	events := h.events[hello.HostId]
	h.mu.Unlock()

	defer func() {
		s.close()
		h.mu.Lock()
		if h.sessions[hello.HostId] == s {
			delete(h.sessions, hello.HostId)
		}
		h.mu.Unlock()
		h.log.Info("agent session ended", "host", hello.HostId)
	}()

	if err := s.send(&agentwire.ControlMessage{Msg: &agentwire.ControlMessage_HelloAck{
		HelloAck: &agentwire.HelloAck{Accepted: true},
	}}); err != nil {
		return err
	}
	h.log.Info("agent session established", "host", hello.HostId, "agent_version", hello.AgentVersion)

	for {
		msg, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		switch m := msg.Msg.(type) {
		case *agentwire.AgentMessage_Result:
			s.deliver(m.Result)
		case *agentwire.AgentMessage_Event:
			events.emit(agentwire.EventFromProto(m.Event))
		case *agentwire.AgentMessage_Snapshot:
			// M3: snapshots inform logs only; desired-state reconcile is
			// milestone 4 (reserved ControlMessage field 3).
			h.log.Debug("agent snapshot", "host", hello.HostId, "processes", len(m.Snapshot.Processes))
		case *agentwire.AgentMessage_Hello:
			return status.Error(codes.InvalidArgument, "duplicate Hello")
		}
	}
}

// FetchArtifact streams a registered transfer's bytes to the agent.
func (h *Hub) FetchArtifact(req *agentwire.FetchArtifactRequest, stream agentwire.ControlPlane_FetchArtifactServer) error {
	r, err := h.transfers.take(req.TransferId)
	if err != nil {
		return status.Error(codes.NotFound, err.Error())
	}
	buf := make([]byte, artifactChunkBytes)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if sendErr := stream.Send(&agentwire.ArtifactChunk{Data: buf[:n]}); sendErr != nil {
				return sendErr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return status.Error(codes.Internal, err.Error())
		}
	}
}

// Enroll is declared in the proto to freeze the wire surface; the join-token
// certificate flow is milestone 4.
func (h *Hub) Enroll(ctx context.Context, req *agentwire.EnrollRequest) (*agentwire.EnrollResponse, error) {
	return nil, status.Error(codes.Unimplemented, "enroll ships with the topology milestone")
}

// live returns the current session for a host.
func (h *Hub) live(hostID string) (*session, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[hostID]
	if !ok {
		return nil, fmt.Errorf("agent %q not connected", hostID)
	}
	return s, nil
}

// hostEventsFor returns the (host-lifetime) subscriber registry.
func (h *Hub) hostEventsFor(hostID string) *hostEvents {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.events[hostID]; !ok {
		h.events[hostID] = &hostEvents{}
	}
	return h.events[hostID]
}

// Connected reports whether a host has a live session (for status surfaces).
func (h *Hub) Connected(hostID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.sessions[hostID]
	return ok
}
