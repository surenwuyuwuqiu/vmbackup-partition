#!/usr/bin/env bash
#
# 真实 VictoriaMetrics 端到端验证。
#
# 验证目标：本工具产出的“按月份筛选”备份，能被官方 vmrestore 正确还原，
# 且还原后的存储是一个可正常启动、可查询的 VictoriaMetrics，只包含所选月份。
#
# 断言策略（关键）：不硬编码“期望出现哪些月份”，而是与官方 vmbackup 的输出做等价性对比——
#   本工具在 [FROM, TO] 区间上的备份产物 == 官方 vmbackup 全量备份产物 ∩ [FROM, TO]
# 这样即便某个分区在某次运行中恰好为空（例如数据量小到没有 big part），断言依然成立且有意义。
#
# 覆盖说明：
#   data/small 与 data/indexdb 在本测试中被真实数据覆盖；
#   data/big 只有当单个分区的数据量超过 getMaxSmallPartSize()（通常数百 MB）时才会产生，
#   端到端测试里不现实，改由 scripts/e2e-synthetic.sh 用带真实文件的合成快照覆盖。
#
# 依赖：VMBIN 目录下需有 victoria-metrics、vmrestore、vmbackup 三个官方二进制，
#       可用 scripts/build-vm-tools.sh 从源码构建。
#
# 用法：
#   VMBIN=/tmp/vm-bin bash scripts/e2e-real.sh
#   VMBIN=/tmp/vm-bin KEEP=1 bash scripts/e2e-real.sh
#
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VMBIN="${VMBIN:-}"
WORK="${WORK:-/tmp/vmbackup-partition-e2e}"
KEEP="${KEEP:-0}"
PORT_MAIN="${PORT_MAIN:-18428}"
PORT_RESTORED="${PORT_RESTORED:-18429}"
CURL=(curl -s --noproxy '*')
MONTHS_ALL=(2026_01 2026_02 2026_03 2026_04 2026_05 2026_06 2026_07 2026_08 2026_09)
MONTHS_ALL_STR="${MONTHS_ALL[*]}"
FROM=2026_02
TO=2026_06
SUBDIRS=(small big indexdb)

PASS=0
FAIL=0

ok()   { echo "  [PASS] $*"; PASS=$((PASS + 1)); }
bad()  { echo "  [FAIL] $*"; FAIL=$((FAIL + 1)); }
info() { echo "── $*"; }

assert_eq() { # desc expected actual
  if [[ "$2" == "$3" ]]; then
    ok "$1 → [$3]"
  else
    bad "$1 → 期望 [$2]，实际 [$3]"
  fi
}

assert_contains() { # desc haystack needle
  if [[ "$2" == *"$3"* ]]; then ok "$1"; else bad "$1（输出中未找到 \"$3\"）"; fi
}

assert_not_contains() { # desc haystack needle
  if [[ "$2" != *"$3"* ]]; then ok "$1"; else bad "$1（输出中意外出现了 \"$3\"）"; fi
}

die() { echo "致命错误：$*" >&2; exit 2; }

VM_PIDS=()
cleanup() {
  local p
  for p in ${VM_PIDS[@]+"${VM_PIDS[@]}"}; do kill "$p" 2>/dev/null; done
  sleep 0.5
  for p in ${VM_PIDS[@]+"${VM_PIDS[@]}"}; do kill -9 "$p" 2>/dev/null; done
  if [[ "$KEEP" != "1" ]]; then
    rm -rf "$WORK"
  else
    echo "（已保留工作目录 ${WORK}）"
  fi
}
trap cleanup EXIT

# ---------------------------------------------------------------
# 前置检查
# ---------------------------------------------------------------
[[ -n "$VMBIN" ]] || die "请用 VMBIN=<官方二进制目录> 指定 victoria-metrics / vmrestore / vmbackup 所在目录"
for b in victoria-metrics vmrestore vmbackup; do
  [[ -x "$VMBIN/$b" ]] || die "缺少可执行文件 ${VMBIN}/${b}"
done
TOOL="$ROOT/bin/vmbackup-partition"
[[ -x "$TOOL" ]] || die "请先执行 make build 生成 ${TOOL}"

# 生成毫秒时间戳（跨平台，不依赖 GNU date）
ts_ms() {
  python3 -c "import sys,datetime;d=datetime.datetime.strptime(sys.argv[1],'%Y-%m-%d').replace(tzinfo=datetime.timezone.utc);print(int(d.timestamp()*1000))" "$1"
}

# 列出目录下形如 YYYY_MM 的子目录，排序后以空格连接
months_in() {
  ls -1 "$1" 2>/dev/null | grep -E '^[0-9]{4}_[0-9]{2}$' | sort | tr '\n' ' ' | sed 's/ $//'
}

# 取交集：$1 为空格分隔的月份集合，$2 为下界，$3 为上界
months_in_range() {
  local out="" m
  for m in $1; do
    if { [[ "$m" > "$2" ]] || [[ "$m" == "$2" ]]; } && { [[ "$m" < "$3" ]] || [[ "$m" == "$3" ]]; }; then
      out="$out $m"
    fi
  done
  echo "${out# }"
}

wait_http() { # port timeout_sec
  local port="$1" n="${2:-60}"
  local i
  for i in $(seq 1 "$((n * 4))"); do
    if "${CURL[@]}" -o /dev/null "http://127.0.0.1:${port}/health"; then return 0; fi
    sleep 0.25
  done
  return 1
}

rm -rf "$WORK"
mkdir -p "$WORK"

echo "================================================================"
echo " 真实 VictoriaMetrics 端到端验证"
echo " 工具      : ${TOOL}"
echo " 官方二进制: ${VMBIN}"
echo " 月份区间  : ${FROM} .. ${TO}"
echo "================================================================"
echo

# ---------------------------------------------------------------
# 1. 启动真实 VM 并灌入横跨 9 个月的数据
# ---------------------------------------------------------------
info "步骤 1：启动真实 VictoriaMetrics（端口 ${PORT_MAIN}）并灌入 ${MONTHS_ALL_STR} 数据"

"$VMBIN/victoria-metrics" \
  -storageDataPath="$WORK/vm" \
  -httpListenAddr="127.0.0.1:${PORT_MAIN}" \
  -retentionPeriod=10y \
  -loggerLevel=ERROR >"$WORK/vm.log" 2>&1 &
VM_PIDS+=("$!")

wait_http "$PORT_MAIN" 60 || die "VictoriaMetrics 未能在 60 秒内就绪，日志见 ${WORK}/vm.log"

for m in "${MONTHS_ALL[@]}"; do
  t="$(ts_ms "2026-${m#2026_}-15")"
  "${CURL[@]}" -o /dev/null -X POST "http://127.0.0.1:${PORT_MAIN}/api/v1/import/prometheus" \
    --data-binary "vm_e2e_month_series{month=\"${m}\",run=\"e2e\"} ${m#2026_} ${t}" ||
    die "灌入 ${m} 的数据失败"
done
sleep 2
ok "已灌入 ${#MONTHS_ALL[@]} 个月的样本"

# ---------------------------------------------------------------
# 2. 校验存储按月份分区
# ---------------------------------------------------------------
info "步骤 2：校验存储分区布局（期望 small / big / indexdb 各 9 个月目录）"

for sub in "${SUBDIRS[@]}"; do
  assert_eq "data/${sub} 分区内容" "$MONTHS_ALL_STR" "$(months_in "$WORK/vm/data/${sub}")"
done

# ---------------------------------------------------------------
# 3. 创建快照
# ---------------------------------------------------------------
info "步骤 3：调用官方 /snapshot/create 创建快照"

SNAP_JSON="$("${CURL[@]}" "http://127.0.0.1:${PORT_MAIN}/snapshot/create")"
SNAP="$(printf '%s' "$SNAP_JSON" | python3 -c "import json,sys;print(json.load(sys.stdin).get('snapshot',''))" 2>/dev/null)"
[[ -n "$SNAP" ]] || die "创建快照失败，响应：${SNAP_JSON}"
ok "快照已创建：${SNAP}"

SNAP_DIR="$WORK/vm/snapshots/${SNAP}"
for sub in "${SUBDIRS[@]}"; do
  link="$(readlink "$SNAP_DIR/data/${sub}" 2>/dev/null)"
  if [[ -L "$SNAP_DIR/data/${sub}" && -d "$SNAP_DIR/data/${sub}" ]]; then
    ok "快照 data/${sub} 是指向 ${link} 的有效相对符号链接"
  else
    bad "快照 data/${sub} 不是有效符号链接（readlink=${link}）"
  fi
done
[[ -d "$SNAP_DIR/metadata" ]] && ok "快照 metadata/ 存在（实体目录，非符号链接）" || bad "快照 metadata/ 缺失"

# ---------------------------------------------------------------
# 4. dryRun
# ---------------------------------------------------------------
info "步骤 4：本工具 -dryRun（不写任何数据、不连远端）"

DRY_OUT="$("$TOOL" -storageDataPath="$WORK/vm" -snapshotName="$SNAP" \
  -fromMonth="$FROM" -toMonth="$TO" -dryRun -planOut="$WORK/plan.json" 2>&1)"
assert_eq "dryRun 退出码" "0" "$?"
assert_contains "计划表含 MONTH 表头" "$DRY_OUT" "MONTH"
for m in "${MONTHS_ALL[@]}"; do
  assert_contains "计划表列出快照中的 ${m}" "$DRY_OUT" "$m"
done
assert_contains "计划表给出 keep 决策" "$DRY_OUT" "keep"
assert_contains "计划表给出 drop 决策" "$DRY_OUT" "drop"
assert_eq "dryRun 未创建目标端" "absent" "$([[ -e "$WORK/backup-tool" ]] && echo present || echo absent)"
[[ -f "$WORK/plan.json" ]] && ok "已输出 -planOut 计划 JSON" || bad "未输出 -planOut 计划 JSON"

# ---------------------------------------------------------------
# 5. 官方 vmbackup 全量对照备份
# ---------------------------------------------------------------
info "步骤 5：官方 vmbackup 对同一快照做全量对照备份（用于步骤 6 的等价性对比）"

DST_OFFICIAL="$WORK/backup-official"
"$VMBIN/vmbackup" -storageDataPath="$WORK/vm" -snapshotName="$SNAP" \
  -dst="fs://${DST_OFFICIAL}" -concurrency=4 >"$WORK/backup-official.log" 2>&1
assert_eq "官方 vmbackup 退出码" "0" "$?"
assert_eq "官方 vmbackup 备份了全部 9 个月（含你不想要的 01/07/08/09）" \
  "$MONTHS_ALL_STR" "$(months_in "$DST_OFFICIAL/data/small")"

# 说明：这里刻意不使用 bash 4 的关联数组，脚本需兼容 macOS 自带的 bash 3.2。
official_in_range() { months_in_range "$(months_in "$DST_OFFICIAL/data/$1")" "$FROM" "$TO"; }

# ---------------------------------------------------------------
# 6. 本工具备份 + 等价性断言
# ---------------------------------------------------------------
info "步骤 6：本工具备份 ${FROM}..${TO} 到 fs://${WORK}/backup-tool"

DST_TOOL="$WORK/backup-tool"
"$TOOL" -storageDataPath="$WORK/vm" -snapshotName="$SNAP" \
  -fromMonth="$FROM" -toMonth="$TO" \
  -dst="fs://${DST_TOOL}" -concurrency=4 >"$WORK/backup-tool.log" 2>&1
assert_eq "本工具备份退出码" "0" "$?"

tool_months() { months_in "$DST_TOOL/data/$1"; }

for sub in "${SUBDIRS[@]}"; do
  # 核心断言：与官方 vmbackup 在区间内的产物完全一致
  assert_eq "目标端 data/${sub} 月份 == 官方全量备份 ∩ [${FROM}..${TO}]" \
    "$(official_in_range "$sub")" "$(tool_months "$sub")"
  # 反向断言：不得出现区间之外的月份
  assert_eq "目标端 data/${sub} 不含区间外月份" "" \
    "$(printf '%s\n' $(tool_months "$sub") | grep -v -E '^2026_0[2-6]$' | tr '\n' ' ' | sed 's/ $//')"
done

assert_eq "目标端 small 分区非空" "present" "$([[ -n "$(tool_months small)" ]] && echo present || echo empty)"

for f in backup_complete.ignore backup_metadata.ignore backup_month_range.ignore; do
  [[ -f "$DST_TOOL/$f" ]] && ok "已写入 ${f}" || bad "缺少 ${f}"
done
assert_eq "backup_month_range.ignore 中的 from_month" "$FROM" \
  "$(python3 -c "import json;print(json.load(open('${DST_TOOL}/backup_month_range.ignore'))['from_month'])" 2>/dev/null)"
assert_eq "backup_month_range.ignore 中的 to_month" "$TO" \
  "$(python3 -c "import json;print(json.load(open('${DST_TOOL}/backup_month_range.ignore'))['to_month'])" 2>/dev/null)"

echo "  信息：本次运行 data/big 备份到的月份为 [$(tool_months big)]（数据量小到未触发 big part 时为空，属正常）"

# ---------------------------------------------------------------
# 7. 官方 vmrestore 还原
# ---------------------------------------------------------------
info "步骤 7：官方 vmrestore 还原本工具的备份（验证格式兼容，未改动任何官方代码）"

RESTORE="$WORK/restored"
"$VMBIN/vmrestore" -src="fs://${DST_TOOL}" -storageDataPath="$RESTORE" -concurrency=4 \
  >"$WORK/restore.log" 2>&1
RC=$?
if [[ $RC -ne 0 ]]; then
  bad "vmrestore 还原失败（退出码 ${RC}），见 ${WORK}/restore.log"
  tail -20 "$WORK/restore.log"
else
  ok "vmrestore 还原成功"
fi

for sub in "${SUBDIRS[@]}"; do
  assert_eq "还原后 data/${sub} 月份 == 备份内容" "$(tool_months "$sub")" "$(months_in "$RESTORE/data/${sub}")"
done

# 逐字节校验：对每个保留月份、每个存储子目录做递归 diff
DIFF_FAILED=0
DIFF_COUNT=0
for sub in "${SUBDIRS[@]}"; do
  for m in $(tool_months "$sub"); do
    DIFF_COUNT=$((DIFF_COUNT + 1))
    if ! diff -r -q "$SNAP_DIR/data/${sub}/${m}" "$RESTORE/data/${sub}/${m}" >/dev/null 2>&1; then
      DIFF_FAILED=$((DIFF_FAILED + 1))
      bad "还原后 data/${sub}/${m} 与快照不一致"
    fi
  done
done
assert_eq "逐字节校验的不一致组合数" "0" "$DIFF_FAILED"
echo "  信息：共对 ${DIFF_COUNT} 个（存储子目录, 月份）组合做了递归逐字节比对"
assert_eq "还原后 metadata/ 与快照逐字节一致" "" "$(diff -r -q "$SNAP_DIR/metadata" "$RESTORE/metadata" 2>&1)"

# ---------------------------------------------------------------
# 8. 在还原数据上启动 VM 并查询
# ---------------------------------------------------------------
info "步骤 8：在还原数据上启动 VictoriaMetrics（端口 ${PORT_RESTORED}）并查询数据"

"$VMBIN/victoria-metrics" \
  -storageDataPath="$RESTORE" \
  -httpListenAddr="127.0.0.1:${PORT_RESTORED}" \
  -retentionPeriod=10y \
  -loggerLevel=ERROR >"$WORK/vm-restored.log" 2>&1 &
VM_PIDS+=("$!")

if wait_http "$PORT_RESTORED" 60; then
  ok "还原后的存储可正常启动并提供服务"
else
  bad "还原后的存储无法启动，见 ${WORK}/vm-restored.log"
  tail -20 "$WORK/vm-restored.log"
fi

Q="start=2026-01-01T00:00:00Z&end=2026-12-31T23:59:59Z"
SERIES_JSON="$("${CURL[@]}" "http://127.0.0.1:${PORT_RESTORED}/api/v1/series?match%5B%5D=vm_e2e_month_series&${Q}")"
SERIES_MONTHS="$(printf '%s' "$SERIES_JSON" | python3 -c "
import json,sys
try:
    rows = json.load(sys.stdin).get('data') or []
except Exception:
    print('PARSE_ERROR'); raise SystemExit(0)
print(' '.join(sorted({r.get('month','?') for r in rows})))
" 2>/dev/null)"
assert_eq "还原后存储中的序列月份（/api/v1/series）" "2026_02 2026_03 2026_04 2026_05 2026_06" "$SERIES_MONTHS"

LABEL_JSON="$("${CURL[@]}" "http://127.0.0.1:${PORT_RESTORED}/api/v1/label/month/values?${Q}")"
LABEL_VALUES="$(printf '%s' "$LABEL_JSON" | python3 -c "
import json,sys
try:
    vals = json.load(sys.stdin).get('data') or []
except Exception:
    print('PARSE_ERROR'); raise SystemExit(0)
print(' '.join(vals))
" 2>/dev/null)"
assert_eq "还原后 month 标签取值（/api/v1/label/month/values）" \
  "2026_02 2026_03 2026_04 2026_05 2026_06" "$LABEL_VALUES"
for m in 2026_01 2026_07 2026_08 2026_09; do
  assert_not_contains "被排除的月份 ${m} 在还原后不可见" "$LABEL_VALUES" "$m"
done

# ---------------------------------------------------------------
# 9. 参数护栏（负向测试）
# ---------------------------------------------------------------
info "步骤 9：参数护栏负向测试"

DST_GUARD="$WORK/backup-guard"

# 9.1 起始月份晚于结束月份 → 必须报错
OUT="$("$TOOL" -storageDataPath="$WORK/vm" -snapshotName="$SNAP" -fromMonth=2026_06 -toMonth=2026_02 -dst="fs://${DST_GUARD}" 2>&1)"
assert_contains "起始月份晚于结束月份时拒绝执行" "$OUT" "早于起始月份"

# 9.2 -expectMonths 指明了快照中不存在的月份 → 必须失败，且不创建目标端
OUT="$("$TOOL" -storageDataPath="$WORK/vm" -snapshotName="$SNAP" \
  -fromMonth="$FROM" -toMonth="$TO" -expectMonths=2026_02,2026_12 -dst="fs://${DST_GUARD}" 2>&1)"
assert_contains "-expectMonths 中缺失的月份被检出" "$OUT" "缺失期望存在的月份"
assert_eq "-expectMonths 失败时未创建目标端" "absent" "$([[ -e "$DST_GUARD" ]] && echo present || echo absent)"

# 9.3 区间内无任何分区数据 → 默认拒绝（即使 metadata/ 会被保留，也必须拒绝）
OUT="$("$TOOL" -storageDataPath="$WORK/vm" -snapshotName="$SNAP" \
  -fromMonth=2027_01 -toMonth=2027_12 -dst="fs://${DST_GUARD}" 2>&1)"
assert_contains "区间无分区数据时默认拒绝写出空备份" "$OUT" "没有任何按月分区的数据 part"
assert_contains "拒绝原因说明只匹配到非分区文件" "$OUT" "非分区文件"
assert_eq "空结果失败时未创建目标端" "absent" "$([[ -e "$DST_GUARD" ]] && echo present || echo absent)"

# 9.4 -allowEmptyResult 可显式放行，但应给出告警，且产物中不含任何月份
DST_EMPTY="$WORK/backup-empty"
OUT="$("$TOOL" -storageDataPath="$WORK/vm" -snapshotName="$SNAP" \
  -fromMonth=2027_01 -toMonth=2027_12 -dst="fs://${DST_EMPTY}" -allowEmptyResult 2>&1)"
assert_not_contains "-allowEmptyResult 放行后不再拒绝" "$OUT" "没有任何按月分区的数据 part"
assert_contains "-allowEmptyResult 放行后给出明确告警" "$OUT" "还原后不会包含任何时间序列"
assert_eq "-allowEmptyResult 产物中不含任何月份" "" "$(months_in "$DST_EMPTY/data/small")"
assert_eq "months_kept 为空" "[]" \
  "$(python3 -c "import json;print(json.load(open('${DST_EMPTY}/backup_month_range.ignore'))['months_kept'])" 2>/dev/null)"

# 9.5 -dst 位于 storageDataPath 内部 → 必须拒绝
OUT="$("$TOOL" -storageDataPath="$WORK/vm" -snapshotName="$SNAP" \
  -fromMonth="$FROM" -toMonth="$TO" -dst="fs://${WORK}/vm/backup-inside" 2>&1)"
assert_contains "-dst 位于数据目录内部时被拒绝" "$OUT" "不能位于 VictoriaMetrics 数据目录内部"

# ---------------------------------------------------------------
# 10. -prune 语义
# ---------------------------------------------------------------
info "步骤 10：验证 -prune 语义（累积写入 vs 对齐删除）"

DST_PRUNE="$WORK/backup-prune"

"$TOOL" -storageDataPath="$WORK/vm" -snapshotName="$SNAP" \
  -fromMonth=2026_02 -toMonth=2026_06 -dst="fs://${DST_PRUNE}" -concurrency=4 \
  >"$WORK/prune-1.log" 2>&1
assert_eq "第一批（02..06）写入后月份" "2026_02 2026_03 2026_04 2026_05 2026_06" "$(months_in "$DST_PRUNE/data/small")"

"$TOOL" -storageDataPath="$WORK/vm" -snapshotName="$SNAP" \
  -fromMonth=2026_07 -toMonth=2026_09 -dst="fs://${DST_PRUNE}" -concurrency=4 -prune=false \
  >"$WORK/prune-2.log" 2>&1
assert_eq "第二批 -prune=false 追加后两批共存" \
  "2026_02 2026_03 2026_04 2026_05 2026_06 2026_07 2026_08 2026_09" "$(months_in "$DST_PRUNE/data/small")"

"$TOOL" -storageDataPath="$WORK/vm" -snapshotName="$SNAP" \
  -fromMonth=2026_07 -toMonth=2026_09 -dst="fs://${DST_PRUNE}" -concurrency=4 -prune=true \
  >"$WORK/prune-3.log" 2>&1
assert_eq "第三批 -prune=true 后目标端被对齐为只剩本次范围" \
  "2026_07 2026_08 2026_09" "$(months_in "$DST_PRUNE/data/small")"
assert_contains "越界删除时日志给出告警" "$(cat "$WORK/prune-3.log")" "位于所选月份范围"

# ---------------------------------------------------------------
# 11. 删除快照
# ---------------------------------------------------------------
info "步骤 11：删除快照"
DEL_JSON="$("${CURL[@]}" "http://127.0.0.1:${PORT_MAIN}/snapshot/delete?snapshot=${SNAP}")"
assert_contains "快照删除成功" "$DEL_JSON" "ok"
assert_eq "快照目录已被移除" "absent" "$([[ -d "$SNAP_DIR" ]] && echo present || echo absent)"

# ---------------------------------------------------------------
# 汇总
# ---------------------------------------------------------------
echo
echo "================================================================"
echo " 端到端验证结果：通过 ${PASS} 项，失败 ${FAIL} 项"
echo "================================================================"
if [[ $FAIL -eq 0 ]]; then
  echo "结论："
  echo "  1. 本工具在 [${FROM}..${TO}] 上的备份产物，与官方 vmbackup 全量备份在该区间上的产物完全一致；"
  echo "  2. 区间之外的月份未进入备份；"
  echo "  3. 官方 vmrestore 可直接还原该备份，且还原结果与快照逐字节一致；"
  echo "  4. 还原后的存储可正常启动，查询结果只包含所选月份。"
  exit 0
fi
echo "存在失败项，请查看上面的 [FAIL] 行与 ${WORK} 下的日志。"
exit 1
