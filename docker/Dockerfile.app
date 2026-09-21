# Build extension and daemon from the same pinned source on the runtime architecture.
# 基础镜像已加国内源前缀（原: node:24-bookworm-slim@sha256:...）；digest 与上游钉扎一致
FROM --platform=$TARGETPLATFORM docker.m.daocloud.io/library/node:24-bookworm-slim@sha256:ba849c60be29959425b8734d57b8b4b7d56f98edd9504c9af091d5281095a71e AS browserskill
WORKDIR /build
RUN apt-get update && \
    apt-get install -y --no-install-recommends git python3 ca-certificates curl build-essential cmake pkg-config && \
    rm -rf /var/lib/apt/lists/*
ENV RUSTUP_HOME=/usr/local/rustup CARGO_HOME=/usr/local/cargo
ENV PATH=/usr/local/cargo/bin:$PATH
# 国内镜像：static.rust-lang.org / crates.io 不可达。rustup 与 cargo 走 rsproxy.cn，
# 可用 build-arg RUSTUP_DIST_ARG 覆盖回官方源（https://static.rust-lang.org）。
ARG RUSTUP_DIST_ARG=https://rsproxy.cn
ENV RUSTUP_DIST_SERVER=${RUSTUP_DIST_ARG} RUSTUP_UPDATE_ROOT=${RUSTUP_DIST_ARG}/rustup
RUN mkdir -p /usr/local/cargo && printf '%s\n' \
    '[source.crates-io]' 'replace-with = "rsproxy-sparse"' \
    '[source.rsproxy-sparse]' 'registry = "sparse+https://rsproxy.cn/index/"' \
    '[net]' 'git-fetch-with-cli = true' \
    > /usr/local/cargo/config.toml
# build_browserskill.sh 里的 npx/pnpm 走国内 npm 镜像
ENV npm_config_registry=https://registry.npmmirror.com
RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --profile minimal --default-toolchain stable
COPY scripts/build_browserskill.sh scripts/browserskill-release.json ./scripts/
COPY patches/browserskill ./patches/browserskill
ARG TARGETOS
ARG TARGETARCH
RUN bash scripts/build_browserskill.sh /opt/weknora/browserskill "${TARGETOS}/${TARGETARCH}"

# Build stage
FROM docker.m.daocloud.io/library/golang:1.26-bookworm AS builder

WORKDIR /app

# 通过构建参数接收敏感信息
ARG GOPRIVATE_ARG
ARG GOPROXY_ARG
ARG GOSUMDB_ARG=off
ARG APK_MIRROR_ARG

# 设置Go环境变量
ENV GOPRIVATE=${GOPRIVATE_ARG}
ENV GOPROXY=${GOPROXY_ARG}
ENV GOSUMDB=${GOSUMDB_ARG}

# Install dependencies
RUN if [ -n "$APK_MIRROR_ARG" ]; then \
        sed -i "s@deb.debian.org@${APK_MIRROR_ARG}@g" /etc/apt/sources.list.d/debian.sources; \
    fi && \
    apt-get update && \
    apt-get install -y git build-essential libsqlite3-dev curl

# Install migrate tool
RUN go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest

# Copy go mod files. go.mod replace-points anydoc at ./third_party/anydoc-go,
# so that module's go.mod must exist before `go mod download`.
COPY go.mod go.sum ./
COPY third_party/anydoc-go/go.mod third_party/anydoc-go/go.mod
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd/download cmd/download
# 国内网络访问 extensions.duckdb.org 不稳定：预置本地扩展镜像时离线安装
# （.build-cache/duckdb/<version>/<platform>/<ext>.duckdb_extension，解压版）。
# 目录常驻 .gitkeep，扩展文件不入库；缺失对应文件时 duckdb.go 回退在线安装，
# 未预置缓存的构建行为不变。
COPY .build-cache/duckdb .build-cache/duckdb
ENV DUCKDB_EXTENSION_DIR=/app/.build-cache/duckdb
RUN go run cmd/download/duckdb/duckdb.go
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod bash ./scripts/copy-licenses.sh /license-bundle

# Get version and commit info for build injection
ARG VERSION_ARG
ARG COMMIT_ID_ARG
ARG BUILD_TIME_ARG
ARG GO_VERSION_ARG

# Set build-time variables
ENV VERSION=${VERSION_ARG}
ENV COMMIT_ID=${COMMIT_ID_ARG}
ENV BUILD_TIME=${BUILD_TIME_ARG}
ENV GO_VERSION=${GO_VERSION_ARG}

# Link the anydoc parser engine (office docs converted in-process, no
# Python docreader). Default on so Hub / compose images ship a working
# engine; pass WITH_ANYDOC=0 to skip the Rust toolchain (~few minutes and
# ~1 GB of build-stage layers).
ARG WITH_ANYDOC=1
ENV RUSTUP_HOME=/usr/local/rustup CARGO_HOME=/usr/local/cargo
ENV PATH=/usr/local/cargo/bin:$PATH
# 同 browserskill 阶段：rustup/cargo 走 rsproxy.cn（见上方说明）
ARG RUSTUP_DIST_ARG=https://rsproxy.cn
ENV RUSTUP_DIST_SERVER=${RUSTUP_DIST_ARG} RUSTUP_UPDATE_ROOT=${RUSTUP_DIST_ARG}/rustup
RUN mkdir -p /usr/local/cargo && printf '%s\n' \
    '[source.crates-io]' 'replace-with = "rsproxy-sparse"' \
    '[source.rsproxy-sparse]' 'registry = "sparse+https://rsproxy.cn/index/"' \
    '[net]' 'git-fetch-with-cli = true' \
    > /usr/local/cargo/config.toml
RUN --mount=type=cache,target=/usr/local/cargo/registry \
    --mount=type=cache,target=/usr/local/cargo/git \
    if [ "$WITH_ANYDOC" = "1" ]; then \
        curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs \
            | sh -s -- -y --profile minimal --default-toolchain stable && \
        ./scripts/build-anydoc-lib.sh; \
    fi

# Build the application with version info
RUN --mount=type=cache,target=/go/pkg/mod \
    if [ "$WITH_ANYDOC" = "1" ]; then \
        make build-prod GO_BUILD_TAGS=anydoc; \
    else \
        make build-prod; \
    fi
RUN --mount=type=cache,target=/go/pkg/mod cp -r /go/pkg/mod/github.com/yanyiwu/ /app/yanyiwu/

# Final stage
FROM docker.m.daocloud.io/library/debian:12.12-slim

WORKDIR /app

ARG APK_MIRROR_ARG

# Pairing derives the gateway URL from the user's page origin by default.
ENV BROWSERSKILL_BINARY=/opt/weknora/browserskill/bsk \
    BROWSERSKILL_EXTENSION_PATH=/opt/weknora/browserskill/browser-skill-weknora-0.3.0.zip
COPY --from=browserskill /opt/weknora/browserskill /opt/weknora/browserskill

# Create a non-root user first
RUN useradd -m -s /bin/bash appuser

# First, install ca-certificates without mirror to ensure HTTPS works
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates && \
    rm -rf /var/lib/apt/lists/*

# Then switch to mirror if specified and install other packages
RUN if [ -n "$APK_MIRROR_ARG" ]; then \
        sed -i "s@deb.debian.org@${APK_MIRROR_ARG}@g" /etc/apt/sources.list.d/debian.sources; \
    fi && \
    apt-get update && \
    apt-get install -y --no-install-recommends \
        build-essential postgresql-client default-mysql-client tzdata sed curl bash vim wget \
        libsqlite3-0 \
        python3 python3-pip python3-dev libffi-dev libssl-dev \
        nodejs npm \
        gosu \
        ffmpeg && \
    python3 -m pip install --break-system-packages --upgrade pip setuptools wheel && \
    mkdir -p /home/appuser/.local/bin && \
    curl -LsSf https://astral.sh/uv/install.sh | CARGO_HOME=/home/appuser/.cargo UV_INSTALL_DIR=/home/appuser/.local/bin sh && \
    chown -R appuser:appuser /home/appuser && \
    ln -sf /home/appuser/.local/bin/uvx /usr/local/bin/uvx && \
    chmod +x /usr/local/bin/uvx && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

# Create data directories and set permissions
RUN mkdir -p /data/files && \
    chown -R appuser:appuser /app /data/files

# Copy migrate tool from builder stage
COPY --from=builder /go/bin/migrate /usr/local/bin/
COPY --from=builder /app/yanyiwu/ /go/pkg/mod/github.com/yanyiwu/

# Copy the binary from the builder stage
COPY --from=builder /app/config ./config
COPY --from=builder /app/scripts ./scripts
COPY --from=builder /app/migrations ./migrations
COPY --from=builder /app/dataset/samples ./dataset/samples
COPY --from=builder /root/.duckdb /home/appuser/.duckdb
COPY --from=builder /app/WeKnora .
COPY --from=builder /license-bundle/ ./

# Copy and make entrypoint script executable
COPY --from=builder /app/scripts/docker-entrypoint.sh ./scripts/docker-entrypoint.sh

# Make scripts executable
RUN chmod +x ./scripts/*.sh

# Expose ports
EXPOSE 8080


ENTRYPOINT ["./scripts/docker-entrypoint.sh"]
CMD ["./WeKnora"]
