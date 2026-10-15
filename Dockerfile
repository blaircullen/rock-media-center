# syntax=docker/dockerfile:1

# Memory. Docker builds the stages side by side, and the web UI build and the Go compile
# peaked together at several GB, which on a small server got one of them killed part-way
# with only the last line ("go build ... exit code 1") to show for it.
#
# So the big compiles (the web UI and the Go binary) run through docker/build-heavy under
# one shared lock — the cache mount with id=arrmada-build-lock and sharing=locked, which one
# step holds at a time — and build-heavy fits each one's parallel jobs to the free memory.
# ARRMADA_BUILD_JOBS (a build argument) fixes the job count.

# --- Stage 1: build the web UI ---
FROM node:24-alpine AS web
WORKDIR /src/web
COPY --chmod=755 docker/build-heavy /usr/local/bin/build-heavy
# Install deps first (cached unless the lockfile changes).
COPY web/package.json web/package-lock.json ./
RUN npm ci
# Build the SPA. Vite emits to ../internal/webui/dist (outside web/), so make it.
COPY web/ ./
ARG ARRMADA_BUILD_JOBS
RUN --mount=type=cache,id=arrmada-build-lock,target=/run/build-lock,sharing=locked \
    mkdir -p /src/internal/webui/dist && build-heavy 800 'npm run build'

# --- Stage 2: build the static Go binary (UI embedded) ---
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY --chmod=755 docker/build-heavy /usr/local/bin/build-heavy
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Overwrite the embed dir with the freshly built UI from stage 1.
COPY --from=web /src/internal/webui/dist ./internal/webui/dist
ARG VERSION=docker
ARG COMMIT=unknown
ARG ARRMADA_BUILD_JOBS
# CGO off → fully static (modernc.org/sqlite is pure Go), so it runs on scratch. The
# compiler's memory is mostly the one big SQLite package, so it isn't split into jobs.
RUN --mount=type=cache,id=arrmada-build-lock,target=/run/build-lock,sharing=locked \
    build-heavy 900 'CGO_ENABLED=0 go build \
    -ldflags "-s -w \
      -X github.com/tristenlammi/arrmada/internal/buildinfo.Version=${VERSION} \
      -X github.com/tristenlammi/arrmada/internal/buildinfo.Commit=${COMMIT}" \
    -o /out/arrmada ./cmd/arrmada'

# --- Stage 3: runtime ---
#
# Ubuntu 24.04 + jellyfin-ffmpeg, NOT Alpine's ffmpeg package: the app reads media files
# with ffprobe (internal/mediainfo), and jellyfin-ffmpeg is the build the project has
# verified there.
FROM ubuntu:24.04
# gosu is the su-exec equivalent: the entrypoint drops from root to a configurable PUID/PGID.
# apprise (Python) is bundled for notifications — one image, 80+ services, no extra container.
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl flac gnupg gosu python3 python3-pip && \
    mkdir -p /etc/apt/keyrings && \
    curl -fsSL https://repo.jellyfin.org/jellyfin_team.gpg.key \
        | gpg --dearmor -o /etc/apt/keyrings/jellyfin.gpg && \
    echo "deb [signed-by=/etc/apt/keyrings/jellyfin.gpg arch=amd64] https://repo.jellyfin.org/ubuntu noble main" \
        > /etc/apt/sources.list.d/jellyfin.list && \
    apt-get update && apt-get install -y --no-install-recommends jellyfin-ffmpeg7 && \
    rm -rf /var/lib/apt/lists/* && \
    ln -sf /usr/lib/jellyfin-ffmpeg/ffmpeg /usr/local/bin/ffmpeg && \
    ln -sf /usr/lib/jellyfin-ffmpeg/ffprobe /usr/local/bin/ffprobe && \
    pip3 install --no-cache-dir --break-system-packages apprise && \
    apprise --version && ffmpeg -version | head -1 && flac --version | head -1 && metaflac --version | head -1 && \
    mkdir -p /data /media/downloads /media/library

# yt-dlp for the music engine's last-resort YouTube lane (internal/ytaudio). NOT the Ubuntu
# package: distro releases lag behind the YouTube changes that break old yt-dlp versions.
# The official zipapp release is pinned to a version and checked against the SHA-256 that
# the release publishes in its SHA2-256SUMS file; it runs on the python3 installed above.
# To update: change both values together (checksum from the release's SHA2-256SUMS).
ARG YT_DLP_VERSION=2026.08.19
ARG YT_DLP_SHA256=1fa6733c37ea6fb51c99ad8fe785e7b7e5f3246c9b980230329d4fb72ed8d4d6
RUN curl -fsSL -o /usr/local/bin/yt-dlp \
        "https://github.com/yt-dlp/yt-dlp/releases/download/${YT_DLP_VERSION}/yt-dlp" && \
    echo "${YT_DLP_SHA256}  /usr/local/bin/yt-dlp" | sha256sum -c - && \
    chmod 0755 /usr/local/bin/yt-dlp && \
    yt-dlp --version

COPY --from=build /out/arrmada /usr/local/bin/arrmada
COPY docker/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# Runs as root only long enough to fix data-dir ownership, then drops to PUID:PGID.
ENV ARRMADA_HOST=0.0.0.0 \
    ARRMADA_PORT=7878 \
    ARRMADA_DATA_DIR=/data \
    ARRMADA_LIBRARY_DIR=/media/library \
    ARRMADA_DOWNLOADS_DIR=/media/downloads \
    PUID=1000 \
    PGID=1000
EXPOSE 7878
VOLUME ["/data", "/media"]

# curl, not wget: the Ubuntu runtime has no wget, so the old check failed forever and
# Docker/Unraid showed the container as unhealthy. The first boot runs migrations, hence
# the minute's grace.
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
    CMD curl -fsS -o /dev/null "http://127.0.0.1:7878/api/health" || exit 1

ENTRYPOINT ["/entrypoint.sh"]

# What update.sh reads before it calls into an image. org.arrmada.cli says the binary
# understands `arrmada <command>`: an older one ignores its arguments and starts a whole
# second server, so scripts never pass arguments to an image without this label.
LABEL org.arrmada.cli="1"
# This build can stage a restore itself (`arrmada restore`), which update.sh --rollback --with-db uses.
LABEL org.arrmada.restore="1"
# Declared last so a new commit only changes the image's metadata, never the cached
# layers above.
ARG VERSION=docker
ARG COMMIT=unknown
LABEL org.arrmada.version="${VERSION}" org.arrmada.commit="${COMMIT}"
