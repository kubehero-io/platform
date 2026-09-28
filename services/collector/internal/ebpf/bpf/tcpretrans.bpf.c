/* SPDX-License-Identifier: (GPL-2.0-only OR Apache-2.0) */
/* Copyright (c) KubeHero contributors */

/*
 * tcpretrans: count TCP retransmissions per flow key.
 *
 * Attached to the stable tracepoint tcp:tcp_retransmit_skb, which fires on
 * the sending host after a retransmitted segment (including SYN
 * retransmits) was handed to the IP layer successfully.
 *
 * The tracepoint record layout is not UAPI and has grown over the years
 * (state, family, ...), so instead of a compiled-in struct the loader
 * parses <tracefs>/events/tcp/tcp_retransmit_skb/format and passes the
 * byte offsets of the four fields we read as read-only constants. The
 * verifier sees those as known scalars (frozen .rodata), so ctx + offset
 * is a fixed-offset pointer it can check. saddr_v6/daddr_v6 are used for
 * both families: for IPv4 sockets the kernel stores them v4-mapped
 * (TP_STORE_ADDRS), which avoids depending on the optional family field.
 */

#include "kh_flow.h"

char LICENSE[] SEC("license") = "GPL";

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 16384);
	__type(key, struct flow_key);
	__type(value, __u64);
} kh_retrans SEC(".maps");

#define KH_RT_ERR_UPDATE 0
#define KH_RT_ERR_READ 1
#define KH_RT_ERR_MAX 2

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, KH_RT_ERR_MAX);
	__type(key, __u32);
	__type(value, __u64);
} kh_rt_errors SEC(".maps");

volatile const __u8 include_loopback = 0;

/* Defaults match every kernel since ~5.x; the loader always overwrites them. */
volatile const __u32 off_sport = 28;
volatile const __u32 off_dport = 30;
volatile const __u32 off_saddr_v6 = 42;
volatile const __u32 off_daddr_v6 = 58;

static __always_inline void kh_rt_error(__u32 idx)
{
	__u64 *v = bpf_map_lookup_elem(&kh_rt_errors, &idx);

	if (v)
		*v += 1;
}

/* Rewrites a v4-mapped address in place into the flow map's IPv4 form. */
static __always_inline void kh_unmap_v4(__u32 *a)
{
	a[0] = a[3];
	a[1] = 0;
	a[2] = 0;
	a[3] = 0;
}

SEC("tracepoint/tcp/tcp_retransmit_skb")
int kh_tcp_retransmit(void *ctx)
{
	struct flow_key key;
	__u16 sport = 0, dport = 0;

	__builtin_memset(&key, 0, sizeof(key));

	if (bpf_probe_read_kernel(&sport, sizeof(sport), (char *)ctx + off_sport) < 0 ||
	    bpf_probe_read_kernel(&dport, sizeof(dport), (char *)ctx + off_dport) < 0 ||
	    bpf_probe_read_kernel(key.saddr, sizeof(key.saddr), (char *)ctx + off_saddr_v6) < 0 ||
	    bpf_probe_read_kernel(key.daddr, sizeof(key.daddr), (char *)ctx + off_daddr_v6) < 0) {
		kh_rt_error(KH_RT_ERR_READ);
		return 0;
	}

	__u32 *s = (__u32 *)key.saddr;
	__u32 *d = (__u32 *)key.daddr;

	if (kh_v6_is_v4mapped(s) && kh_v6_is_v4mapped(d)) {
		kh_unmap_v4(s);
		kh_unmap_v4(d);
		if (!include_loopback &&
		    (kh_v4_is_loopback(key.saddr) || kh_v4_is_loopback(key.daddr)))
			return 0;
		key.family = KH_AF_INET;
	} else {
		if (!include_loopback && (kh_v6_is_loopback(s) || kh_v6_is_loopback(d)))
			return 0;
		key.family = KH_AF_INET6;
	}

	/* Tracepoint ports are already host byte order (ntohs in TP_fast_assign). */
	key.port = sport < dport ? sport : dport;
	key.protocol = KH_IPPROTO_TCP;
	key.direction = KH_DIR_EGRESS; /* retransmits are always seen at the sender */

	__u64 *cnt = bpf_map_lookup_elem(&kh_retrans, &key);

	if (cnt) {
		__sync_fetch_and_add(cnt, 1);
		return 0;
	}

	__u64 one = 1;
	long err = bpf_map_update_elem(&kh_retrans, &key, &one, BPF_NOEXIST);

	if (err == -KH_EEXIST) {
		cnt = bpf_map_lookup_elem(&kh_retrans, &key);
		if (cnt) {
			__sync_fetch_and_add(cnt, 1);
			return 0;
		}
	}
	if (err)
		kh_rt_error(KH_RT_ERR_UPDATE);
	return 0;
}
