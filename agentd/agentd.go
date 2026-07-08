// SPDX-License-Identifier: Apache-2.0

// Package agentd is the per-host agent side of the gateway↔agentd protocol
// (docs/AGENT-ARCHITECTURE.md Horizon B): it dials the control plane, holds
// one persistent Session stream, executes commands against an embedded
// agent.ProcessAgent, and forwards lifecycle events. All local reactions
// (gating, cascades, disarm, adoption) stay inside the embedded
// ProcessManager — losing the control plane never affects them.
package agentd

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"runtime/debug"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"

	"github.com/match/admin-gateway/agent"
	"github.com/match/admin-gateway/agentwire"
	"github.com/match/admin-gateway/logging"
)

// eventRing buffers events while disconnected and replays them after the
// next handshake (bounded; oldest dropped — the contract is best-effort).
const eventRingSize = 256

type Config struct {
	Control      string // control-plane address (host:port)
	HostID       string
	Token        string      // static bearer token (M3); empty = none
	TLS          *tls.Config // nil + Insecure=true → plaintext (loopback only)
	Insecure     bool
	AgentVersion string

	// Reconnect backoff bounds (defaults 1s → 30s).
	BackoffMin time.Duration
	BackoffMax time.Duration

	// ExtraDialOptions are appended to the gRPC dial options (tests inject
	// bufconn dialers here).
	ExtraDialOptions []grpc.DialOption
}

// Run dials the control plane and serves sessions until ctx ends. Each lost
// connection is retried with exponential backoff; commands already in flight
// die with the stream (the control plane sees the disconnect).
func Run(ctx context.Context, cfg Config, pa agent.ProcessAgent) error {
	log := logging.Component("agentd")
	if cfg.Control == "" || cfg.HostID == "" {
		return fmt.Errorf("agentd needs a control address and a host id")
	}
	if cfg.TLS == nil && !cfg.Insecure {
		return fmt.Errorf("refusing to dial without TLS; set Insecure only for loopback")
	}
	if cfg.BackoffMin <= 0 {
		cfg.BackoffMin = time.Second
	}
	if cfg.BackoffMax <= 0 {
		cfg.BackoffMax = 30 * time.Second
	}

	creds := insecure.NewCredentials()
	if cfg.TLS != nil {
		creds = credentials.NewTLS(cfg.TLS)
	}
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time: 30 * time.Second, Timeout: 10 * time.Second, PermitWithoutStream: true,
		}),
	}
	opts = append(opts, cfg.ExtraDialOptions...)
	conn, err := grpc.NewClient(cfg.Control, opts...)
	if err != nil {
		return fmt.Errorf("dial %s: %w", cfg.Control, err)
	}
	defer conn.Close()
	client := agentwire.NewControlPlaneClient(conn)

	// One subscription for the whole run; the ring carries events across
	// reconnects.
	events, unsub := pa.Subscribe(eventRingSize)
	defer unsub()
	ring := make(chan *agentwire.Event, eventRingSize)
	go func() {
		for ev := range events {
			pev := agentwire.EventToProto(ev)
			for {
				select {
				case ring <- pev:
				default:
					select { // full: drop oldest
					case <-ring:
						continue
					default:
					}
				}
				break
			}
		}
	}()

	backoff := cfg.BackoffMin
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := runSession(ctx, cfg, client, pa, ring, log)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Warn("session ended, reconnecting", "err", err, "backoff", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		backoff *= 2
		if backoff > cfg.BackoffMax {
			backoff = cfg.BackoffMax
		}
	}
}

// authCtx attaches the bearer token to outgoing RPCs.
func authCtx(ctx context.Context, cfg Config) context.Context {
	if cfg.Token == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+cfg.Token)
}

func runSession(ctx context.Context, cfg Config, client agentwire.ControlPlaneClient,
	pa agent.ProcessAgent, ring chan *agentwire.Event, log *slog.Logger) error {

	sctx, cancel := context.WithCancel(authCtx(ctx, cfg))
	defer cancel()

	stream, err := client.Session(sctx)
	if err != nil {
		return err
	}

	// Single writer: everything leaves through the outbox. Command results
	// must never drop; events ride the same channel (bounded by the ring).
	outbox := make(chan *agentwire.AgentMessage, eventRingSize)
	sendErr := make(chan error, 1)
	go func() {
		for {
			select {
			case msg := <-outbox:
				if err := stream.Send(msg); err != nil {
					select {
					case sendErr <- err:
					default:
					}
					cancel() // tear the session down
					return
				}
			case <-sctx.Done():
				return
			}
		}
	}()

	// Handshake, inline on the stream (not via the outbox: nothing else may
	// precede Hello).
	if err := stream.Send(&agentwire.AgentMessage{Msg: &agentwire.AgentMessage_Hello{Hello: &agentwire.Hello{
		HostId: cfg.HostID, AgentVersion: cfg.AgentVersion, ProtoVersion: protoVersion,
	}}}); err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	ack := first.GetHelloAck()
	if ack == nil {
		return fmt.Errorf("expected HelloAck, got %T", first.Msg)
	}
	if !ack.Accepted {
		return fmt.Errorf("control plane rejected session: %s", ack.Reason)
	}
	log.Info("session established", "control", cfg.Control, "host", cfg.HostID)

	// Full state snapshot, then replay buffered events.
	snapshot := &agentwire.StateSnapshot{}
	for _, in := range pa.List() {
		snapshot.Processes = append(snapshot.Processes, agentwire.ProcessInfoToProto(in))
	}
	outbox <- &agentwire.AgentMessage{Msg: &agentwire.AgentMessage_Snapshot{Snapshot: snapshot}}
	for {
		select {
		case ev := <-ring:
			outbox <- &agentwire.AgentMessage{Msg: &agentwire.AgentMessage_Event{Event: ev}}
			continue
		default:
		}
		break
	}

	// Live event forwarding for the rest of the session.
	go func() {
		for {
			select {
			case ev := <-ring:
				select {
				case outbox <- &agentwire.AgentMessage{Msg: &agentwire.AgentMessage_Event{Event: ev}}:
				case <-sctx.Done():
					return
				}
			case <-sctx.Done():
				return
			}
		}
	}()

	// Receive loop: each command dispatches on its own goroutine — a Start
	// can legally block 25s in the driver gate and must not stall the stream.
	for {
		msg, err := stream.Recv()
		if err != nil {
			select {
			case werr := <-sendErr:
				return werr
			default:
			}
			if err == io.EOF {
				return fmt.Errorf("control plane closed the session")
			}
			return err
		}
		cmd := msg.GetCommand()
		if cmd == nil {
			continue // HelloAck duplicates / future message kinds
		}
		go func(cmd *agentwire.Command) {
			var res *agentwire.CommandResult
			func() {
				// A panic in any single command handler must not tear down the
				// whole agent — recover and report it as a failed result so the
				// session (and every other command) survives.
				defer func() {
					if r := recover(); r != nil {
						slog.Error("agentd: command handler panicked",
							"cmdId", cmd.Id, "panic", r, "stack", string(debug.Stack()))
						res = &agentwire.CommandResult{Error: fmt.Sprintf("command handler panicked: %v", r)}
					}
				}()
				res = execute(sctx, cfg, client, pa, cmd)
			}()
			res.Id = cmd.Id
			select {
			case outbox <- &agentwire.AgentMessage{Msg: &agentwire.AgentMessage_Result{Result: res}}:
			case <-sctx.Done():
			}
		}(cmd)
	}
}

const protoVersion = 1

// execute runs one command against the embedded agent.
func execute(ctx context.Context, cfg Config, client agentwire.ControlPlaneClient,
	pa agent.ProcessAgent, cmd *agentwire.Command) *agentwire.CommandResult {

	res := &agentwire.CommandResult{}
	fail := func(err error) *agentwire.CommandResult {
		if err != nil {
			res.Error = err.Error()
		}
		return res
	}

	switch v := cmd.Verb.(type) {
	case *agentwire.Command_List:
		pl := &agentwire.ProcessList{}
		for _, in := range pa.List() {
			pl.Processes = append(pl.Processes, agentwire.ProcessInfoToProto(in))
		}
		res.Payload = &agentwire.CommandResult_List{List: pl}

	case *agentwire.Command_Get:
		g := &agentwire.GetResponse{}
		if in := pa.Get(v.Get.Name); in != nil {
			g.Found = true
			g.Info = agentwire.ProcessInfoToProto(*in)
		}
		res.Payload = &agentwire.CommandResult_Get{Get: g}

	case *agentwire.Command_Summary:
		res.Payload = &agentwire.CommandResult_Summary{Summary: agentwire.SummaryToProto(pa.Summary())}

	case *agentwire.Command_Start:
		if v.Start.Unchecked {
			return fail(pa.StartUnchecked(v.Start.Name))
		}
		return fail(pa.Start(v.Start.Name))

	case *agentwire.Command_Stop:
		if v.Stop.Force {
			return fail(pa.ForceStop(v.Stop.Name))
		}
		return fail(pa.Stop(v.Stop.Name))

	case *agentwire.Command_Restart:
		return fail(pa.Restart(v.Restart.Name))

	case *agentwire.Command_Bulk:
		var results []agent.ActionResult
		switch v.Bulk.Op {
		case agentwire.BulkRequest_START_ALL:
			results = pa.StartAll()
		case agentwire.BulkRequest_STOP_ALL:
			results = pa.StopAll()
		case agentwire.BulkRequest_RESTART_ALL:
			results = pa.RestartAll()
		default:
			res.Error = fmt.Sprintf("unknown bulk op %v", v.Bulk.Op)
			return res
		}
		res.Payload = &agentwire.CommandResult_Results{Results: agentwire.ActionResultsToProto(results)}

	case *agentwire.Command_TailLog:
		lines, err := pa.TailLog(v.TailLog.Service, int(v.TailLog.Lines))
		if err != nil {
			return fail(err)
		}
		res.Payload = &agentwire.CommandResult_Log{Log: &agentwire.LogLines{Lines: lines}}

	case *agentwire.Command_NodeCounters:
		cd, err := pa.NodeCounters(int(v.NodeCounters.NodeId))
		if err != nil {
			return fail(err)
		}
		res.Payload = &agentwire.CommandResult_Counters{Counters: agentwire.CounterDataToProto(cd)}

	case *agentwire.Command_Install:
		return fail(installFromControlPlane(ctx, cfg, client, pa, v.Install))

	default:
		res.Unsupported = true
	}
	return res
}

// installFromControlPlane pulls the artifact bytes back over FetchArtifact
// (separate HTTP/2 stream) and feeds them into the embedded agent's local
// temp-file → sha-verify → atomic-rename install.
func installFromControlPlane(ctx context.Context, cfg Config, client agentwire.ControlPlaneClient,
	pa agent.ProcessAgent, req *agentwire.InstallArtifactRequest) error {

	fctx, cancel := context.WithCancel(authCtx(ctx, cfg))
	defer cancel()
	stream, err := client.FetchArtifact(fctx, &agentwire.FetchArtifactRequest{TransferId: req.TransferId})
	if err != nil {
		return fmt.Errorf("fetch artifact: %w", err)
	}
	return pa.InstallArtifact(agentwire.ArtifactSpecFromInstall(req), &chunkReader{stream: stream})
}

// chunkReader adapts the FetchArtifact stream to io.Reader.
type chunkReader struct {
	stream agentwire.ControlPlane_FetchArtifactClient
	buf    []byte
}

func (c *chunkReader) Read(p []byte) (int, error) {
	for len(c.buf) == 0 {
		chunk, err := c.stream.Recv()
		if err == io.EOF {
			return 0, io.EOF
		}
		if err != nil {
			return 0, err
		}
		c.buf = chunk.Data
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}
