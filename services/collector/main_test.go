// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServeHelpDocumentsEnvAndFlags(t *testing.T) {
	c := serveCmd()
	var buf bytes.Buffer
	c.SetOut(&buf)
	c.SetArgs([]string{"--help"})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	help := buf.String()
	for _, want := range []string{
		"NODE_NAME", "POD_NAME", "POD_NAMESPACE", "CLUSTER_ID", "CONTROL_PLANE_URL", "CONTROL_PLANE_TOKEN", "PRICING_ENGINE_URL",
		"--addr", "--demo", "--scan-interval", "--usage-interval", "--kubelet-url", "--leader-elect", "--events",
		"--logs-root", "--logs-positions", "--logs-from", "--logs-rate-limit", "--logs-burst", "--logs-exclude-namespaces",
		"--logs-pod-labels", "--logs-max-buffer-bytes", "--profiles", "--profile-interval", "--profile-cpu-seconds",
		"--ebpf", "--ebpf-netflow", "--ebpf-profiler", "--ebpf-profile-hz", "--cgroup-root", "--shutdown-timeout",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("serve --help does not document %s", want)
		}
	}
}

func TestVersionCommand(t *testing.T) {
	old := version
	version = "v0.3.0-test"
	defer func() { version = old }()
	c := versionCmd()
	var buf bytes.Buffer
	c.SetOut(&buf)
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(buf.String()) != "v0.3.0-test" {
		t.Fatalf("version printed %q", buf.String())
	}
}

func TestServeRejectsInvalidFlags(t *testing.T) {
	for _, args := range [][]string{{"--logs-from=middle"}, {"--log-level=loud"}} {
		c := serveCmd()
		c.SetOut(&bytes.Buffer{})
		c.SetErr(&bytes.Buffer{})
		c.SetArgs(args)
		if err := c.Execute(); err == nil {
			t.Errorf("serve %v must fail validation before starting", args)
		}
	}
}

func TestPodNamespaceFallsBackToServiceAccount(t *testing.T) {
	dir := t.TempDir()
	old := saNamespaceFile
	saNamespaceFile = filepath.Join(dir, "namespace")
	defer func() { saNamespaceFile = old }()

	t.Setenv("POD_NAMESPACE", "")
	if got := podNamespace(); got != "" {
		t.Fatalf("outside a cluster: %q", got)
	}
	if err := os.WriteFile(saNamespaceFile, []byte("kubehero-system\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := podNamespace(); got != "kubehero-system" {
		t.Fatalf("service account fallback = %q", got)
	}
	if got := defaultExcludes(); len(got) != 1 || got[0] != "kubehero-system" {
		t.Fatalf("default log excludes = %v", got)
	}
	t.Setenv("POD_NAMESPACE", "explicit")
	if got := podNamespace(); got != "explicit" {
		t.Fatalf("env must win: %q", got)
	}
	t.Setenv("POD_NAME", "")
	if podName() == "" {
		t.Fatal("pod name must fall back to the hostname")
	}
}
