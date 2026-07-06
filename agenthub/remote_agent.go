// SPDX-License-Identifier: Apache-2.0
package agenthub

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/match/admin-gateway/agent"
	"github.com/match/admin-gateway/agentwire"
)

// Per-verb deadlines: generous enough for the agent-side realities (a gated
// node start legally blocks 25s in waitForGate; StartAll staggers), tight
// enough that a wedged agent surfaces as an error instead of a hang.
var (
	queryDeadline     = 5 * time.Second
	tailLogDeadline   = 15 * time.Second
	lifecycleDeadline = 2 * time.Minute
	bulkDeadline      = 10 * time.Minute
	installDeadline   = 10 * time.Minute
)

// RemoteAgent implements agent.ProcessAgent over a hub session. It holds
// (hub, hostID) rather than a session, so it survives agent reconnects; a
// disconnected host fails fast with a clear error, never hangs.
type RemoteAgent struct {
	hub    *Hub
	hostID string
}

var _ agent.ProcessAgent = (*RemoteAgent)(nil)

// Agent returns the ProcessAgent view of one host. Cheap; safe to call per
// request.
func (h *Hub) Agent(hostID string) *RemoteAgent {
	return &RemoteAgent{hub: h, hostID: hostID}
}

func (r *RemoteAgent) roundTrip(deadline time.Duration, cmd *agentwire.Command) (*agentwire.CommandResult, error) {
	s, err := r.hub.live(r.hostID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	return s.roundTrip(ctx, cmd)
}

func (r *RemoteAgent) List() []agent.ProcessInfo {
	res, err := r.roundTrip(queryDeadline, &agentwire.Command{Verb: &agentwire.Command_List{List: &agentwire.ListRequest{}}})
	if err != nil {
		return nil
	}
	pl := res.GetList()
	if pl == nil {
		return nil
	}
	out := make([]agent.ProcessInfo, len(pl.Processes))
	for i, p := range pl.Processes {
		out[i] = agentwire.ProcessInfoFromProto(p)
	}
	return out
}

func (r *RemoteAgent) Get(name string) *agent.ProcessInfo {
	res, err := r.roundTrip(queryDeadline, &agentwire.Command{Verb: &agentwire.Command_Get{Get: &agentwire.GetRequest{Name: name}}})
	if err != nil {
		return nil
	}
	g := res.GetGet()
	if g == nil || !g.Found {
		return nil
	}
	info := agentwire.ProcessInfoFromProto(g.Info)
	return &info
}

func (r *RemoteAgent) Summary() map[string]interface{} {
	res, err := r.roundTrip(queryDeadline, &agentwire.Command{Verb: &agentwire.Command_Summary{Summary: &agentwire.SummaryRequest{}}})
	if err != nil {
		return map[string]interface{}{"error": err.Error()}
	}
	return agentwire.SummaryFromProto(res.GetSummary())
}

func (r *RemoteAgent) Start(name string) error {
	_, err := r.roundTrip(lifecycleDeadline, &agentwire.Command{Verb: &agentwire.Command_Start{Start: &agentwire.StartRequest{Name: name}}})
	return err
}

func (r *RemoteAgent) StartUnchecked(name string) error {
	_, err := r.roundTrip(lifecycleDeadline, &agentwire.Command{Verb: &agentwire.Command_Start{Start: &agentwire.StartRequest{Name: name, Unchecked: true}}})
	return err
}

func (r *RemoteAgent) Stop(name string) error {
	_, err := r.roundTrip(lifecycleDeadline, &agentwire.Command{Verb: &agentwire.Command_Stop{Stop: &agentwire.StopRequest{Name: name}}})
	return err
}

func (r *RemoteAgent) ForceStop(name string) error {
	_, err := r.roundTrip(lifecycleDeadline, &agentwire.Command{Verb: &agentwire.Command_Stop{Stop: &agentwire.StopRequest{Name: name, Force: true}}})
	return err
}

func (r *RemoteAgent) Restart(name string) error {
	_, err := r.roundTrip(lifecycleDeadline, &agentwire.Command{Verb: &agentwire.Command_Restart{Restart: &agentwire.RestartRequest{Name: name}}})
	return err
}

func (r *RemoteAgent) bulk(op agentwire.BulkRequest_Op) []agent.ActionResult {
	res, err := r.roundTrip(bulkDeadline, &agentwire.Command{Verb: &agentwire.Command_Bulk{Bulk: &agentwire.BulkRequest{Op: op}}})
	if err != nil {
		return []agent.ActionResult{{Service: r.hostID, Action: "bulk", Success: false, Error: err.Error()}}
	}
	return agentwire.ActionResultsFromProto(res.GetResults())
}

func (r *RemoteAgent) StartAll() []agent.ActionResult { return r.bulk(agentwire.BulkRequest_START_ALL) }
func (r *RemoteAgent) StopAll() []agent.ActionResult  { return r.bulk(agentwire.BulkRequest_STOP_ALL) }
func (r *RemoteAgent) RestartAll() []agent.ActionResult {
	return r.bulk(agentwire.BulkRequest_RESTART_ALL)
}

func (r *RemoteAgent) TailLog(service string, lines int) ([]string, error) {
	res, err := r.roundTrip(tailLogDeadline, &agentwire.Command{Verb: &agentwire.Command_TailLog{
		TailLog: &agentwire.TailLogRequest{Service: service, Lines: int32(lines)}}})
	if err != nil {
		return nil, err
	}
	if l := res.GetLog(); l != nil {
		return l.Lines, nil
	}
	return nil, nil
}

func (r *RemoteAgent) NodeCounters(nodeID int) (*agent.CounterData, error) {
	res, err := r.roundTrip(queryDeadline, &agentwire.Command{Verb: &agentwire.Command_NodeCounters{
		NodeCounters: &agentwire.NodeCountersRequest{NodeId: int32(nodeID)}}})
	if err != nil {
		return nil, err
	}
	cd := agentwire.CounterDataFromProto(res.GetCounters())
	if cd == nil {
		return nil, fmt.Errorf("agent %q returned no counters", r.hostID)
	}
	return cd, nil
}

// InstallArtifact registers the content under a single-use transfer id and
// commands the agent, which pulls the bytes back via FetchArtifact and runs
// its local temp-file → sha-verify → atomic-rename install. The command
// result arrives after the agent finished (or failed) the whole install.
func (r *RemoteAgent) InstallArtifact(spec agent.ArtifactSpec, content io.Reader) error {
	id := r.hub.transfers.register(content)
	_, err := r.roundTrip(installDeadline, &agentwire.Command{Verb: &agentwire.Command_Install{Install: &agentwire.InstallArtifactRequest{
		TransferId: id,
		DestPath:   spec.DestPath,
		Sha256:     spec.Sha256,
		Mode:       uint32(spec.Mode),
	}}})
	if err != nil {
		r.hub.transfers.drop(id) // never fetched (or failed early)
		return err
	}
	return nil
}

// Subscribe attaches to the host's event fan-out, which lives on the hub and
// survives agent reconnects (best-effort delivery, same contract as the
// LocalAgent).
func (r *RemoteAgent) Subscribe(buf int) (<-chan agent.Event, func()) {
	return r.hub.hostEventsFor(r.hostID).subscribe(buf)
}

// Close detaches locally; a control-plane close must never touch the
// agent's managed processes.
func (r *RemoteAgent) Close() {}
