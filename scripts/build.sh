#!/bin/bash
set -euo pipefail

readonly APP_NAME="octopus" # 发布产物和容器内的可执行文件名。
readonly OUTPUT_DIR="build" # 所有构建、归档和容器输入的根目录。
# workflow 已校验的发布 tag 优先经环境变量传入，避免本地 git describe 与发布事实不一致。
if [ -z "${VERSION:-}" ]; then
    VERSION="$(git describe --tags --abbrev=0 2>/dev/null || echo 'dev')"
fi
readonly VERSION # 当前发布版本。
readonly COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo 'unknown')" # 当前提交短哈希。
readonly LDFLAGS="-X 'github.com/bestruirui/${APP_NAME}/internal/conf.Version=${VERSION}' \
                  -X 'github.com/bestruirui/${APP_NAME}/internal/conf.BuildTime=$(TZ='Asia/Shanghai' date +'%F %T %z')' \
                  -X 'github.com/bestruirui/${APP_NAME}/internal/conf.Author=shenyanshu' \
                  -X 'github.com/bestruirui/${APP_NAME}/internal/conf.Commit=${COMMIT}' \
                  -s -w" # 注入版本信息并缩小发布二进制。

build_target() {
    # 固定矩阵直接使用 GOOS/GOARCH，全部无 cgo 依赖。
    local os go_arch
    IFS=: read -r os go_arch <<<"$1"
    echo "Building ${os}/${go_arch}"
    env GOOS="${os}" GOARCH="${go_arch}" CGO_ENABLED=0 \
        go build -trimpath -o "${OUTPUT_DIR}/bin/${APP_NAME}-${os}-${go_arch}" -ldflags="${LDFLAGS}" -tags=jsoniter .
}

readonly -a TARGETS=(
    "linux:amd64"
    "linux:arm64"
    "windows:amd64"
    "windows:arm64"
    "darwin:amd64"
    "darwin:arm64"
) # 与 Docker 多架构输入对齐的固定发布矩阵。

# 构建工具不会创建父目录，因此只保留一次直接创建。
mkdir -p "${OUTPUT_DIR}/bin" "${OUTPUT_DIR}/archives" \
    "${OUTPUT_DIR}/docker/linux/amd64" "${OUTPUT_DIR}/docker/linux/arm64"
rm -f "${OUTPUT_DIR}"/bin/"${APP_NAME}"-* "${OUTPUT_DIR}"/archives/*.zip "${OUTPUT_DIR}/archives/SHA256SUMS"

echo "Building ${APP_NAME} ${VERSION} (${COMMIT})"
for target in "${TARGETS[@]}"; do
    build_target "${target}"
done

# Docker buildx 按 TARGETPLATFORM 读取固定目录中的同名可执行文件。
cp "${OUTPUT_DIR}/bin/${APP_NAME}-linux-amd64" "${OUTPUT_DIR}/docker/linux/amd64/${APP_NAME}"
cp "${OUTPUT_DIR}/bin/${APP_NAME}-linux-arm64" "${OUTPUT_DIR}/docker/linux/arm64/${APP_NAME}"

# 每个平台只替换可执行文件名，许可证报告和发布文档保持一致。
GOFLAGS="-tags=jsoniter" go run github.com/google/go-licenses/v2@v2.0.1 report . \
    --ignore "github.com/bestruirui/${APP_NAME}" >"${OUTPUT_DIR}/THIRD_PARTY_LICENSES.csv"
cp README.md LICENSE "${OUTPUT_DIR}/THIRD_PARTY_LICENSES.csv" "${OUTPUT_DIR}/archives/"
for file in "${OUTPUT_DIR}"/bin/"${APP_NAME}"-*; do
    archive_name="$(basename "${file}").zip"
    executable_name="${APP_NAME}"
    if [[ "${file}" == *-windows-* ]]; then
        executable_name="${APP_NAME}.exe"
    fi
    cp "${file}" "${OUTPUT_DIR}/archives/${executable_name}"
    (cd "${OUTPUT_DIR}/archives" && zip -q "${archive_name}" "${executable_name}" README.md LICENSE THIRD_PARTY_LICENSES.csv)
    rm -f "${OUTPUT_DIR:?}/archives/${executable_name}"
done
rm -f "${OUTPUT_DIR:?}/archives/README.md" "${OUTPUT_DIR}/archives/LICENSE" \
    "${OUTPUT_DIR}/archives/THIRD_PARTY_LICENSES.csv"
(cd "${OUTPUT_DIR}/archives" && sha256sum ./*.zip >SHA256SUMS)
echo "Artifacts: ${OUTPUT_DIR}/archives"
