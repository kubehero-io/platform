/* SPDX-License-Identifier: (GPL-2.0-only OR Apache-2.0) */
/* Copyright (c) KubeHero contributors */

/*
 * kh_uapi.h is the hand-written, minimal subset of kernel definitions the
 * KubeHero BPF programs need. It deliberately replaces both vmlinux.h and
 * the <linux/...> UAPI headers:
 *
 *   - Only stable ABI is used: the scalar typedefs, a handful of UAPI
 *     constants and the leading fields of struct __sk_buff (UAPI context
 *     structs are append-only, so a verbatim prefix keeps its offsets).
 *   - No kernel-internal struct is touched, so the objects need no CO-RE
 *     relocations and therefore no kernel BTF. Docker Desktop's LinuxKit
 *     kernel, for one, ships without /sys/kernel/btf/vmlinux.
 *   - No <asm/...> headers means the object is byte-identical no matter
 *     which architecture the generator container runs on.
 */

#ifndef KH_UAPI_H
#define KH_UAPI_H

typedef signed char __s8;
typedef unsigned char __u8;
typedef short __s16;
typedef unsigned short __u16;
typedef int __s32;
typedef unsigned int __u32;
typedef long long __s64;
typedef unsigned long long __u64;

typedef __u16 __be16;
typedef __u32 __be32;
typedef __u64 __be64;
typedef __u32 __wsum;

/* include/uapi/linux/bpf.h: enum bpf_map_type (subset). */
enum {
	BPF_MAP_TYPE_HASH = 1,
	BPF_MAP_TYPE_ARRAY = 2,
	BPF_MAP_TYPE_PERCPU_ARRAY = 6,
	BPF_MAP_TYPE_STACK_TRACE = 7,
	BPF_MAP_TYPE_LRU_HASH = 9,
};

/* include/uapi/linux/bpf.h: bpf_map_update_elem flags. */
enum {
	BPF_ANY = 0,
	BPF_NOEXIST = 1,
	BPF_EXIST = 2,
};

/* include/uapi/linux/bpf.h: bpf_get_stackid / bpf_get_stack flags. */
#define BPF_F_USER_STACK (1ULL << 8)

/* include/uapi/asm-generic/errno-base.h / errno.h */
#define KH_ENOMEM 12
#define KH_EFAULT 14
#define KH_EEXIST 17

/* include/linux/socket.h */
#define KH_AF_INET 2
#define KH_AF_INET6 10

/* include/uapi/linux/in.h, in6.h */
#define KH_IPPROTO_HOPOPTS 0
#define KH_IPPROTO_ICMP 1
#define KH_IPPROTO_TCP 6
#define KH_IPPROTO_UDP 17
#define KH_IPPROTO_ROUTING 43
#define KH_IPPROTO_FRAGMENT 44
#define KH_IPPROTO_ICMPV6 58
#define KH_IPPROTO_DSTOPTS 60

/*
 * include/uapi/linux/bpf.h: struct __sk_buff, verbatim up to gso_segs (the
 * last field these programs read). The verifier rewrites accesses by
 * offset, so the prefix must match the UAPI layout exactly; the offset
 * comments are asserted with _Static_assert below.
 */
struct __sk_buff {
	__u32 len;             /* 0 */
	__u32 pkt_type;        /* 4 */
	__u32 mark;            /* 8 */
	__u32 queue_mapping;   /* 12 */
	__u32 protocol;        /* 16 */
	__u32 vlan_present;    /* 20 */
	__u32 vlan_tci;        /* 24 */
	__u32 vlan_proto;      /* 28 */
	__u32 priority;        /* 32 */
	__u32 ingress_ifindex; /* 36 */
	__u32 ifindex;         /* 40 */
	__u32 tc_index;        /* 44 */
	__u32 cb[5];           /* 48 */
	__u32 hash;            /* 68 */
	__u32 tc_classid;      /* 72 */
	__u32 data;            /* 76 */
	__u32 data_end;        /* 80 */
	__u32 napi_id;         /* 84 */
	__u32 family;          /* 88 */
	__u32 remote_ip4;      /* 92 */
	__u32 local_ip4;       /* 96 */
	__u32 remote_ip6[4];   /* 100 */
	__u32 local_ip6[4];    /* 116 */
	__u32 remote_port;     /* 132 */
	__u32 local_port;      /* 136 */
	__u32 data_meta;       /* 140 */
	__u64 flow_keys;       /* 144: __bpf_md_ptr(struct bpf_flow_keys *) */
	__u64 tstamp;          /* 152 */
	__u32 wire_len;        /* 160 */
	__u32 gso_segs;        /* 164 */
};

_Static_assert(__builtin_offsetof(struct __sk_buff, len) == 0, "__sk_buff.len");
_Static_assert(__builtin_offsetof(struct __sk_buff, flow_keys) == 144, "__sk_buff.flow_keys");
_Static_assert(__builtin_offsetof(struct __sk_buff, gso_segs) == 164, "__sk_buff.gso_segs");

/*
 * include/uapi/linux/bpf.h: struct bpf_pidns_info, filled by
 * bpf_get_ns_current_pid_tgid() (Linux 5.7+).
 */
struct bpf_pidns_info {
	__u32 pid;
	__u32 tgid;
};

#endif /* KH_UAPI_H */
