# 多阶段构建：先编译，再打包最小运行时镜像
FROM golang:1.22-alpine AS builder

WORKDIR /src

# 先复制依赖清单，利用层缓存
COPY go.mod ./
RUN go mod download 2>/dev/null || true

COPY . .

# 静态编译，便于放进 scratch/alpine 运行
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/rerank-gateway ./cmd/server

# 运行阶段
FROM alpine:3.20

RUN addgroup -S app && adduser -S app -G app

COPY --from=builder /out/rerank-gateway /usr/local/bin/rerank-gateway

USER app

# 暴露服务端口（可用 SERVER_ADDR 覆盖）
EXPOSE 8080

ENTRYPOINT ["rerank-gateway"]
