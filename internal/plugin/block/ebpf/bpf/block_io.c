// internal/plugin/block/ebpf/bpf/block_io.c
//
// Block-device IO latency distribution — the metric /proc/diskstats cannot
// provide (it only has throughput and weighted time). Pattern follows bcc's
// biolatency: timestamp at issue, compute delta at completion, aggregate into
// a log2 histogram IN KERNEL. Userspace only drains summaries
// (read-then-clear), which is the edge pre-aggregation story.
//
// Hooks are raw tracepoints: their argument layout is the tracepoint ABI,
// which is stable across kernel versions (unlike kprobe signatures).
//
// Complements the /proc-based disk plugin: throughput from /proc (cheaper),
// latency distribution from eBPF (unavailable in /proc).

#include "vmlinux.h"
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

// REQ_OP_MASK is a macro (not in BTF); enum values like REQ_OP_WRITE are.
#define REQ_OP_MASK 0xff

// In-flight requests: request pointer -> issue timestamp (ns).
struct {
  __uint(type, BPF_MAP_TYPE_HASH);
  __uint(max_entries, 10240);
  __type(key, struct request *);
  __type(value, __u64);
} io_start SEC(".maps");

// Latency histogram + totals, key layout (u32):
//   0..27    : log2 latency buckets (microseconds), reads
//   64..91   : same buckets, writes
//   100/101  : sample count, read / write
//   110/111  : latency sum (microseconds), read / write
// Keep in sync with the Go side (tracer.go).
struct {
  __uint(type, BPF_MAP_TYPE_HASH);
  __uint(max_entries, 256);
  __type(key, __u32);
  __type(value, __u64);
} io_latency SEC(".maps");

// Per-device completed IO counter. Key: devt = (major << 20) | minor.
struct {
  __uint(type, BPF_MAP_TYPE_HASH);
  __uint(max_entries, 256);
  __type(key, __u64);
  __type(value, __u64);
} io_completed SEC(".maps");

static __always_inline void map_incr(void *map, __u32 key, __u64 delta) {
  __u64 *v = bpf_map_lookup_elem(map, &key);
  if (v) {
    __sync_fetch_and_add(v, delta);
  } else {
    bpf_map_update_elem(map, &key, &delta, BPF_ANY);
  }
}

// log2 slot for a microsecond value: slot i holds (2^(i-1), 2^i] us.
// NB: no __builtin_clzll — it crashes the LLVM 14 BPF backend in DAG
// instruction selection (no native CTLZ on BPF). The shift ladder is also
// verifier-friendly: fixed instruction count, no loops.
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
  if (r > 27) {
    r = 27;
  }
  return r;
}

SEC("raw_tracepoint/block_rq_issue")
int block_rq_issue(struct bpf_raw_tracepoint_args *ctx) {
  // tracepoint block_rq_issue(struct request_queue *q, struct request *rq)
  struct request *rq = (struct request *)ctx->args[1];
  __u64 ts = bpf_ktime_get_ns();
  bpf_map_update_elem(&io_start, &rq, &ts, BPF_ANY);
  return 0;
}

// kprobe fallback for kernels where the raw tracepoint cannot be attached
// (observed on Ubuntu 5.15 generic: BPF_RAW_TRACEPOINT_OPEN returns EINVAL
// even though the tracepoint exists in available_events).
SEC("kprobe/blk_mq_start_request")
int blk_start(struct pt_regs *ctx) {
  struct request *rq = (struct request *)PT_REGS_PARM1(ctx);
  __u64 ts = bpf_ktime_get_ns();
  bpf_map_update_elem(&io_start, &rq, &ts, BPF_ANY);
  return 0;
}

// Shared completion logic between the raw tracepoint and kprobe variants.
static __always_inline void complete_rq(struct request *rq) {
  __u64 *tsp = bpf_map_lookup_elem(&io_start, &rq);
  if (!tsp) {
    return; // we did not see the issue (e.g. agent started mid-flight)
  }
  __u64 lat_ns = bpf_ktime_get_ns() - *tsp;
  bpf_map_delete_elem(&io_start, &rq);

  __u64 lat_us = lat_ns / 1000;
  __u32 cmd_flags = BPF_CORE_READ(rq, cmd_flags);
  __u32 rw = (cmd_flags & REQ_OP_MASK) == REQ_OP_WRITE ? 1 : 0;

  __u32 slot = log2_slot(lat_us);
  map_incr(&io_latency, slot + rw * 64, 1);
  map_incr(&io_latency, 100 + rw, 1);
  map_incr(&io_latency, 110 + rw, lat_us);

  __u32 major = BPF_CORE_READ(rq, rq_disk, major);
  __u32 minor = BPF_CORE_READ(rq, rq_disk, first_minor);
  __u64 devt = ((__u64)major << 20) | minor;
  __u64 *c = bpf_map_lookup_elem(&io_completed, &devt);
  if (c) {
    __sync_fetch_and_add(c, 1);
  } else {
    __u64 one = 1;
    bpf_map_update_elem(&io_completed, &devt, &one, BPF_ANY);
  }
}

SEC("raw_tracepoint/block_rq_complete")
int block_rq_complete(struct bpf_raw_tracepoint_args *ctx) {
  // tracepoint block_rq_complete(struct request *rq, blk_status_t error,
  // unsigned int nr_bytes)
  struct request *rq = (struct request *)ctx->args[0];
  complete_rq(rq);
  return 0;
}

SEC("kprobe/blk_mq_end_request")
int blk_done(struct pt_regs *ctx) {
  struct request *rq = (struct request *)PT_REGS_PARM1(ctx);
  complete_rq(rq);
  return 0;
}

char _license[] SEC("license") = "GPL";
