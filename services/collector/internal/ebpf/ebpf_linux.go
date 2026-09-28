// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

//go:build linux

package ebpf

import (
	"context"
	"fmt"
)

// start is replaced by the real loader (netflow.go / profiler.go).
func start(context.Context, Config) error {
	return fmt.Errorf("%w: kernel programs not built into this binary yet", ErrUnsupported)
}
