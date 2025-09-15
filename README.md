# Kon-Agent

一个高性能的边缘计算监控代理，使用Go语言开发，支持eBPF网络监控和QUIC协议传输。

## 特性

- 🔍 **实时监控**: 支持CPU使用率、网络流量等指标采集
- 🚀 **高性能传输**: 基于QUIC协议实现低延迟、高吞吐量的数据传输
- 🔒 **安全可靠**: 支持权限降级和安全的网络通信
- 📊 **插件架构**: 模块化设计，支持动态加载和卸载监控插件
- 🛡️ **eBPF支持**: 使用eBPF技术实现内核级别的网络监控
- 💾 **缓存机制**: 内置环形缓冲区，支持断网重连和数据持久化

## 架构概览

```
Kon-Agent
├── cmd/edge-agent/          # 主程序入口
├── internal/               # 内部核心模块
│   ├── config/            # 配置加载和验证
│   ├── edge-agent/        # 核心代理逻辑
│   ├── plugin/            # 插件管理系统
│   │   ├── network/ebpf/  # eBPF网络监控插件
│   │   └── system/cpu/    # CPU监控插件
│   ├── security/          # 安全相关功能
│   ├── transport/         # 数据传输层
│   │   ├── buffer/        # 环形缓冲区管理
│   │   └── quic/          # QUIC客户端实现
│   └── utils/             # 工具函数
├── pkg/                   # 公共包
│   ├── plugin/            # 插件接口定义
│   └── protocol/          # Protobuf协议定义
├── configs/               # 配置文件
├── scripts/              # 构建脚本
└── server/              # 测试用示例服务器
```

## 快速开始

### 前置要求

- Go 1.24+
- Clang和LLVM（用于编译eBPF程序）
- Linux内核支持eBPF（推荐5.8+）

### 安装依赖

```bash
# 安装系统依赖
sudo apt-get update
sudo apt-get install -y clang llvm

# 安装Go依赖
go mod download
```

### 构建eBPF程序

```bash
# 编译eBPF监控程序
./scripts/build_ebpf.sh
```

### 构建和运行代理

```bash
# 构建主程序
CGO_ENABLED=1 CC=clang go build -o edge-agent ./cmd/edge-agent/main.go

# 运行代理（需要root权限用于eBPF）
sudo ./kon-agent -config configs/agent.yaml
```

### 运行示例服务器

```bash
# 运行测试用QUIC服务器
go run ./server/main/quic_server.go
```

## 配置说明

### 代理配置 (configs/agent.yaml)

```yaml
server: "quic://localhost:7843"
client-id: "edge-agent-1"
plugins:
  cpu:
    enable: true
    period: 5s
  ebpf:
    enable: true
    period: 5s
cache:
  path: /tmp/edge_agent.cache
  max_size: 50MB
```

### 配置字段说明

- `server`: QUIC服务器地址，格式为 `quic://host:port`
- `client-id`: 客户端标识符
- `plugins`: 插件配置
  - `enable`: 是否启用插件
  - `period`: 数据采集间隔
- `cache`: 缓存配置
  - `path`: 状态缓存文件路径
  - `max_size`: 最大缓存大小

## 插件系统

### 内置插件

#### CPU监控插件
- 采集系统CPU使用率
- 通过读取 `/proc/stat` 获取数据
- 支持用户态CPU使用率统计

#### eBPF网络监控插件
- 使用XDP技术监控网络流量
- 实时统计网络包数量
- 支持多网卡监控

### 自定义插件开发

实现 `pkg/plugin.Plugin` 接口：

```go
type Plugin interface {
    Name() string
    Config() PluginConfig
    Run(ctx context.Context, out chan<- Event) error
    Stop() error
}
```

注册插件：

```go
func init() {
    plugin.Register("my-plugin", New)
}
```

## 数据传输协议

### Protobuf协议定义

项目使用Protobuf v3定义数据传输格式，支持：

- 单指标传输 (`Metric`)
- 批量指标传输 (`BatchMetricsRequest`)
- 多种指标类型（CPU、内存、网络等）

### QUIC传输层

- 基于QUIC协议实现高效传输
- 支持连接池和流复用
- 自动重连和错误处理

## 性能特性

### 缓冲区管理

- 环形缓冲区设计，避免内存泄漏
- 批量处理机制，减少网络开销
- 断网重连时数据持久化

### 资源管理

- 实时监控代理资源使用情况
- 自动降级和熔断机制
- 优雅的关闭和重启

## 安全特性

### 权限控制

- 支持权限降级（Drop Privileges）
- 最小权限原则运行



## 故障排除

### 常见问题

1. **eBPF编译失败**
   - 检查clang/llvm安装
   - 确认内核版本支持eBPF

3. **网络连接失败**
   - 确认QUIC服务器运行
   - 检查防火墙设置
