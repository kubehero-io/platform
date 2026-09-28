// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

//go:build linux

package ebpf

import (
	"encoding/binary"
	"errors"
	"fmt"

	cebpf "github.com/cilium/ebpf"
)

// drainChunk is how many entries one batch syscall moves: large enough
// that a 128k-entry table drains in a few dozen syscalls, small enough
// that the buffers stay a few hundred KB.
const drainChunk = 4096

// forceIterDrain makes drainMap take the pre-5.6 fallback path; tests use
// it to exercise that path on new kernels.
var forceIterDrain bool

// drainMap reads and deletes every entry of a hash map, passing them to
// fn in chunks, and returns how many entries it drained. It uses
// BPF_MAP_LOOKUP_AND_DELETE_BATCH (5.6+: one syscall per chunk, atomic
// per bucket, so no increments are lost) and falls back to walking keys
// on older kernels.
func drainMap[K comparable, V any](m *cebpf.Map, fn func(keys []K, vals []V)) (int, error) {
	if !forceIterDrain {
		n, err := drainBatch(m, fn)
		if !errors.Is(err, cebpf.ErrNotSupported) {
			return n, err
		}
	}
	return drainIter(m, fn)
}

func drainBatch[K comparable, V any](m *cebpf.Map, fn func([]K, []V)) (int, error) {
	keys := make([]K, drainChunk)
	vals := make([]V, drainChunk)
	var cursor cebpf.MapBatchCursor
	total := 0
	for {
		n, err := m.BatchLookupAndDelete(&cursor, keys, vals, nil)
		if n > 0 {
			fn(keys[:n], vals[:n])
			total += n
		}
		switch {
		case errors.Is(err, cebpf.ErrKeyNotExist):
			return total, nil // reached the end
		case err != nil:
			return total, err
		case n == 0:
			return total, fmt.Errorf("batch drain of %s made no progress", m)
		}
	}
}

// drainIter is the fallback: snapshot the keys, then take each entry out.
// Deleting while iterating would make the kernel restart the walk.
func drainIter[K comparable, V any](m *cebpf.Map, fn func([]K, []V)) (int, error) {
	limit := int(m.MaxEntries())
	seen := make(map[K]struct{})
	var keys []K
	var k K
	var v V
	it := m.Iterate()
	for len(keys) < limit && it.Next(&k, &v) {
		if _, dup := seen[k]; !dup { // concurrent inserts can restart the walk
			seen[k] = struct{}{}
			keys = append(keys, k)
		}
	}
	if err := it.Err(); err != nil {
		return 0, err
	}
	var outK []K
	var outV []V
	total := 0
	flush := func() {
		fn(outK, outV)
		total += len(outK)
		outK, outV = nil, nil
	}
	atomicTake := true // LookupAndDelete on hash maps needs 5.14
	for i := range keys {
		var val V
		var err error
		if atomicTake {
			err = m.LookupAndDelete(&keys[i], &val)
			if errors.Is(err, cebpf.ErrNotSupported) {
				atomicTake = false
			}
		}
		if !atomicTake {
			// Increments landing between these two calls are lost; only
			// kernels without batch ops (< 5.6) ever get here.
			if err = m.Lookup(&keys[i], &val); err == nil {
				err = m.Delete(&keys[i])
			}
		}
		if errors.Is(err, cebpf.ErrKeyNotExist) {
			continue // evicted or drained concurrently
		}
		if err != nil {
			flush()
			return total, err
		}
		outK = append(outK, keys[i])
		outV = append(outV, val)
		if len(outK) == drainChunk {
			flush()
		}
	}
	flush()
	return total, nil
}

// sumPerCPU reads slot idx of a per-CPU u64 array and sums all CPUs.
func sumPerCPU(m *cebpf.Map, idx uint32) (uint64, error) {
	var vals []uint64
	if err := m.Lookup(idx, &vals); err != nil {
		return 0, err
	}
	var total uint64
	for _, v := range vals {
		total += v
	}
	return total, nil
}

// checkLayout guards the hand-written Go mirrors of the C structs: a
// drifted layout would silently decode garbage.
func checkLayout(spec *cebpf.MapSpec, key, value any) error {
	if spec == nil {
		return errors.New("map missing from object")
	}
	if ks := binary.Size(key); spec.KeySize != uint32(ks) {
		return fmt.Errorf("map %s: key is %d bytes in BPF, %d in Go", spec.Name, spec.KeySize, ks)
	}
	if vs := binary.Size(value); spec.ValueSize != uint32(vs) {
		return fmt.Errorf("map %s: value is %d bytes in BPF, %d in Go", spec.Name, spec.ValueSize, vs)
	}
	return nil
}
