// SPDX-License-Identifier: Apache-2.0
package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// runRoot executes the root command with args and returns combined output.
func runRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	buf := &bytes.Buffer{}
	r := Root()
	r.SetOut(buf)
	r.SetErr(buf)
	r.SetArgs(args)
	err := r.Execute()
	return buf.String(), err
}

func TestCapArmCallsArmPolicy(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"policyName":"gpu-inference-cap","armed":true,"effectiveAtUnix":"1767225600","auditId":"aud-42"}`))
	}))
	t.Cleanup(srv.Close)

	out, err := runRoot(t,
		"cap", "--arm",
		"--policy", "gpu-inference-cap",
		"--cluster", "eks-use1-prod",
		"--reason", "runaway GPU spend",
		"--endpoint", srv.URL,
	)
	if err != nil {
		t.Fatalf("cap --arm: %v\n%s", err, out)
	}

	if want := "/kubehero.v1.ControlPlaneService/ArmPolicy"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	var req struct {
		ClusterID  string `json:"clusterId"`
		PolicyName string `json:"policyName"`
		Armed      bool   `json:"armed"`
		Reason     string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(gotBody), &req); err != nil {
		t.Fatalf("request body not JSON: %v · %s", err, gotBody)
	}
	if req.ClusterID != "eks-use1-prod" || req.PolicyName != "gpu-inference-cap" ||
		!req.Armed || req.Reason != "runaway GPU spend" {
		t.Errorf("request = %+v", req)
	}

	for _, want := range []string{"armed", "gpu-inference-cap", "aud-42"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

func TestCapDisarmSendsArmedFalse(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"policyName":"gpu-inference-cap","armed":false,"auditId":"aud-43"}`))
	}))
	t.Cleanup(srv.Close)

	out, err := runRoot(t,
		"cap", "--disarm",
		"--policy", "gpu-inference-cap",
		"--cluster", "eks-use1-prod",
		"--endpoint", srv.URL,
	)
	if err != nil {
		t.Fatalf("cap --disarm: %v\n%s", err, out)
	}
	if !strings.Contains(gotBody, `"armed":false`) {
		t.Errorf("request should carry armed=false: %s", gotBody)
	}
	if !strings.Contains(out, "disarmed") {
		t.Errorf("output should say disarmed:\n%s", out)
	}
}

func TestCapFlagValidation(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"no verb", []string{"cap", "--policy", "x"}, "pass --arm"},
		{"both verbs", []string{"cap", "--arm", "--disarm", "--policy", "x"}, "mutually exclusive"},
		{"missing policy", []string{"cap", "--arm"}, "--policy is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runRoot(t, tc.args...)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want mention of %q", err.Error(), tc.wantErr)
			}
		})
	}
}
