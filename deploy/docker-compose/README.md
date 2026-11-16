# Kon-Agent SRE 服务端栈 · docker-compose 部署

一键起完整 SRE 闭环：`VictoriaMetrics → vmalert → Alertmanager → webhook`，外加 `Grafana` 可视化与自研 `kon-agent-server` QUIC 接收端。

## 组件与端口

| 服务 | 端口 | 说明 |
|---|---|---|
| victoria-metrics | 8428 | 时序存储，`/health` 健康检查 |
| kon-agent-server | 7843/**udp** | 自研 QUIC 接收端，写入 VM |
| vmalert | 8880 | 规则评估（告警 + recording rules） |
| alertmanager | 9093 | 告警分组/路由/静默/抑制 |
| grafana | 3000 | 可视化，默认 `admin/admin` |

配置单一来源在 `../config/`（告警规则、Alertmanager、Grafana provisioning/仪表盘），compose 直接挂载该目录；后续 Helm chart 通过 `deploy/sync-config.sh` 复制同一份配置，避免双份漂移。

## 快速开始

### 路径 A：CCR 镜像已推送（目标态）

```bash
cp .env.example .env          # 按需修改 CCR_REGISTRY / 密码 / 端口
docker compose up -d
```

### 路径 B：CCR 镜像未就绪，先用上游镜像本地调试

```bash
cp .env.example .env
docker compose -f docker-compose.yml -f docker-compose.upstream.yml up -d --build
```

`--build` 会本地构建 `kon-agent-server`（Dockerfile.server 在仓库根目录）。

### 验证

```bash
docker compose ps                                  # 全部 running/healthy
curl -s http://localhost:8428/health               # VM OK
open http://localhost:8880                         # vmalert：规则与告警状态
open http://localhost:9093                         # Alertmanager
open http://localhost:3000                         # Grafana → Kon-Agent 文件夹 → 黄金信号仪表盘
```

起栈后 vmalert 的 `AgentFleetDown` 会处于 firing（还没有 agent 上报），属预期行为。

### 可选：本地端到端演示（Linux）

```bash
docker compose -f docker-compose.yml -f docker-compose.upstream.yml --profile agent up -d --build
```

要求：内核 ≥ 5.8 且带 BTF（`/sys/kernel/btf/vmlinux` 存在）。agent 以 host 网络连接 `quic://localhost:7843`，几秒后 Grafana 仪表盘出图。

## 推送镜像到腾讯云 CCR（一次性）

前置：CCR 控制台创建个人版实例与命名空间（示例用 `konpure`），本机 `docker login ccr.ccs.tencentyun.com`。

```bash
REG=ccr.ccs.tencentyun.com/konpure
cd /path/to/Kon-Agent   # 仓库根目录

# 1) 开源组件：拉取 → tag → push
for img in \
  "victoriametrics/victoria-metrics:v1.155.0 victoria-metrics:v1.155.0" \
  "victoriametrics/vmalert:v1.155.0          vmalert:v1.155.0" \
  "prom/alertmanager:v0.34.2                 alertmanager:v0.34.2" \
  "grafana/grafana:13.3.0                    grafana:13.3.0"; do
  set -- $img
  docker pull "$1" && docker tag "$1" "$REG/$2" && docker push "$REG/$2"
done

# 2) 自研组件：构建 → tag → push
docker build -f Dockerfile.server -t "$REG/kon-agent-server:latest" .
docker push "$REG/kon-agent-server:latest"

docker build -f Dockerfile.agent -t "$REG/edge-agent:latest" .
docker push "$REG/edge-agent:latest"
```

推完后把 `.env` 里 `CCR_REGISTRY` 指向该命名空间，走路径 A 启动。

## 常用命令

```bash
docker compose logs -f kon-agent-server     # 跟踪某服务日志
docker compose restart vmalert              # 改了 ../config/vmalert/rules 后生效
docker compose down                         # 停止（数据卷保留）
docker compose down -v                      # 停止并清空全部数据
```

## 目录结构

```text
deploy/
├── config/                        # 配置单一来源（compose 挂载，helm 经 sync-config.sh 复制）
│   ├── vmalert/rules/             # 黄金信号 + SLO burn-rate 规则
│   ├── alertmanager/              # 路由/分组/抑制，webhook 占位
│   └── grafana/                   # 数据源 + 仪表盘 provisioning，黄金信号仪表盘
└── docker-compose/
    ├── docker-compose.yml         # 主文件（镜像走 CCR，可用环境变量覆盖）
    ├── docker-compose.upstream.yml# 上游镜像 override（本地调试用）
    ├── .env.example               # 复制为 .env 后修改
    └── README.md
```

> Helm chart（K8s 部署）在后续迭代交付，位置 `deploy/helm/kon-agent-stack/`。
