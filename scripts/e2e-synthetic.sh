#!/usr/bin/env bash
#
# 合成快照端到端验证（离线、快速、确定性，不需要启动 VictoriaMetrics）。
#
# 用 scripts/make-fake-snapshot.sh 生成一个与真实 vmstorage 快照同构的目录树
# （9 个月份分区、small/big/indexdb 三类存储子目录、metadata/ 实体目录、相对符号链接），
# 然后完整走一遍：dryRun → 备份 → 官方 vmbackup 对照 → 官方 vmrestore 还原 → 逐字节比对。
#
# 与 e2e-real.sh 的分工：
#   e2e-real.sh      用真实 VictoriaMetrics 产生的数据验证集成正确性，
#                    但因其单分区数据量不足以触发 big part，无法覆盖 data/big；
#   e2e-synthetic.sh 用带真实文件的合成快照，确定性覆盖 data/small、data/big、data/indexdb 三条路径。
#
# 用法：
#   bash scripts/e2e-synthetic.sh
#   VMBIN=/tmp/vm-bin bash scripts/e2e-synthetic.sh      # 额外做还原验证
#   KEEP=1 bash scripts/e2e-synthetic.sh
#
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VMBIN="${VMBIN:-}"
WORK="${WORK:-/tmp/vmbackup-partition-synthetic}"
KEEP="${KEEP:-0}"
SNAP="20260921120000-1A2B3C4D"
FROM=2026_02
TO=2026_06
MONTHS_ALL=(2026_01 2026_02 2026_03 2026_04 2026_05 2026_06 2026_07 2026_08 2026_09)
MONTHS_ALL_STR="${MONTHS_ALL[*]}"
EXPECT_KEPT="2026_02 2026_03 2026_04 2026_05 2026_06"
SUBDIRS=(small big indexdb)

PASS=0
FAIL=0
ok()   { echo "  [PASS] $*"; PASS=$((PASS + 1)); }
bad()  { echo "  [FAIL] $*"; FAIL=$((FAIL + 1)); }
info() { echo "── $*"; }

assert_eq() { # desc expected actual
  if [[ "$2" == "$3" ]]; then ok "$1 → [$3]"; else bad "$1 → 期望 [$2]，实际 [$3]"; fi
}
assert_contains() {
  if [[ "$2" == *"$3"* ]]; then ok "$1"; else bad "$1（输出中未找到 \"$3\"）"; fi
}

TOOL="$ROOT/bin/vmbackup-partition"
[[ -x "$TOOL" ]] || { echo "请先执行 make build 生成 ${TOOL}" >&2; exit 1; }

VMRESTORE=""
VMBACKUP=""
if [[ -n "$VMBIN" && -x "$VMBIN/vmrestore" && -x "$VMBIN/vmbackup" ]]; then
  VMRESTORE="$VMBIN/vmrestore"
  VMBACKUP="$VMBIN/vmbackup"
fi

months_in() {
  ls -1 "$1" 2>/dev/null | grep -E '^[0-9]{4}_[0-9]{2}$' | sort | tr '\n' ' ' | sed 's/ $//'
}

rm -rf "$WORK"
mkdir -p "$WORK"

echo "================================================================"
echo " 合成快照端到端验证"
echo " 工具      : ${TOOL}"
echo " 还原验证  : ${VMRESTORE:-<未提供 VMBIN，跳过还原验证>}"
echo " 月份区间  : ${FROM} .. ${TO}"
echo "================================================================"
echo

info "步骤 1：生成与真实 vmstorage 快照同构的合成快照"
bash "$ROOT/scripts/make-fake-snapshot.sh" "$WORK/vm" "$SNAP" "${MONTHS_ALL[@]}" >"$WORK/gen.log" 2>&1 ||
  { cat "$WORK/gen.log"; echo "生成合成快照失败" >&2; exit 2; }
tail -3 "$WORK/gen.log"
SNAP_DIR="$WORK/vm/snapshots/${SNAP}"
for sub in "${SUBDIRS[@]}"; do
  assert_eq "快照 data/${sub} 月份" "$MONTHS_ALL_STR" "$(months_in "$SNAP_DIR/data/${sub}")"
  [[ -L "$SNAP_DIR/data/${sub}" ]] && ok "data/${sub} 是符号链接（与官方 MustSymlinkRelative 一致）" ||
    bad "data/${sub} 不是符号链接"
done
assert_eq "快照 metadata 非分区文件数" "2" "$(find "$SNAP_DIR/metadata" -type f | wc -l | tr -d ' ')"

info "步骤 2：-dryRun 计划"
DRY_OUT="$("$TOOL" -storageDataPath="$WORK/vm" -snapshotName="$SNAP" \
  -fromMonth="$FROM" -toMonth="$TO" -dryRun -planOut="$WORK/plan.json" 2>&1)"
assert_eq "dryRun 退出码" "0" "$?"
for m in "${MONTHS_ALL[@]}"; do
  assert_contains "计划表列出 ${m}" "$DRY_OUT" "$m"
done
assert_contains "计划表列出 non-partitioned 汇总行" "$DRY_OUT" "(non-partitioned)"
assert_contains "计划表列出 metadata/metadata.json" "$DRY_OUT" "metadata/metadata.json"
assert_eq "dryRun 未创建目标端" "absent" "$([[ -e "$WORK/backup-tool" ]] && echo present || echo absent)"
assert_eq "dryRun 计划中 small 为 keep" "keep" \
  "$(python3 -c "
import json
p=json.load(open('$WORK/plan.json'))
print([m['decision'] for m in p['months'] if m['month']=='2026_02'][0])" 2>/dev/null)"

info "步骤 3：本工具备份 ${FROM}..${TO}"
DST_TOOL="$WORK/backup-tool"
"$TOOL" -storageDataPath="$WORK/vm" -snapshotName="$SNAP" \
  -fromMonth="$FROM" -toMonth="$TO" -dst="fs://${DST_TOOL}" -concurrency=4 >"$WORK/backup.log" 2>&1
assert_eq "备份退出码" "0" "$?"
for sub in "${SUBDIRS[@]}"; do
  assert_eq "目标端 data/${sub} 月份" "$EXPECT_KEPT" "$(months_in "$DST_TOOL/data/${sub}")"
done
assert_eq "目标端 metadata 子目录存在" "present" "$([[ -d "$DST_TOOL/metadata" ]] && echo present || echo absent)"

info "步骤 4：官方 vmbackup 全量对照"
if [[ -n "$VMBACKUP" ]]; then
  DST_OFFICIAL="$WORK/backup-official"
  "$VMBACKUP" -storageDataPath="$WORK/vm" -snapshotName="$SNAP" \
    -dst="fs://${DST_OFFICIAL}" -concurrency=4 >"$WORK/backup-official.log" 2>&1
  assert_eq "官方 vmbackup 退出码" "0" "$?"
  assert_eq "官方 vmbackup 备份全部 9 个月（证明开源版缺口）" "$MONTHS_ALL_STR" \
    "$(months_in "$DST_OFFICIAL/data/small")"

  # 等价性：本工具产物 == 官方产物 ∩ [FROM, TO]
  for sub in "${SUBDIRS[@]}"; do
    OFF_IN_RANGE="$(months_in "$DST_OFFICIAL/data/${sub}" | tr ' ' '\n' | grep -E '^2026_0[2-6]$' | tr '\n' ' ' | sed 's/ $//')"
    assert_eq "目标端 data/${sub} == 官方全量 ∩ [${FROM}..${TO}]" "$OFF_IN_RANGE" "$(months_in "$DST_TOOL/data/${sub}")"
  done
else
  echo "  (跳过：未提供 VMBIN 或其中缺少 vmbackup)"
fi

info "步骤 5：官方 vmrestore 还原 + 逐字节比对"
if [[ -n "$VMRESTORE" ]]; then
  RESTORE="$WORK/restored"
  "$VMRESTORE" -src="fs://${DST_TOOL}" -storageDataPath="$RESTORE" -concurrency=4 >"$WORK/restore.log" 2>&1
  RC=$?
  assert_eq "vmrestore 退出码" "0" "$RC"
  if [[ $RC -ne 0 ]]; then
    tail -20 "$WORK/restore.log"
  fi
  for sub in "${SUBDIRS[@]}"; do
    assert_eq "还原后 data/${sub} 月份" "$EXPECT_KEPT" "$(months_in "$RESTORE/data/${sub}")"
  done

  DIFF_FAILED=0
  DIFF_COUNT=0
  for sub in "${SUBDIRS[@]}"; do
    for m in ${EXPECT_KEPT}; do
      DIFF_COUNT=$((DIFF_COUNT + 1))
      if ! diff -r -q "$SNAP_DIR/data/${sub}/${m}" "$RESTORE/data/${sub}/${m}" >/dev/null 2>&1; then
        DIFF_FAILED=$((DIFF_FAILED + 1))
        bad "还原后 data/${sub}/${m} 与快照不一致"
      fi
    done
  done
  assert_eq "逐字节不一致组合数" "0" "$DIFF_FAILED"
  echo "  信息：共对 ${DIFF_COUNT} 个（存储子目录, 月份）组合做了递归逐字节比对"
  assert_eq "还原后 metadata/ 逐字节一致" "" "$(diff -r -q "$SNAP_DIR/metadata" "$RESTORE/metadata" 2>&1)"
  # 被排除的月份不得出现在还原结果中
  for m in 2026_01 2026_07 2026_08 2026_09; do
    for sub in "${SUBDIRS[@]}"; do
      assert_eq "被排除月份 ${m} 未出现在还原后 data/${sub}" "absent" \
        "$([[ -d "$RESTORE/data/${sub}/${m}" ]] && echo present || echo absent)"
    done
  done
else
  echo "  (跳过：未提供 VMBIN 或其中缺少 vmrestore)"
fi

info "步骤 6：-prune 语义"
DST_PRUNE="$WORK/backup-prune"
"$TOOL" -storageDataPath="$WORK/vm" -snapshotName="$SNAP" \
  -fromMonth=2026_02 -toMonth=2026_06 -dst="fs://${DST_PRUNE}" -concurrency=4 >"$WORK/p1.log" 2>&1
assert_eq "第一批（02..06）" "$EXPECT_KEPT" "$(months_in "$DST_PRUNE/data/small")"

"$TOOL" -storageDataPath="$WORK/vm" -snapshotName="$SNAP" \
  -fromMonth=2026_07 -toMonth=2026_09 -dst="fs://${DST_PRUNE}" -concurrency=4 -prune=false >"$WORK/p2.log" 2>&1
# 第二批只追加 07..09，且 -prune=false 不删旧的；2026_01 从未被备份，故期望为 02..09
assert_eq "-prune=false 追加后共存" "$EXPECT_KEPT 2026_07 2026_08 2026_09" "$(months_in "$DST_PRUNE/data/small")"

"$TOOL" -storageDataPath="$WORK/vm" -snapshotName="$SNAP" \
  -fromMonth=2026_07 -toMonth=2026_09 -dst="fs://${DST_PRUNE}" -concurrency=4 -prune=true >"$WORK/p3.log" 2>&1
assert_eq "-prune=true 后对齐为本次范围" "2026_07 2026_08 2026_09" "$(months_in "$DST_PRUNE/data/small")"
assert_contains "-prune=true 越界删除有告警" "$(cat "$WORK/p3.log")" "位于所选月份范围"

info "步骤 7：参数护栏"
GUARD="$WORK/guard"

OUT="$("$TOOL" -storageDataPath="$WORK/vm" -snapshotName="$SNAP" -fromMonth=2026_06 -toMonth=2026_02 -dst="fs://${GUARD}" 2>&1)"
assert_contains "区间反转被拒绝" "$OUT" "早于起始月份"

OUT="$("$TOOL" -storageDataPath="$WORK/vm" -snapshotName="$SNAP" -fromMonth=2026_02 -toMonth=2026_06 -dst="fs://${GUARD}" -expectMonths=2026_02,2026_11 2>&1)"
assert_contains "-expectMonths 缺失被检出" "$OUT" "缺失期望存在的月份"
assert_eq "-expectMonths 失败未创建目标端" "absent" "$([[ -e "$GUARD" ]] && echo present || echo absent)"

OUT="$("$TOOL" -storageDataPath="$WORK/vm" -snapshotName="$SNAP" -fromMonth=2027_01 -toMonth=2027_12 -dst="fs://${GUARD}" 2>&1)"
assert_contains "区间无分区数据被拒绝" "$OUT" "没有任何按月分区的数据 part"
assert_eq "空结果未创建目标端" "absent" "$([[ -e "$GUARD" ]] && echo present || echo absent)"

OUT="$("$TOOL" -storageDataPath="$WORK/vm" -snapshotName="$SNAP" -fromMonth="$FROM" -toMonth="$TO" -dst="fs://${WORK}/vm/inside" 2>&1)"
assert_contains "-dst 位于数据目录内被拒绝" "$OUT" "不能位于 VictoriaMetrics 数据目录内部"

OUT="$("$TOOL" -storageDataPath="$WORK/vm" -snapshotName="$SNAP" -toMonth=2026_06 -dryRun 2>&1)"
assert_contains "缺少 -fromMonth 被拒绝" "$OUT" "-fromMonth 与 -toMonth 均为必填参数"

OUT="$("$TOOL" -storageDataPath="$WORK/vm" -snapshotName=20990101000000-DEADBEEF -fromMonth="$FROM" -toMonth="$TO" -dryRun 2>&1)"
assert_contains "不存在的快照被拒绝" "$OUT" "无法打开快照目录"

info "步骤 8：限定到单个月份"
DST_ONE="$WORK/backup-one"
"$TOOL" -storageDataPath="$WORK/vm" -snapshotName="$SNAP" \
  -fromMonth=2026_04 -toMonth=2026_04 -dst="fs://${DST_ONE}" -concurrency=4 >"$WORK/one.log" 2>&1
assert_eq "单月区间只备份该月" "2026_04" "$(months_in "$DST_ONE/data/small")"
assert_eq "单月区间 indexdb 同样只含该月" "2026_04" "$(months_in "$DST_ONE/data/indexdb")"

echo
echo "================================================================"
echo " 合成快照验证结果：通过 ${PASS} 项，失败 ${FAIL} 项"
echo "================================================================"
if [[ $FAIL -eq 0 ]]; then
  echo "覆盖：data/small、data/big、data/indexdb 三条路径；月份上界、下界、区间外均被正确过滤。"
  [[ "$KEEP" == "1" ]] && echo "（已保留工作目录 ${WORK}）"
  [[ "$KEEP" != "1" ]] && rm -rf "$WORK"
  exit 0
fi
echo "存在失败项，工作目录保留在 ${WORK}。"
exit 1
