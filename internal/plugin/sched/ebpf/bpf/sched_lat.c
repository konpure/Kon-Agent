// internal/plugin/sched/ebpf/bpf/sched_lat.c
//
// CPU runqueue wait latency (golden signal: saturation). High CPU utilization
// (from /proc) does not imply scheduler saturation; the time a runnable task
// waits for a CPU is the real signal. Pattern follows bcc's runqlat:
// timestamp at wakeup, compute the delta at the context switch, bucket in
// kernel.
//
// wake_ts is a plain HASH on purpose: a task can be woken on CPU A and
// scheduled on CPU B, so the correlation map must be shared across CPUs.
// Only the output histogram is per-CPU (hot path, no correlation needed).

#include "vmlinux.h"
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

// Wake timestamps: pid -> wake time (ns). Entries are deleted when the task
// is scheduled in; a task that never runs leaves a stale entry, which is
// bounded by the number of tasks and harmless.
struct {
  __uint(type, BPF_MAP_TYPE_HASH);
  __uint(max_entries, 10240);
  __type(key, __u32);
  __type(value, __u64);
} wake_ts SEC(".maps");

// Runqueue wait histogram, key layout (u32), keep in sync with tracer.go:
//   0..21 : log2 buckets (microseconds), 1us .. ~1s
//   100   : sample count
//   101   : sum (microseconds)
//   102   : total context switches (regardless of wake correlation)
struct {
  __uint(type, BPF_MAP_TYPE_PERCPU_HASH);
  __uint(max_entries, 64);
  __type(key, __u32);
  __type(value, __u64);
} runq_wait SEC(".maps");

#define RUNQ_KEY_COUNT 100
#define RUNQ_KEY_SUM 101
#define RUNQ_KEY_SWITCHES 102

// Per-CPU map: plain increments are race-free (eBPF is non-preemptible).
static __always_inline void runq_incr(__u32 key, __u64 delta) {
  __u64 *v = bpf_map_lookup_elem(&runq_wait, &key);
  if (v) {
    *v += delta;
  } else {
    bpf_map_update_elem(&runq_wait, &key, &delta, BPF_ANY);
  }
}

// log2 slot via shift ladder (no clzll — crashes the LLVM 14 BPF backend).
static __always_inline __u32 log2_slot(__u64 v) {
  __u32 r = 0;
  if (v == 0) {
    return 0;
  }
  if (v >= (1ULL << 32)) {
    v >>= 32;
    r += 32;
  }
  if (v >= (1ULL << 16)) {
    v >>= 16;
    r += 16;
  }
  if (v >= (1ULL << 8)) {
    v >>= 8;
    r += 8;
  }
  if (v >= (1ULL << 4)) {
    v >>= 4;
    r += 4;
  }
  if (v >= (1ULL << 2)) {
    v >>= 2;
    r += 2;
  }
  if (v >= (1ULL << 1)) {
    r += 1;
  }
  if (r > 21) {
    r = 21;
  }
  return r;
}

// sched:sched_wakeup / sched:sched_wakeup_new arg layout (offsets must match
// the kernel tracepoint format; only pid is read).
struct sched_wakeup_args {
  __u64 _common; // common_type/flags/preempt_count/pid
  char comm[16];
  __s32 pid;
  __s32 prio;
  __s32 success;
  __s32 target_cpu;
};

static __always_inline void record_wake(__s32 pid) {
  if (pid == 0) {
    return; // idle task
  }
  __u32 key = (__u32)pid;
  __u64 ts = bpf_ktime_get_ns();
  bpf_map_update_elem(&wake_ts, &key, &ts, BPF_ANY);
}

SEC("tracepoint/sched/sched_wakeup")
int on_wakeup(struct sched_wakeup_args *ctx) {
  record_wake(ctx->pid);
  return 0;
}

SEC("tracepoint/sched/sched_wakeup_new")
int on_wakeup_new(struct sched_wakeup_args *ctx) {
  record_wake(ctx->pid);
  return 0;
}

// sched:sched_switch arg layout; only next_pid is read.
struct sched_switch_args {
  __u64 _common;
  char prev_comm[16];
  __s32 prev_pid;
  __s32 prev_prio;
  __s64 prev_state;
  char next_comm[16];
  __s32 next_pid;
  __s32 next_prio;
};

SEC("tracepoint/sched/sched_switch")
int on_switch(struct sched_switch_args *ctx) {
  runq_incr(RUNQ_KEY_SWITCHES, 1);

  __u32 pid = (__u32)ctx->next_pid;
  if (pid == 0) {
    return 0; // idle task
  }
  __u64 *tsp = bpf_map_lookup_elem(&wake_ts, &pid);
  if (!tsp) {
    return 0; // woken before agent start, or woken by an untracked path
  }
  __u64 delta_ns = bpf_ktime_get_ns() - *tsp;
  bpf_map_delete_elem(&wake_ts, &pid);

  __u64 delta_us = delta_ns / 1000;
  runq_incr(log2_slot(delta_us), 1);
  runq_incr(RUNQ_KEY_COUNT, 1);
  runq_incr(RUNQ_KEY_SUM, delta_us);
  return 0;
}

char _license[] SEC("license") = "GPL";
