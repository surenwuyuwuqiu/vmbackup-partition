#!/bin/bash
# 端到端验证：按月备份 → 导入新 vm-cluster 后是否可用
#
# 覆盖真实集群场景（e2e-real.sh 只覆盖单节点）：
#   ① 3 节点源集群灌 9 个月数据（按序列分片，模拟 vminsert 一致性哈希）
#   ② 逐节点快照 + 按月备份 [FROM..TO] 到 3 个独立目标端
#   ③ 官方 vmrestore 还原到新集群（空目录、节点一一对应）
#   ④ 长 retention 启动 → 查询只含所选月份 + 写入新数据后新旧共存
#   ⑤ 对照：默认 retention 启动 → 验证历史数据会被自动删除（风险实证）
#   ⑥ 单节点还原：验证 v1.150.0 存储层统一，vmstorage 备份可被单节点查询
#
# 前置：VMBIN 目录下需有 victoria-metrics 与 vmrestore
#   VM_SRC=/path/to/VictoriaMetrics bash scripts/build-vm-tools.sh
#   （v1.150.0 起 victoria-metrics 二进制自带 vmstorage 能力，无需单独构建 vmstorage）
#
# 用法：VMBIN=~/.cache/vmbackup-partition/vm-bin bash scripts/e2e-cluster.sh
# 环境变量：WORK（默认 /tmp/vmbackup-partition-cluster）、KEEP=1（保留工作目录）

set -uo pipefail

VMBIN=${VMBIN:-$HOME/.cache/vmbackup-partition/vm-bin}
WORK=${WORK:-/tmp/vmbackup-partition-cluster}
TOOL_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TOOL="$TOOL_DIR/bin/vmbackup-partition"

FROM=${FROM:-2026_02}
TO=${TO:-2026_06}

# 查询时间范围：label values API 默认只查最近窗口，历史数据必须显式指定 start/end
S=1767225600   # 2026-01-01
E=1790812800   # 2026-10-01

pass=0; fail=0
ok(){ echo "  [PASS] $1"; pass=$((pass+1)); }
no(){ echo "  [FAIL] $1"; fail=$((fail+1)); }

wait_up(){ for i in $(seq 1 60); do curl -s -o /dev/null "http://127.0.0.1:$1/health" 2>/dev/null && return 0; sleep 0.5; done; return 1; }
months(){ curl -s "http://127.0.0.1:$1/api/v1/label/month/values?start=$S&end=$E" 2>/dev/null \
  | python3 -c "import sys,json;print(' '.join(sorted(json.load(sys.stdin).get('data',[]))))" 2>/dev/null; }
union(){ echo "$@" | tr ' ' '\n' | sort -u | tr '\n' ' '; }

for b in victoria-metrics vmrestore; do
  [ -x "$VMBIN/$b" ] || { echo "缺少 $VMBIN/$b，请先运行 build-vm-tools.sh"; exit 1; }
done
[ -x "$TOOL" ] || { echo "缺少 $TOOL，请先 make build"; exit 1; }

rm -rf "$WORK"; mkdir -p "$WORK"

echo "──────── 步骤 1：启动源集群（3 节点，retention=100y）────────"
for i in 0 1 2; do
  mkdir -p "$WORK/c1/node$i"
  "$VMBIN/victoria-metrics" -storageDataPath="$WORK/c1/node$i" \
    -httpListenAddr=":1842$i" -vmselectAddr=":1845$i" -retentionPeriod=100y \
    > "$WORK/c1-node$i.log" 2>&1 &
done
sleep 6
for i in 0 1 2; do wait_up "1842$i" || { echo "源节点 $i 启动失败"; exit 1; }; done
echo "  源集群就绪"

echo "──────── 步骤 2：灌入 9 个月数据（按序列分片）────────"
python3 - <<'PY'
import json, subprocess, calendar, datetime
for mo in range(1, 10):
    mstr = f"2026_{mo:02d}"
    ts = int(calendar.timegm(datetime.datetime(2026, mo, 15).timetuple())) * 1000
    for s in range(3):
        body = json.dumps({"metric": {"__name__": "cluster_test", "month": mstr, "shard": str(s)},
                           "values": [1.0], "timestamps": [ts]})
        subprocess.run(["curl", "-s", "-o", "/dev/null", "-X", "POST",
                        f"http://127.0.0.1:1842{s % 3}/api/v1/import", "--data-binary", body], check=False)
PY
sleep 3
for i in 0 1 2; do
  echo "    c1/node$i: $(ls "$WORK/c1/node$i/data/small" 2>/dev/null | grep -v snapshots | tr '\n' ' ')"
done

echo "──────── 步骤 3：快照 + 按月备份 [$FROM .. $TO] ────────"
for i in 0 1 2; do
  "$TOOL" -storageDataPath="$WORK/c1/node$i" \
    -snapshot.createURL="http://127.0.0.1:1842$i/snapshot/create" \
    -fromMonth=$FROM -toMonth=$TO -dst="fs://$WORK/bak/node$i" -concurrency=4 \
    > "$WORK/backup-node$i.log" 2>&1 || { echo "备份节点 $i 失败"; tail -5 "$WORK/backup-node$i.log"; exit 1; }
done
expect=""
for i in 0 1 2; do
  got=$(ls "$WORK/bak/node$i/data/small" 2>/dev/null | grep -v snapshots | tr '\n' ' ')
  echo "    bak/node$i: $got"
  [ "$(echo $got)" = "2026_02 2026_03 2026_04 2026_05 2026_06" ] \
    && ok "备份 node$i 只含所选月份" || no "备份 node$i 月份异常: [$got]"
done

pkill -f "$WORK/c1"; sleep 3

echo "──────── 步骤 4：还原到新集群 c2（空目录、节点一一对应，retention=100y）────────"
for i in 0 1 2; do
  mkdir -p "$WORK/c2/node$i"
  "$VMBIN/vmrestore" -src="fs://$WORK/bak/node$i" -storageDataPath="$WORK/c2/node$i" \
    > "$WORK/restore-node$i.log" 2>&1 || { echo "还原节点 $i 失败"; tail -5 "$WORK/restore-node$i.log"; }
done
for i in 0 1 2; do
  "$VMBIN/victoria-metrics" -storageDataPath="$WORK/c2/node$i" \
    -httpListenAddr=":1852$i" -vmselectAddr=":1855$i" -retentionPeriod=100y \
    > "$WORK/c2-node$i.log" 2>&1 &
done
sleep 8
all=""
for i in 0 1 2; do
  m=$(months "1852$i"); echo "    c2/node$i 可查: [$m]"; all="$all $m"
done
u=$(union $all)
[ "$(echo $u)" = "2026_02 2026_03 2026_04 2026_05 2026_06" ] \
  && ok "导入新集群后只含所选月份" || no "导入后月份异常: [$u]"

echo "──────── 步骤 5：写入新数据，验证新旧共存 ────────"
NOW=$(python3 -c "import calendar,datetime;print(int(calendar.timegm(datetime.datetime(2026,9,15).timetuple()))*1000)")
for s in 0 1 2; do
  curl -s -o /dev/null -X POST "http://127.0.0.1:1852$s/api/v1/import" --data-binary \
    "{\"metric\":{\"__name__\":\"cluster_test\",\"month\":\"2026_09\",\"shard\":\"$s\"},\"values\":[9.0],\"timestamps\":[$NOW]}"
done
sleep 4
all=""
for i in 0 1 2; do all="$all $(months "1852$i")"; done
u2=$(union $all)
echo "$u2" | grep -q "2026_09" && ok "新写入数据可查询" || no "新写入数据查不到"
echo "$u2" | grep -q "2026_02" && ok "历史月份仍可查询（新旧共存）" || no "历史月份丢失"
echo "$u2" | grep -q "2026_01" && no "被排除的 2026_01 出现了" || ok "被排除月份 2026_01 不可见"
pkill -f "$WORK/c2"; sleep 3

echo "──────── 步骤 6：单节点还原（vmstorage 备份 → 单节点）────────"
mkdir -p "$WORK/single"
"$VMBIN/vmrestore" -src="fs://$WORK/bak/node0" -storageDataPath="$WORK/single" > "$WORK/restore-single.log" 2>&1
"$VMBIN/victoria-metrics" -storageDataPath="$WORK/single" -httpListenAddr=":18700" \
  -retentionPeriod=100y > "$WORK/single.log" 2>&1 &
sleep 7
wait_up 18700 >/dev/null || echo "  单节点未就绪"
sm=$(months 18700)
[ -n "$sm" ] && ok "vmstorage 备份可还原到单节点并查询（v1.150.0 存储层统一）: [$sm]" || no "单节点查不到数据"
pkill -f "$WORK/single"; sleep 2

echo "──────── 步骤 7：对照 —— 默认 retention 下历史数据会被删除 ────────"
mkdir -p "$WORK/c3/node0"
"$VMBIN/vmrestore" -src="fs://$WORK/bak/node0" -storageDataPath="$WORK/c3/node0" > "$WORK/restore3.log" 2>&1
"$VMBIN/victoria-metrics" -storageDataPath="$WORK/c3/node0" -httpListenAddr=":18800" \
  -retentionPeriod=1M > "$WORK/c3.log" 2>&1 &
sleep 8
before=$(months 18800)
echo "    t=0s    可查: [$before]"
sleep 70
after=$(months 18800)
parts=$(ls "$WORK/c3/node0/data/small" 2>/dev/null | grep -v snapshots | tr '\n' ' ')
echo "    t=70s   可查: [$after]  磁盘分区: [$parts]"
[ -n "$before" ] && [ -z "$after" ] \
  && ok "实证：默认 retention 会删除还原的历史数据（故必须调大 -retentionPeriod）" \
  || no "retention 行为与预期不符: before=[$before] after=[$after]"
pkill -f "$WORK/c3"; sleep 2

[ -z "${KEEP:-}" ] && rm -rf "$WORK"

echo
echo "================================================================"
echo " 集群导入验证结果：通过 $pass 项，失败 $fail 项"
echo "================================================================"
[ "$fail" -eq 0 ] || exit 1
