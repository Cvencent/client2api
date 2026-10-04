# syntax=docker/dockerfile:1
# 单二进制：网关、面板与七个客户端模块全部编进同一个可执行文件。面板走 go:embed，
# 所以运行时不需要任何静态资源、不需要 node、也不需要第二个进程。
#
# 构建器固定在与 go.mod 的 `go` 指令同一个次版本上。go.mod 要求的最低版本更高时，
# 构建器版本也要跟着抬 —— 否则 go 会拒绝编译，而不是悄悄降级。
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO_ENABLED=0 得到静态链接的 musl 二进制。依赖只有 utls 与 golang.org/x/net，
# 两者都是纯 Go，关掉 cgo 不会失去任何能力；换来的是镜像里不必带 libc 工具链，
# 也不会因为构建器与运行时的 libc 版本不同而在启动时炸掉。
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/client2api ./cmd/client2api

FROM alpine:3.20
# ca-certificates：所有上游都是 https。tzdata：排程要按本地时区算「今天 4 点」这类边界。
# wget：下面的 HEALTHCHECK（busybox 自带）。
RUN apk add --no-cache ca-certificates tzdata wget \
 && adduser -D -u 10001 app \
 && mkdir -p /app/configs /app/data \
 && chown -R app:app /app
WORKDIR /app
COPY --from=build /out/client2api /app/client2api
USER app
EXPOSE 8788
# 探针读 /healthz，而不是只看端口在不在听：账号池没有任何可服务账号时它答 503，
# 所以这个探针会把「进程活着但一个能用的号都没有」的容器标成 unhealthy。
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
  CMD wget -qO- http://127.0.0.1:8788/healthz || exit 1
# -listen 是必须的：程序内建默认是 127.0.0.1:8788（单机开发用），在容器里那等于只监听
# 自己的回环，宿主机连不上。想让面板「监听地址」字段说了算，去掉这面旗子即可 ——
# 但在容器里那样做之前，先把配置文件里的 listen 改成 0.0.0.0:8788。
#
# 镜像刻意不带任何配置文件：带一份就意味着要么是别人的密钥，要么是一个全世界都知道的
# 占位密钥。首次启动时 /app/configs/client2api.json 不存在，程序会生成一个随机 api_key、
# 写进该文件并打到日志里（`client2api: generated api_key: …`）。
ENTRYPOINT ["/app/client2api", "-config", "/app/configs/client2api.json", "-listen", "0.0.0.0:8788"]
