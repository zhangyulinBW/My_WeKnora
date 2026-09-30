#!/usr/bin/env bash
# 将本机 WeKnora 全套镜像推送到阿里云容器镜像服务（ACR 个人版），
# 并生成镜像清单 scripts/acr-images.txt —— 该清单需随配置文件一起传到虚拟机，
# 供 scripts/acr-pull.sh 拉取并恢复原始镜像名。
#
# 用法（在仓库根目录或任意目录执行均可）：
#   ACR_NAMESPACE=你的命名空间 ./scripts/acr-push.sh [--profile neo4j] ...
#
# 环境变量：
#   ACR_REGISTRY   ACR 地址，默认杭州；换成你开通的地域（同地域 ECS 可用 registry-vpc 加速）
#   ACR_NAMESPACE  （必填）ACR 命名空间
#   ACR_REPO       ACR 仓库名，默认 weknora；需先在控制台创建（私有）
#
# 额外参数原样透传给 docker compose（如 --profile neo4j / --profile full），
# 决定要推送哪些服务对应的镜像。
set -euo pipefail
cd "$(dirname "$0")/.."

ACR_REGISTRY="${ACR_REGISTRY:-registry.cn-hangzhou.aliyuncs.com}"
ACR_REPO="${ACR_REPO:-weknora}"
: "${ACR_NAMESPACE:?请先 export ACR_NAMESPACE=你的ACR命名空间}"
REMOTE="$ACR_REGISTRY/$ACR_NAMESPACE/$ACR_REPO"
MANIFEST="scripts/acr-images.txt"

echo "==> 目标仓库: $REMOTE"
# SKIP_LOGIN=1: 本机凭据管理器已存有该仓库登录凭据时，跳过交互式登录
if [ -z "${SKIP_LOGIN:-}" ]; then
  docker login "$ACR_REGISTRY"
fi

# 解析（含透传 --profile）后实际需要的镜像清单
mapfile -t IMAGES < <(docker compose "$@" config --images | sort -u)

: > "$MANIFEST"
skipped=0
for img in "${IMAGES[@]}"; do
  if ! docker image inspect "$img" >/dev/null 2>&1; then
    echo "!! 本地缺少镜像，跳过: $img（先 docker compose pull/build 再重跑本脚本）" >&2
    skipped=$((skipped + 1))
    continue
  fi
  # 镜像名中的 / 和 : 转成 - 作为 tag，一个仓库装下全部镜像，免去建多个仓库
  tag="${img//[\/:]/-}"
  echo "==> push $img  ->  $REMOTE:$tag"
  docker tag "$img" "$REMOTE:$tag"
  docker push "$REMOTE:$tag"
  echo "$img" >> "$MANIFEST"
done

echo "==> 完成: 推送 $(wc -l < "$MANIFEST") 个镜像, 跳过 $skipped 个"
echo "==> 清单已写入 $MANIFEST, 随配置一起传到虚拟机"
