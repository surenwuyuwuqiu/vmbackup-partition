<div align="center">
  <h1>vmbackup-partition</h1>
  <p>按月份选择性备份 VictoriaMetrics vmstorage 快照<br>零 fork 官方代码，产物 100% 兼容官方 vmrestore</p>
</div>

<div align="center">

<a href="README.md">English</a> &nbsp;|&nbsp; 简体中文

<br>

<a href="https://github.com/surenwuyuwuqiu/vmbackup-partition/releases/latest"><img src="https://img.shields.io/github/v/release/surenwuyuwuqiu/vmbackup-partition" alt="GitHub release" /></a>
<a href="https://github.com/surenwuyuwuqiu/vmbackup-partition"><img src="https://img.shields.io/badge/platform-Linux%20%7C%20macOS-blue" alt="Platform" /></a>
<a href="https://github.com/surenwuyuwuqiu/vmbackup-partition"><img src="https://img.shields.io/badge/go-1.26.6%2B-00ADD8" alt="Go version" /></a>
<a href="https://github.com/surenwuyuwuqiu/vmbackup-partition"><img src="https://img.shields.io/badge/VictoriaMetrics-v1.150.0-6E3FBE" alt="VictoriaMetrics version" /></a>
<a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-green" alt="License" /></a>
<a href="https://github.com/surenwuyuwuqiu/vmbackup-partition"><img src="https://img.shields.io/github/stars/surenwuyuwuqiu/vmbackup-partition?style=social" alt="GitHub stars" /></a>

</div>

## 目录

- [为什么用 vmbackup-partition](#为什么用-vmbackup-partition)
- [适用场景](#适用场景)
- [原理](#原理)
- [下载安装](#下载安装)
- [从源码构建](#从源码构建)
- [快速开始](#快速开始)
- [命令行参数](#命令行参数)
- [解读 dryRun 计划](#解读-dryrun-计划)
- [备份产物结构](#备份产物结构)
- [还原与验证](#还原与验证)
- [集群备份](#集群备份)
- [生产备份 SOP](#生产备份-sop)
- [多批月份累积处置](#多批月份累积处置)
- [实现说明](#实现说明)
- [验证结果](#验证结果)
- [深入文档](#深入文档)
- [参考](#参考)
- [反馈与贡献](#反馈与贡献)
- [开源协议](#开源协议)

## 为什么用 vmbackup-partition

VictoriaMetrics 把数据按**自然月**分区存储，每个数据目录的名字就是 `YYYY_MM`。但开源版 `vmbackup` 只能整份镜像一个 vmstorage 数据目录——**没法只备份你指定的那几个月**；按时间窗口管理备份是商业版 `vmbackupmanager` 才有的能力。

vmbackup-partition 补上了这块能力：只把落在 `[fromMonth, toMonth]` 闭区间内的月份分区写入目标端，区间外的月份根本不会出现在备份里。它**不 fork、不复制、不修改官方任何一行代码**，而是直接复用官方 `lib/backup` 的传输层（`fslocal` / `fsremote` / `s3remote` / `gcsremote` / `azremote`）与并发、限速、重试机制。

它在功能上等价于企业版 `vmbackupmanager` 的"按时间选择备份"能力，但**免费、无授权依赖**，且产物格式与开源版 `vmrestore` 完全兼容。

- **零 fork** —— 官方 `lib/backup/**` 原样 import；跟进上游只需改 `go.mod` 里的一行版本号
- **产物标准** —— 备份本质是"按 part 路径寻址的远端对象集合"，官方 `vmrestore` 一行都不用改
- **默认安全** —— 空备份护栏、期望月份护栏、`-prune` 越界删除告警，三道拦截都在写目标端之前生效

## 适用场景

VictoriaMetrics 单机版或集群版（v1.150.0 已验证），需要**只备份某个时间窗口**的数据：

- **冷热分层 / 历史归档**：只把 `2026_02` ~ `2026_06` 的半年数据搬到冷存储或对象存储，近期月份留在原地
- **合规留存**：监管只要求保留某几个自然月，避免把全部数据（含敏感时段）搬离受控环境
- **跨集群迁移子集**：把某个集群的指定时间窗口迁到另一个集群，其余数据不动
- **分区级恢复演练**：只取一个窗口的数据做隔离恢复验证，不必搬运 TB 级全量

**不适用**的场景：需要按 metric / label 维度过滤（那是 `vmagent` 采集端 relabel 或 `vmctl` 的事）；需要跨 vmstorage 的副本合并（各节点数据独立，见 [集群备份](#集群备份)）。

## 原理

### 存储布局

VictoriaMetrics v1.150.0 的存储目录结构：

```
<storageDataPath>/
├── data/
│   ├── small/<YYYY_MM>/<partID>/…     ← 常规 part
│   ├── big/<YYYY_MM>/<partID>/…       ← 单分区数据超过阈值时才产生的 part
│   └── indexdb/<YYYY_MM>/<partID>/…   ← 索引 part
├── metadata/                          ← 租户与索引元数据，不按月分区
└── snapshots/<snapName>/
    ├── data/                          ← 指向 data/*/snapshots/<snapName> 的相对符号链接
    └── metadata/                      ← 实体目录
```

两条关键事实：

- 分区目录名由 `lib/storage/time.go` 的 `timestampToPartitionName()` 以 `t.Format("2006_01")` 产生，严格等价于 `YYYY_MM`
- part 目录名是 16 位十六进制 ID，与 `YYYY_MM` 的 7 字符格式**不可能混淆**，因此按目录名判断月份是安全且确定的

### 过滤点只有一处

```
源端快照                                          目标端
<storageDataPath>/snapshots/<name>/             s3:// | gs:// | azblob:// | fs://
├── data/            ← 相对符号链接
│   ├── small/2026_02/<partID>/…  ─┐
│   ├── big/2026_03/<partID>/…     ├─▶ ① fslocal.ListParts()
│   └── indexdb/2026_06/<partID>/… ┘    ② 月份过滤（唯一的侵入点）
└── metadata/        ← 非分区，默认保留   ③ 护栏：空备份 / 期望月份
                                          ④ 官方传输层上传（原样复用）
```

为什么过滤必须放在"源端列举之后、上传之前"这一步，而不是配置项或者包装一层？源码给出了答案，也是这个项目存在的理由：

1. `lib/backup/actions/backup.go` 的结构体字段是具体类型 `Backup.Src *fslocal.FS`（**不是接口**），无法用装饰器替换
2. 真正干活的 `runBackup()` 是包内私有函数，包外无法调用、无法包装
3. 所以官方那条 `src → dst` 的固定流水线**没有任何注入点**——只能由我们重新编排这一小段流程

### 为什么 vmrestore 不用改

备份产物的远端文件名由 `lib/backup/common/part.go` 的 `common.Part.RemotePath()` 生成（形如 `prefix/path/FILESIZE_OFFSET_SIZE`），还原时由 `ParseFromRemotePath()` 反向解析；**官方 `vmrestore` 不校验任何清单文件**，`parts.json` 是本地存储自己维护的、不属于远端格式的一部分。

> 推论：只要在源端按 part 路径过滤，产出的备份就是**完全合法**的 VM 备份，官方 `vmrestore` 一行都不用改。

这一条已通过逐字节比对验证（见 [验证结果](#验证结果)）。

### 审计文件如何不污染备份

本工具会往目标端写一个 `backup_month_range.ignore`，记录本次备份的月份范围与筛选审计信息。它以 `.ignore` 结尾，而官方 `ListParts` 的实现（`lib/backup/fscommon/fscommon.go` 的 `IgnorePath`）会**自动忽略所有 `.ignore` 后缀文件**——所以它既不会被误判成 part，也不影响 `vmrestore`。

## 下载安装

预编译二进制发布在 [GitHub Releases](https://github.com/surenwuyuwuqiu/vmbackup-partition/releases)。

- **Linux (amd64)**：`vmbackup-partition-linux-amd64` —— 静态编译（`CGO_ENABLED=0`），无运行时依赖

```bash
curl -fL -O https://github.com/surenwuyuwuqiu/vmbackup-partition/releases/latest/download/vmbackup-partition-linux-amd64
curl -fL -O https://github.com/surenwuyuwuqiu/vmbackup-partition/releases/latest/download/vmbackup-partition-linux-amd64.sha256

sha256sum -c vmbackup-partition-linux-amd64.sha256   # 校验完整性
chmod +x vmbackup-partition-linux-amd64
sudo mv vmbackup-partition-linux-amd64 /usr/local/bin/vmbackup-partition

vmbackup-partition -version
# vmbackup-partition v1.0.0 based-on VictoriaMetrics v1.150.0
```

其他平台目前没有预编译产物，请从源码构建（见下文）。

## 从源码构建

需要 [Go](https://go.dev/dl/) 1.26.6 或更高（对齐 VictoriaMetrics v1.150.0 的 `go.mod` 声明）。

```bash
git clone https://github.com/surenwuyuwuqiu/vmbackup-partition.git
cd vmbackup-partition

# 国内网络需要设置 GOPROXY
export GOPROXY=https://goproxy.cn,direct

make build              # 本机平台编译
make release            # 交叉编译 linux/amd64 + sha256 校验和 → bin/
make check              # gofmt + go vet + go test

# 或不用 make
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o vmbackup-partition-linux-amd64 ./cmd/vmbackup-partition
```

> 若本机 Go 低于 1.26.6 且 `GOTOOLCHAIN=auto` 无法自动下载工具链（校验失败或代理不通），请手动安装 Go 1.26.6 后以 `GOTOOLCHAIN=local` 构建，可完全离线。完整的工具链处理方式与故障排查见 [docs/02-编译指南.md](docs/02-编译指南.md)。

## 快速开始

```bash
TOOL=/usr/local/bin/vmbackup-partition
VM_DATA=/var/lib/victoria-metrics-data           # vmstorage 的 -storageDataPath
SNAP_API=http://127.0.0.1:8482/snapshot/create   # vmstorage 的 HTTP 端口

# ① 先看计划：不写任何数据，不需要 -dst，也不需要远端凭据
$TOOL -storageDataPath=$VM_DATA -snapshot.createURL=$SNAP_API \
      -fromMonth=2026_02 -toMonth=2026_06 \
      -dryRun -planOut=/tmp/plan.json

# ② 确认无误后正式备份
$TOOL -storageDataPath=$VM_DATA -snapshot.createURL=$SNAP_API \
      -fromMonth=2026_02 -toMonth=2026_06 \
      -dst=fs:///backup/vmstorage-0 -concurrency=10 \
      -expectMonths=2026_02,2026_03,2026_04,2026_05,2026_06

# ③ 用官方 vmrestore 还原（官方工具无需任何改动）
vmrestore -src=fs:///backup/vmstorage-0 -storageDataPath=/var/lib/victoria-metrics-restored
```

`-snapshot.createURL` 会在备份开始前自动创建快照，并在备份结束后自动删除；也可以先用 `-snapshotName=<既有快照名>` 复用已有快照（两者互斥）。

## 命令行参数

### 本工具新增参数

| 参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `-fromMonth` | string | **必填** | 起始月份（闭区间）。接受 `YYYY_MM` / `YYYY-MM` / `YYYYMM` 三种写法 |
| `-toMonth` | string | **必填** | 结束月份（闭区间）。格式同上；早于 `-fromMonth` 立即报错退出 |
| `-includeNonPartitioned` | bool | `true` | 是否保留非月份分区文件（主要是 `metadata/` 下的租户与索引元数据）。**强烈建议保持 true** |
| `-prune` | bool | `true` | 是否删除目标端存在、而本次备份范围内不存在的 part（把目标端对齐到本次范围）。语义与官方 `vmbackup` 一致 |
| `-dryRun` | bool | `false` | 只列举快照并输出备份计划，不产生任何远端写入；该模式不需要 `-dst` |
| `-allowEmptyResult` | bool | `false` | 当区间内**没有任何按月分区的 part** 时是否允许继续。默认拒绝，防止写出还原后无数据的无效备份 |
| `-expectMonths` | string | 空 | 安全护栏：逗号分隔的月份列表。若快照中缺失其中任意一月，立即报错退出，**不写目标端** |
| `-planOut` | string | 空 | 把备份计划以 JSON 写入指定文件，便于审计与自动化比对 |

### 完整兼容官方 vmbackup 的参数

| 参数 | 默认 | 说明 |
|---|---|---|
| `-storageDataPath` | `victoria-metrics-data` | 必须与 vmstorage / VictoriaMetrics 的该参数一致 |
| `-snapshotName` | 空 | 使用既有快照。与 `-snapshot.createURL` 互斥 |
| `-snapshot.createURL` | 空 | 自动创建快照，例 `http://vmstorage:8482/snapshot/create` |
| `-snapshot.deleteURL` | 空 | 未指定时自动由 `createURL` 推导；创建的快照会在备份结束后自动删除 |
| `-dst` | 空 | 目标端：`fs://` `s3://` `gs://` `azblob://` |
| `-origin` | 空 | 旧备份位置，用于**服务端拷贝**加速（免下载） |
| `-concurrency` | `10` | 并发工作协程数 |
| `-maxBytesPerSecond` | `0` | 上传限速（如 `50MB`）；`0` 表示不限速 |
| `-httpListenAddr` | `:8420` | `/metrics` 暴露地址 |
| S3 / GCS / AzBlob 全套参数 | — | 如 `-customS3Endpoint`、`-s3ForcePathStyle`、`-credsFilePath` 等，解析逻辑完全复用官方实现 |

## 解读 dryRun 计划

真实输出示例（9 个月 × `{small,big,indexdb}` × 2 个 partID + 2 个元数据文件 = 164 个 part）：

```
================ 备份计划（试运行，未写入任何数据）================
快照目录      : /var/lib/victoria-metrics-data/snapshots/20260921120000-1A2B3C4D
快照名        : 20260921120000-1A2B3C4D
月份过滤区间  : 2026_02..2026_06
保留非分区文件: true
目标端删除策略: -prune=true

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

非分区文件样例（最多 16 个）:
  - metadata/metadata.json
  - metadata/tenant-0/metadata.json
```

这张表要重点看三件事：

1. **`snapshot total` 里出现的月份集合** —— 是否与你的数据目录实际月份一致。少了某个月说明快照不完整，或 `-storageDataPath` 指错了
2. **`DECISION` 列** —— 区间内应为 `keep`，区间外应为 `drop`
3. **`(non-partitioned)` 行的 `PARTS` 数** —— 应为个位数（`metadata/` 下的元数据文件）。若这一行数字很大，说明有非预期路径被归入非分区，需要人工核对下方"非分区文件样例"

`-planOut` 输出的 JSON 便于接入 CI/巡检：把本次计划与上次计划做 diff，即可发现"数据分布异常"。字段结构（节选）如下：

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

## 备份产物结构

```
<dst>/
├── backup_complete.ignore        ← 完成标记（官方 vmrestore / vmbackup 均识别）
├── backup_metadata.ignore        ← 官方格式的备份元数据（created_at / completed_at）
├── backup_month_range.ignore     ← 本工具新增：月份选择审计记录
├── data/
│   ├── small/2026_02/<partID>/…  ← 只含 [2026_02, 2026_06]
│   ├── big/2026_02/<partID>/…
│   └── indexdb/2026_02/<partID>/…
└── metadata/                     ← 非分区元数据（-includeNonPartitioned=true 时保留）
```

`backup_month_range.ignore` 内容示例：

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

`months_present_in_snapshot` 与 `months_kept` 的差集就是被排除的月份，可作为审计证据留存。

## 还原与验证

### 用官方 vmrestore 还原

```bash
# 集群版：各节点分别还原到该节点自己的目录
vmrestore -src=s3://my-bucket/vm-cluster/2026H1/vmstorage-0 \
          -storageDataPath=/var/lib/victoria-metrics-data

# 单机版直接启动即可查询
victoria-metrics -storageDataPath=/var/lib/victoria-metrics-restored -httpListenAddr=:8428
```

### 还原后的验证清单

```bash
# ① 目标端只剩所选月份
find <restoredDataPath>/data -maxdepth 2 -mindepth 2 -type d | sort
#    期望只列出 2026_02 … 2026_06

# ② 查询数据，确认月份标签只含所选月份
curl -s 'http://localhost:8428/api/v1/label/month/values' | jq
#    期望："data": ["2026_02","2026_03","2026_04","2026_05","2026_06"]

# ③ 确认被排除的月份确实无数据
curl -s 'http://localhost:8428/api/v1/query?query=count({month="2026_01"})' | jq
#    期望：result 为空数组
```

## 集群备份

VictoriaMetrics 集群版中**每个 vmstorage 的数据相互独立**，必须分节点备份、分节点写入**各自独立的目标目录**。

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

脚本会在每个节点上执行（目标端为 `<dstPrefix>/<nodeName>`）：

```
<tool> -storageDataPath=<path> \
       -snapshot.createURL=http://127.0.0.1:<port>/snapshot/create \
       -fromMonth=… -toMonth=… -concurrency=10 -prune=true \
       -dst=<dstPrefix>/<nodeName>
```

加 `--dry-run` 可先在三个节点上同时输出计划（不写目标端、不需要远端凭据）。

```
s3://my-bucket/vm-cluster/2026H1/vmstorage-0/
s3://my-bucket/vm-cluster/2026H1/vmstorage-1/
s3://my-bucket/vm-cluster/2026H1/vmstorage-2/
```

> ⚠️ **三个节点的备份绝不能写入同一个目录。** 不同 vmstorage 上可能出现**相同的 part 路径但内容不同**，混写会因 `-prune` 的差集计算而互相删除/覆盖。

## 生产备份 SOP

务必先在隔离环境完整演练一遍，切勿未经验证直接在生产上操作。

```bash
TOOL=/usr/local/bin/vmbackup-partition
VM_DATA=/var/lib/victoria-metrics-data
SNAP_API=http://127.0.0.1:8482/snapshot/create
DST=s3://my-bucket/vm-cluster/2026H1/vmstorage-0

# ===== 1. 只读预演：确认月份分布与将被保留的数据量 =====
$TOOL -storageDataPath=$VM_DATA -snapshot.createURL=$SNAP_API \
      -fromMonth=2026_02 -toMonth=2026_06 -dryRun -planOut=/backup/plan.json
#   核对：snapshot total 的月份集合与磁盘一致；DECISION 列 keep/drop 正确；
#         will backup 的行数与体积在预期内

# ===== 2. 隔离环境验证还原（强烈推荐） =====
#   备份到临时目标端 → 用官方 vmrestore 还原到隔离目录 → 启动查询核对
$TOOL … -dst=fs:///tmp/verify-backup
vmrestore -src=fs:///tmp/verify-backup -storageDataPath=/tmp/verify-restore
victoria-metrics -storageDataPath=/tmp/verify-restore -httpListenAddr=:8428
#   检查：能正常启动；/api/v1/label/month/values 只含所选月份

# ===== 3. 正式备份（带期望月份护栏） =====
$TOOL -storageDataPath=$VM_DATA -snapshot.createURL=$SNAP_API \
      -fromMonth=2026_02 -toMonth=2026_06 \
      -dst=$DST -concurrency=10 \
      -expectMonths=2026_02,2026_03,2026_04,2026_05,2026_06

# ===== 4. 核对完成日志与审计文件 =====
#   日志中出现"备份完成"行，part 数与体积与步骤 1 的计划一致
#   目标端 backup_month_range.ignore 中的 months_kept 与预期一致
```

注意：`-snapshot.createURL` 创建的快照会在备份流程正常结束时自动删除；若进程被 `kill -9`，快照可能残留，可用 `http://<vmstorage>:8482/snapshot/list` 查看并手工删除。

## 多批月份累积处置

若你希望**把不同月份范围的多批备份累积到同一个目标端**（例如先备 `02..06`，过一阵再追加 `07..09`），**必须显式设置 `-prune=false`**：

```bash
# 第一批：02..06
$TOOL … -fromMonth=2026_02 -toMonth=2026_06 -dst=fs:///backup/vm-0 -prune=true

# 第二批：07..09 追加，必须显式关闭 prune
$TOOL … -fromMonth=2026_07 -toMonth=2026_09 -dst=fs:///backup/vm-0 -prune=false
```

若第二批沿用默认 `-prune=true`，工具会把第一批写入的 `02..06` 全部删除，只留下 `07..09`。为降低这类误操作的风险，当 `-prune=true` 且差集中含有"月份范围之外"的 part 时，工具会主动告警：

```
warn  目标端 fsremote "/backup/vm-0" 中有 90 个 part 位于所选月份范围 2026_07..2026_09 之外，本次将被删除（-prune=true）。
      若你希望通过多批不同月份范围累积到同一个 -dst，请显式设置 -prune=false
```

其他运维手段：

- **增量备份** —— 把 `-dst` 指向上一次的备份目录，工具只上传差集（等价于官方 `vmbackup` 的增量语义）
- **`-origin` 加速** —— `-origin` 指向旧备份，先尝试服务端拷贝（不落地、不耗出口带宽），剩余部分才上传；若跨后端类型导致 `CopyPart` 失败，会自动退化为"下载再上传"
- **`-expectMonths` 防呆** —— 推荐始终加上，它能拦住"月份记错"这一类最难发现的错误；命中时报错退出且**不创建目标端**

## 实现说明

本工具自己的代码只做"列举 → 过滤 → 统计 → 护栏 → 编排"，传输、重试、并发、限速全部交给官方实现。

| 包 | 职责 |
|---|---|
| `internal/monthfilter` | 月份字面量解析与区间模型、part 路径分类（按月分区 / 非分区）、过滤与增量聚合统计、计划表格与 JSON 渲染。零外部依赖 |
| `internal/backupaction` | 备份编排：打开快照 → `ListParts` → 月份过滤 → 护栏 → 差集计算 → `-prune` → 服务端拷贝 → 上传 → 写元数据与审计文件 |
| `cmd/vmbackup-partition` | CLI：完整兼容官方 `vmbackup` 的全部 flag、快照生命周期管理、信号取消、`/metrics` 服务 |

**原样 import、未做任何修改的官方组件**：`lib/backup/common`、`lib/backup/fslocal`、`lib/backup/fsremote`、`lib/backup/s3remote`、`lib/backup/gcsremote`、`lib/backup/azremote`、`lib/backup/backupnames`、`lib/snapshot`。

与官方 `lib/backup/actions/backup.go` 的对应关系：

| 官方实现 | 本工具 |
|---|---|
| `Backup.Run()` 的参数校验（`hasFilepathPrefix` 等） | 逐项对齐，语义一致 |
| `runBackup()` 的 `ListParts` → `PartsDifference` → `CopyPart` → `UploadPart` → 写 `backup_complete` | 保留同一顺序，中间插入月份过滤与护栏 |
| 私有 `runBackup()`（不可调用） | 自行编排，传输层原样调用官方接口 |
| `-concurrency` 并行上传 | 同语义的自实现并行调度（带 context 取消、首个错误优先） |

`internal/monthfilter` 有 12 项单元测试覆盖月份解析、区间展开、路径分类、过滤聚合、JSON 输出与字节格式化。

## 验证结果

验证分三层：单元测试 → 合成快照端到端 → 真实 VictoriaMetrics 端到端。

| 层级 | 命令 | 结果 |
|---|---|---|
| 单元测试 | `make test` | `internal/monthfilter` 12 项全绿 |
| 静态检查 | `make check` | `gofmt` / `go vet` / `go test` 全部通过 |
| 合成快照端到端（覆盖 `small` + `big` + `indexdb`） | `VMBIN=<vm-bin> bash scripts/e2e-synthetic.sh` | **通过 63 项，失败 0 项** |
| 真实 VictoriaMetrics 端到端 | `VMBIN=<vm-bin> bash scripts/e2e-real.sh` | **通过 69 项，失败 0 项** |

**为什么需要合成快照这一层**：真实 VM 只有在单个分区数据超过 `getMaxSmallPartSize()`（数百 MB）时才会产生 `big` part，小数据量环境测不到 `big/` 路径。`scripts/make-fake-snapshot.sh` 生成与真实 vmstorage 快照**完全同构**的目录结构（含 `data/*` 的三级上跳相对符号链接），从而确定性地覆盖三条存储路径。

真实环境端到端的核心结论（脚本原文输出）：

```
[PASS] 目标端 data/small   == 官方全量 ∩ [2026_02..2026_06]
[PASS] 目标端 data/big     == 官方全量 ∩ [2026_02..2026_06]
[PASS] 目标端 data/indexdb == 官方全量 ∩ [2026_02..2026_06]

── 官方 vmrestore 还原本工具的备份（验证格式兼容，未改动任何官方代码）
  [PASS] vmrestore 还原成功
  [PASS] 还原后 data/small 月份 == 备份内容 → [2026_02 2026_03 2026_04 2026_05 2026_06]
  [PASS] 逐字节校验的不一致组合数 → [0]
  信息：共对 10 个（存储子目录, 月份）组合做了递归逐字节比对
  [PASS] 还原后 metadata/ 与快照逐字节一致

── 在还原数据上启动 VictoriaMetrics 并查询
  [PASS] 还原后的存储可正常启动并提供服务
  [PASS] 还原后 month 标签取值（/api/v1/label/month/values） → [2026_02 2026_03 2026_04 2026_05 2026_06]
  [PASS] 被排除的月份 2026_01 在还原后不可见
  [PASS] 被排除的月份 2026_07 / 2026_08 / 2026_09 在还原后不可见
```

四条结论：

1. 本工具在 `[2026_02..2026_06]` 上的备份产物，与官方 `vmbackup` 全量备份在该区间上的产物**完全一致**
2. 区间之外的月份**未进入**备份
3. 官方 `vmrestore` 可**直接**还原该备份，且还原结果与快照**逐字节一致**（不一致组合数 0）
4. 还原后的存储可正常启动并服务，查询结果**只包含所选月份**

端到端脚本会一次性构建官方对照工具（`victoria-metrics` / `vmrestore` / `vmbackup`）：

```bash
git clone --depth 1 --branch v1.150.0 \
  https://github.com/VictoriaMetrics/VictoriaMetrics.git /opt/VictoriaMetrics

VM_SRC=/opt/VictoriaMetrics OUT=$HOME/.cache/vmbackup-partition/vm-bin \
  bash scripts/build-vm-tools.sh

export VMBIN=$HOME/.cache/vmbackup-partition/vm-bin
bash scripts/e2e-synthetic.sh          # 63 项
bash scripts/e2e-real.sh               # 69 项
```

未设置 `VMBIN` 时，脚本会跳过依赖官方二进制的步骤，其余断言照常执行。

## 深入文档

仓库 `docs/` 下有四份中文深度文档（本 README 已覆盖操作所需内容，需要源码级细节时按需查阅）：

| 文档 | 内容 |
|---|---|
| [docs/01-调研与设计.md](docs/01-调研与设计.md) | 开源版能力边界的**源码证据**、4 个备选方案对比、存储布局事实、关键设计决策、边界与限制 |
| [docs/02-编译指南.md](docs/02-编译指南.md) | Go 工具链要求与坑、三种构建路径、交叉编译、产物校验、质量门禁、编译问题排查 |
| [docs/03-使用与验证.md](docs/03-使用与验证.md) | 完整参数表、`-dryRun` 输出解读、单/多节点备份、还原验证、完整验证结果、故障排查表 |
| [docs/04-源码走读.md](docs/04-源码走读.md) | 包结构与调用链、逐函数走读、与官方代码的逐项对照、开发中修正的三个真实缺陷 |

## 参考

- VictoriaMetrics 官方仓库（v1.150.0 对照基线）：<https://github.com/VictoriaMetrics/VictoriaMetrics/tree/v1.150.0>
- 官方 `vmbackup` 源码（本工具对齐的编排语义）：<https://github.com/VictoriaMetrics/VictoriaMetrics/blob/v1.150.0/lib/backup/actions/backup.go>
- 分区命名实现：<https://github.com/VictoriaMetrics/VictoriaMetrics/blob/v1.150.0/lib/storage/time.go>（`timestampToPartitionName`）
- 存储目录常量：<https://github.com/VictoriaMetrics/VictoriaMetrics/blob/v1.150.0/lib/storage/filenames.go>
- `.ignore` 忽略规则：<https://github.com/VictoriaMetrics/VictoriaMetrics/blob/v1.150.0/lib/backup/fscommon/fscommon.go>（`IgnorePath`）
- 远端 part 路径格式：<https://github.com/VictoriaMetrics/VictoriaMetrics/blob/v1.150.0/lib/backup/common/part.go>
- [`vmbackup` 文档](https://docs.victoriametrics.com/victoriametrics/vmbackup/) ｜ [`vmrestore` 文档](https://docs.victoriametrics.com/victoriametrics/vmrestore/) ｜ [`vmbackupmanager`（企业版）文档](https://docs.victoriametrics.com/victoriametrics/vmbackupmanager/)
- 快照 API（`/snapshot/create` / `/snapshot/delete` / `/snapshot/list`）：<https://docs.victoriametrics.com/victoriametrics/#how-to-work-with-snapshots>
- Cluster 版备份须知（各 vmstorage 独立备份）：<https://docs.victoriametrics.com/victoriametrics/cluster-victoriametrics/#backup-and-restore>
- 设计参考的学习文档：<https://www.cnblogs.com/ahfuzhang/p/17989390>

## 反馈与贡献

欢迎通过 [GitHub Issues](https://github.com/surenwuyuwuqiu/vmbackup-partition/issues) 提交问题、建议或使用反馈，涉及月份筛选异常时请附上 `-dryRun -planOut` 产出的计划 JSON。也欢迎提交 Pull Request，参见 [CONTRIBUTING.md](CONTRIBUTING.md)。

## 开源协议

Apache-2.0 —— 详见 [LICENSE](LICENSE)。

本工具重新实现的备份编排流程，语义对照上游 `lib/backup/actions/backup.go`（VictoriaMetrics v1.150.0，同样以 Apache-2.0 发布）；官方库以原样 import 方式复用，未做任何修改。
