<div align="center">
  <h1>vmbackup-partition</h1>
  <p>Selectively mirror VictoriaMetrics vmstorage snapshots by month<br>Zero fork of upstream code — 100% vmrestore-compatible output</p>
</div>

<div align="center">

English &nbsp;|&nbsp; <a href="README_CN.md">简体中文</a>

<br>

<a href="https://github.com/surenwuyuwuqiu/vmbackup-partition/releases/latest"><img src="https://img.shields.io/github/v/release/surenwuyuwuqiu/vmbackup-partition" alt="GitHub release" /></a>
<a href="https://github.com/surenwuyuwuqiu/vmbackup-partition"><img src="https://img.shields.io/badge/platform-Linux%20%7C%20macOS-blue" alt="Platform" /></a>
<a href="https://github.com/surenwuyuwuqiu/vmbackup-partition"><img src="https://img.shields.io/badge/go-1.26.6%2B-00ADD8" alt="Go version" /></a>
<a href="https://github.com/surenwuyuwuqiu/vmbackup-partition"><img src="https://img.shields.io/badge/VictoriaMetrics-v1.150.0-6E3FBE" alt="VictoriaMetrics version" /></a>
<a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-green" alt="License" /></a>
<a href="https://github.com/surenwuyuwuqiu/vmbackup-partition"><img src="https://img.shields.io/github/stars/surenwuyuwuqiu/vmbackup-partition?style=social" alt="GitHub stars" /></a>

</div>

## Table of Contents

- [Why vmbackup-partition](#why-vmbackup-partition)
- [When to use it](#when-to-use-it)
- [How it works](#how-it-works)
- [Download](#download)
- [Build from Source](#build-from-source)
- [Quick Start](#quick-start)
- [Command-line flags](#command-line-flags)
- [Reading the dryRun plan](#reading-the-dryrun-plan)
- [Backup layout](#backup-layout)
- [Restore & verify](#restore--verify)
- [Cluster backup](#cluster-backup)
- [Production backup SOP](#production-backup-sop)
- [Handling multi-batch accumulation](#handling-multi-batch-accumulation)
- [Implementation Notes](#implementation-notes)
- [Verification](#verification)
- [In-depth documentation](#in-depth-documentation)
- [References](#references)
- [Feedback & Contributing](#feedback--contributing)
- [License](#license)

## Why vmbackup-partition

VictoriaMetrics partitions storage by **calendar month** — every data directory is named `YYYY_MM`. But the open-source `vmbackup` can only mirror an entire vmstorage data directory; **there is no way to back up just the months you care about**. Time-windowed backup management is a feature of the commercial `vmbackupmanager`.

vmbackup-partition fills that gap: it writes only the partitions falling inside the closed range `[fromMonth, toMonth]` to the destination; months outside the range never appear in the backup. It **does not fork, copy, or modify a single line of upstream code** — it imports the official `lib/backup` transport layer (`fslocal` / `fsremote` / `s3remote` / `gcsremote` / `azremote`) and reuses its concurrency, rate-limiting, and retry machinery as-is.

It is functionally equivalent to the "backup by time window" capability of the commercial `vmbackupmanager`, but **free, with no licence dependency**, and the output remains fully compatible with the open-source `vmrestore`.

- **Zero fork** — upstream `lib/backup/**` is imported verbatim; tracking upstream means editing one version line in `go.mod`
- **Standard output** — a backup is inherently a *set of remote objects addressed by part path*, so official `vmrestore` needs no changes at all
- **Safe by default** — empty-backup guard, expected-month guard, and out-of-range `-prune` warning all fire before anything is written to the destination

## When to use it

VictoriaMetrics single-node or cluster (verified on v1.150.0) when you need to back up **only one time window** of data:

- **Tiering / historical archiving** — move only the `2026_02` ~ `2026_06` half-year into cold or object storage, leaving recent months in place
- **Compliance retention** — regulation requires keeping only specific calendar months; avoid shipping the entire dataset (including sensitive periods) out of the controlled environment
- **Subset migration between clusters** — move one time window of a cluster to another cluster and leave the rest untouched
- **Partition-level restore drills** — pull a single window into an isolated restore to validate data, without moving terabytes

**Not applicable to**: filtering by metric/label (that is `vmagent` relabel or `vmctl` territory); merging replicas across vmstorages (each node's data is independent — see [Cluster backup](#cluster-backup)).

## How it works

### Storage layout

VictoriaMetrics v1.150.0 on-disk layout:

```
<storageDataPath>/
├── data/
│   ├── small/<YYYY_MM>/<partID>/…     ← regular parts
│   ├── big/<YYYY_MM>/<partID>/…       ← parts created only once a partition exceeds the size threshold
│   └── indexdb/<YYYY_MM>/<partID>/…   ← index parts
├── metadata/                          ← tenant & index metadata, not month-partitioned
└── snapshots/<snapName>/
    ├── data/                          ← relative symlinks into data/*/snapshots/<snapName>
    └── metadata/                      ← a real directory
```

Two facts matter here:

- The partition directory name is produced by `timestampToPartitionName()` in `lib/storage/time.go` as `t.Format("2006_01")`, strictly equivalent to `YYYY_MM`
- Part directory names are 16-hex-digit IDs and **cannot be confused** with the 7-character `YYYY_MM` format, so deciding "which month is this?" by directory name is safe and deterministic

### Exactly one filter point

```
snapshot (source)                                 destination
<storageDataPath>/snapshots/<name>/             s3:// | gs:// | azblob:// | fs://
├── data/            ← relative symlinks
│   ├── small/2026_02/<partID>/…  ─┐
│   ├── big/2026_03/<partID>/…     ├─▶ ① fslocal.ListParts()
│   └── indexdb/2026_06/<partID>/… ┘    ② month filter (the only intrusion point)
└── metadata/        ← non-partitioned, kept ③ guards: empty result / expected months
                                              ④ upload via the official transport layer
```

Why must filtering sit between "source enumeration" and "upload", rather than being a flag or a wrapper? The source answers this — and it is the entire reason this project exists:

1. The upstream struct field is a concrete type: `Backup.Src *fslocal.FS` (**not an interface**), so it cannot be swapped via a decorator
2. The function that actually does the work, `runBackup()`, is package-private — unreachable and unwrappable from outside
3. Hence upstream's fixed `src → dst` pipeline has **no injection point whatsoever**; that small stretch of orchestration has to be re-implemented

### Why vmrestore needs no changes

Remote object names are produced by `common.Part.RemotePath()` in `lib/backup/common/part.go` (shaped like `prefix/path/FILESIZE_OFFSET_SIZE`) and parsed back by `ParseFromRemotePath()`; **official `vmrestore` validates no manifest at all**. `parts.json` is maintained locally by the storage layer and is not part of the remote format.

> Corollary: filtering on the source side by part path yields a **fully legitimate** VictoriaMetrics backup — official `vmrestore` needs no changes.

This is verified by byte-for-byte comparison (see [Verification](#verification)).

### How the audit file avoids polluting the backup

The tool writes a `backup_month_range.ignore` object recording the month range and filtering audit data. Because of its `.ignore` suffix, the official `ListParts` implementations (`IgnorePath` in `lib/backup/fscommon/fscommon.go`) **skip every `.ignore`-suffixed file automatically** — so it is never mistaken for a part, and `vmrestore` ignores it.

## Download

Pre-built binaries are available from [GitHub Releases](https://github.com/surenwuyuwuqiu/vmbackup-partition/releases).

- **Linux (amd64)**: `vmbackup-partition-linux-amd64` — static binary (`CGO_ENABLED=0`), no runtime dependencies

```bash
curl -fL -o vmbackup-partition https://github.com/surenwuyuwuqiu/vmbackup-partition/releases/latest/download/vmbackup-partition-linux-amd64
curl -fL -O https://github.com/surenwuyuwuqiu/vmbackup-partition/releases/latest/download/vmbackup-partition-linux-amd64.sha256

sha256sum -c vmbackup-partition-linux-amd64.sha256   # verify integrity
chmod +x vmbackup-partition && sudo mv vmbackup-partition /usr/local/bin/

vmbackup-partition -version
# vmbackup-partition v1.0.0 based-on VictoriaMetrics v1.150.0
```

Other platforms are not pre-built yet — build from source (see below).

## Build from Source

Requires [Go](https://go.dev/dl/) 1.26.6 or newer (matching the `go.mod` declaration of VictoriaMetrics v1.150.0).

```bash
git clone https://github.com/surenwuyuwuqiu/vmbackup-partition.git
cd vmbackup-partition

# Set GOPROXY if needed (e.g. mainland China network)
export GOPROXY=https://goproxy.cn,direct

make build              # build for the host platform
make release            # cross-compile linux/amd64 + sha256 checksum → bin/
make check              # gofmt + go vet + go test

# or without make
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o vmbackup-partition-linux-amd64 ./cmd/vmbackup-partition
```

> If your local Go is older than 1.26.6 and `GOTOOLCHAIN=auto` cannot fetch the toolchain (checksum failure or blocked proxy), install Go 1.26.6 manually and build with `GOTOOLCHAIN=local` — fully offline. See [docs/02-编译指南.md](docs/02-编译指南.md) (Chinese) for the full toolchain walkthrough and troubleshooting.

## Quick Start

```bash
TOOL=/usr/local/bin/vmbackup-partition
VM_DATA=/var/lib/victoria-metrics-data           # vmstorage's -storageDataPath
SNAP_API=http://127.0.0.1:8482/snapshot/create   # vmstorage's HTTP port

# 1. Look at the plan first: writes nothing, needs no -dst and no remote credentials
$TOOL -storageDataPath=$VM_DATA -snapshot.createURL=$SNAP_API \
      -fromMonth=2026_02 -toMonth=2026_06 \
      -dryRun -planOut=/tmp/plan.json

# 2. Run the real backup once the plan looks right
$TOOL -storageDataPath=$VM_DATA -snapshot.createURL=$SNAP_API \
      -fromMonth=2026_02 -toMonth=2026_06 \
      -dst=fs:///backup/vmstorage-0 -concurrency=10 \
      -expectMonths=2026_02,2026_03,2026_04,2026_05,2026_06

# 3. Restore with the official vmrestore (the official tool is untouched)
vmrestore -src=fs:///backup/vmstorage-0 -storageDataPath=/var/lib/victoria-metrics-restored
```

`-snapshot.createURL` creates a snapshot before the backup starts and deletes it afterwards; alternatively pass `-snapshotName=<existing-snapshot>` to reuse one (the two are mutually exclusive).

## Command-line flags

### Flags added by this tool

| Flag | Type | Default | Description |
|---|---|---|---|
| `-fromMonth` | string | **required** | Start month (inclusive). Accepts `YYYY_MM` / `YYYY-MM` / `YYYYMM` |
| `-toMonth` | string | **required** | End month (inclusive). Same formats; errors out immediately if earlier than `-fromMonth` |
| `-includeNonPartitioned` | bool | `true` | Keep non-month-partitioned files (mainly tenant/index metadata under `metadata/`). **Strongly recommended to leave true** |
| `-prune` | bool | `true` | Delete destination parts that are absent from this backup's range (aligns the destination with the range). Semantics match official `vmbackup` |
| `-dryRun` | bool | `false` | Only enumerate the snapshot and print the plan; performs no remote write. Does not need `-dst` |
| `-allowEmptyResult` | bool | `false` | Whether to continue when the range contains **no month-partitioned part at all**. Refused by default to prevent writing a backup that would restore to nothing |
| `-expectMonths` | string | empty | Guard: comma-separated month list. If any of them is missing from the snapshot, error out immediately **without writing the destination** |
| `-planOut` | string | empty | Write the backup plan as JSON to a file, for auditing and automated comparison |

### Flags fully compatible with official vmbackup

| Flag | Default | Description |
|---|---|---|
| `-storageDataPath` | `victoria-metrics-data` | Must match the same flag of vmstorage / VictoriaMetrics |
| `-snapshotName` | empty | Use an existing snapshot. Mutually exclusive with `-snapshot.createURL` |
| `-snapshot.createURL` | empty | Create a snapshot automatically, e.g. `http://vmstorage:8482/snapshot/create` |
| `-snapshot.deleteURL` | empty | Derived from `createURL` when unset; the created snapshot is deleted after the backup |
| `-dst` | empty | Destination: `fs://` `s3://` `gs://` `azblob://` |
| `-origin` | empty | Previous backup location, used for **server-side copy** acceleration (no download) |
| `-concurrency` | `10` | Number of concurrent workers |
| `-maxBytesPerSecond` | `0` | Upload rate limit (e.g. `50MB`); `0` means unlimited |
| `-httpListenAddr` | `:8420` | Address exposing `/metrics` |
| Full S3 / GCS / AzBlob flag set | — | e.g. `-customS3Endpoint`, `-s3ForcePathStyle`, `-credsFilePath`; parsing logic is reused verbatim from upstream |

## Reading the dryRun plan

Real output (9 months × `{small,big,indexdb}` × 2 partIDs + 2 metadata files = 164 parts):

```
================ backup plan (dry run, nothing written) ================
snapshot dir       : /var/lib/victoria-metrics-data/snapshots/20260921120000-1A2B3C4D
snapshot name      : 20260921120000-1A2B3C4D
month range        : 2026_02..2026_06
keep non-partitioned: true
destination prune  : -prune=true

MONTH               DIRS                             PARTS          SIZE  DECISION
------------------------------------------------------------------------------
2026_01             big,indexdb,small                   18      86.08KiB  drop
2026_02             big,indexdb,small                   18      86.08KiB  keep
2026_03             big,indexdb,small                   18      86.08KiB  keep
2026_04             big,indexdb,small                   18      86.08KiB  keep
2026_05             big,indexdb,small                   18      86.08KiB  keep
2026_06             big,indexdb,small                   18      86.08KiB  keep
2026_07             big,indexdb,small                   18      86.08KiB  drop
2026_08             big,indexdb,small                   18      86.08KiB  drop
2026_09             big,indexdb,small                   18      86.08KiB  drop
(non-partitioned)   -                                    2          384B  keep
------------------------------------------------------------------------------
snapshot total      -                                  164     775.10KiB
will backup         -                                   92     430.78KiB  keep
will skip           -                                   72     344.32KiB  drop

non-partitioned file samples (max 16):
  - metadata/metadata.json
  - metadata/tenant-0/metadata.json
```

Three things to check in this table:

1. **The set of months under `snapshot total`** — must match your data directory. A missing month means an incomplete snapshot or a wrong `-storageDataPath`
2. **The `DECISION` column** — `keep` inside the range, `drop` outside
3. **The `PARTS` count of the `(non-partitioned)` row** — should be a single digit (metadata files under `metadata/`). A large number here means unexpected paths were classified as non-partitioned; cross-check the "non-partitioned file samples" list

The JSON emitted by `-planOut` suits CI/inspection: diffing the current plan against the previous one surfaces "abnormal data distribution" immediately. Field structure (excerpt):

```json
{
  "month_range": "2026_02..2026_06",
  "months": [
    { "month": "2026_02", "dirs": ["big","indexdb","small"],
      "total": { "parts": 18, "bytes": 88146, "human_bytes": "86.08KiB" },
      "decision": "keep" }
  ],
  "non_partitioned": { "parts": 2,   "bytes": 384,    "human_bytes": "384B" },
  "total_snapshot":  { "parts": 164, "bytes": 793698, "human_bytes": "775.10KiB" },
  "total_kept":      { "parts": 92,  "bytes": 441114, "human_bytes": "430.78KiB" },
  "total_dropped":   { "parts": 72,  "bytes": 352584, "human_bytes": "344.32KiB" }
}
```

## Backup layout

```
<dst>/
├── backup_complete.ignore        ← completion marker (recognised by official vmrestore / vmbackup)
├── backup_metadata.ignore        ← backup metadata in official format (created_at / completed_at)
├── backup_month_range.ignore     ← added by this tool: month-selection audit record
├── data/
│   ├── small/2026_02/<partID>/…  ← contains only [2026_02, 2026_06]
│   ├── big/2026_02/<partID>/…
│   └── indexdb/2026_02/<partID>/…
└── metadata/                     ← non-partitioned metadata (kept when -includeNonPartitioned=true)
```

Sample `backup_month_range.ignore`:

```json
{
  "from_month": "2026_02",
  "to_month": "2026_06",
  "month_range": "2026_02..2026_06",
  "include_non_partitioned": true,
  "months_present_in_snapshot": ["2026_01","2026_02","2026_03","2026_04","2026_05","2026_06","2026_07","2026_08","2026_09"],
  "months_kept": ["2026_02","2026_03","2026_04","2026_05","2026_06"],
  "snapshot_name": "20260921120000-1A2B3C4D",
  "prune": true,
  "parts_kept": 92,
  "bytes_kept": 441114,
  "parts_skipped": 72,
  "bytes_skipped": 352584,
  "tool": "vmbackup-partition"
}
```

The difference between `months_present_in_snapshot` and `months_kept` is exactly the set of excluded months — useful as retained audit evidence.

## Restore & verify

### Restore with the official vmrestore

```bash
# Cluster: restore each node into its own directory
vmrestore -src=s3://my-bucket/vm-cluster/2026H1/vmstorage-0 \
          -storageDataPath=/var/lib/victoria-metrics-data

# Single node: just start it and query
victoria-metrics -storageDataPath=/var/lib/victoria-metrics-restored -httpListenAddr=:8428
```

### Post-restore checklist

```bash
# 1. Only the selected months remain
find <restoredDataPath>/data -maxdepth 2 -mindepth 2 -type d | sort
#    expected: only 2026_02 … 2026_06

# 2. Confirm the month label carries only the selected months
curl -s 'http://localhost:8428/api/v1/label/month/values' | jq
#    expected: "data": ["2026_02","2026_03","2026_04","2026_05","2026_06"]

# 3. Confirm excluded months really hold no data
curl -s 'http://localhost:8428/api/v1/query?query=count({month="2026_01"})' | jq
#    expected: result is an empty array
```

## Cluster backup

In VictoriaMetrics cluster mode **every vmstorage's data is independent**, so you must back up per node and write to a **separate destination directory per node**.

```bash
bash scripts/backup-cluster.sh \
  --node vmstorage-0=10.0.0.11 \
  --node vmstorage-1=10.0.0.12 \
  --node vmstorage-2=10.0.0.13 \
  --ssh-user root \
  --tool /usr/local/bin/vmbackup-partition \
  --storage-data-path /var/lib/victoria-metrics-data \
  --vmstorage-port 8482 \
  --from-month 2026_02 --to-month 2026_06 \
  --dst-prefix s3://my-bucket/vm-cluster/2026H1
```

The script runs the following on each node (destination is `<dstPrefix>/<nodeName>`):

```
<tool> -storageDataPath=<path> \
       -snapshot.createURL=http://127.0.0.1:<port>/snapshot/create \
       -fromMonth=… -toMonth=… -concurrency=10 -prune=true \
       -dst=<dstPrefix>/<nodeName>
```

Add `--dry-run` to print the plan on all three nodes at once (no destination write, no remote credentials needed).

```
s3://my-bucket/vm-cluster/2026H1/vmstorage-0/
s3://my-bucket/vm-cluster/2026H1/vmstorage-1/
s3://my-bucket/vm-cluster/2026H1/vmstorage-2/
```

> Warning: **never write the backups of several nodes into the same directory.** Different vmstorages can produce the **same part path with different content**, and mixing them makes the `-prune` difference calculation delete/overwrite each other's data.

## Production backup SOP

Always rehearse in an isolated environment first — never run this against production without a dry run.

```bash
TOOL=/usr/local/bin/vmbackup-partition
VM_DATA=/var/lib/victoria-metrics-data
SNAP_API=http://127.0.0.1:8482/snapshot/create
DST=s3://my-bucket/vm-cluster/2026H1/vmstorage-0

# ===== 1. Read-only rehearsal: confirm month distribution and volume to keep =====
$TOOL -storageDataPath=$VM_DATA -snapshot.createURL=$SNAP_API \
      -fromMonth=2026_02 -toMonth=2026_06 -dryRun -planOut=/backup/plan.json
#   Check: the month set in "snapshot total" matches disk; DECISION column is right;
#          the part count and size of "will backup" are within expectations

# ===== 2. Verify the restore in isolation (strongly recommended) =====
#   Back up to a temporary destination -> restore with official vmrestore -> start and query
$TOOL … -dst=fs:///tmp/verify-backup
vmrestore -src=fs:///tmp/verify-backup -storageDataPath=/tmp/verify-restore
victoria-metrics -storageDataPath=/tmp/verify-restore -httpListenAddr=:8428
#   Check: starts cleanly; /api/v1/label/month/values lists only the selected months

# ===== 3. Production backup (with the expected-month guard) =====
$TOOL -storageDataPath=$VM_DATA -snapshot.createURL=$SNAP_API \
      -fromMonth=2026_02 -toMonth=2026_06 \
      -dst=$DST -concurrency=10 \
      -expectMonths=2026_02,2026_03,2026_04,2026_05,2026_06

# ===== 4. Cross-check the completion log and the audit file =====
#   A "backup complete" line appears; part count and size match the step-1 plan
#   months_kept in the destination's backup_month_range.ignore matches expectations
```

Note: a snapshot created by `-snapshot.createURL` is deleted automatically when the backup finishes normally. If the process is `kill -9`ed the snapshot may linger — list them via `http://<vmstorage>:8482/snapshot/list` and delete manually.

## Handling multi-batch accumulation

If you want to **accumulate several backups with different month ranges into the same destination** (e.g. back up `02..06` now, append `07..09` later), you **must explicitly set `-prune=false`**:

```bash
# First batch: 02..06
$TOOL … -fromMonth=2026_02 -toMonth=2026_06 -dst=fs:///backup/vm-0 -prune=true

# Second batch: append 07..09 — prune must be explicitly disabled
$TOOL … -fromMonth=2026_07 -toMonth=2026_09 -dst=fs:///backup/vm-0 -prune=false
```

With the default `-prune=true`, the second batch deletes everything the first batch wrote (`02..06`) and leaves only `07..09`. To reduce that risk, when `-prune=true` and the difference set contains parts **outside** the selected month range, the tool emits a warning:

```
warn  destination fsremote "/backup/vm-0" holds 90 parts outside the selected month range 2026_07..2026_09; they will be deleted (-prune=true).
      To accumulate several month ranges into the same -dst, set -prune=false explicitly
```

Other operational levers:

- **Incremental backup** — point `-dst` at the previous backup directory and only the difference is uploaded (equivalent to official `vmbackup`'s incremental semantics)
- **`-origin` acceleration** — set `-origin` to an older backup; the tool first attempts a server-side copy (no local landing, no egress bandwidth) and only uploads the remainder. If a cross-backend `CopyPart` fails it falls back to download-then-upload automatically
- **`-expectMonths` as a guard** — recommended always on; it catches "wrong month" mistakes, the hardest class to notice. On a hit it errors out and **does not create the destination**

## Implementation Notes

The tool's own code only does "enumerate → filter → aggregate → guard → orchestrate"; transport, retries, concurrency, and rate limiting are all delegated to upstream.

| Package | Responsibility |
|---|---|
| `internal/monthfilter` | Month literal parsing and range model, part-path classification (month-partitioned vs. non-partitioned), filtering with incremental aggregation, plan table and JSON rendering. Zero external dependencies |
| `internal/backupaction` | Backup orchestration: open snapshot → `ListParts` → month filter → guards → difference calculation → `-prune` → server-side copy → upload → metadata and audit file |
| `cmd/vmbackup-partition` | CLI: every official `vmbackup` flag, snapshot lifecycle, signal cancellation, `/metrics` server |

**Imported verbatim, never modified**: `lib/backup/common`, `lib/backup/fslocal`, `lib/backup/fsremote`, `lib/backup/s3remote`, `lib/backup/gcsremote`, `lib/backup/azremote`, `lib/backup/backupnames`, `lib/snapshot`.

Mapping against upstream `lib/backup/actions/backup.go`:

| Upstream | This tool |
|---|---|
| `Backup.Run()` argument validation (`hasFilepathPrefix`, etc.) | Aligned item by item, same semantics |
| `runBackup()`'s `ListParts` → `PartsDifference` → `CopyPart` → `UploadPart` → write `backup_complete` | Same order preserved, with the month filter and guards inserted in between |
| Private `runBackup()` (uncallable) | Re-orchestrated here; the transport layer is called through official interfaces as-is |
| `-concurrency` parallel upload | Same semantics, locally implemented scheduler (context cancellation, first-error-wins) |

`internal/monthfilter` carries 12 unit tests covering month parsing, range expansion, path classification, filter aggregation, JSON output, and byte formatting.

## Verification

Verification runs on three layers: unit tests → synthetic-snapshot end-to-end → real VictoriaMetrics end-to-end.

| Layer | Command | Result |
|---|---|---|
| Unit tests | `make test` | `internal/monthfilter` 12/12 green |
| Static checks | `make check` | `gofmt` / `go vet` / `go test` all pass |
| Synthetic snapshot E2E (covers `small` + `big` + `indexdb`) | `VMBIN=<vm-bin> bash scripts/e2e-synthetic.sh` | **63 passed, 0 failed** |
| Real VictoriaMetrics E2E | `VMBIN=<vm-bin> bash scripts/e2e-real.sh` | **69 passed, 0 failed** |

**Why the synthetic layer is needed**: a real VM only produces `big` parts once a single partition exceeds `getMaxSmallPartSize()` (hundreds of MB), so small test datasets never exercise the `big/` path. `scripts/make-fake-snapshot.sh` generates a directory structure **isomorphic to a real vmstorage snapshot** (including the three-level-up relative symlinks under `data/*`), deterministically covering all three storage paths.

Core conclusions of the real end-to-end run (verbatim script output):

```
[PASS] destination data/small   == official full backup ∩ [2026_02..2026_06]
[PASS] destination data/big     == official full backup ∩ [2026_02..2026_06]
[PASS] destination data/indexdb == official full backup ∩ [2026_02..2026_06]

-- restore this tool's backup with the official vmrestore (format compatibility, zero upstream changes)
  [PASS] vmrestore completed
  [PASS] restored data/small months == backup content -> [2026_02 2026_03 2026_04 2026_05 2026_06]
  [PASS] number of byte-for-byte mismatches -> [0]
  info: recursively compared 10 (storage subdirectory, month) combinations byte by byte
  [PASS] restored metadata/ identical to the snapshot byte for byte

-- start VictoriaMetrics on the restored data and query it
  [PASS] restored storage starts and serves normally
  [PASS] month label values after restore (/api/v1/label/month/values) -> [2026_02 2026_03 2026_04 2026_05 2026_06]
  [PASS] excluded month 2026_01 is invisible after restore
  [PASS] excluded months 2026_07 / 2026_08 / 2026_09 are invisible after restore
```

Four conclusions:

1. For `[2026_02..2026_06]` this tool's output is **identical** to an official `vmbackup` full backup restricted to that range
2. Months outside the range **never enter** the backup
3. Official `vmrestore` restores that backup **directly**, and the result is **byte-for-byte identical** to the snapshot (0 mismatches)
4. The restored storage starts and serves normally, and queries return **only the selected months**

The E2E scripts build the official reference binaries (`victoria-metrics` / `vmrestore` / `vmbackup`) once:

```bash
git clone --depth 1 --branch v1.150.0 \
  https://github.com/VictoriaMetrics/VictoriaMetrics.git /opt/VictoriaMetrics

VM_SRC=/opt/VictoriaMetrics OUT=$HOME/.cache/vmbackup-partition/vm-bin \
  bash scripts/build-vm-tools.sh

export VMBIN=$HOME/.cache/vmbackup-partition/vm-bin
bash scripts/e2e-synthetic.sh          # 63 checks
bash scripts/e2e-real.sh               # 69 checks
```

Without `VMBIN`, the scripts skip the steps that depend on the official binaries and still run every other assertion.

## In-depth documentation

The repository ships four Chinese in-depth documents under `docs/` (this README already covers everything needed operationally; consult them for source-level detail):

| Document | Contents |
|---|---|
| [docs/01-调研与设计.md](docs/01-调研与设计.md) | **Source-level evidence** for the upstream capability boundary, comparison of 4 alternative designs, storage-layout facts, key design decisions, boundaries and limits |
| [docs/02-编译指南.md](docs/02-编译指南.md) | Go toolchain requirements and pitfalls, three build paths, cross-compilation, artefact verification, quality gate, build troubleshooting |
| [docs/03-使用与验证.md](docs/03-使用与验证.md) | Full flag table, reading the `-dryRun` output, single/multi-node backups, restore verification, full verification results, troubleshooting table |
| [docs/04-源码走读.md](docs/04-源码走读.md) | Package structure and call chain, function-by-function walkthrough, item-by-item comparison with upstream, the three real defects fixed during development |

## References

- VictoriaMetrics upstream repository (v1.150.0 baseline): <https://github.com/VictoriaMetrics/VictoriaMetrics/tree/v1.150.0>
- Official `vmbackup` source (the orchestration semantics this tool aligns with): <https://github.com/VictoriaMetrics/VictoriaMetrics/blob/v1.150.0/lib/backup/actions/backup.go>
- Partition naming implementation: <https://github.com/VictoriaMetrics/VictoriaMetrics/blob/v1.150.0/lib/storage/time.go> (`timestampToPartitionName`)
- Storage directory constants: <https://github.com/VictoriaMetrics/VictoriaMetrics/blob/v1.150.0/lib/storage/filenames.go>
- `.ignore` skipping rule: <https://github.com/VictoriaMetrics/VictoriaMetrics/blob/v1.150.0/lib/backup/fscommon/fscommon.go> (`IgnorePath`)
- Remote part path format: <https://github.com/VictoriaMetrics/VictoriaMetrics/blob/v1.150.0/lib/backup/common/part.go>
- [`vmbackup` docs](https://docs.victoriametrics.com/victoriametrics/vmbackup/) | [`vmrestore` docs](https://docs.victoriametrics.com/victoriametrics/vmrestore/) | [`vmbackupmanager` (enterprise) docs](https://docs.victoriametrics.com/victoriametrics/vmbackupmanager/)
- Snapshot API (`/snapshot/create` / `/snapshot/delete` / `/snapshot/list`): <https://docs.victoriametrics.com/victoriametrics/#how-to-work-with-snapshots>
- Cluster backup notes (back up each vmstorage independently): <https://docs.victoriametrics.com/victoriametrics/cluster-victoriametrics/#backup-and-restore>
- Study document that informed the design: <https://www.cnblogs.com/ahfuzhang/p/17989390>

## Feedback & Contributing

Issues, suggestions, and feedback are welcome via [GitHub Issues](https://github.com/surenwuyuwuqiu/vmbackup-partition/issues); when reporting a month-filtering problem please attach the plan JSON produced by `-dryRun -planOut`. Pull Requests are welcome too — see [CONTRIBUTING.md](CONTRIBUTING.md).

## License

Apache-2.0 — see [LICENSE](LICENSE).

The re-orchestrated backup flow mirrors the semantics of upstream `lib/backup/actions/backup.go` (VictoriaMetrics v1.150.0, also released under Apache-2.0); upstream libraries are reused as unmodified imports.
