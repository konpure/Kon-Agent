#!/bin/bash

# scripts/build_ebpf.sh

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

# BPF 源码独立于 Go 包目录（避免 cgo 用宿主编译器误编译），产物仍输出到包目录下
EBPF_SRC=internal/plugin/network/ebpf/bpf
EBPF_OUT=internal/plugin/network/ebpf/output

mkdir -p $EBPF_OUT

# vmlinux.h 体积较大（MB 级）且与运行内核绑定，不纳入版本管理（见 .gitignore）；
# 仅在缺失或显式删除后重新生成
if [ ! -f $EBPF_SRC/vmlinux.h ]; then
    echo "Generating vmlinux.h from kernel BTF..."
    bpftool btf dump file /sys/kernel/btf/vmlinux format c > $EBPF_SRC/vmlinux.h
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

# 编译 eBPF 程序，-g 生成 BTF 调试信息供 CO-RE 重定位
echo "Compiling eBPF program with CO-RE support (target arch: $BPF_ARCH)..."
clang -g -O2 -target bpf -D__TARGET_ARCH_${BPF_ARCH} -I $EBPF_SRC \
    -c $EBPF_SRC/network_monitor.c -o $EBPF_OUT/network_monitor.o

echo "eBPF program compiled successfully!"
echo "Output: $EBPF_OUT/network_monitor.o"
