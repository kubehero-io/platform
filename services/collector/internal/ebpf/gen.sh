#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (c) KubeHero contributors
#
# Regenerates the committed bpf2go outputs (<prog>_bpfel.go + <prog>_bpfel.o)
# from bpf/*.bpf.c. Compilation happens inside a pinned Debian toolchain
# container so contributors need Docker, not clang, and every regeneration
# uses the same compiler and libbpf headers. A plain `go build` never needs
# any of this: the objects are embedded from the committed files.
#
# -target bpfel: one little-endian object serves amd64 and arm64.
# -tags linux:   the generated Go files only build on Linux; everything else
#                gets the ErrUnsupported stub in ebpf_other.go.
#
# Usage: internal/ebpf/gen.sh   (or `make bpf` in services/collector)

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
collector="$(cd "${here}/../.." && pwd)"

# bpf2go must match the cilium/ebpf version the loader is compiled against,
# or the generated code may reference APIs that don't exist.
ebpf_version="$(awk '{ for (i = 1; i < NF; i++) if ($i == "github.com/cilium/ebpf") { print $(i + 1); exit } }' "${collector}/go.mod")"
if [[ -z "${ebpf_version}" ]]; then
	echo "gen.sh: github.com/cilium/ebpf not found in ${collector}/go.mod" >&2
	exit 1
fi

# The toolchain image is derived from the same Go image the repo builds
# with. clang/llvm are pinned to the Debian 13 major version; the tag is
# content-addressed so an edit here rebuilds it and nothing else does.
dockerfile="$(cat <<'EOF'
FROM golang:1.26.5-trixie
RUN apt-get update \
 && apt-get install -y --no-install-recommends clang-19 llvm-19 libbpf-dev \
 && rm -rf /var/lib/apt/lists/*
EOF
)"
if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum; }
else
	sha256() { shasum -a 256; }
fi
tag="kubehero-bpf-builder:$(printf '%s' "${dockerfile}" | sha256 | cut -c1-12)"
if ! docker image inspect "${tag}" >/dev/null 2>&1; then
	echo "gen.sh: building ${tag}" >&2
	printf '%s\n' "${dockerfile}" | docker build -q -t "${tag}" - >/dev/null
fi

cflags="-O2 -g -Wall -Werror"
programs=(netflow tcpretrans profiler)

# The module cache and build cache live in named volumes so repeated runs
# are fast without touching the host's caches. Generated files are chowned
# back to the caller (a no-op on Docker Desktop, needed on Linux hosts).
docker run --rm \
	-v "${here}:/work" -w /work \
	-v kubehero-bpf-gomod:/go/pkg/mod \
	-v kubehero-bpf-gocache:/root/.cache/go-build \
	-e GOWORK=off -e GOTOOLCHAIN=local -e GOFLAGS=-modcacherw \
	-e HOST_UID="$(id -u)" -e HOST_GID="$(id -g)" \
	-e EBPF_VERSION="${ebpf_version}" -e CFLAGS="${cflags}" \
	"${tag}" bash -euo pipefail -c '
		for prog in '"${programs[*]}"'; do
			echo "bpf2go: ${prog} (cilium/ebpf ${EBPF_VERSION})" >&2
			go run "github.com/cilium/ebpf/cmd/bpf2go@${EBPF_VERSION}" \
				-cc clang-19 -strip llvm-strip-19 \
				-target bpfel -go-package ebpf -tags linux \
				-cflags "${CFLAGS}" \
				"${prog}" "bpf/${prog}.bpf.c"
		done
		chown "${HOST_UID}:${HOST_GID}" ./*_bpfel.go ./*_bpfel.o
	'

# bpf2go output is gofmt-clean, but make that a checked property.
if command -v gofmt >/dev/null 2>&1; then
	unformatted="$(gofmt -l "${here}"/*_bpfel.go)"
	if [[ -n "${unformatted}" ]]; then
		echo "gen.sh: generated files are not gofmt-clean: ${unformatted}" >&2
		exit 1
	fi
fi
echo "gen.sh: done" >&2
