#!/bin/bash

# deploy/sync-config.sh
# 将 deploy/config（compose 与 helm 的配置单一来源）同步进 helm chart。
#
# 背景：helm 只能读取 chart 目录内的文件，因此规则/告警配置/仪表盘
# 修改后的标准流程是：
#   1. 修改 deploy/config/ 下对应文件
#   2. ./deploy/sync-config.sh
#   3. helm upgrade <release> deploy/helm/kon-agent-stack
#
# 不要直接编辑 chart 内的 rules/ alertmanager/ dashboards/ —— 会被覆盖。

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SRC="$SCRIPT_DIR/config"
DST="$SCRIPT_DIR/helm/kon-agent-stack"

if [ ! -d "$SRC" ]; then
    echo "Error: config source not found: $SRC"
    exit 1
fi

mkdir -p "$DST/rules" "$DST/alertmanager" "$DST/dashboards"

cp -v "$SRC"/vmalert/rules/*.yml          "$DST/rules/"
cp -v "$SRC"/alertmanager/alertmanager.yml "$DST/alertmanager/"
cp -v "$SRC"/grafana/dashboards/*.json     "$DST/dashboards/"

echo "Config synced into $DST"
