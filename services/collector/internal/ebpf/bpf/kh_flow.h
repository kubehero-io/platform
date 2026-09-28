/* SPDX-License-Identifier: (GPL-2.0-only OR Apache-2.0) */
/* Copyright (c) KubeHero contributors */

/*
 * Flow key shared by the cgroup_skb accounting programs (netflow.bpf.c) and
 * the TCP retransmit tracepoint (tcpretrans.bpf.c). Both maps use the same
 * key so userspace can join retransmits onto egress flows byte-for-byte.
 *
 * The Go mirror is flowKey in flow.go; its layout is asserted against the
 * bpf2go-generated type in layout_linux_test.go.
 */

#ifndef KH_FLOW_H
#define KH_FLOW_H

#include "kh_uapi.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>

#define KH_DIR_EGRESS 0
#define KH_DIR_INGRESS 1

/*
 * Addresses are stored in network byte order. IPv4 uses the first four
 * bytes of the 16-byte fields, zero padded. port is the lower of the two
 * transport ports in host byte order ("server port" heuristic: ephemeral
 * client ports collapse in the kernel already), 0 when not TCP/UDP.
 * pad must stay zero: the kernel hashes the whole key.
 */
struct flow_key {
	__u8 saddr[16];
	__u8 daddr[16];
	__u16 port;
	__u8 family;
	__u8 protocol;
	__u8 direction;
	__u8 pad[3];
} __attribute__((aligned(8)));

_Static_assert(sizeof(struct flow_key) == 40, "flow_key must be 40 bytes");

struct flow_val {
	__u64 bytes;
	__u64 packets;
};

/* 127.0.0.0/8; addr in network byte order. */
static __always_inline int kh_v4_is_loopback(const __u8 *addr)
{
	return addr[0] == 127;
}

/* ::1 */
static __always_inline int kh_v6_is_loopback(const __u32 *a)
{
	return a[0] == 0 && a[1] == 0 && a[2] == 0 && a[3] == bpf_htonl(1);
}

/* ::ffff:0:0/96 */
static __always_inline int kh_v6_is_v4mapped(const __u32 *a)
{
	return a[0] == 0 && a[1] == 0 && a[2] == bpf_htonl(0xffff);
}

#endif /* KH_FLOW_H */
