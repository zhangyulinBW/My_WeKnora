# WeKnora 数据备份 / 还原脚本（PowerShell 版，供虚拟机等无 Git Bash 的环境使用）
#
# 备份内容（与 scripts/backup-data.sh 的产物格式完全一致，可互换还原）：
#   1. PostgreSQL 逻辑导出（pg_dump 自定义格式，运行中导出，安全）
#   2. data-files 数据卷（上传的原始文件）
#   3. neo4j-data 数据卷（知识图谱；打包前会短暂停掉 neo4j，之后自动启动）
#   4. .env / config/config.yaml / docker-compose.yml
#
# 不备份：redis（缓存）、docreader-tmp（临时文件）、镜像本身。
#
# 用法（在项目目录的 PowerShell 中）：
#   powershell -ExecutionPolicy Bypass -File scripts\backup-data.ps1                     # 备份到 .\backup\<时间戳>\
#   powershell -ExecutionPolicy Bypass -File scripts\backup-data.ps1 -BackupRoot D:\bk    # 备份到指定目录
#   powershell -ExecutionPolicy Bypass -File scripts\backup-data.ps1 -Restore -BackupDir .\backup\20261104-180000
#                                                                                        # 从指定备份还原
#   追加 -RestoreConfigs 可同时还原 .env / config.yaml（默认不动现有配置）
#
# 注意：不要用 PowerShell 的 > 重定向保存 docker 输出的二进制流（5.1 会写坏），
# 本脚本一律通过 docker cp / 目录挂载落盘。

param(
    [switch]$Restore,
    [string]$BackupDir = "",
    [string]$BackupRoot = "",
    [switch]$RestoreConfigs
)

$ErrorActionPreference = "Stop"

# ---- 与 docker-compose.yml 保持一致的固定名称（容器名写死在 compose 里；卷名 = 项目名 my_weknora + 卷名）----
$PgContainer   = "WeKnora-postgres"
$Neo4jContainer = "WeKnora-neo4j"
$AppContainer  = "WeKnora-app"
$DocContainer  = "WeKnora-docreader"
$VolDataFiles  = "my_weknora_data-files"
$VolNeo4j      = "my_weknora_neo4j-data"
# 复用本地已有的 alpine 系镜像做 tar，避免额外拉取；如镜像名不同请改这里
$TarImage      = "docker.1ms.run/library/redis:7.0-alpine"

$ProjectDir = Split-Path -Parent $PSScriptRoot
if (-not $BackupRoot) { $BackupRoot = Join-Path $ProjectDir "backup" }

function Read-Env([string]$Key, [string]$Default) {
    $envFile = Join-Path $ProjectDir ".env"
    if (Test-Path $envFile) {
        foreach ($line in [IO.File]::ReadAllLines($envFile)) {
            if ($line -match "^\s*$Key=(.*)$") { return $Matches[1].Trim() }
        }
    }
    return $Default
}

function Check-Ready {
    docker info *> $null
    if ($LASTEXITCODE -ne 0) { throw "Docker 引擎不可用：请先启动 Docker Desktop（等鲸鱼图标稳定）" }
    docker image inspect $TarImage *> $null
    if ($LASTEXITCODE -ne 0) { throw "本地没有镜像 $TarImage（用来执行 tar），请修改脚本开头的 `$TarImage 为本机任一 alpine 系镜像" }
}

$DbUser = Read-Env "DB_USER" "postgres"
$DbName = Read-Env "DB_NAME" "WeKnora"
$DbPassword = Read-Env "DB_PASSWORD" ""

if (-not $Restore) {
    # ==================== 备份 ====================
    Check-Ready
    $stamp = Get-Date -Format "yyyyMMdd-HHmmss"
    $dest = Join-Path $BackupRoot $stamp
    New-Item -ItemType Directory -Force -Path $dest | Out-Null
    Write-Host "==> 备份到 $dest"

    Write-Host "==> [1/4] PostgreSQL 逻辑导出 ($DbName) ..."
    docker exec -e PGPASSWORD="$DbPassword" $PgContainer pg_dump -U $DbUser -d $DbName -Fc -f /tmp/weknora-backup.dump
    if ($LASTEXITCODE -ne 0) { throw "pg_dump 失败" }
    docker cp "${PgContainer}:/tmp/weknora-backup.dump" (Join-Path $dest "$DbName.dump")
    docker exec $PgContainer rm /tmp/weknora-backup.dump

    Write-Host "==> [2/4] 打包 data-files 数据卷（上传的原始文件）..."
    docker run --rm -v "${VolDataFiles}:/data:ro" -v "${dest}:/backup" --entrypoint tar $TarImage czf /backup/data-files.tar.gz -C /data .
    if ($LASTEXITCODE -ne 0) { throw "打包 data-files 失败" }

    Write-Host "==> [3/4] 打包 neo4j-data 数据卷（需短暂停止 neo4j 保证一致性）..."
    docker stop $Neo4jContainer | Out-Null
    try {
        docker run --rm -v "${VolNeo4j}:/data:ro" -v "${dest}:/backup" --entrypoint tar $TarImage czf /backup/neo4j-data.tar.gz -C /data .
        if ($LASTEXITCODE -ne 0) { throw "打包 neo4j 失败" }
    } finally {
        docker start $Neo4jContainer | Out-Null
    }
    Write-Host "    neo4j 已重新启动"

    Write-Host "==> [4/4] 复制配置文件 ..."
    Copy-Item (Join-Path $ProjectDir ".env") (Join-Path $dest "env.backup")
    Copy-Item (Join-Path $ProjectDir "config\config.yaml") (Join-Path $dest "config.yaml")
    Copy-Item (Join-Path $ProjectDir "docker-compose.yml") (Join-Path $dest "docker-compose.yml")

    $files = @(Get-ChildItem $dest -File)
    $hashLines = foreach ($f in $files) {
        "{0}  {1}" -f (Get-FileHash $f.FullName -Algorithm SHA256).Hash, $f.Name
    }
    $hashLines | Out-File (Join-Path $dest "SHA256SUMS") -Encoding ascii

    Write-Host "==> 完成。备份清单："
    Get-ChildItem $dest -File | Format-Table Name, @{L="SizeMB";E={[math]::Round($_.Length/1MB,1)}} -AutoSize
    Write-Host "还原命令：powershell -ExecutionPolicy Bypass -File scripts\backup-data.ps1 -Restore -BackupDir `"$dest`""
    exit 0
}

# ==================== 还原 ====================
if (-not $BackupDir) { throw "还原需要 -BackupDir 指定备份目录（backup\ 下带时间戳的那个）" }
$BackupDir = (Resolve-Path $BackupDir).Path
$dumpFile = Join-Path $BackupDir "$DbName.dump"
$filesTar = Join-Path $BackupDir "data-files.tar.gz"
$neo4jTar = Join-Path $BackupDir "neo4j-data.tar.gz"
foreach ($f in @($dumpFile, $filesTar, $neo4jTar)) {
    if (-not (Test-Path $f)) { throw "备份目录里缺少文件：$f" }
}
Check-Ready

Write-Host "==> [1/5] 停止应用容器（避免还原过程有写入）..."
docker stop $AppContainer $DocContainer | Out-Null

Write-Host "==> [2/5] 还原 PostgreSQL（覆盖 $DbName 库）..."
docker cp $dumpFile "${PgContainer}:/tmp/weknora-restore.dump"
if ($LASTEXITCODE -ne 0) { throw "拷入 dump 失败" }
docker exec -e PGPASSWORD="$DbPassword" $PgContainer pg_restore -U $DbUser -d $DbName --clean --if-exists /tmp/weknora-restore.dump
if ($LASTEXITCODE -ne 0) { Write-Host "    pg_restore 返回码 $LASTEXITCODE（如仅为 drop 不存在对象的告警可忽略）" }
docker exec $PgContainer rm /tmp/weknora-restore.dump

Write-Host "==> [3/5] 还原上传文件（data-files 卷）..."
docker run --rm -v "${VolDataFiles}:/data" -v "${BackupDir}:/backup:ro" --entrypoint tar $TarImage xzf /backup/data-files.tar.gz -C /data
if ($LASTEXITCODE -ne 0) { throw "解包 data-files 失败" }

Write-Host "==> [4/5] 还原 Neo4j（清空卷后解包）..."
docker stop $Neo4jContainer | Out-Null
try {
    docker run --rm -v "${VolNeo4j}:/data" --entrypoint sh $TarImage -c "rm -rf /data/* /data/.[!.]* 2>/dev/null; true"
    docker run --rm -v "${VolNeo4j}:/data" -v "${BackupDir}:/backup:ro" --entrypoint tar $TarImage xzf /backup/neo4j-data.tar.gz -C /data
    if ($LASTEXITCODE -ne 0) { throw "解包 neo4j 失败" }
} finally {
    docker start $Neo4jContainer | Out-Null
}

Write-Host "==> [5/5] 启动应用容器 ..."
docker start $AppContainer $DocContainer | Out-Null

if ($RestoreConfigs) {
    Copy-Item (Join-Path $BackupDir "env.backup") (Join-Path $ProjectDir ".env") -Force
    Copy-Item (Join-Path $BackupDir "config.yaml") (Join-Path $ProjectDir "config\config.yaml") -Force
    Write-Host "    已还原 .env 和 config.yaml（如 docker-compose.yml 也变了，手动替换后需 docker compose up -d）"
} else {
    Write-Host "    未动现有 .env / config.yaml；需要一并还原请加 -RestoreConfigs"
}

Write-Host "==> 还原完成。稍等 1-2 分钟后检查：docker compose --profile neo4j ps （app 应显示 healthy）"
