/* SPDX-License-Identifier: (GPL-2.0-only OR Apache-2.0) */
/* Copyright (c) KubeHero contributors */

/*
 * netflow: L3/L4 byte accounting per endpoint pair.
 *
 * Two BPF_PROG_TYPE_CGROUP_SKB programs attached (multi-attach) to the
 * cgroup v2 root see every packet any inet socket on the node sends or
 * receives, in every network namespace, because cgroup hooks follow the
 * socket's cgroup rather than the netns. The kernel hands them the packet
 * starting at the network header, for both directions.
 *
 * Accounting only: both programs always return 1 (allow). They must never
 * drop or delay traffic, whatever happens in here.
 */

#include "kh_flow.h"

char LICENSE[] SEC("license") = "GPL";

/*
 * Flow table, drained (lookup-and-delete) by userspace every flush
 * interval. LRU so that a flood of unique peers between drains evicts the
 * coldest entries instead of failing updates. max_entries is rewritten at
 * load time when the operator sizes it differently.
 */
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 131072);
	__type(key, struct flow_key);
	__type(value, struct flow_val);
} kh_flows SEC(".maps");

/* Error counters, per CPU, cumulative; userspace sums and diffs them. */
#define KH_NF_ERR_FLOW_UPDATE 0
#define KH_NF_ERR_MAX 1

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, KH_NF_ERR_MAX);
	__type(key, __u32);
	__type(value, __u64);
} kh_nf_errors SEC(".maps");

/* Set by the loader; loopback traffic is skipped unless it is non-zero. */
volatile const __u8 include_loopback = 0;

/* Minimal IPv4 header (no bitfields: the version/IHL byte is split by hand). */
struct kh_iphdr {
	__u8 ver_ihl;
	__u8 tos;
	__be16 tot_len;
	__be16 id;
	__be16 frag_off;
	__u8 ttl;
	__u8 protocol;
	__u16 check;
	__be32 saddr;
	__be32 daddr;
};

/* Addresses as u32 words keeps them 4-byte aligned for cheap comparisons. */
struct kh_ipv6hdr {
	__u8 ver_tc_flow[4];
	__be16 payload_len;
	__u8 nexthdr;
	__u8 hop_limit;
	__u32 saddr[4];
	__u32 daddr[4];
};

/* Generic IPv6 extension header prefix (hop-by-hop, routing, dest opts). */
struct kh_ipv6_opt {
	__u8 nexthdr;
	__u8 hdrlen; /* in 8-octet units, not counting the first 8 */
};

struct kh_ipv6_frag {
	__u8 nexthdr;
	__u8 reserved;
	__be16 frag_off; /* offset << 3 | flags */
	__be32 identification;
};

#define KH_IP_OFFSET 0x1fff    /* IPv4 fragment offset mask */
#define KH_IP6_OFFSET 0xfff8   /* IPv6 fragment offset mask */
#define KH_MAX_IPV6_EXTHDR 4   /* bounded walk; deeper chains count as protocol "other" */

static __always_inline void kh_count_error(__u32 idx)
{
	__u64 *v = bpf_map_lookup_elem(&kh_nf_errors, &idx);

	if (v)
		*v += 1; /* per-CPU slot: no atomics needed */
}

static __always_inline void kh_account(struct __sk_buff *skb, __u8 direction)
{
	struct flow_key key;
	union {
		struct kh_iphdr v4;
		struct kh_ipv6hdr v6;
	} hdr;
	__u32 l4off;
	__u8 proto;
	int have_ports = 1;

	__builtin_memset(&key, 0, sizeof(key));

	if (bpf_skb_load_bytes(skb, 0, &hdr, sizeof(hdr.v4)) < 0)
		return;

	switch (hdr.v4.ver_ihl >> 4) {
	case 4:
		if (!include_loopback && (kh_v4_is_loopback((const __u8 *)&hdr.v4.saddr) ||
					  kh_v4_is_loopback((const __u8 *)&hdr.v4.daddr)))
			return;
		key.family = KH_AF_INET;
		__builtin_memcpy(key.saddr, &hdr.v4.saddr, 4);
		__builtin_memcpy(key.daddr, &hdr.v4.daddr, 4);
		proto = hdr.v4.protocol;
		l4off = (__u32)(hdr.v4.ver_ihl & 0x0f) * 4;
		/* Non-first fragments carry no transport header. */
		if (hdr.v4.frag_off & bpf_htons(KH_IP_OFFSET))
			have_ports = 0;
		break;
	case 6:
		if (bpf_skb_load_bytes(skb, 0, &hdr, sizeof(hdr.v6)) < 0)
			return;
		if (!include_loopback &&
		    (kh_v6_is_loopback(hdr.v6.saddr) || kh_v6_is_loopback(hdr.v6.daddr)))
			return;
		key.family = KH_AF_INET6;
		__builtin_memcpy(key.saddr, hdr.v6.saddr, 16);
		__builtin_memcpy(key.daddr, hdr.v6.daddr, 16);
		proto = hdr.v6.nexthdr;
		l4off = sizeof(hdr.v6);

#pragma unroll
		for (int i = 0; i < KH_MAX_IPV6_EXTHDR; i++) {
			if (proto == KH_IPPROTO_HOPOPTS || proto == KH_IPPROTO_ROUTING ||
			    proto == KH_IPPROTO_DSTOPTS) {
				struct kh_ipv6_opt opt;

				if (bpf_skb_load_bytes(skb, l4off, &opt, sizeof(opt)) < 0) {
					have_ports = 0;
					break;
				}
				proto = opt.nexthdr;
				l4off += ((__u32)opt.hdrlen + 1) * 8;
			} else if (proto == KH_IPPROTO_FRAGMENT) {
				struct kh_ipv6_frag frag;

				if (bpf_skb_load_bytes(skb, l4off, &frag, sizeof(frag)) < 0) {
					have_ports = 0;
					break;
				}
				proto = frag.nexthdr;
				l4off += sizeof(frag);
				if (frag.frag_off & bpf_htons(KH_IP6_OFFSET))
					have_ports = 0;
			} else {
				break;
			}
		}
		break;
	default:
		return;
	}

	key.protocol = proto;
	key.direction = direction;

	if (have_ports && (proto == KH_IPPROTO_TCP || proto == KH_IPPROTO_UDP)) {
		__be16 ports[2];

		if (bpf_skb_load_bytes(skb, l4off, ports, sizeof(ports)) == 0) {
			__u16 sport = bpf_ntohs(ports[0]);
			__u16 dport = bpf_ntohs(ports[1]);

			key.port = sport < dport ? sport : dport;
		}
	}

	/*
	 * A GSO (egress) or GRO (ingress) super-packet stands for gso_segs wire
	 * packets; skb->len is its full L3 length.
	 */
	__u64 bytes = skb->len;
	__u64 packets = skb->gso_segs;

	if (packets == 0)
		packets = 1;

	struct flow_val *val = bpf_map_lookup_elem(&kh_flows, &key);

	if (val) {
		__sync_fetch_and_add(&val->bytes, bytes);
		__sync_fetch_and_add(&val->packets, packets);
		return;
	}

	struct flow_val init = {
		.bytes = bytes,
		.packets = packets,
	};
	long err = bpf_map_update_elem(&kh_flows, &key, &init, BPF_NOEXIST);

	if (err == -KH_EEXIST) {
		/* Another CPU created the entry first: add to it instead. */
		val = bpf_map_lookup_elem(&kh_flows, &key);
		if (val) {
			__sync_fetch_and_add(&val->bytes, bytes);
			__sync_fetch_and_add(&val->packets, packets);
			return;
		}
	}
	if (err)
		kh_count_error(KH_NF_ERR_FLOW_UPDATE);
}

SEC("cgroup_skb/egress")
int kh_flow_egress(struct __sk_buff *skb)
{
	kh_account(skb, KH_DIR_EGRESS);
	return 1;
}

SEC("cgroup_skb/ingress")
int kh_flow_ingress(struct __sk_buff *skb)
{
	kh_account(skb, KH_DIR_INGRESS);
	return 1;
}
