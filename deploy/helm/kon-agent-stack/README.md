# kon-agent-stack · Helm Chart

Kon-Agent SRE 服务端栈的 Kubernetes 部署：VictoriaMetrics + vmalert + Alertmanager + Grafana + kon-agent-server。全自研模板，无外部 chart 依赖，镜像统一走腾讯云 CCR。

## 前置条件

1. Kubernetes ≥ 1.23，helm ≥ 3.8
2. 镜像就绪（二选一）：
   - **CCR 模式（默认）**：`values.yaml` 的 `global.imageRegistry` 指向已推送镜像的命名空间（`konpure`），见 `deploy/docker-compose/README.md` 的推送清单
   - **本地调试**：`--set global.imageRegistry=""` 使用上游镜像（需集群能访问 Docker Hub）
3. CCR 命名空间为**私有**时，创建拉取凭证并启用：

```bash
kubectl -n monitoring create secret docker-registry ccr-secret \
  --docker-server=ccr.ccs.tencentyun.com \
  --docker-username=<腾讯云账号ID> \
  --docker-password=<token/密码>
# 安装时加：--set global.imagePullSecrets[0].name=ccr-secret
```

## 安装

```bash
cd deploy/helm/kon-agent-stack
helm install konstack . -n monitoring --create-namespace
helm -n monitoring status konstack    # 查看 NOTES（访问入口）
kubectl -n monitoring get pods,svc
```

默认出口：Grafana `NodePort 30300`、kon-agent-server `NodePort 30784 (UDP)`。

## 采集端接入（kon-agent-operator）

在 KonAgent CR 中指定 server 地址（agent 在集群外）：

```yaml
spec:
  server: "quic://<节点IP>:30784"   # NodePort；云安全组放行 UDP 30784
```

如 `server.service.type=LoadBalancer`，改用 CLB 的 UDP 监听器地址。注意：接收端暂无健康检查端点（等 P1 `/metrics` 落地），CLB 只能做端口级探测。

## 修改告警规则 / 仪表盘 / Alertmanager 配置

配置单一来源在 `deploy/config/`，chart 内的 `rules/ alertmanager/ dashboards/` 是同步产物：

```bash
# 1. 修改 deploy/config/ 下文件
# 2. 同步进 chart
./deploy/sync-config.sh
# 3. 滚动更新（ConfigMap 变更后需重启对应组件加载）
helm upgrade konstack . -n monitoring
kubectl -n monitoring rollout restart deploy/konstack-kon-agent-stack-vmalert
kubectl -n monitoring rollout restart deploy/konstack-kon-agent-stack-alertmanager
```

## 常用运维

```bash
helm upgrade konstack . -n monitoring -f my-values.yaml   # 覆盖配置
helm -n monitoring get values konstack                    # 当前生效值
helm uninstall konstack -n monitoring
```

数据保留说明：
- **VM 数据（volumeClaimTemplates 创建的 PVC）在 `helm uninstall` 后保留**，重装同名 release 会复用；彻底清理需手动 `kubectl delete pvc`
- Grafana PVC（若开启 `grafana.storage.enabled`）随 release 删除
- server 采用 `Recreate` 更新策略：重启瞬间的指标由采集端 WAL 兜底补传

## 与 compose 栈的关系

`deploy/docker-compose/`（单机演示）与本 chart（K8s）共用 `deploy/config/` 的同一份告警规则、Alertmanager 路由和 Grafana 仪表盘——两套部署产物始终一致。
