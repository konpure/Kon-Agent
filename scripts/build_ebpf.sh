#!/bin/bash

# scripts/build_ebpf.sh
# Builds all eBPF programs (network monitor, block IO latency, ...).

set -e

# 检查是否安装了 clang
if ! command -v clang &> /dev/null; then
    echo "Error: clang is not installed"
    exit 1
fi

# CO-RE 需要 bpftool 从内核 BTF 生成 vmlinux.h
if ! command -v bpftool &> /dev/null; then
    echo "Error: bpftool is not installed (required to generate vmlinux.h for CO-RE)"
    exit 1
fi

if [ ! -f /sys/kernel/btf/vmlinux ]; then
    echo "Error: kernel BTF not found at /sys/kernel/btf/vmlinux"
    exit 1
fi

# bpf_tracing.h 的 PT_REGS_PARM1 等宏需要 __TARGET_ARCH_xxx 选定寄存器布局；
# 按主机架构自动映射（边缘设备常见 arm64）
ARCH=$(uname -m)
case "$ARCH" in
    x86_64)  BPF_ARCH=x86 ;;
    aarch64) BPF_ARCH=arm64 ;;
    armv7l)  BPF_ARCH=arm ;;
    ppc64le) BPF_ARCH=powerpc ;;
    s390x)   BPF_ARCH=s390 ;;
    riscv64) BPF_ARCH=riscv ;;
    *)
        echo "Error: unsupported architecture for CO-RE: $ARCH"
        exit 1
        ;;
esac

# vmlinux.h 为所有 BPF 程序共享，体积大且与运行内核绑定，不入版本管理
SHARED_INC=internal/plugin/bpf
mkdir -p $SHARED_INC
if [ ! -f $SHARED_INC/vmlinux.h ]; then
    echo "Generating vmlinux.h from kernel BTF..."
    bpftool btf dump file /sys/kernel/btf/vmlinux format c > $SHARED_INC/vmlinux.h
fi

build_prog() {
    local src=$1
    local out=$2
    mkdir -p "$(dirname "$out")"
    echo "Compiling $src (target arch: $BPF_ARCH)..."
    # -g 生成 BTF 调试信息供 CO-RE 重定位
    clang -g -O2 -target bpf -D__TARGET_ARCH_${BPF_ARCH} -I $SHARED_INC \
        -c "$src" -o "$out"
}

build_prog internal/plugin/network/ebpf/bpf/network_monitor.c \
           internal/plugin/network/ebpf/output/network_monitor.o

build_prog internal/plugin/block/ebpf/bpf/block_io.c \
           internal/plugin/block/ebpf/output/block_io.o

echo "All eBPF programs compiled successfully!"
