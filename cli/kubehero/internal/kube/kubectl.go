// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

// Package kube runs kubectl for the few CLI commands that act on the
// cluster directly (kubehero undo). Shelling out keeps the binary lean
// — no client-go — and inherits every kubeconfig auth plugin (EKS, GKE,
// AKS, OIDC) and the user's own RBAC for free.
package kube

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Runner executes kubectl with args and returns stdout.
type Runner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

// maxOutput caps what we read back from kubectl (a big `get -A -o json`).
const maxOutput = 256 << 20

// Kubectl is the real runner.
type Kubectl struct {
	// Path defaults to $KUBECTL, then "kubectl" on PATH.
	Path       string
	Kubeconfig string
	Context    string
}

func (k Kubectl) Run(ctx context.Context, args ...string) ([]byte, error) {
	path := k.Path
	if path == "" {
		path = os.Getenv("KUBECTL")
	}
	if path == "" {
		path = "kubectl"
	}
	full := make([]string, 0, len(args)+4)
	if k.Kubeconfig != "" {
		full = append(full, "--kubeconfig", k.Kubeconfig)
	}
	if k.Context != "" {
		full = append(full, "--context", k.Context)
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, path, full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitedBuffer{buf: &stdout, max: maxOutput}
	cmd.Stderr = &limitedBuffer{buf: &stderr, max: 64 << 10}
	if err := cmd.Run(); err != nil {
		var ee *exec.Error
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("kubectl not found (%s): install it or set KUBECTL", path)
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("kubectl %s: %s", args[0], msg)
	}
	return stdout.Bytes(), nil
}

type limitedBuffer struct {
	buf *bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if l.buf.Len()+len(p) > l.max {
		return 0, errors.New("kubectl output exceeds the size limit")
	}
	return l.buf.Write(p)
}
