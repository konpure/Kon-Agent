# kon-agent-operator

Kon-Agent 边缘监控 agent 的 Kubernetes operator：通过 `KonAgent` CR 声明式管理节点级采集 agent 的部署与配置。

## 工作原理

```
KonAgent CR  →  reconcile  →  ConfigMap(agent.yaml)  →  DaemonSet(每节点一个 agent pod)
                              （配置变更经 config-hash 注解触发滚动）
```

- **DaemonSet 模型**：节点级监控组件每节点恰好一个 pod，`nodeSelector`/`tolerations` 支持边缘节点分组调度
- **特权容器**：agent 使用 eBPF（XDP/kprobe/tracepoint），pod 以 `privileged` + `hostNetwork` 运行，挂载宿主机 `/proc`、`/sys`、`tracefs` 与持久化缓存目录（WAL 数据跨 pod 重启保留）
- **配置即事实源**：CR 变更 → operator 渲染新的 agent.yaml → ConfigMap → config-hash 变化 → DaemonSet 滚动重启

## KonAgent CRD 字段

| 字段 | 说明 |
|------|------|
| `spec.clientId` | agent 标识（必填） |
| `spec.server` | 上报端点，如 `quic://vm:7843`（必填） |
| `spec.image` | agent 镜像，默认 `konpure/kon-agent:latest` |
| `spec.plugins` | 插件开关与采集周期：`enable`/`period`(如 `5s`)/`pluginType` |
| `spec.codec` | 线序编码：`gorilla`（默认）/`snappy`/`raw` |
| `spec.wal` | 本地 WAL：`enable`/`dir`/`segmentSize`/`maxSize`/`fsyncInterval` |
| `spec.sampling` | 降采样：`mode`(off/auto/always)/`summaryWindow`/`degradedWindow` |
| `spec.cache` | 缓存目录与上限 |
| `spec.nodeSelector` / `spec.tolerations` | 调度约束 |

status 回写：`activePlugins`（由 spec 推导）、`lastHeartbeatTime`、conditions（`ConfigReady`/`DaemonSetReady`）。

## 部署

```sh
# 1. 安装 CRD 与 operator（需先有 agent 镜像，见根目录 Dockerfile.agent）
make install
make docker-build docker-push IMG=<registry>/kon-agent-operator:tag
make deploy IMG=<registry>/kon-agent-operator:tag

# 2. 创建 CR
kubectl apply -f config/samples/core_v1alpha1_konagent.yaml

# 3. 查看状态
kubectl get konagent -o wide
kubectl get daemonset -A
```

## 开发

```sh
make manifests generate   # 修改 api/ 后重新生成 CRD 与 deepcopy
go test ./controllers/    # 渲染单测（无集群依赖）；envtest 套件需 KUBEBUILDER_ASSETS
```

## License

Copyright 2025. Licensed under the Apache License, Version 2.0.
