/* SPDX-License-Identifier: (GPL-2.0-only OR Apache-2.0) */
/* Copyright (c) KubeHero contributors */

/*
 * profiler: whole-node on-CPU sampling.
 *
 * Attached to one PERF_COUNT_SW_CPU_CLOCK perf event per online CPU. Each
 * sample records (cgroup id, tgid, user stack id, kernel stack id) -> count.
 * Stacks are deduplicated by the kernel in a STACK_TRACE map; userspace
 * symbolizes them.
 *
 * Nested PID namespaces (kind, k3d, Docker Desktop: every "node" is a
 * container sharing one kernel): bpf_get_current_pid_tgid() returns PIDs
 * of the kernel's top-level namespace, which the collector can't see in
 * its /proc. With ns_mode set, userspace fills kh_pidns with the PID
 * namespace of each container cgroup it can see, and samples record the
 * process's tgid inside that namespace instead
 * (bpf_get_ns_current_pid_tgid). Samples from cgroups without an entry
 * - another node's containers, or one started since the last refresh -
 * are dropped before any stack is taken.
 *
 * Double buffering: every map exists twice and kh_active selects the set
 * samples go to. Userspace flips the selector, waits for in-flight samples
 * to finish, then reads and clears the idle set at its leisure. Without
 * this, clearing a stack map while samples keep landing races: a stack id
 * read by a fresh count could be deleted, or worse, recycled for a
 * different stack before the next drain.
 */

#include "kh_uapi.h"
#include <bpf/bpf_helpers.h>

char LICENSE[] SEC("license") = "GPL";

/* PERF_MAX_STACK_DEPTH; the value is an array of instruction pointers. */
#define KH_STACK_DEPTH 127

/* Key layout mirrored by stackKey in profile.go. pad must stay zero. */
struct stack_key {
	__u64 cgroup_id;
	__u32 tgid;
	__s32 user_stack_id;   /* < 0: no user stack (kernel thread, error) */
	__s32 kernel_stack_id; /* < 0: sample hit user mode */
	__u32 pad;
};

_Static_assert(sizeof(struct stack_key) == 24, "stack_key must be 24 bytes");

#define KH_COUNTS_MAP                                   \
	struct {                                        \
		__uint(type, BPF_MAP_TYPE_HASH);        \
		__uint(max_entries, 16384);             \
		__type(key, struct stack_key);          \
		__type(value, __u64);                   \
	}

/*
 * The kernel indexes a STACK_TRACE map by hash & (roundup_pow_of_two(
 * max_entries) - 1) but preallocates only max_entries ~1 KiB buckets, and a
 * new stack whose bucket is taken is dropped (-EEXIST). 2^13 + 1 entries
 * buys 2^14 buckets - half the collisions of 8192 - for ~8.5 MiB per map.
 */
#define KH_STACKS_MAP                                              \
	struct {                                                   \
		__uint(type, BPF_MAP_TYPE_STACK_TRACE);            \
		__uint(max_entries, 8193);                         \
		__uint(key_size, sizeof(__u32));                   \
		__uint(value_size, KH_STACK_DEPTH * sizeof(__u64)); \
	}

KH_COUNTS_MAP kh_counts0 SEC(".maps");
KH_COUNTS_MAP kh_counts1 SEC(".maps");
KH_STACKS_MAP kh_stacks0 SEC(".maps");
KH_STACKS_MAP kh_stacks1 SEC(".maps");

/* Index 0 holds the active buffer (0 or 1). */
struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, __u32);
} kh_active SEC(".maps");

#define KH_PROF_ERR_COUNTS_FULL 0 /* sample lost: counts map full */
#define KH_PROF_ERR_STACK_LOST 1  /* stack lost: bucket collision or map full */
#define KH_PROF_ERR_NO_PIDNS 2    /* ns_mode: cgroup not in kh_pidns (not ours, or new) */
#define KH_PROF_ERR_NS_PID 3      /* ns_mode: task not in its cgroup's namespace */
#define KH_PROF_ERR_MAX 4

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, KH_PROF_ERR_MAX);
	__type(key, __u32);
	__type(value, __u64);
} kh_prof_errors SEC(".maps");

/* The collector's own tgid (host PID namespace); never sampled. */
volatile const __u32 self_tgid = 0;

/* Set by the loader when the collector runs in a nested PID namespace. */
volatile const __u8 ns_mode = 0;

/* Key: cgroup id. Value: the PID namespace of that cgroup's processes,
 * as stat(2) reports /proc/<pid>/ns/pid. Filled by userspace. */
struct kh_pidns_id {
	__u64 dev;
	__u64 ino;
};

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 8192);
	__type(key, __u64);
	__type(value, struct kh_pidns_id);
} kh_pidns SEC(".maps");

static __always_inline void kh_prof_error(__u32 idx)
{
	__u64 *v = bpf_map_lookup_elem(&kh_prof_errors, &idx);

	if (v)
		*v += 1;
}

/* Inlined per call site so the verifier sees a constant map pointer. */
static __always_inline void kh_count(void *counts, struct stack_key *key)
{
	__u64 *cnt = bpf_map_lookup_elem(counts, key);

	if (cnt) {
		__sync_fetch_and_add(cnt, 1);
		return;
	}

	__u64 one = 1;
	long err = bpf_map_update_elem(counts, key, &one, BPF_NOEXIST);

	if (err == -KH_EEXIST) {
		cnt = bpf_map_lookup_elem(counts, key);
		if (cnt) {
			__sync_fetch_and_add(cnt, 1);
			return;
		}
	}
	if (err)
		kh_prof_error(KH_PROF_ERR_COUNTS_FULL);
}

/* -EFAULT (no user stack / user-mode sample) is normal and not counted. */
static __always_inline int kh_stack_lost(__s32 id)
{
	return id == -KH_EEXIST || id == -KH_ENOMEM;
}

static __always_inline void kh_note_lost_stacks(const struct stack_key *key)
{
	if (kh_stack_lost(key->user_stack_id) || kh_stack_lost(key->kernel_stack_id))
		kh_prof_error(KH_PROF_ERR_STACK_LOST);
}

SEC("perf_event")
int kh_profile(void *ctx)
{
	__u64 id = bpf_get_current_pid_tgid();
	__u32 tgid = id >> 32;
	__u32 zero = 0;

	if ((__u32)id == 0) /* the idle task */
		return 0;
	if (tgid == self_tgid)
		return 0;

	__u64 cgroup_id = bpf_get_current_cgroup_id();
	if (ns_mode) {
		struct kh_pidns_id *ns = bpf_map_lookup_elem(&kh_pidns, &cgroup_id);
		if (!ns) {
			kh_prof_error(KH_PROF_ERR_NO_PIDNS);
			return 0;
		}
		struct bpf_pidns_info info = {};
		if (bpf_get_ns_current_pid_tgid(ns->dev, ns->ino, &info, sizeof(info))) {
			kh_prof_error(KH_PROF_ERR_NS_PID);
			return 0;
		}
		tgid = info.tgid;
	}

	struct stack_key key;

	__builtin_memset(&key, 0, sizeof(key));
	key.cgroup_id = cgroup_id;
	key.tgid = tgid;

	__u32 *active = bpf_map_lookup_elem(&kh_active, &zero);

	if (active && *active) {
		key.user_stack_id = bpf_get_stackid(ctx, &kh_stacks1, BPF_F_USER_STACK);
		key.kernel_stack_id = bpf_get_stackid(ctx, &kh_stacks1, 0);
		kh_note_lost_stacks(&key);
		kh_count(&kh_counts1, &key);
	} else {
		key.user_stack_id = bpf_get_stackid(ctx, &kh_stacks0, BPF_F_USER_STACK);
		key.kernel_stack_id = bpf_get_stackid(ctx, &kh_stacks0, 0);
		kh_note_lost_stacks(&key);
		kh_count(&kh_counts0, &key);
	}
	return 0;
}
