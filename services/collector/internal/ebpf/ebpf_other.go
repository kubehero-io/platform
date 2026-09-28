// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

//go:build !linux

package ebpf

import "context"

func start(context.Context, Config) error { return ErrUnsupported }
