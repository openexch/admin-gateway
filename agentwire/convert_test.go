// SPDX-License-Identifier: Apache-2.0
package agentwire

import (
	"reflect"
	"testing"
	"time"

	"github.com/match/admin-gateway/agent"
)

func TestProcessInfoRoundTrip(t *testing.T) {
	in := agent.ProcessInfo{
		Name: "node0", Display: "Cluster Node 0", Role: agent.RoleClusterNode,
		Port: 9002, Running: true, PID: 4242, MemoryBytes: 1 << 30,
		CPUPercent: 42.5, UptimeMs: 123456, StartedAt: "2026-07-06T12:00:00+03:00",
		RestartCount: 3, Enabled: true, Status: "running", LastError: "",
	}
	got := ProcessInfoFromProto(ProcessInfoToProto(in))
	if !reflect.DeepEqual(in, got) {
		t.Fatalf("round trip lost data:\n in=%+v\ngot=%+v", in, got)
	}
}

func TestActionResultsRoundTrip(t *testing.T) {
	in := []agent.ActionResult{
		{Service: "node0", Action: "start", Success: true},
		{Service: "oms", Action: "stop", Success: false, Error: "boom"},
	}
	got := ActionResultsFromProto(ActionResultsToProto(in))
	if !reflect.DeepEqual(in, got) {
		t.Fatalf("round trip lost data:\n in=%+v\ngot=%+v", in, got)
	}
}

func TestCounterDataRoundTrip(t *testing.T) {
	in := &agent.CounterData{CommitPosition: 329448032, SnapshotCount: 12, NodeRole: 2}
	got := CounterDataFromProto(CounterDataToProto(in))
	if !reflect.DeepEqual(in, got) {
		t.Fatalf("round trip lost data: in=%+v got=%+v", in, got)
	}
	if CounterDataToProto(nil) != nil || CounterDataFromProto(nil) != nil {
		t.Fatal("nil must stay nil (Get semantics)")
	}
}

func TestEventRoundTrip(t *testing.T) {
	at := time.Now().Truncate(time.Millisecond) // wire precision is ms
	in := agent.Event{Type: agent.EventCrashed, Service: "node1", PID: 99, Detail: "exit 137", At: at}
	got := EventFromProto(EventToProto(in))
	if got.Type != in.Type || got.Service != in.Service || got.PID != in.PID || got.Detail != in.Detail {
		t.Fatalf("round trip lost fields: %+v vs %+v", in, got)
	}
	if !got.At.Equal(at) {
		t.Fatalf("timestamp drifted: %v vs %v", got.At, at)
	}
}

func TestSummaryRoundTripKeepsHTTPKeys(t *testing.T) {
	in := map[string]interface{}{
		"total": 9, "running": 8, "stopped": 0, "failed": 1,
		"totalMemoryMB": int64(16896),
		"failedServices": map[string]string{
			"sim": "artifact missing: /x/market-sim",
		},
	}
	got := SummaryFromProto(SummaryToProto(in))
	if !reflect.DeepEqual(in, got) {
		t.Fatalf("summary map keys must survive the wire byte-compatibly:\n in=%+v\ngot=%+v", in, got)
	}

	// No failures: the key is omitted, exactly like the LocalAgent map.
	lean := map[string]interface{}{
		"total": 2, "running": 2, "stopped": 0, "failed": 0, "totalMemoryMB": int64(0),
	}
	got = SummaryFromProto(SummaryToProto(lean))
	if _, ok := got["failedServices"]; ok {
		t.Fatal("failedServices must be omitted when empty")
	}
	if !reflect.DeepEqual(lean, got) {
		t.Fatalf("lean summary drifted: %+v vs %+v", lean, got)
	}
}

func TestArtifactSpecFromInstall(t *testing.T) {
	got := ArtifactSpecFromInstall(&InstallArtifactRequest{
		TransferId: "t1", DestPath: "/opt/x.jar", Sha256: "abc", Mode: 0640, Size: 100,
	})
	if got.DestPath != "/opt/x.jar" || got.Sha256 != "abc" || got.Mode != 0640 {
		t.Fatalf("spec mapping wrong: %+v", got)
	}
}
