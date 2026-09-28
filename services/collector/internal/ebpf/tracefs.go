// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package ebpf

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// tracefsRoots are the two places tracefs is conventionally mounted.
// Newer systems use /sys/kernel/tracing; Docker Desktop's LinuxKit (and
// many older distros) only have it under debugfs.
var tracefsRoots = []string{"/sys/kernel/tracing", "/sys/kernel/debug/tracing"}

// tpField is one field of a tracepoint record as described by its
// tracefs "format" file.
type tpField struct {
	Offset int
	Size   int
}

// retransmitOffsets are the byte offsets of the tcp_retransmit_skb record
// fields the tcpretrans program reads.
type retransmitOffsets struct {
	Sport, Dport, SaddrV6, DaddrV6 uint32
}

// parseTracepointFormat extracts field name -> (offset, size) from a
// tracefs format file:
//
//	field:__u16 sport;	offset:28;	size:2;	signed:0;
//	field:__u8 saddr_v6[16];	offset:42;	size:16;	signed:0;
func parseTracepointFormat(r io.Reader) (map[string]tpField, error) {
	fields := make(map[string]tpField)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "field:") {
			continue
		}
		var decl string
		f := tpField{Offset: -1, Size: -1}
		for _, part := range strings.Split(line, ";") {
			part = strings.TrimSpace(part)
			key, val, ok := strings.Cut(part, ":")
			if !ok {
				continue
			}
			switch key {
			case "field":
				decl = val
			case "offset":
				n, err := strconv.Atoi(val)
				if err != nil {
					return nil, fmt.Errorf("tracepoint format: bad offset in %q", line)
				}
				f.Offset = n
			case "size":
				n, err := strconv.Atoi(val)
				if err != nil {
					return nil, fmt.Errorf("tracepoint format: bad size in %q", line)
				}
				f.Size = n
			}
		}
		name := fieldName(decl)
		if name == "" || f.Offset < 0 || f.Size <= 0 {
			return nil, fmt.Errorf("tracepoint format: unparseable field line %q", line)
		}
		fields[name] = f
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(fields) == 0 {
		return nil, errors.New("tracepoint format: no fields")
	}
	return fields, nil
}

// fieldName returns the identifier of a C declaration such as
// "const void * skbaddr" or "__u8 saddr_v6[16]".
func fieldName(decl string) string {
	decl = strings.TrimSpace(decl)
	if i := strings.IndexByte(decl, '['); i >= 0 {
		decl = decl[:i]
	}
	decl = strings.TrimRight(decl, " \t")
	i := strings.LastIndexAny(decl, " \t*")
	return decl[i+1:]
}

// retransmitOffsetsFrom validates the fields tcpretrans.bpf.c relies on.
// Sizes are checked as well as presence: a field that changed type would
// otherwise be read with the wrong width.
func retransmitOffsetsFrom(fields map[string]tpField) (off retransmitOffsets, err error) {
	get := func(name string, size int) (uint32, error) {
		f, ok := fields[name]
		switch {
		case !ok:
			return 0, fmt.Errorf("tcp_retransmit_skb has no %q field", name)
		case f.Size != size:
			return 0, fmt.Errorf("tcp_retransmit_skb field %q is %d bytes, want %d", name, f.Size, size)
		case f.Offset > 4096: // records are a few hundred bytes at most
			return 0, fmt.Errorf("tcp_retransmit_skb field %q at implausible offset %d", name, f.Offset)
		}
		return uint32(f.Offset), nil
	}
	if off.Sport, err = get("sport", 2); err != nil {
		return off, err
	}
	if off.Dport, err = get("dport", 2); err != nil {
		return off, err
	}
	if off.SaddrV6, err = get("saddr_v6", 16); err != nil {
		return off, err
	}
	off.DaddrV6, err = get("daddr_v6", 16)
	return off, err
}

// readRetransmitOffsets finds tcp_retransmit_skb's format under any
// mounted tracefs.
func readRetransmitOffsets() (retransmitOffsets, error) {
	var errs []error
	for _, root := range tracefsRoots {
		f, err := os.Open(filepath.Join(root, "events", "tcp", "tcp_retransmit_skb", "format"))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		fields, err := parseTracepointFormat(io.LimitReader(f, 64<<10))
		_ = f.Close()
		if err != nil {
			return retransmitOffsets{}, err
		}
		return retransmitOffsetsFrom(fields)
	}
	return retransmitOffsets{}, fmt.Errorf("tcp_retransmit_skb format not readable (is tracefs mounted?): %w", errors.Join(errs...))
}
