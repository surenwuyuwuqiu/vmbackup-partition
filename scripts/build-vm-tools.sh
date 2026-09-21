#!/usr/bin/env bash
#
# 从 VictoriaMetrics 官方源码构建验证所需的官方二进制。
#
# 这些二进制只用于「对照实验」与「还原验证」，不参与本工具的构建：
#   victoria-metrics  产生真实存储与快照（与 vmstorage 共用 lib/storage，布局完全一致）
#   vmrestore         还原本工具的备份，验证格式兼容
#   vmbackup          全量对照备份，用于等价性断言
#
# 用法：
#   VM_SRC=/path/to/VictoriaMetrics bash scripts/build-vm-tools.sh
#   VM_SRC=... OUT=/tmp/vm-bin TAG=v1.150.0 bash scripts/build-vm-tools.sh
#
set -euo pipefail

VM_SRC="${VM_SRC:-}"
OUT="${OUT:-/tmp/vm-bin}"
TAG="${TAG:-v1.150.0}"
GO="${GO:-go}"
APPS="${APPS:-victoria-metrics vmrestore vmbackup}"

[[ -n "$VM_SRC" ]] || {
  echo "用法：VM_SRC=<VictoriaMetrics 源码目录> bash $0" >&2
  echo "  例如：" >&2
  echo "    git clone --depth 1 --branch ${TAG} https://github.com/VictoriaMetrics/VictoriaMetrics.git /tmp/VictoriaMetrics" >&2
  echo "    VM_SRC=/tmp/VictoriaMetrics bash $0" >&2
  exit 1
}
[[ -f "$VM_SRC/go.mod" ]] || { echo "错误：${VM_SRC} 下没有 go.mod" >&2; exit 1; }

echo "== 构建官方二进制 =="
echo "源码目录: ${VM_SRC}"
echo "版本     : $(cd "$VM_SRC" && git describe --tags 2>/dev/null || echo unknown)"
echo "输出目录: ${OUT}"
echo "Go       : $($GO env GOVERSION)"
echo

mkdir -p "$OUT"
cd "$VM_SRC"

# 官方仓库自带 vendor 目录，使用 -mod=vendor 可完全离线构建。
MODFLAG=""
[[ -d "$VM_SRC/vendor" ]] && MODFLAG="-mod=vendor"

for a in $APPS; do
  echo "--- 构建 ${a}"
  # shellcheck disable=SC2086
  "$GO" build $MODFLAG -o "${OUT}/${a}" "./app/${a}"
  chmod +x "${OUT}/${a}"
done

echo
echo "== 完成 =="
ls -l "$OUT"
echo
echo "后续可用："
echo "  VMBIN=${OUT} bash scripts/e2e-real.sh"
echo "  VMBIN=${OUT} bash scripts/e2e-synthetic.sh"
