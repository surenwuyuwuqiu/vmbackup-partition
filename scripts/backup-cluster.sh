#!/usr/bin/env bash
#
# 多节点 vm-cluster 备份编排脚本。
#
# 背景：VictoriaMetrics 集群版里，每个 vmstorage 节点的数据是**相互独立**的，
# 官方要求对每个 vmstorage 节点分别执行一次备份，且各节点的备份必须写入**各自独立的目录**
# （因为不同节点的 part 路径可能相同但内容不同，混写会互相删除/覆盖）。
#
# 本脚本从一个控制机出发，通过 SSH 在每台 vmstorage 上完成：
#   1. 由本工具调用该节点本地的 /snapshot/create 创建快照（-snapshot.createURL）
#   2. 按月份区间备份到 <DstPrefix>/<nodeName>
#   3. 备份结束后由本工具自动调用 /snapshot/delete 删除快照
#
# 用法示例：
#   bash scripts/backup-cluster.sh \
#       --node vmstorage-0=10.0.0.11 \
#       --node vmstorage-1=10.0.0.12 \
#       --node vmstorage-2=10.0.0.13 \
#       --ssh-user root \
#       --tool /usr/local/bin/vmbackup-partition \
#       --storage-data-path /var/lib/victoria-metrics-data \
#       --vmstorage-port 8482 \
#       --from-month 2026_02 --to-month 2026_06 \
#       --dst-prefix s3://my-bucket/vm-cluster/2026H1
#
# 先试运行（不写目标端，也不需要远端凭据）：
#   ... --dry-run
#
set -euo pipefail

NODES=()
SSH_USER=""
SSH_OPTS="${SSH_OPTS:-}"
TOOL="/usr/local/bin/vmbackup-partition"
STORAGE_DATA_PATH="/var/lib/victoria-metrics-data"
VMSTORAGE_PORT="8482"
FROM_MONTH=""
TO_MONTH=""
DST_PREFIX=""
DRY_RUN="0"
PRUNE="true"
CONCURRENCY="10"
EXTRA_ARGS=""

usage() {
  sed -n '2,32p' "$0"
  exit "${1:-0}"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --node) NODES+=("$2"); shift 2 ;;
    --ssh-user) SSH_USER="$2"; shift 2 ;;
    --tool) TOOL="$2"; shift 2 ;;
    --storage-data-path) STORAGE_DATA_PATH="$2"; shift 2 ;;
    --vmstorage-port) VMSTORAGE_PORT="$2"; shift 2 ;;
    --from-month) FROM_MONTH="$2"; shift 2 ;;
    --to-month) TO_MONTH="$2"; shift 2 ;;
    --dst-prefix) DST_PREFIX="$2"; shift 2 ;;
    --concurrency) CONCURRENCY="$2"; shift 2 ;;
    --prune) PRUNE="$2"; shift 2 ;;
    --extra-args) EXTRA_ARGS="$2"; shift 2 ;;
    --dry-run) DRY_RUN="1"; shift ;;
    -h|--help) usage 0 ;;
    *) echo "未知参数：$1" >&2; usage 1 ;;
  esac
done

[[ ${#NODES[@]} -gt 0 ]] || { echo "错误：至少需要一个 --node name=host" >&2; exit 1; }
[[ -n "$FROM_MONTH" && -n "$TO_MONTH" ]] || { echo "错误：--from-month 与 --to-month 必填" >&2; exit 1; }
if [[ "$DRY_RUN" != "1" ]]; then
  [[ -n "$DST_PREFIX" ]] || { echo "错误：非 --dry-run 模式下 --dst-prefix 必填" >&2; exit 1; }
fi

SSH_BASE=(ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new)
if [[ -n "$SSH_OPTS" ]]; then
  # shellcheck disable=SC2206
  SSH_BASE+=($SSH_OPTS)
fi
SSH_TARGET_PREFIX=""
if [[ -n "$SSH_USER" ]]; then
  SSH_TARGET_PREFIX="${SSH_USER}@"
fi

echo "================================================================"
echo " vm-cluster 按月份分区备份"
echo " 节点      : ${NODES[*]}"
echo " 月份区间  : ${FROM_MONTH} .. ${TO_MONTH}"
echo " 存储路径  : ${STORAGE_DATA_PATH}（各节点本机路径）"
echo " 远端工具  : ${TOOL}（各节点本机路径）"
echo " 目标前缀  : ${DST_PREFIX:-<试运行，不写目标端>}"
echo " -prune    : ${PRUNE}"
echo " 试运行    : ${DRY_RUN}"
echo "================================================================"
echo

FAILED=0
SUCCEEDED=()

for spec in "${NODES[@]}"; do
  name="${spec%%=*}"
  host="${spec#*=}"
  [[ "$name" != "$spec" ]] || { echo "错误：--node 需要 name=host 形式，收到 ${spec}" >&2; exit 1; }

  echo "──────────────────────────────────────────────"
  echo "节点 ${name} (${host})"
  echo "──────────────────────────────────────────────"

  remote_cmd="$TOOL -storageDataPath=$(printf '%q' "$STORAGE_DATA_PATH")"
  remote_cmd="$remote_cmd -snapshot.createURL=http://127.0.0.1:${VMSTORAGE_PORT}/snapshot/create"
  remote_cmd="$remote_cmd -fromMonth=$(printf '%q' "$FROM_MONTH") -toMonth=$(printf '%q' "$TO_MONTH")"
  remote_cmd="$remote_cmd -concurrency=${CONCURRENCY} -prune=${PRUNE}"

  if [[ "$DRY_RUN" == "1" ]]; then
    remote_cmd="$remote_cmd -dryRun -planOut=/tmp/backup-plan-${name}.json"
  else
    remote_cmd="$remote_cmd -dst=$(printf '%q' "${DST_PREFIX}/${name}")"
  fi
  if [[ -n "$EXTRA_ARGS" ]]; then
    remote_cmd="$remote_cmd ${EXTRA_ARGS}"
  fi

  echo "远端命令："
  echo "  ${remote_cmd}"
  echo

  rc=0
  "${SSH_BASE[@]}" "${SSH_TARGET_PREFIX}${host}" "$remote_cmd" || rc=$?
  if [[ $rc -eq 0 ]]; then
    SUCCEEDED+=("$name")
    echo "[OK] 节点 ${name} 完成"
  else
    FAILED=$((FAILED + 1))
    echo "[FAIL] 节点 ${name} 失败（退出码 ${rc}）" >&2
  fi
  echo
done

echo "================================================================"
echo " 汇总：成功 ${#SUCCEEDED[@]} 个节点，失败 ${FAILED} 个节点"
if [[ ${#SUCCEEDED[@]} -gt 0 ]]; then
  echo " 成功：${SUCCEEDED[*]}"
fi
echo "================================================================"
echo
echo "还原提示：每个节点需要分别还原到该节点自己的数据目录，例如"
for spec in "${NODES[@]}"; do
  name="${spec%%=*}"
  echo "  # 在 ${name} 上执行："
  echo "  vmrestore -src=${DST_PREFIX:-<dst>}/${name} -storageDataPath=${STORAGE_DATA_PATH}"
done
echo
echo "注意：各节点的备份不能合并到一个目录（part 路径会互相覆盖），也不要互相还原。"
echo "      集群的 -replicationFactor>1 会造成节点间数据重叠；如需合并为单实例历史库，"
echo "      请参考 ahfuzhang 的 vmfile 工具思路，见 docs/01-调研与设计.md 的「延伸阅读」。"

[[ $FAILED -eq 0 ]]
