#!/usr/bin/env bash
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$DIR"

if [ ! -f config.json ]; then
  cp config.example.json config.json
  echo "已生成 config.json。"
  echo "请先编辑 gateway.api_key / gateway.admin_key 和 upstream[].api_key，然后重新运行 ./start.sh"
  exit 0
fi

if [ ! -x ai-quota-gateway ]; then
  chmod +x ai-quota-gateway 2>/dev/null || true
fi

exec ./ai-quota-gateway
