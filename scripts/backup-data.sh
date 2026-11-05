#!/usr/bin/env bash
# 备份 WeKnora 运行数据（数据卷 + 配置）到本机目录。
#
# 备份内容：
#   1. PostgreSQL 逻辑导出（pg_dump 自定义格式，容器运行中即可导，最安全）
#   2. data-files 数据卷（上传的原始文件）
#   3. neo4j-data 数据卷（知识图谱；tar 前会短暂停掉 neo4j 容器保证一致性，之后自动启动）
#   4. .env / config/config.yaml / docker-compose.yml
#
# 不备份：redis（缓存/队列，丢了可重建）、docreader-tmp（临时文件）、镜像本身
#（镜像如需离线保存见《镜像同步与虚拟机部署指南.md》或 docker save）。
#
# 用法：
#   bash scripts/backup-data.sh              # 备份到 ./backup/<时间戳>/
#   bash scripts/backup-data.sh /d/backup    # 备份到指定目录（Git Bash 路径）
#
# 恢复方法见每个备份目录里的 RESTORE.txt。

set -euo pipefail
# Git Bash 会把传给 docker.exe 的容器路径（如 /data）改写成 Windows 路径，禁掉
export MSYS_NO_PATHCONV=1

COMPOSE_DIR="$(cd "$(dirname "$0")/.." && pwd)"
BACKUP_ROOT="${1:-$COMPOSE_DIR/backup}"
STAMP="$(date +%Y%m%d-%H%M%S)"
DEST="$BACKUP_ROOT/$STAMP"
mkdir -p "$DEST"

# 与 docker-compose.yml 中声明的卷名保持一致（compose 项目名 = my_weknora）
PG_CONTAINER="WeKnora-postgres"
NEO4J_CONTAINER="WeKnora-neo4j"
VOL_DATA_FILES="my_weknora_data-files"
VOL_NEO4J="my_weknora_neo4j-data"

# 复用本地已有的 alpine 系镜像做 tar，避免额外拉取
TAR_IMAGE="docker.1ms.run/library/redis:7.0-alpine"

DB_USER="$(grep -E '^DB_USER=' "$COMPOSE_DIR/.env" | cut -d= -f2-)"
DB_NAME="$(grep -E '^DB_NAME=' "$COMPOSE_DIR/.env" | cut -d= -f2-)"
DB_PASSWORD="$(grep -E '^DB_PASSWORD=' "$COMPOSE_DIR/.env" | cut -d= -f2-)"
DB_USER="${DB_USER:-postgres}"
DB_NAME="${DB_NAME:-WeKnora}"

echo "==> 备份到 $DEST"

echo "==> [1/4] PostgreSQL 逻辑导出 ($DB_NAME) ..."
docker exec -e PGPASSWORD="$DB_PASSWORD" "$PG_CONTAINER" \
  pg_dump -U "$DB_USER" -d "$DB_NAME" -Fc \
  > "$DEST/${DB_NAME}.dump"

echo "==> [2/4] 打包 data-files 数据卷（上传的原始文件）..."
docker run --rm --entrypoint tar -v "$VOL_DATA_FILES":/data:ro "$TAR_IMAGE" \
  czf - -C /data . > "$DEST/data-files.tar.gz"

echo "==> [3/4] 打包 neo4j-data 数据卷（需短暂停止 neo4j，保证一致性）..."
docker stop "$NEO4J_CONTAINER" >/dev/null
# 停容器后即使 tar 失败也要把容器拉起来
trap 'docker start "$NEO4J_CONTAINER" >/dev/null' EXIT
docker run --rm --entrypoint tar -v "$VOL_NEO4J":/data:ro "$TAR_IMAGE" \
  czf - -C /data . > "$DEST/neo4j-data.tar.gz"
docker start "$NEO4J_CONTAINER" >/dev/null
trap - EXIT
echo "    neo4j 已重新启动"

echo "==> [4/4] 复制配置文件 ..."
cp "$COMPOSE_DIR/.env" "$DEST/env.backup"
cp "$COMPOSE_DIR/config/config.yaml" "$DEST/config.yaml"
cp "$COMPOSE_DIR/docker-compose.yml" "$DEST/docker-compose.yml"

cat > "$DEST/RESTORE.txt" <<EOF
恢复步骤（在本项目目录下、Git Bash 中执行；卷名/容器名如不同请先调整）

1) 恢复配置（如新机器）：
   cp env.backup ../.env && cp config.yaml ../config/config.yaml

2) 启动一套全新栈：
   docker compose up -d

3) 恢复 PostgreSQL（覆盖现有 ${DB_NAME} 库；DB_PASSWORD 在 .env 里）：
   docker exec -i -e PGPASSWORD="\$DB_PASSWORD" WeKnora-postgres \\
     pg_restore -U ${DB_USER} -d ${DB_NAME} --clean --if-exists < ${DB_NAME}.dump

4) 恢复上传文件：
   docker run --rm -i --entrypoint tar -v my_weknora_data-files:/data \\
     ${TAR_IMAGE} xzf - -C /data < data-files.tar.gz

5) 恢复 Neo4j（先停容器、清空卷再解包，最后启动）：
   docker stop WeKnora-neo4j
   docker run --rm --entrypoint sh -v my_weknora_neo4j-data:/data ${TAR_IMAGE} \\
     -c 'rm -rf /data/* /data/.[!.]* 2>/dev/null || true'
   docker run --rm -i --entrypoint tar -v my_weknora_neo4j-data:/data \\
     ${TAR_IMAGE} xzf - -C /data < neo4j-data.tar.gz
   docker start WeKnora-neo4j

6) 重启应用让缓存失效：
   docker restart WeKnora-app WeKnora-docreader
EOF

echo "==> 生成校验和 ..."
(cd "$DEST" && sha256sum ./* > SHA256SUMS)

echo "==> 完成。备份清单："
ls -lh "$DEST"
echo
echo "恢复方法见 $DEST/RESTORE.txt"
