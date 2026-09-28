// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

//go:build !unix

package logs

import "os"

// inode is unavailable off Unix; rotation detection then relies on
// size shrinking (truncation) only. The collector ships for Linux.
func inode(os.FileInfo) uint64 { return 0 }
