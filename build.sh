#!/bin/sh
# Build NASArchiveSync on Linux (or any host Go supports).
#
# Usage:
#   ./build.sh                          # build for the current OS/architecture
#   ./build.sh --version 0.7.1          # inject a version number
#   ./build.sh -o dist/NASArchiveSync-arm64
#   GOOS=linux GOARCH=arm64 ./build.sh  # cross compile
#
# The binary is statically linked by default (CGO_ENABLED=0), which makes it
# easy to copy to a NAS or a minimal Linux distribution.

set -eu

version="0.7.1"
output=""
go_os="${GOOS:-}"
go_arch="${GOARCH:-}"

usage() {
    cat <<'EOF'
用法: ./build.sh [选项]

选项:
  -v, --version VER   注入的版本号（默认 0.7.1）
  -o, --output PATH   输出路径（默认 dist/NASArchiveSync，
                      GOOS=windows 时为 dist/NASArchiveSync.exe）
      --goos OS       目标操作系统，默认使用 go env GOOS
      --goarch ARCH   目标架构，默认使用 go env GOARCH
  -h, --help          显示本帮助

环境变量:
  GOOS / GOARCH       与 --goos/--goarch 等价
  CGO_ENABLED         默认 0，生成静态二进制

示例:
  ./build.sh
  ./build.sh --version 0.7.1 -o dist/NASArchiveSync-linux-amd64
  GOOS=linux GOARCH=arm64 ./build.sh
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        -h|--help)
            usage
            exit 0
            ;;
        -v|--version)
            [ $# -ge 2 ] || { echo "缺少 --version 的值" >&2; exit 2; }
            version="$2"
            shift 2
            ;;
        -o|--output)
            [ $# -ge 2 ] || { echo "缺少 --output 的值" >&2; exit 2; }
            output="$2"
            shift 2
            ;;
        --goos)
            [ $# -ge 2 ] || { echo "缺少 --goos 的值" >&2; exit 2; }
            go_os="$2"
            shift 2
            ;;
        --goarch)
            [ $# -ge 2 ] || { echo "缺少 --goarch 的值" >&2; exit 2; }
            go_arch="$2"
            shift 2
            ;;
        *)
            echo "未知参数: $1" >&2
            usage >&2
            exit 2
            ;;
    esac
done

[ -n "$version" ] || { echo "版本号不能为空" >&2; exit 2; }

if ! command -v go >/dev/null 2>&1; then
    echo "未找到 go，请先安装 Go 1.26 或更高版本" >&2
    exit 1
fi

root=$(CDPATH= cd "$(dirname "$0")" && pwd)
cd "$root"

[ -n "$go_os" ] || go_os=$(go env GOOS)
[ -n "$go_arch" ] || go_arch=$(go env GOARCH)

if [ -z "$output" ]; then
    case "$go_os" in
        windows) output="dist/NASArchiveSync.exe" ;;
        *)       output="dist/NASArchiveSync" ;;
    esac
fi

out_dir=$(dirname "$output")
[ "$out_dir" = "." ] || mkdir -p "$out_dir"

cgo=${CGO_ENABLED:-0}

echo "Building $output (version $version, GOOS=$go_os GOARCH=$go_arch, CGO_ENABLED=$cgo)"
CGO_ENABLED="$cgo" GOOS="$go_os" GOARCH="$go_arch" \
    go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$output" .

echo "Built $output (version $version)"
