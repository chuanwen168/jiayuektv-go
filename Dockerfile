# Go 版 KTV 服务端镜像（多阶段构建）
#
# 运行阶段依赖 ffmpeg 与 VAAPI 用户态驱动库：
#   - libva2/libva-drm2  ：VAAPI 运行时库
#   - va-driver-all      ：自动装好 Intel(iHD/i965)、AMD/Mesa(radeonsi) 等常见
#                          核显/独显的 VAAPI 驱动实现
#   - vainfo             ：诊断工具，确认容器内能否看到并枚举硬件能力
# 没有这些库时 h264_vaapi 初始化会失败，但代码里已处理"VAAPI 不可用"情形，
# 会自动回退到 libx264 软件编码，不会导致转码彻底失败。

# ---------- 构建阶段 ----------
FROM golang:1.27-bookworm AS builder

WORKDIR /build

# 先复制依赖清单以利用构建缓存。
COPY go.mod go.sum ./
RUN go env -w GOFLAGS=-mod=mod GOPROXY=https://goproxy.cn,direct GOSUMDB=off && \
    go mod download

# 复制源码并静态编译（modernc.org/sqlite 为纯 Go 实现，无需 CGO）。
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o ktv-server .

# ---------- 运行阶段 ----------
FROM debian:bookworm-slim

# 国内网络无法直连 deb.debian.org（apt update 超时），换成阿里云镜像源。
# debian:bookworm 的 apt 源在 deb822 格式的 debian.sources 中，替换域名即可。
RUN sed -i 's|deb.debian.org|mirrors.aliyun.com|g' /etc/apt/sources.list.d/debian.sources

RUN apt-get update && apt-get install -y \
      ffmpeg \
      libva2 libva-drm2 va-driver-all vainfo \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=builder /build/ktv-server .
COPY web ./web

ENV DATA_DIR=/data
ENV MV_DIR=/mv
ENV MV_NET_DIR=/mv-net
ENV SINGER_DIR=/singer
ENV PORT=8080

EXPOSE 8080
# /data 数据库/封面 · /mv 本地曲库 · /mv-net 网盘曲库 · /singer 歌手头像
VOLUME ["/data", "/mv", "/mv-net", "/singer"]

CMD ["./ktv-server"]
