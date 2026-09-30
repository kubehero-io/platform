# SPDX-License-Identifier: Apache-2.0
# Copyright (c) KubeHero contributors
#
# Developer entry points for the collector. A normal build needs none of
# this - `go build ./...` works on a fresh clone because the eBPF objects
# are committed. These targets regenerate those objects and run the kernel
# integration tests; both need Docker, neither needs clang on the host.

REPO_ROOT := $(abspath ../..)
GO_IMAGE ?= golang:1.26.8

.PHONY: bpf ebpf-test ebpf-test-nested

# Regenerate internal/ebpf/*_bpfel.{go,o} from internal/ebpf/bpf/*.bpf.c.
bpf:
	./internal/ebpf/gen.sh

# Load and attach the real programs inside a privileged container on the
# Docker host's kernel (Linux, or the Docker Desktop VM). --cgroupns=host
# lets the profiler test move a child into a fake kubepods cgroup.
ebpf-test:
	docker run --rm --privileged --pid=host --cgroupns=host \
		-v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel:/sys/kernel \
		-v $(REPO_ROOT):/src -w /src/services/collector \
		-v kubehero-bpf-gomod:/go/pkg/mod -v kubehero-bpf-gocache:/root/.cache/go-build \
		$(GO_IMAGE) go test -tags ebpfintegration -count=1 -v ./internal/ebpf/...

# The profiler the way it runs on kind / k3d / Docker Desktop nodes: no
# --pid=host, so the test runs in a nested PID namespace and the profiled
# child gets one of its own.
ebpf-test-nested:
	docker run --rm --privileged --cgroupns=host -e KH_EBPF_EXPECT=nested \
		-v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel:/sys/kernel \
		-v $(REPO_ROOT):/src -w /src/services/collector \
		-v kubehero-bpf-gomod:/go/pkg/mod -v kubehero-bpf-gocache:/root/.cache/go-build \
		$(GO_IMAGE) go test -tags ebpfintegration -count=1 -v -run 'Nested' ./internal/ebpf/...
