#!/usr/bin/env bash
# 虚拟机侧使用: 从阿里云 ACR 拉取 scripts/acr-images.txt 清单中的镜像,
# 并打回原始镜像名, 之后 docker compose up 直接使用本地镜像, 不再访问外网仓库。
#
# 用法（在虚拟机仓库根目录执行）：
#   ACR_NAMESPACE=你的命名空间 ./scripts/acr-pull.sh
#
# 环境变量同 acr-push.sh: ACR_REGISTRY / ACR_NAMESPACE / ACR_REPO。
set -euo pipefail
cd "$(dirname "$0")/.."

ACR_REGISTRY="${ACR_REGISTRY:-registry.cn-hangzhou.aliyuncs.com}"
ACR_REPO="${ACR_REPO:-weknora}"
: "${ACR_NAMESPACE:?请先 export ACR_NAMESPACE=你的ACR命名空间}"
REMOTE="$ACR_REGISTRY/$ACR_NAMESPACE/$ACR_REPO"
MANIFEST="scripts/acr-images.txt"

[ -f "$MANIFEST" ] || { echo "缺少 $MANIFEST（从本机随配置一起 scp 过来）" >&2; exit 1; }

if [ -z "${SKIP_LOGIN:-}" ]; then
  docker login "$ACR_REGISTRY"
fi

while IFS= read -r img; do
  [ -z "$img" ] && continue
  tag="${img//[\/:]/-}"
  echo "==> pull $REMOTE:$tag  ->  $img"
  docker pull "$REMOTE:$tag"
  docker tag "$REMOTE:$tag" "$img"
done < "$MANIFEST"

echo "==> 完成。可用 docker images 确认镜像名已恢复, 然后与推送侧相同的 profile 参数执行:"
echo "    docker compose --profile neo4j up -d"
