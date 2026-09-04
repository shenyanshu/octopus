# syntax=docker/dockerfile:1

# 前端阶段：在容器内从源码构建，产物输出到 /src/static/out，不依赖宿主机预构建文件。
FROM node:24-alpine AS frontend
# 工作流固定使用 pnpm 11.10.0，此处保持同一版本保证 lockfile 行为一致。
RUN npm install -g pnpm@11.10.0
WORKDIR /src/web
ARG VERSION=dev
COPY web/package.json web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN VITE_APP_VERSION="${VERSION}" pnpm run build

# 后端阶段：按 go.mod 声明的精确 Go 版本构建，嵌入刚构建的前端产物。
FROM golang:1.26.4-alpine AS backend
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /src/static/out ./static/out
# TARGETOS/TARGETARCH 由 buildx 按目标平台自动提供，配合 QEMU 覆盖 amd64/arm64。
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
# ldflags 与 scripts/build.sh 保持同一注入行为：版本、构建时间、作者、提交。
RUN set -eu; \
    ldflags="-X 'github.com/bestruirui/octopus/internal/conf.Version=${VERSION}' \
             -X 'github.com/bestruirui/octopus/internal/conf.BuildTime=$(TZ='Asia/Shanghai' date +'%F %T %z')' \
             -X 'github.com/bestruirui/octopus/internal/conf.Author=shenyanshu' \
             -X 'github.com/bestruirui/octopus/internal/conf.Commit=${COMMIT}' \
             -s -w"; \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -trimpath -tags=jsoniter -ldflags="${ldflags}" -o /out/octopus .
# 第三方许可证报告在容器内生成，保持与旧发布产物一致的合规信息。
RUN GOFLAGS="-tags=jsoniter" go run github.com/google/go-licenses/v2@v2.0.1 report . \
    --ignore "github.com/bestruirui/octopus" > /out/THIRD_PARTY_LICENSES.csv

# 运行阶段：保留原镜像的时区、入口与权限降级行为。
FROM alpine
ARG APP_NAME=octopus
ENV TZ=Asia/Shanghai \
    APP_NAME=octopus

RUN apk add --no-cache alpine-conf ca-certificates su-exec && \
    /usr/sbin/setup-timezone -z Asia/Shanghai && \
    apk del alpine-conf && \
    rm -rf /var/cache/apk/* && \
    mkdir -p /app

COPY --from=backend /out/octopus /app/${APP_NAME}
COPY --from=backend /out/THIRD_PARTY_LICENSES.csv /app/THIRD_PARTY_LICENSES.csv
COPY LICENSE /app/LICENSE
COPY scripts/dockerfile/entrypoint.sh /entrypoint.sh

RUN chmod +x /entrypoint.sh

CMD ["/entrypoint.sh"]
