#!/usr/bin/env bash
#
# 交叉编译 vmbackup-partition 到 Linux amd64，并校验产物。
#
# 用法：
#   scripts/build-linux-amd64.sh                 # 使用 go.mod 声明的官方库版本
#   VERSION=v1.0.1 scripts/build-linux-amd64.sh
#   VM_SRC=/path/to/VictoriaMetrics scripts/build-linux-amd64.sh   # 用源码检出替换官方库
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

APP=vmbackup-partition
VERSION="${VERSION:-v1.0.0}"
VM_VERSION="${VM_VERSION:-v1.150.0}"
OUT_DIR="${OUT_DIR:-$ROOT/bin}"
GO="${GO:-go}"

if ! command -v "$GO" >/dev/null 2>&1; then
  echo "错误：找不到 go 可执行文件（可用 GO=/path/to/go 指定）" >&2
  exit 1
fi

GO_VERSION="$("$GO" env GOVERSION)"
echo "== 构建环境 =="
echo "Go          : $GO_VERSION ($("$GO" env GOOS)/$("$GO" env GOARCH))"
echo "目标平台    : linux/amd64"
echo "工具版本    : $APP $VERSION (based on VictoriaMetrics $VM_VERSION)"
echo

# 若指定了官方源码检出目录，则用 replace 指向它，以便离线构建或锁定到特定 commit。
if [[ -n "${VM_SRC:-}" ]]; then
  if [[ ! -f "$VM_SRC/go.mod" ]]; then
    echo "错误：VM_SRC=$VM_SRC 下没有 go.mod" >&2
    exit 1
  fi
  echo "== 使用本地官方源码：$VM_SRC =="
  echo "   该目录会被追加到临时 modfile 的 replace 指令中，不修改仓库内的 go.mod"
  MODFILE="$(mktemp -d)/build.mod"
  cp go.mod "$MODFILE"
  cp go.sum "$(dirname "$MODFILE")/build.sum"
  cat >>"$MODFILE" <<EOF

replace github.com/VictoriaMetrics/VictoriaMetrics => $VM_SRC
EOF
  GOFLAGS_EXTRA=(-modfile="$MODFILE")
  trap 'rm -rf "$(dirname "$MODFILE")"' EXIT
else
  GOFLAGS_EXTRA=()
fi

mkdir -p "$OUT_DIR"

BUILDINFO_PKG="github.com/VictoriaMetrics/VictoriaMetrics/lib/buildinfo"
LDFLAGS="-s -w -X '${BUILDINFO_PKG}.Version=${APP} ${VERSION} based-on VictoriaMetrics ${VM_VERSION}'"

echo "== 编译 =="
# bash 3.2（macOS 自带）在 set -u 下展开空数组会报 unbound variable，故用 ${arr[@]+...} 惯用法
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 "$GO" build \
  -trimpath \
  ${GOFLAGS_EXTRA[@]+"${GOFLAGS_EXTRA[@]}"} \
  -ldflags "$LDFLAGS" \
  -o "$OUT_DIR/${APP}-linux-amd64" \
  ./cmd/${APP}

echo
echo "== 产物校验 =="
ls -l "$OUT_DIR/${APP}-linux-amd64"
if command -v file >/dev/null 2>&1; then
  file "$OUT_DIR/${APP}-linux-amd64"
fi

# 生成校验和（macOS 用 shasum，Linux 用 sha256sum）
( cd "$OUT_DIR" && { shasum -a 256 "${APP}-linux-amd64" 2>/dev/null || sha256sum "${APP}-linux-amd64"; } > "${APP}-linux-amd64.sha256" )
cat "$OUT_DIR/${APP}-linux-amd64.sha256"

echo
echo "交付物："
echo "  $OUT_DIR/${APP}-linux-amd64"
echo "  $OUT_DIR/${APP}-linux-amd64.sha256"
echo
echo "在 Linux amd64 目标机上的校验方式："
echo "  sha256sum -c ${APP}-linux-amd64.sha256"
echo "  ./${APP}-linux-amd64 -version"
