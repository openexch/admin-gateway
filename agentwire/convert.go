// SPDX-License-Identifier: Apache-2.0
package agentwire

import (
	"os"
	"time"

	"github.com/match/admin-gateway/agent"
)

// Converters between the wire messages and package agent's Go types. The
// agent types own the JSON surface (admin HTTP API); these keep the wire
// round-trip lossless so RemoteAgent responses are byte-compatible with the
// LocalAgent's.

func ProcessInfoToProto(in agent.ProcessInfo) *ProcessInfo {
	return &ProcessInfo{
		Name:         in.Name,
		Display:      in.Display,
		Role:         string(in.Role),
		Port:         int32(in.Port),
		Running:      in.Running,
		Pid:          int32(in.PID),
		MemoryBytes:  in.MemoryBytes,
		CpuPercent:   in.CPUPercent,
		UptimeMs:     in.UptimeMs,
		StartedAt:    in.StartedAt,
		RestartCount: int32(in.RestartCount),
		Enabled:      in.Enabled,
		Status:       in.Status,
		LastError:    in.LastError,
	}
}

func ProcessInfoFromProto(in *ProcessInfo) agent.ProcessInfo {
	if in == nil {
		return agent.ProcessInfo{}
	}
	return agent.ProcessInfo{
		Name:         in.Name,
		Display:      in.Display,
		Role:         agent.ServiceRole(in.Role),
		Port:         int(in.Port),
		Running:      in.Running,
		PID:          int(in.Pid),
		MemoryBytes:  in.MemoryBytes,
		CPUPercent:   in.CpuPercent,
		UptimeMs:     in.UptimeMs,
		StartedAt:    in.StartedAt,
		RestartCount: int(in.RestartCount),
		Enabled:      in.Enabled,
		Status:       in.Status,
		LastError:    in.LastError,
	}
}

func ActionResultToProto(in agent.ActionResult) *ActionResult {
	return &ActionResult{Service: in.Service, Action: in.Action, Success: in.Success, Error: in.Error}
}

func ActionResultFromProto(in *ActionResult) agent.ActionResult {
	if in == nil {
		return agent.ActionResult{}
	}
	return agent.ActionResult{Service: in.Service, Action: in.Action, Success: in.Success, Error: in.Error}
}

func ActionResultsToProto(in []agent.ActionResult) *ActionResults {
	out := &ActionResults{Results: make([]*ActionResult, len(in))}
	for i, r := range in {
		out.Results[i] = ActionResultToProto(r)
	}
	return out
}

func ActionResultsFromProto(in *ActionResults) []agent.ActionResult {
	if in == nil {
		return nil
	}
	out := make([]agent.ActionResult, len(in.Results))
	for i, r := range in.Results {
		out[i] = ActionResultFromProto(r)
	}
	return out
}

func CounterDataToProto(in *agent.CounterData) *CounterData {
	if in == nil {
		return nil
	}
	return &CounterData{
		CommitPosition: in.CommitPosition,
		SnapshotCount:  in.SnapshotCount,
		NodeRole:       in.NodeRole,
	}
}

func CounterDataFromProto(in *CounterData) *agent.CounterData {
	if in == nil {
		return nil
	}
	return &agent.CounterData{
		CommitPosition: in.CommitPosition,
		SnapshotCount:  in.SnapshotCount,
		NodeRole:       in.NodeRole,
	}
}

func EventToProto(in agent.Event) *Event {
	return &Event{
		Type:     string(in.Type),
		Service:  in.Service,
		Pid:      int32(in.PID),
		Detail:   in.Detail,
		AtUnixMs: in.At.UnixMilli(),
	}
}

func EventFromProto(in *Event) agent.Event {
	if in == nil {
		return agent.Event{}
	}
	return agent.Event{
		Type:    agent.EventType(in.Type),
		Service: in.Service,
		PID:     int(in.Pid),
		Detail:  in.Detail,
		At:      time.UnixMilli(in.AtUnixMs),
	}
}

// SummaryToProto flattens the LocalAgent's Summary() map. Unknown keys are
// dropped — the map shape is pinned by the admin HTTP API and tested in the
// conformance suite.
func SummaryToProto(in map[string]interface{}) *Summary {
	out := &Summary{}
	if v, ok := in["total"].(int); ok {
		out.Total = int32(v)
	}
	if v, ok := in["running"].(int); ok {
		out.Running = int32(v)
	}
	if v, ok := in["stopped"].(int); ok {
		out.Stopped = int32(v)
	}
	if v, ok := in["failed"].(int); ok {
		out.Failed = int32(v)
	}
	if v, ok := in["totalMemoryMB"].(int64); ok {
		out.TotalMemoryMb = v
	}
	if fs, ok := in["failedServices"].(map[string]string); ok && len(fs) > 0 {
		out.FailedServices = fs
	}
	return out
}

// SummaryFromProto rebuilds the exact map keys the HTTP API serves, so a
// RemoteAgent-backed /api/admin/processes/summary is byte-compatible.
func SummaryFromProto(in *Summary) map[string]interface{} {
	if in == nil {
		return map[string]interface{}{}
	}
	out := map[string]interface{}{
		"total":         int(in.Total),
		"running":       int(in.Running),
		"stopped":       int(in.Stopped),
		"failed":        int(in.Failed),
		"totalMemoryMB": in.TotalMemoryMb,
	}
	if len(in.FailedServices) > 0 {
		out["failedServices"] = in.FailedServices
	}
	return out
}

func ArtifactSpecFromInstall(in *InstallArtifactRequest) agent.ArtifactSpec {
	if in == nil {
		return agent.ArtifactSpec{}
	}
	return agent.ArtifactSpec{
		DestPath: in.DestPath,
		Sha256:   in.Sha256,
		Mode:     os.FileMode(in.Mode),
	}
}
