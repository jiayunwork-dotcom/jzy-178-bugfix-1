# syntax=docker/dockerfile:1

# ---- 构建阶段：官方 golang:1.22-alpine ----
FROM golang:1.22-alpine AS build
WORKDIR /src

# 先单独拷模块描述，利用层缓存拉依赖
COPY go.mod go.sum ./
RUN go mod download

# 拷贝源码并静态编译（CGO_ENABLED=0，纯 Go，无 libc 依赖）
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/raim-server ./cmd/server

# ---- 运行阶段：最小 alpine ----
FROM alpine:3.20
RUN adduser -D -u 10001 raim && apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /out/raim-server /app/raim-server

# 运行档与会话文件落在挂载进来的目录
ENV RAIM_DATA_DIR=/data
RUN mkdir -p /data && chown raim:raim /data
VOLUME ["/data"]
USER raim
EXPOSE 8080
ENTRYPOINT ["/app/raim-server", "--data", "/data"]
