# Docker 沙箱排障：permission denied 连不上 docker.sock

适用症状：Docker 沙箱后端报
`permission denied while trying to connect to the docker API at unix:///var/run/docker.sock`，
或「设置 → 沙箱后端」里 Docker 配置连通性检查失败。后端设计与配置见
[Docker 沙箱后端](./sandbox-docker-backend.md)，本文只讲排障。

## 原理：为什么会被拒

app 容器内的主进程以非 root 的 `appuser` 运行。入口脚本
`scripts/docker-entrypoint.sh` 以 root 启动，在 gosu 降权**之前**执行
`grant_docker_sock_to_appuser`：读出 `/var/run/docker.sock` 的 GID，把该 GID
对应的组加给 `appuser`。因此 appuser 能否连 daemon，取决于 socket 是否
**组可写且 GID 非 0**：

| socket 属主 | 入口脚本行为 | 结果 |
| --- | --- | --- |
| `root:docker 0660`（Linux 常态） | 自动建组、加组 | 正常，无需处理 |
| `root:root 0660`（Docker Desktop） | **拒绝补权**，打印告警后放行 | appuser 连不上 → 本文症状 |
| 未挂载 | 跳过 | 连 `unix:///var/run/docker.sock` 直接失败 |

拒绝 GID 0 是刻意的：把 `appuser` 加进 root 组等于没降权，
`docker.sock` 等同宿主机 root，补权必须来自宿主侧的明确决定。

## 第一步：定位是哪种场景

```bash
# 容器是否在跑、健康与否（healthcheck 打 http://localhost:8080/health）
docker ps --filter name=WeKnora-app --format "{{.Status}}"

# 入口脚本有没有告警（每次容器启动打印一次）
docker logs WeKnora-app 2>&1 | grep -i "weknora:"

# socket 在容器内的属主与 appuser 的组
docker exec WeKnora-app sh -c 'stat -c "uid=%u gid=%g mode=%a" /var/run/docker.sock'
docker exec WeKnora-app id appuser
```

判定：

- 告警含 `owned by GID 0` → **场景 B（Docker Desktop）**。
- 告警含 `cannot stat` → socket 没挂进容器，检查 compose 卷
  `/var/run/docker.sock:/var/run/docker.sock` 是否还在、daemon 是否在跑。
- 无告警但 `id appuser` 没有额外组 → 入口脚本是旧版本或被绕过，重建容器。
- app 不在容器里、直接跑在宿主机 → **场景 C**。

> 注意区分：`docker compose up` 报 `WeKnora-app is unhealthy` 不一定是本问题。
> healthcheck 只等 60s，app 启动期被其它环节卡住（例如 `NEO4J_ENABLE=true`
> 但没起 neo4j 服务，30 次重试约 3 分钟）同样会 unhealthy。先看
> `docker logs WeKnora-app` 卡在哪一段，再对症。

## 场景 A：Linux 宿主机 daemon，app 在 compose 容器里

常态下 socket 是 `root:docker 0660`，入口脚本全自动处理，无需任何操作。
若仍失败，按第一步排查：

```bash
# 宿主机上确认属主（GID 非 0、组可写即可）
ls -ln /var/run/docker.sock
```

如果 socket 是 `root:root`（比如管理员改过），在宿主机把它交给一个非 root 组：

```bash
sudo chgrp docker /var/run/docker.sock   # 或任选非 0 GID，保持 0660
docker compose up -d --force-recreate app
```

## 场景 B：Docker Desktop（Windows / WSL2 后端）——本文重点

Docker Desktop 的 daemon 跑在 `docker-desktop` WSL 发行版里。**关键坑：容器里
`/var/run/docker.sock` 挂的不是 dockerd 的原始 socket**，而是 Docker Desktop
的代理 socket；chgrp 原始 socket 不会改变容器内看到的属主。实测
（Docker Desktop 4.84.0，2026-09）对应关系：

| docker-desktop 发行版内路径 | 作用 | 容器视角 |
| --- | --- | --- |
| `/run/host-services/docker.proxy.sock` | 挂给容器用的代理（`660 root:root`） | **就是它**，改这个 |
| `/tmp/docker-desktop-root/run/docker.sock` | dockerd 原始 socket | 改它无效 |

### 修复（两步）

```powershell
# 1) 在 docker-desktop 发行版里给代理 socket 换一个非 0 组（保持 0660 组可写）
wsl -d docker-desktop -u root -- chgrp 999 /run/host-services/docker.proxy.sock

# 2) 重建 app 容器，让入口脚本重新给 appuser 补权
docker compose up -d --force-recreate app
```

Git Bash 下执行第 1 步要防路径改写：前缀 `MSYS_NO_PATHCONV=1`。

若上表路径在你版本里不存在，找真实代理 socket 的办法：先看容器内
`stat` 输出的属主/模式，再到发行版里比对候选：

```bash
wsl -d docker-desktop -u root -- sh -c 'find /run /var/run /tmp -name "*docker*.sock" -type s -exec ls -ln {} +'
```

属主与模式和容器视角一致的那个就是挂载源，对它 chgrp。

### 验证

```bash
docker exec WeKnora-app id appuser
# 期望 groups 含 999(dockersock)

docker exec -u appuser WeKnora-app sh -c \
  'test -r /var/run/docker.sock && test -w /var/run/docker.sock && echo SOCKET OK'

# 端到端：以 appuser 身份实际调一次 Engine API
docker exec -u appuser WeKnora-app sh -c \
  'curl -s --unix-socket /var/run/docker.sock http://localhost/version | head -c 80'
# 期望返回 {"Platform":...,"Version":...}
```

全部通过后，到「设置 → 沙箱后端」重新保存/测试 Docker 配置即可。

### Docker Desktop 重启后失效

重启 Docker Desktop（或重启电脑）会重建 socket，属主回到 `root:root`，需要重跑
上面两步。嫌手动麻烦可以把 chgrp 一行放进登录任务（Windows 任务计划程序，
触发器「登录时」），`--force-recreate` 只在 chgrp 后需要时手动执行。

macOS 的 Docker Desktop 不走本文路径：官方支持的方式是
Settings → Advanced → "Allow the default Docker socket to be used (requires password)"。

## 场景 C：app 直接跑在 Linux / WSL 宿主机上

`go run` 或二进制直跑时，是当前系统用户没进 docker 组：

```bash
sudo usermod -aG docker $USER   # 之后注销重登，或 newgrp docker
docker ps                        # 不加 sudo 能列出容器即修复
```

改完重启 app 进程。

## 不要做的事

- **不要 `chmod 666 /var/run/docker.sock`**：向本机所有用户开放 root 等价权限。
  入口脚本注释与 [Docker 沙箱后端](./sandbox-docker-backend.md) 都明确反对。
- **不要依赖 compose `group_add`**：入口脚本用 gosu 降权会 `initgroups`，
  compose 注入的附加组会被丢掉，这正是入口脚本亲自加组的原因。
- **不要把 socket chgrp 到 GID 0 或给 appuser 加 root 组**：入口脚本会拒绝，
  就算绕过也等于放弃降权。
- **不要在 Docker Desktop 里去 chgrp `/tmp/docker-desktop-root/run/docker.sock`**：
  容器挂的是代理 socket（见场景 B 表格），改它无效。

## 相关文件

- `scripts/docker-entrypoint.sh` — `grant_docker_sock_to_appuser` 补权逻辑
- `docker-compose.yml` — socket 挂载与 `WEKNORA_SANDBOX_DOCKER_ENABLED` 注释
- [Docker 沙箱后端](./sandbox-docker-backend.md) — 设计、配置字段与边界
- [沙箱集群与标准模板](./sandbox-cluster.md) — Cube / E2B 后端
