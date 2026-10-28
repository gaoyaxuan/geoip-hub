# ----------------- 构建阶段 -----------------
ARG GO_VERSION=1.26.10
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
WORKDIR /src

# 1. 下载依赖 (利用 Docker 缓存机制)
# 只有 go.mod 变动时才重新下载依赖；需要时可用 --build-arg GOPROXY=<代理地址> 指定 Go 模块代理
ARG GOPROXY
COPY go.mod go.sum ./
RUN go mod download

# 2. 复制源码
COPY . .

# 3. 编译优化
# -ldflags="-s -w" : 去除符号表和调试信息 (减小体积)
# -trimpath        : 移除文件系统路径信息 (安全性)
ARG TARGETOS TARGETARCH
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH CGO_ENABLED=0 \
    go build -ldflags="-s -w" -trimpath -o /app/geoip-hub .

# 4. 准备只有必要文件的 "根文件系统"
# 创建一个非 root 用户
RUN echo "appuser:x:10001:10001::/app:/sbin/nologin" > /app/passwd && \
    echo "appuser:x:10001:" > /app/group

# 安装 CA 根证书，供运行阶段复制
RUN apk add --no-cache ca-certificates

# ----------------- 运行阶段 (Scratch) -----------------
FROM scratch

WORKDIR /app

# 1. 从构建层只复制必要文件
COPY --from=build /app/passwd /etc/passwd
COPY --from=build /app/group /etc/group
# CA 根证书：scratch 是空镜像，没有任何证书。程序访问 HTTPS 接口（ipwho.is、ip-api pro 版）时
# 要用它校验对方服务器的证书，缺少时请求会报 x509: certificate signed by unknown authority
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# 2. 复制二进制文件
COPY --from=build /app/geoip-hub /app/geoip-hub

# 3. 复制数据文件 (如果有默认文件想打进镜像)
# COPY ip2region_v4.xdb /app/ip2region_v4.xdb

# 时区默认为 UTC，只影响日志时间；运行时用 TZ 环境变量指定（时区数据已内嵌在程序中）

# 4. 指定非 root 用户运行
USER appuser:appuser

EXPOSE 8080

CMD [ "/app/geoip-hub" ]
