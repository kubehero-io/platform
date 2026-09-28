// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

//go:build unix

package logs

import (
	"os"
	"syscall"
)

// inode identifies a file independently of its name, which is what
// survives a rename-based rotation.
func inode(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino) //nolint:unconvert // Ino is uint32 on some platforms
	}
	return 0
}
