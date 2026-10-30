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
#include <bpf/bpf_endian.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

// ---------------------------------------------------------------------------
// Packet flow stats (golden signal: traffic).
//
// pkt_stats is keyed by (ifindex, direction, l4proto) and holds
// packets+bytes per series. Ingress is parsed at XDP (eth/IP headers);
// egress uses a kprobe on dev_queue_xmit, which sees the egress ifindex but
// no parsed headers, so its l4proto stays 0 ("all").
//
// (A TC clsact program would be the cleaner egress hook — no function
// signature dependence — but it needs netlink qdisc management; deferred.)
// ---------------------------------------------------------------------------
#define DIR_INGRESS 0
#define DIR_EGRESS 1

#define PROTO_ALL 0 // unparsed (egress kprobe) or non-IP

struct pkt_key {
  __u32 ifindex;
  __u8 direction;
  __u8 l4proto;
  __u16 pad;
};

struct pkt_val {
  __u64 packets;
  __u64 bytes;
};

struct {
  __uint(type, BPF_MAP_TYPE_PERCPU_HASH);
  __uint(max_entries, 1024);
  __type(key, struct pkt_key);
  __type(value, struct pkt_val);
} pkt_stats SEC(".maps");

// Packet size histogram (ingress only): bucket i covers
// (bounds[i-1], bounds[i]] bytes with bounds = 64..32768 (log2).
// Key 0..10 are buckets; 100 = sample count; 101 = bytes sum.
#define SIZE_BUCKET_OVERFLOW 10
#define SIZE_KEY_COUNT 100
#define SIZE_KEY_SUM 101

struct {
  __uint(type, BPF_MAP_TYPE_PERCPU_HASH);
  __uint(max_entries, 16);
  __type(key, __u32);
  __type(value, __u64);
} pkt_size SEC(".maps");

// Fixed-comparison bucket assignment (no clzll — it crashes the LLVM 14 BPF
// backend; see block_io.c for the same note).
static __always_inline __u32 size_slot(__u32 b) {
  if (b <= 64)
    return 0;
  if (b <= 128)
    return 1;
  if (b <= 256)
    return 2;
  if (b <= 512)
    return 3;
  if (b <= 1024)
    return 4;
  if (b <= 2048)
    return 5;
  if (b <= 4096)
    return 6;
  if (b <= 8192)
    return 7;
  if (b <= 16384)
    return 8;
  if (b <= 32768)
    return 9;
  return SIZE_BUCKET_OVERFLOW;
}

// Per-CPU maps: each CPU owns its copy, so plain increments are race-free
// (eBPF programs are non-preemptible) and there is no cross-core cacheline
// contention — no atomics needed on the hot path.
static __always_inline void pkt_stats_add(__u32 ifindex, __u8 direction,
                                          __u8 l4proto, __u32 bytes) {
  struct pkt_key key = {
      .ifindex = ifindex,
      .direction = direction,
      .l4proto = l4proto,
      .pad = 0,
  };
  struct pkt_val *v = bpf_map_lookup_elem(&pkt_stats, &key);
  if (v) {
    v->packets += 1;
    v->bytes += bytes;
  } else {
    struct pkt_val init = {.packets = 1, .bytes = bytes};
    bpf_map_update_elem(&pkt_stats, &key, &init, BPF_ANY);
  }
}

static __always_inline void size_incr(__u32 key, __u64 delta) {
  __u64 *v = bpf_map_lookup_elem(&pkt_size, &key);
  if (v) {
    *v += delta;
  } else {
    bpf_map_update_elem(&pkt_size, &key, &delta, BPF_ANY);
  }
}

SEC("xdp")
int track_packets(struct xdp_md *ctx) {
  void *data = (void *)(long)ctx->data;
  void *data_end = (void *)(long)ctx->data_end;
  __u32 len = (__u32)(data_end - data);
  __u32 ifindex = ctx->ingress_ifindex;

  __u8 l4proto = PROTO_ALL;
  struct ethhdr *eth = data;
  if ((void *)(eth + 1) <= data_end) {
    __u16 h_proto = bpf_ntohs(eth->h_proto);
    if (h_proto == 0x0800) { // IPv4
      struct iphdr *ip = (void *)(eth + 1);
      if ((void *)(ip + 1) <= data_end) {
        l4proto = ip->protocol;
      }
    } else if (h_proto == 0x86DD) { // IPv6
      struct ipv6hdr *ip6 = (void *)(eth + 1);
      if ((void *)(ip6 + 1) <= data_end) {
        l4proto = ip6->nexthdr;
      }
    }
  }

  pkt_stats_add(ifindex, DIR_INGRESS, l4proto, len);

  size_incr(size_slot(len), 1);
  size_incr(SIZE_KEY_COUNT, 1);
  size_incr(SIZE_KEY_SUM, len);
  return XDP_PASS;
}

// Egress hook: dev_queue_xmit sees the packet as it is handed to the driver.
SEC("kprobe/dev_queue_xmit")
int dev_egress(struct pt_regs *ctx) {
  struct sk_buff *skb = (struct sk_buff *)PT_REGS_PARM1(ctx);
  __u32 len = BPF_CORE_READ(skb, len);
  struct net_device *dev = BPF_CORE_READ(skb, dev);
  if (!dev) {
    return 0;
  }
  __u32 ifindex = BPF_CORE_READ(dev, ifindex);
  pkt_stats_add(ifindex, DIR_EGRESS, PROTO_ALL, len);
  return 0;
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

// Per-cgroup RTT attribution was reverted: bpf_sk_cgroup_id is not available
// to tracing programs on Ubuntu 5.15 generic (verifier: "unknown func
// bpf_sk_cgroup_id#128" at load). Plain bucket keys; attribution is future
// work (newer kernels or sock_ops programs).
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

static __always_inline void record_rtt(struct sock *sk) {
  struct tcp_sock *tp = (struct tcp_sock *)sk;

  // srtt_us is the kernel's smoothed RTT estimator, stored scaled by 8.
  __u64 rtt_us = (__u64)BPF_CORE_READ(tp, srtt_us) >> 3;
  if (rtt_us == 0) {
    return;
  }

  rtt_incr(rtt_bucket(rtt_us), 1);
  rtt_incr(RTT_KEY_COUNT, 1);
  rtt_incr(RTT_KEY_SUM, rtt_us);
}

SEC("kprobe/tcp_rcv_established")
int tcp_rtt(struct pt_regs *ctx) {
  record_rtt((struct sock *)PT_REGS_PARM1(ctx));
  return 0;
}

// fentry variant: typed args via BTF, no pt_regs layout dependence.
SEC("fentry/tcp_rcv_established")
int BPF_PROG(tcp_rtt_fentry, struct sock *sk) {
  record_rtt(sk);
  return 0;
}

// ---------------------------------------------------------------------------
// TCP connection health (SRE golden signal: errors).
//
// One small map with three keys:
//   key 0: retransmissions since last drain   (read-then-clear -> Sum delta)
//   key 1: connections established since drain (read-then-clear -> Sum delta)
//   key 2: currently established connections  (read WITHOUT clear -> Gauge)
//
// Caveat: key 2 starts at 0 when the agent loads, so connections established
// before startup are invisible until they close — the gauge is a trend
// signal, not an absolute census.
// ---------------------------------------------------------------------------
struct {
  __uint(type, BPF_MAP_TYPE_HASH);
  __uint(max_entries, 8);
  __type(key, __u32);
  __type(value, __u64);
} tcp_health SEC(".maps");

#define TCP_HEALTH_RETRANS 0
#define TCP_HEALTH_ESTABLISHED_DELTA 1
#define TCP_HEALTH_ESTABLISHED_CUR 2

// enum tcp_state values (defined locally to avoid BTF enum dependence).
#define TCP_ST_ESTABLISHED 1
#define TCP_ST_CLOSE 7

static __always_inline void tcp_health_add(__u32 key, __s64 delta) {
  __u64 *v = bpf_map_lookup_elem(&tcp_health, &key);
  if (v) {
    if (delta >= 0) {
      __sync_fetch_and_add(v, (__u64)delta);
    } else if (*v >= (__u64)(-delta)) {
      __sync_fetch_and_sub(v, (__u64)(-delta));
    } else {
      *v = 0; // clamp: never report a negative gauge
    }
  } else if (delta >= 0) {
    __u64 init = (__u64)delta;
    bpf_map_update_elem(&tcp_health, &key, &init, BPF_ANY);
  }
}

SEC("tracepoint/tcp/tcp_retransmit_skb")
int tcp_retrans(void *ctx) {
  tcp_health_add(TCP_HEALTH_RETRANS, 1);
  return 0;
}

// Minimal tracepoint arg layout for sock:inet_sock_set_state; only the two
// state fields are read, but offsets must match the kernel format.
struct set_state_args {
  __u64 _common; // common_type/flags/preempt_count/pid
  const void *skaddr;
  __s32 oldstate;
  __s32 newstate;
};

SEC("tracepoint/sock/inet_sock_set_state")
int tcp_set_state(struct set_state_args *ctx) {
  if (ctx->newstate == TCP_ST_ESTABLISHED) {
    tcp_health_add(TCP_HEALTH_ESTABLISHED_DELTA, 1);
    tcp_health_add(TCP_HEALTH_ESTABLISHED_CUR, 1);
  } else if (ctx->oldstate == TCP_ST_ESTABLISHED) {
    tcp_health_add(TCP_HEALTH_ESTABLISHED_CUR, -1);
  }
  return 0;
}

// ---------------------------------------------------------------------------
// Packet drop event channel (ring buffer).
//
// Drops are rare, so this channel is event-driven (ringbuf) instead of the
// polling pattern used for counters — the polling-vs-event-driven contrast
// is deliberate. The drop reason field only exists on kernel 5.17+; its
// offset is probed from the tracepoint format file by userspace and passed
// through drop_cfg (0xFFFFFFFF = absent on this kernel).
// ---------------------------------------------------------------------------
struct drop_event {
  __u32 reason;
  __u16 protocol;
  __u8 pad[2];
  char comm[16];
};

struct {
  __uint(type, BPF_MAP_TYPE_RINGBUF);
  __uint(max_entries, 256 * 1024);
} drop_events SEC(".maps");

struct {
  __uint(type, BPF_MAP_TYPE_ARRAY);
  __uint(max_entries, 1);
  __type(key, __u32);
  __type(value, __u32); // 1 = the reason field exists at DROP_REASON_OFF
} drop_cfg SEC(".maps");

// The verifier requires constant ctx offsets, so the field offset cannot be
// passed through the map; userspace probes the format file and only sets the
// flag when reason sits at the layout's fixed position (protocol at 24,
// 2 bytes padding, reason at 28 on 5.17+).
#define DROP_REASON_OFF 28

SEC("tracepoint/skb/kfree_skb")
int on_kfree_skb(void *ctx) {
  struct drop_event *e = bpf_ringbuf_reserve(&drop_events, sizeof(*e), 0);
  if (!e) {
    return 0;
  }
  // protocol is at offset 24 in the tracepoint record (all kernels).
  e->protocol = *(__u16 *)((char *)ctx + 24);
  bpf_get_current_comm(&e->comm, sizeof(e->comm));

  __u32 zero = 0;
  __u32 *has = bpf_map_lookup_elem(&drop_cfg, &zero);
  if (has && *has == 1) {
    e->reason = *(__u32 *)((char *)ctx + DROP_REASON_OFF);
  } else {
    e->reason = 0; // unknown on this kernel
  }
  bpf_ringbuf_submit(e, 0);
  return 0;
}

// License declaration (required)
char _license[] SEC("license") = "GPL";
