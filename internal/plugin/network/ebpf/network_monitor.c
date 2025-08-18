// internal/plugin/network/ebpf/network_monitor.c
#include <linux/bpf.h>
#include <bpf/bpf_helpers.h>
#include <linux/if_ether.h>
#include <linux/ip.h>

// 定义一个map来存储数据包计数，名称与Go代码中引用的名称一致
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 256);
    __type(key, __u32);
    __type(value, __u64);
} pkt_count SEC(".maps");

// eBPF程序：跟踪网络数据包
SEC("xdp")
int track_packets(struct xdp_md *ctx) {
    __u32 key = 0;  // 简化示例，使用固定键值
    __u64 *count;

    // 查找当前计数
    count = bpf_map_lookup_elem(&pkt_count, &key);
    if (count) {
        // 增加计数
        __sync_fetch_and_add(count, 1);
    } else {
        // 初始化计数
        __u64 init_val = 1;
        bpf_map_update_elem(&pkt_count, &key, &init_val, BPF_ANY);
    }

    return XDP_PASS;
}

// 许可证声明（必须）
char _license[] SEC("license") = "GPL";
