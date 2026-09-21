#!/usr/bin/env bash
#
# 构造一个与真实 VictoriaMetrics vmstorage 快照“逐字节同构”的合成快照，用于离线验证。
#
# 真实布局（由 lib/storage/storage.go:MustCreateSnapshot 与 table.go:MustCreateSnapshot 产生）：
#
#   <root>/data/small/snapshots/<SNAP>/<YYYY_MM>/<partID>/{timestamps,values,index,metaindex}.bin
#   <root>/data/small/snapshots/<SNAP>/<YYYY_MM>/parts.json
#   <root>/data/big/snapshots/<SNAP>/<YYYY_MM>/...
#   <root>/data/indexdb/snapshots/<SNAP>/<YYYY_MM>/...
#   <root>/snapshots/<SNAP>/data/small   -> ../../../data/small/snapshots/<SNAP>   (相对符号链接，三级上跳)
#   <root>/snapshots/<SNAP>/data/big     -> ../../../data/big/snapshots/<SNAP>
#   <root>/snapshots/<SNAP>/data/indexdb -> ../../../data/indexdb/snapshots/<SNAP>
#   <root>/snapshots/<SNAP>/metadata/    (实体目录，非符号链接)
#
# 关键点：快照里的 data/* 是符号链接，真实数据是硬链接（这里用普通文件模拟）。
# 官方 vmbackup 通过 fscommon.AppendFiles 解引用符号链接并把路径重写回 data/<subdir>/<M>/...，
# 因此备份中的 part 路径形如 data/small/2026_02/<partID>/timestamps.bin。
#
# 用法：
#   scripts/make-fake-snapshot.sh <root> <snapshotName> <month...>
# 示例：
#   scripts/make-fake-snapshot.sh /tmp/e2e/vm 20260921120000-1A2B3C4D 2026_01 2026_02 ...
#
set -euo pipefail

ROOT="${1:?用法: $0 <root> <snapshotName> <month...>}"
SNAP="${2:?用法: $0 <root> <snapshotName> <month...>}"
shift 2
MONTHS=("$@")

if [[ ${#MONTHS[@]} -eq 0 ]]; then
  echo "错误：至少需要一个月份参数" >&2
  exit 1
fi

# 每个 part 的文件大小（字节）。刻意用非整数 KiB 以覆盖非对齐长度。
SMALL_PART_SIZE="${SMALL_PART_SIZE:-4096}"
BIG_PART_SIZE="${BIG_PART_SIZE:-12288}"
INDEX_PART_SIZE="${INDEX_PART_SIZE:-2048}"

# 生成确定性内容的辅助函数（内容由 seed 决定，便于复现与校验）。
gen_file() {
  local path="$1" size="$2" seed="$3"
  mkdir -p "$(dirname "$path")"
  python3 - "$path" "$size" "$seed" <<'PY'
import hashlib, sys
path, size, seed = sys.argv[1], int(sys.argv[2]), sys.argv[3]
out = bytearray()
h = hashlib.sha256(seed.encode()).digest()
while len(out) < size:
    out += h
    h = hashlib.sha256(h).digest()
with open(path, "wb") as f:
    f.write(bytes(out[:size]))
PY
}

part_id_for() {
  # 生成 16 位十六进制的 part 目录名，形状与真实 part ID 一致。
  printf '%016X' "$(python3 -c "import hashlib,sys;print(int(hashlib.sha256(sys.argv[1].encode()).hexdigest()[:15],16))" "$1")"
}

rm -rf "$ROOT"
mkdir -p "$ROOT/snapshots/$SNAP/data"
mkdir -p "$ROOT/snapshots/$SNAP/metadata"

for m in "${MONTHS[@]}"; do
  # ---- data/small/<M> ----
  sm_dir="$ROOT/data/small/snapshots/$SNAP/$m"
  pid_small="$(part_id_for "small-$m-a")"
  for f in timestamps.bin values.bin index.bin metaindex.bin; do
    gen_file "$sm_dir/$pid_small/$f" "$SMALL_PART_SIZE" "small-$m-$f"
  done
  # 第二个 small part，覆盖“同一个月多个 part”的情形
  pid_small2="$(part_id_for "small-$m-b")"
  for f in timestamps.bin values.bin index.bin metaindex.bin; do
    gen_file "$sm_dir/$pid_small2/$f" "$SMALL_PART_SIZE" "small-$m-b-$f"
  done
  # parts.json：由 partition.MustCreateSnapshotAt 写入，官方备份也会把它当作一个 part
  printf '["%s","%s"]\n' "$pid_small" "$pid_small2" >"$sm_dir/parts.json"

  # ---- data/big/<M> ----
  big_dir="$ROOT/data/big/snapshots/$SNAP/$m"
  pid_big="$(part_id_for "big-$m-a")"
  for f in timestamps.bin values.bin index.bin metaindex.bin; do
    gen_file "$big_dir/$pid_big/$f" "$BIG_PART_SIZE" "big-$m-$f"
  done
  printf '["%s"]\n' "$pid_big" >"$big_dir/parts.json"

  # ---- data/indexdb/<M> ----
  idx_dir="$ROOT/data/indexdb/snapshots/$SNAP/$m"
  pid_idx="$(part_id_for "indexdb-$m-a")"
  for f in metaindex.bin index.bin values.bin; do
    gen_file "$idx_dir/$pid_idx/$f" "$INDEX_PART_SIZE" "indexdb-$m-$f"
  done
  printf '["%s"]\n' "$pid_idx" >"$idx_dir/parts.json"
done

# ---- 快照根下的相对符号链接 ----
# 相对路径由官方 lib/fs/fs.go:MustSymlinkRelative 计算：
#   filepath.Rel(filepath.Dir(dstPath), srcPath)
# 其中 dstPath = <root>/snapshots/<SNAP>/data/<subdir>，故 baseDir = <root>/snapshots/<SNAP>/data，
# 需要向上三级才能回到 <root>，即 ../../../data/<subdir>/snapshots/<SNAP>。
ln -s "../../../data/small/snapshots/$SNAP" "$ROOT/snapshots/$SNAP/data/small"
ln -s "../../../data/big/snapshots/$SNAP" "$ROOT/snapshots/$SNAP/data/big"
ln -s "../../../data/indexdb/snapshots/$SNAP" "$ROOT/snapshots/$SNAP/data/indexdb"

# 自检：三个符号链接都必须能解析，否则说明布局与官方不一致。
for d in small big indexdb; do
  if [[ ! -d "$ROOT/snapshots/$SNAP/data/$d" ]]; then
    echo "错误：符号链接 snapshots/$SNAP/data/$d 无法解析" >&2
    exit 1
  fi
done

# ---- metadata/（实体目录，非分区文件，用于验证非分区策略）----
gen_file "$ROOT/snapshots/$SNAP/metadata/metadata.json" 128 "metadata-root"
gen_file "$ROOT/snapshots/$SNAP/metadata/tenant-0/metadata.json" 256 "metadata-tenant-0"

echo "已生成合成快照：$ROOT/snapshots/$SNAP"
echo "  月份：${MONTHS[*]}"
echo "  快照内文件总数（跟随符号链接）：$(find -L "$ROOT/snapshots/$SNAP/" -type f | wc -l | tr -d ' ')"
echo "  快照总字节数（跟随符号链接）：$(du -skL "$ROOT/snapshots/$SNAP" | awk '{print $1*1024}')"
