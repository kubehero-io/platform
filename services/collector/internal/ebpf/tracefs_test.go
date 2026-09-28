// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package ebpf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Verbatim from Linux 6.10 (Docker Desktop LinuxKit).
const retransmitFormat610 = `name: tcp_retransmit_skb
ID: 2600
format:
	field:unsigned short common_type;	offset:0;	size:2;	signed:0;
	field:unsigned char common_flags;	offset:2;	size:1;	signed:0;
	field:unsigned char common_preempt_count;	offset:3;	size:1;	signed:0;
	field:int common_pid;	offset:4;	size:4;	signed:1;

	field:const void * skbaddr;	offset:8;	size:8;	signed:0;
	field:const void * skaddr;	offset:16;	size:8;	signed:0;
	field:int state;	offset:24;	size:4;	signed:1;
	field:__u16 sport;	offset:28;	size:2;	signed:0;
	field:__u16 dport;	offset:30;	size:2;	signed:0;
	field:__u16 family;	offset:32;	size:2;	signed:0;
	field:__u8 saddr[4];	offset:34;	size:4;	signed:0;
	field:__u8 daddr[4];	offset:38;	size:4;	signed:0;
	field:__u8 saddr_v6[16];	offset:42;	size:16;	signed:0;
	field:__u8 daddr_v6[16];	offset:58;	size:16;	signed:0;

print fmt: "skbaddr=%p skaddr=%p family=%s sport=%hu dport=%hu", REC->skbaddr, REC->skaddr
`

// Linux 4.15 had neither state nor family, so every later field sat 8
// bytes earlier: exactly the drift parsing the format absorbs.
const retransmitFormat415 = `name: tcp_retransmit_skb
format:
	field:unsigned short common_type;	offset:0;	size:2;	signed:0;
	field:int common_pid;	offset:4;	size:4;	signed:1;

	field:const void * skbaddr;	offset:8;	size:8;	signed:0;
	field:const void * skaddr;	offset:16;	size:8;	signed:0;
	field:__u16 sport;	offset:24;	size:2;	signed:0;
	field:__u16 dport;	offset:26;	size:2;	signed:0;
	field:__u8 saddr[4];	offset:28;	size:4;	signed:0;
	field:__u8 daddr[4];	offset:32;	size:4;	signed:0;
	field:__u8 saddr_v6[16];	offset:36;	size:16;	signed:0;
	field:__u8 daddr_v6[16];	offset:52;	size:16;	signed:0;
`

func TestRetransmitOffsets(t *testing.T) {
	tests := []struct {
		name    string
		format  string
		want    retransmitOffsets
		wantErr string
	}{
		{"linux 6.10", retransmitFormat610, retransmitOffsets{Sport: 28, Dport: 30, SaddrV6: 42, DaddrV6: 58}, ""},
		{"linux 4.15 layout", retransmitFormat415, retransmitOffsets{Sport: 24, Dport: 26, SaddrV6: 36, DaddrV6: 52}, ""},
		{"missing field", strings.ReplaceAll(retransmitFormat610, "daddr_v6", "dst6"), retransmitOffsets{}, `no "daddr_v6"`},
		{"changed width", strings.Replace(retransmitFormat610, "__u16 sport;	offset:28;	size:2;", "__u32 sport;	offset:28;	size:4;", 1), retransmitOffsets{}, "is 4 bytes"},
		{"implausible offset", strings.Replace(retransmitFormat610, "offset:58;", "offset:99999;", 1), retransmitOffsets{}, "implausible"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fields, err := parseTracepointFormat(strings.NewReader(tt.format))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			got, err := retransmitOffsetsFrom(fields)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("got %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

func TestParseTracepointFormatFields(t *testing.T) {
	fields, err := parseTracepointFormat(strings.NewReader(retransmitFormat610))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]tpField{
		"common_type": {0, 2}, "skbaddr": {8, 8}, "state": {24, 4}, "saddr": {34, 4}, "daddr_v6": {58, 16},
	} {
		if got := fields[name]; got != want {
			t.Errorf("%s = %+v, want %+v", name, got, want)
		}
	}
}

func TestParseTracepointFormatErrors(t *testing.T) {
	for name, in := range map[string]string{
		"empty":       "name: x\nformat:\n",
		"bad offset":  "\tfield:int x;\toffset:abc;\tsize:4;\tsigned:1;\n",
		"no size":     "\tfield:int x;\toffset:4;\tsigned:1;\n",
		"no ident":    "\tfield:;\toffset:4;\tsize:4;\n",
		"only prints": "print fmt: \"x\"\n",
	} {
		if _, err := parseTracepointFormat(strings.NewReader(in)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestFieldName(t *testing.T) {
	for decl, want := range map[string]string{
		"const void * skbaddr":       "skbaddr",
		"__u8 saddr_v6[16]":          "saddr_v6",
		"int state":                  "state",
		"unsigned short common_type": "common_type",
		"char *name":                 "name",
		"__data_loc char[] dev":      "dev",
	} {
		if got := fieldName(decl); got != want {
			t.Errorf("fieldName(%q) = %q, want %q", decl, got, want)
		}
	}
}

// TestReadRetransmitOffsetsSearchesMounts mirrors LinuxKit: nothing at
// /sys/kernel/tracing, the format under the debugfs mount.
func TestReadRetransmitOffsetsSearchesMounts(t *testing.T) {
	saved := tracefsRoots
	t.Cleanup(func() { tracefsRoots = saved })

	debugfs := t.TempDir()
	dir := filepath.Join(debugfs, "events", "tcp", "tcp_retransmit_skb")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "format"), []byte(retransmitFormat610), 0o644); err != nil {
		t.Fatal(err)
	}
	tracefsRoots = []string{filepath.Join(t.TempDir(), "missing"), debugfs}
	got, err := readRetransmitOffsets()
	if err != nil || got != (retransmitOffsets{Sport: 28, Dport: 30, SaddrV6: 42, DaddrV6: 58}) {
		t.Fatalf("got %+v, %v", got, err)
	}

	tracefsRoots = []string{filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")}
	if _, err := readRetransmitOffsets(); err == nil || !strings.Contains(err.Error(), "tracefs") {
		t.Fatalf("no tracefs: err = %v", err)
	}
}
