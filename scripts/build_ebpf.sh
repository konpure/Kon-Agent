#!/bin/bash

# scripts/build_ebpf.sh

set -e

# 检查是否安装了 clang 和 llvm-strip
if ! command -v clang &> /dev/null; then
    echo "Error: clang is not installed"
    exit 1
fi

if ! command -v llc &> /dev/null; then
    echo "Error: llc is not installed"
    exit 1
fi

# 创建输出目录
mkdir -p internal/plugin/network/ebpf/output

# 编译 eBPF 程序，添加 -g 选项生成包含 BTF 信息的调试符号
echo "Compiling eBPF program with BTF support..."
clang -g -O2 -target bpf -c internal/plugin/network/ebpf/network_monitor.c \
    -o internal/plugin/network/ebpf/output/network_monitor.o

# 检查编译是否成功
if [ $? -eq 0 ]; then
    echo "eBPF program compiled successfully!"
    echo "Output: internal/plugin/network/ebpf/output/network_monitor.o"
else
    echo "Error: Failed to compile eBPF program"
    exit 1
fi