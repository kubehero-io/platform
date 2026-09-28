// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
)

// fakeKubectl answers `get` with canned JSON and records every call.
type fakeKubectl struct {
	list  string
	calls [][]string
	fail  error
}

func (f *fakeKubectl) Run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	if f.fail != nil {
		return nil, f.fail
	}
	if args[0] == "get" {
		return []byte(f.list), nil
	}
	return []byte("patched"), nil
}

func (f *fakeKubectl) patch(t *testing.T) map[string]any {
	t.Helper()
	for _, c := range f.calls {
		if c[0] == "patch" {
			for i, a := range c {
				if a == "-p" {
					var m map[string]any
					if err := json.Unmarshal([]byte(c[i+1]), &m); err != nil {
						t.Fatal(err)
					}
					return m
				}
			}
		}
	}
	return nil
}

// history: rs-00000002 (newest) moved cpu 1→500m; rs-00000001 moved cpu 2→1 and memory 4Gi→3Gi.
const historyJSON = `{"version":1,"changes":[` +
	`{"changeId":"rs-00000002","appliedAt":"2026-09-28T10:00:00Z","policy":"kubehero-system/rs","containers":[` +
	`{"name":"app","previous":{"requests":{"cpu":"1"}},"applied":{"requests":{"cpu":"500m"}}}]},` +
	`{"changeId":"rs-00000001","appliedAt":"2026-09-27T10:00:00Z","policy":"kubehero-system/rs","containers":[` +
	`{"name":"app","previous":{"requests":{"cpu":"2","memory":"4Gi"}},"applied":{"requests":{"cpu":"1","memory":"3Gi"}}}]}]}`

func workloadList(cpu, mem string) string {
	hist, _ := json.Marshal(historyJSON)
	return `{"items":[{"kind":"Deployment","metadata":{"name":"api","namespace":"payments","resourceVersion":"77",` +
		`"annotations":{"kubehero.io/rightsize-previous":` + string(hist) + `}},` +
		`"spec":{"template":{"spec":{"containers":[{"name":"app","resources":{"requests":{"cpu":"` + cpu + `","memory":"` + mem + `"}}}]}}}}]}`
}

func runUndoCmd(t *testing.T, k *fakeKubectl, id string, force, dryRun bool) (string, error) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBEHERO_ENDPOINT", "")
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := runUndo(cmd, k, id, "", force, dryRun, time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC))
	return out.String(), err
}

func TestUndoRestoresAndRewindsHistory(t *testing.T) {
	k := &fakeKubectl{list: workloadList("500m", "3Gi")}
	out, err := runUndoCmd(t, k, "rs-00000001", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(k.calls[0], " "); !strings.Contains(got, "get deployments.apps,statefulsets.apps -o json --all-namespaces") {
		t.Errorf("get call = %q", got)
	}
	p := k.patch(t)
	if p == nil {
		t.Fatal("no patch applied")
	}
	meta := p["metadata"].(map[string]any)
	if meta["resourceVersion"] != "77" {
		t.Errorf("patch must carry the resourceVersion precondition: %v", meta)
	}
	ann := meta["annotations"].(map[string]any)
	if ann["kubehero.io/rightsize-reverted"] != "2026-09-28T12:00:00Z" {
		t.Errorf("reverted marker = %v", ann["kubehero.io/rightsize-reverted"])
	}
	if v, ok := ann["kubehero.io/rightsize-previous"]; !ok || v != nil {
		t.Errorf("history should be cleared after undoing the oldest change, got %v", v)
	}
	c := p["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	req := c["resources"].(map[string]any)["requests"].(map[string]any)
	// Undoing rs-00000001 also unwinds the newer rs-00000002: cpu goes
	// back to 2 (before both), memory to 4Gi.
	if req["cpu"] != "2" || req["memory"] != "4Gi" {
		t.Errorf("restored requests = %v", req)
	}
	for _, c := range k.calls {
		if c[0] == "patch" && !strings.Contains(strings.Join(c, " "), "--field-manager=kubehero-cli") {
			t.Error("patch must set the kubehero-cli field manager")
		}
	}
	mustContain(t, out, "restored payments/api", "1 later change(s) also reverted", "requests.cpu=2")
}

func TestUndoNewestKeepsOlderHistory(t *testing.T) {
	k := &fakeKubectl{list: workloadList("500m", "3Gi")}
	if _, err := runUndoCmd(t, k, "rs-00000002", false, false); err != nil {
		t.Fatal(err)
	}
	p := k.patch(t)
	ann := p["metadata"].(map[string]any)["annotations"].(map[string]any)
	var h rightsizeHistory
	if err := json.Unmarshal([]byte(ann["kubehero.io/rightsize-previous"].(string)), &h); err != nil {
		t.Fatal(err)
	}
	if len(h.Changes) != 1 || h.Changes[0].ChangeID != "rs-00000001" {
		t.Errorf("remaining history = %+v", h)
	}
	req := p["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["resources"].(map[string]any)["requests"].(map[string]any)
	if req["cpu"] != "1" || req["memory"] != nil {
		t.Errorf("restored = %v (only cpu was touched by the newest change)", req)
	}
}

func TestUndoRefusesDriftUnlessForced(t *testing.T) {
	k := &fakeKubectl{list: workloadList("750m", "3Gi")} // someone changed cpu after KubeHero
	_, err := runUndoCmd(t, k, "rs-00000001", false, false)
	if err == nil || !strings.Contains(err.Error(), "modified after KubeHero's last change") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("err = %v", err)
	}
	if k.patch(t) != nil {
		t.Fatal("drifted workload must not be patched")
	}
	k2 := &fakeKubectl{list: workloadList("750m", "3Gi")}
	if _, err := runUndoCmd(t, k2, "rs-00000001", true, false); err != nil {
		t.Fatal(err)
	}
	if k2.patch(t) == nil {
		t.Fatal("--force should patch anyway")
	}
	// Equivalent quantities are not drift.
	k3 := &fakeKubectl{list: workloadList("0.5", "3072Mi")}
	if _, err := runUndoCmd(t, k3, "rs-00000002", false, false); err != nil {
		t.Fatalf("0.5 == 500m and 3072Mi == 3Gi: %v", err)
	}
}

func TestUndoDryRunAndErrors(t *testing.T) {
	k := &fakeKubectl{list: workloadList("500m", "3Gi")}
	out, err := runUndoCmd(t, k, "rs-00000002", false, true)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "would patch deployment/api -n payments")
	if k.patch(t) != nil {
		t.Error("dry run must not patch")
	}
	for _, tc := range []struct{ id, want string }{
		{"aud-1042", "audit-log id"},
		{"1042", "audit-log id"},
		{"banana", "not a rightsizing change id"},
		{"rs-deadbeef", "no Deployment or StatefulSet carries change rs-deadbeef"},
	} {
		if _, err := runUndoCmd(t, &fakeKubectl{list: workloadList("500m", "3Gi")}, tc.id, false, false); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.id, err, tc.want)
		}
	}
	if _, err := runUndoCmd(t, &fakeKubectl{fail: errors.New("kubectl: forbidden")}, "rs-00000002", false, false); err == nil {
		t.Error("kubectl failure must surface")
	}
}

func TestUndoRecordsAuditEntry(t *testing.T) {
	f := newFakeCP(t, "tok")
	k := &fakeKubectl{list: workloadList("500m", "3Gi")}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBEHERO_ENDPOINT", f.srv.URL)
	t.Setenv("KUBEHERO_TOKEN", "tok")
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := runUndo(cmd, k, "rs-00000002", "", false, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	req, _ := f.got("AppendAuditEntry").(*kuberov1.AppendAuditEntryRequest)
	if req == nil || req.GetAction() != "rightsize.undo" || req.GetOutcome() != "reverted" || req.GetTargetName() != "payments/api" {
		t.Fatalf("audit entry = %+v", req)
	}
	var payload map[string]any
	if err := json.Unmarshal(req.GetPayload(), &payload); err != nil || payload["changeId"] != "rs-00000002" {
		t.Errorf("payload = %s (%v)", req.GetPayload(), err)
	}
	mustContain(t, out.String(), "recorded rightsize.undo")
}
