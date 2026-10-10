// internal/plugin/network/ebpf/bpf/network_monitor.c
//
// NOTE: this file lives OUTSIDE the Go package directory on purpose.
// cgo compiles every .c file found in the package dir with the host C
// compiler, which cannot handle kernel/BTF headers (and the MB-sized
// vmlinux.h). Keeping the BPF sources here means only scripts/build_ebpf.sh
// (clang -target bpf) ever compiles them.
//
// Kernel-side pre-aggregation (Hubble/Monarch style): the kernel maintains
// aggregated maps, and userspace periodically reads and clears them
// (read-then-clear -> delta temporality on the wire).
//
//   1) pkt_count: per-interface packet counter (XDP)
//   2) rtt_stats: TCP RTT histogram + sample count + sum
//      (kprobe on tcp_rcv_established, reading the kernel's smoothed RTT
//      estimator srtt_us)
//
// CO-RE: kernel structs come from vmlinux.h (generated from kernel BTF by
// scripts/build_ebpf.sh), so the program is portable across kernel versions.

#include "vmlinux.h"
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

// ---------------------------------------------------------------------------
// Per-interface packet counter.
// ---------------------------------------------------------------------------
struct {
  __uint(type, BPF_MAP_TYPE_HASH);
  __uint(max_entries, 256);
  __type(key, __u32);   // ifindex
  __type(value, __u64); // packets seen
} pkt_count SEC(".maps");

SEC("xdp")
int track_packets(struct xdp_md *ctx) {
  __u32 key = ctx->ingress_ifindex;
  __u64 *count = bpf_map_lookup_elem(&pkt_count, &key);
  if (count) {
    __sync_fetch_and_add(count, 1);
  } else {
    __u64 init_val = 1;
    bpf_map_update_elem(&pkt_count, &key, &init_val, BPF_ANY);
  }
  return XDP_PASS;
}

// ---------------------------------------------------------------------------
// TCP RTT histogram.
//
// Bucket layout (microseconds), keep in sync with rttBoundsSeconds on the
// Go side (tracer.go):
//   bounds (s): 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5
//   key 0..9 : bucket counters, key i counts (bound[i-1], bound[i]]
//   key 10   : overflow bucket (> 2.5s)
//   key 100  : total sample count
//   key 101  : total RTT sum (microseconds)
// ---------------------------------------------------------------------------
#define RTT_BUCKET_OVERFLOW 10
#define RTT_KEY_COUNT 100
#define RTT_KEY_SUM 101

struct {
  __uint(type, BPF_MAP_TYPE_HASH);
  __uint(max_entries, 128);
  __type(key, __u32);
  __type(value, __u64);
} rtt_stats SEC(".maps");

static __always_inline void rtt_incr(__u32 key, __u64 delta) {
  __u64 *v = bpf_map_lookup_elem(&rtt_stats, &key);
  if (v) {
    __sync_fetch_and_add(v, delta);
  } else {
    bpf_map_update_elem(&rtt_stats, &key, &delta, BPF_ANY);
  }
}

static __always_inline __u32 rtt_bucket(__u64 rtt_us) {
  if (rtt_us < 1000)
    return 0; // < 1ms
  if (rtt_us < 5000)
    return 1; // < 5ms
  if (rtt_us < 10000)
    return 2; // < 10ms
  if (rtt_us < 25000)
    return 3; // < 25ms
  if (rtt_us < 50000)
    return 4; // < 50ms
  if (rtt_us < 100000)
    return 5; // < 100ms
  if (rtt_us < 250000)
    return 6; // < 250ms
  if (rtt_us < 500000)
    return 7; // < 500ms
  if (rtt_us < 1000000)
    return 8; // < 1s
  if (rtt_us < 2500000)
    return 9; // < 2.5s
  return RTT_BUCKET_OVERFLOW;
}

SEC("kprobe/tcp_rcv_established")
int tcp_rtt(struct pt_regs *ctx) {
  struct sock *sk = (struct sock *)PT_REGS_PARM1(ctx);
  struct tcp_sock *tp = (struct tcp_sock *)sk;

  // srtt_us is the kernel's smoothed RTT estimator, stored scaled by 8.
  __u64 rtt_us = (__u64)BPF_CORE_READ(tp, srtt_us) >> 3;
  if (rtt_us == 0) {
    return 0;
  }

  rtt_incr(rtt_bucket(rtt_us), 1);
  rtt_incr(RTT_KEY_COUNT, 1);
  rtt_incr(RTT_KEY_SUM, rtt_us);
  return 0;
}

// License declaration (required)
char _license[] SEC("license") = "GPL";
