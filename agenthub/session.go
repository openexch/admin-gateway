// SPDX-License-Identifier: Apache-2.0
package agenthub

import (
	"context"
	"fmt"
	"sync"

	"github.com/match/admin-gateway/agent"
	"github.com/match/admin-gateway/agentwire"
)

// session is one live agent connection: it owns command correlation and the
// per-host event fan-out. The recv loop lives in Hub.Session; senders go
// through send() so exactly one goroutine writes the stream at a time.
type session struct {
	hostID string
	stream agentwire.ControlPlane_SessionServer

	sendMu sync.Mutex

	mu      sync.Mutex
	nextID  uint64
	pending map[uint64]chan *agentwire.CommandResult
	done    chan struct{} // closed when the session ends
}

func newSession(hostID string, stream agentwire.ControlPlane_SessionServer) *session {
	return &session{
		hostID:  hostID,
		stream:  stream,
		pending: make(map[uint64]chan *agentwire.CommandResult),
		done:    make(chan struct{}),
	}
}

func (s *session) send(msg *agentwire.ControlMessage) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.stream.Send(msg)
}

// roundTrip sends one command and waits for its correlated result, the
// session ending, or the context deadline.
func (s *session) roundTrip(ctx context.Context, cmd *agentwire.Command) (*agentwire.CommandResult, error) {
	ch := make(chan *agentwire.CommandResult, 1)
	s.mu.Lock()
	s.nextID++
	cmd.Id = s.nextID
	s.pending[cmd.Id] = ch
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.pending, cmd.Id)
		s.mu.Unlock()
	}()

	if err := s.send(&agentwire.ControlMessage{Msg: &agentwire.ControlMessage_Command{Command: cmd}}); err != nil {
		return nil, fmt.Errorf("send to agent %q: %w", s.hostID, err)
	}
	select {
	case res := <-ch:
		if res.Unsupported {
			return nil, fmt.Errorf("agent %q does not support this command (older agent?)", s.hostID)
		}
		if res.Error != "" {
			return nil, fmt.Errorf("%s", res.Error)
		}
		return res, nil
	case <-s.done:
		return nil, fmt.Errorf("agent %q disconnected mid-command", s.hostID)
	case <-ctx.Done():
		return nil, fmt.Errorf("command to agent %q: %w", s.hostID, ctx.Err())
	}
}

// deliver routes a result to its waiter (drops results for abandoned ids).
func (s *session) deliver(res *agentwire.CommandResult) {
	s.mu.Lock()
	ch, ok := s.pending[res.Id]
	s.mu.Unlock()
	if ok {
		select {
		case ch <- res:
		default:
		}
	}
}

func (s *session) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
	default:
		close(s.done)
	}
}

// hostEvents is the per-host subscriber registry. It lives on the Hub (not
// the session) so Subscribe survives agent reconnects: subscribers keep
// their channel while sessions come and go.
type hostEvents struct {
	mu   sync.Mutex
	subs map[int]chan agent.Event
	next int
}

func (h *hostEvents) subscribe(buf int) (<-chan agent.Event, func()) {
	if buf <= 0 {
		buf = 64
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs == nil {
		h.subs = map[int]chan agent.Event{}
	}
	id := h.next
	h.next++
	ch := make(chan agent.Event, buf)
	h.subs[id] = ch
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := h.subs[id]; ok {
			delete(h.subs, id)
			close(ch)
		}
	}
}

// emit fans out without blocking (best-effort, same contract as the
// LocalAgent's eventHub).
func (h *hostEvents) emit(ev agent.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}
