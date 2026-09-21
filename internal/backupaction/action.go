// Package backupaction 实现“按月份分区筛选”的 VictoriaMetrics 备份执行流程。
//
// 该包不修改、不 fork 官方代码，而是复用官方 lib/backup 提供的传输协议与存储后端
// （fs://、s3://、gs://、azblob://），只在官方 actions.Backup 的
// “列举源 part → 上传”之间插入一层月份过滤器。
//
// 为什么必须自行实现这一小段流程：官方 lib/backup/actions/backup.go 中
//
//	type Backup struct {
//		Src    *fslocal.FS      // 具体类型，不是接口，无法用包装类型替换
//		Dst    common.RemoteFS
//		Origin common.OriginFS
//	}
//
// 的 Src 是具体类型，且实际执行函数 runBackup 是包内私有，无法从外部注入过滤逻辑。
// 因此这里按 Apache-2.0 许可重新实现同一套流程，语义与上游保持一致
// （backup_complete.ignore 的删除/创建顺序、parts 差集删除、服务端拷贝、并发与进度日志），
// 唯一差异是列举出的 srcParts 会先经过月份过滤。
//
// 上游实现对照：lib/backup/actions/backup.go（v1.150.0）。
package backupaction

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/backup/backupnames"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/backup/common"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/backup/fslocal"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/snapshot/snapshotutil"

	"github.com/surenwuyuwuqiu/vmbackup-partition/internal/monthfilter"
)

// MonthRangeFilename 是记录本次备份月份选择范围的辅助文件。
//
// 以 .ignore 结尾，因此会被远端 ListParts 实现自动忽略
// （fscommon.IgnorePath 只判断 ".ignore" 后缀，见 lib/backup/fscommon/fscommon.go:234；
// 调用方如 lib/backup/fsremote/fsremote.go:56、lib/backup/s3remote/s3.go:326），
// 既不影响 vmrestore，也不会被误判成 part。
const MonthRangeFilename = "backup_month_range.ignore"

// Options 是备份执行参数。
type Options struct {
	// MonthRange 指定要备份的月份闭区间。
	MonthRange monthfilter.Range

	// IncludeNonPartitioned 决定非分区文件（如 metadata/ 下的元数据）是否保留。
	IncludeNonPartitioned bool

	// Concurrency 是并发工作协程数，对应官方 -concurrency。
	Concurrency int

	// MaxBytesPerSecond 是上传限速（字节/秒），0 表示不限速，对应官方 -maxBytesPerSecond。
	MaxBytesPerSecond int

	// Prune 为 true 时，删除目标端存在、而本次源端不存在的 part，
	// 语义与官方 vmbackup 一致（把目标端“对齐”到本次快照的子集）。
	//
	// 为 false 时只追加，绝不删除目标端已有数据。
	// 当多批不同月份范围要写入同一个 -dst 时，必须设为 false，否则先前写入的月份会被删除。
	Prune bool

	// DryRun 为 true 时只计算并输出备份计划，不产生任何远端写操作。
	DryRun bool

	// AllowEmptyResult 为 true 时，允许「区间内没有任何按月分区的 part」时继续执行。
	// 默认 false：此时会拒绝写入一个只含 metadata/ 这类非分区文件、还原后没有任何
	// 时间序列数据的无效备份。
	AllowEmptyResult bool

	// ExpectMonths 是可选安全护栏：若非空，则要求快照中必须存在这些月份的 part，
	// 否则报错退出，用于防止月份记错导致备份了错误的数据集。
	ExpectMonths []monthfilter.Month
}

// Result 汇总一次备份（或试运行）的结果。
type Result struct {
	SnapshotName string
	SnapshotPath string

	Stats     *monthfilter.Stats
	PlanTable string

	Duration time.Duration
	DryRun   bool

	PartsInSnapshot int
	PartsToBackup   int
	BytesToBackup   uint64

	DstPartsBefore int
	OriginParts    int

	DeletedParts int
	DeletedBytes uint64

	CopiedParts int
	CopiedBytes uint64

	UploadedParts int
	UploadedBytes uint64

	// OutOfRangePartsToDelete 是 -prune=true 时将被删除、但月份范围之外的目标端 part 数量，
	// 是“误删历史备份”风险的量化指标。
	OutOfRangePartsToDelete int
}

// Plan 只执行本地计算：打开快照、列举 part、按月份过滤、跑安全护栏。
// 它完全不接触远端存储，因此 -dryRun 在没有 -dst 凭据时也能用。
func Plan(snapshotPath string, opt Options) (*Result, error) {
	opt.DryRun = true
	p, err := buildPlan(snapshotPath, 0, opt)
	if err != nil {
		return nil, err
	}
	defer p.close()
	return p.res, nil
}

// Run 执行一次带月份过滤的完整备份。
//
// snapshotPath 必须是 VictoriaMetrics 快照目录，即 <storageDataPath>/snapshots/<snapshotName>；
// 目录名本身即快照名（形如 20260921120000-XXXXXXXX），用于生成备份元数据。
func Run(ctx context.Context, snapshotPath string, dst common.RemoteFS, origin common.OriginFS, opt Options) (*Result, error) {
	startTime := time.Now()

	if dst == nil {
		return nil, fmt.Errorf("备份目标 -dst 不能为空")
	}
	if origin == nil {
		return nil, fmt.Errorf("origin 不能为空；没有 origin 时应传入 fsnil.FS")
	}
	if origin.String() == dst.String() {
		return nil, fmt.Errorf("origin 与 dst 不能指向同一位置：%s", dst)
	}

	p, err := buildPlan(snapshotPath, opt.MaxBytesPerSecond, opt)
	if err != nil {
		return nil, err
	}
	defer p.close()

	res := p.res
	if opt.DryRun {
		return res, nil
	}

	filter := p.filter
	kept := p.kept

	// ---------------------------------------------------------------
	// 1. 目标端：先摘掉完成标记（与官方顺序一致），再计算差集
	// ---------------------------------------------------------------
	if err := dst.DeleteFile(backupnames.BackupCompleteFilename); err != nil {
		return nil, fmt.Errorf("无法删除目标端 %s 的完成标记文件: %w", dst, err)
	}

	logger.Infof("正在列举目标端 %s 已有的 part ...", dst)
	dstParts, err := dst.ListParts()
	if err != nil {
		return nil, fmt.Errorf("列举目标端 %s 的 part 失败: %w", dst, err)
	}
	res.DstPartsBefore = len(dstParts)

	logger.Infof("正在列举 origin %s 的 part ...", origin)
	originParts, err := origin.ListParts()
	if err != nil {
		return nil, fmt.Errorf("列举 origin %s 的 part 失败: %w", origin, err)
	}
	res.OriginParts = len(originParts)

	// ---------------------------------------------------------------
	// 2. 删除目标端多余 part（仅 -prune=true）
	// ---------------------------------------------------------------
	notInBackup := common.PartsDifference(dstParts, kept)
	if opt.Prune {
		res.OutOfRangePartsToDelete = countOutOfRange(notInBackup, filter.Range)
		if res.OutOfRangePartsToDelete > 0 {
			logger.Warnf("目标端 %s 中有 %d 个 part 位于所选月份范围 %s 之外，本次将被删除（-prune=true）。"+
				"若你希望通过多批不同月份范围累积到同一个 -dst，请显式设置 -prune=false",
				dst, res.OutOfRangePartsToDelete, filter.Range)
		}
		n, size, err := deleteParts(ctx, dst, notInBackup, opt.Concurrency)
		res.DeletedParts, res.DeletedBytes = n, size
		if err != nil {
			return nil, fmt.Errorf("删除目标端多余 part 失败: %w", err)
		}
	} else if len(notInBackup) > 0 {
		logger.Infof("-prune=false：目标端有 %d 个 part 不在本次备份范围内，将原样保留", len(notInBackup))
	}

	// ---------------------------------------------------------------
	// 3. 从 origin 服务端拷贝（免下载）
	// ---------------------------------------------------------------
	partsToCopy := common.PartsDifference(kept, dstParts)
	originPartsToCopy := common.PartsIntersect(originParts, partsToCopy)
	if len(originPartsToCopy) > 0 {
		n, size, err := copyParts(ctx, origin, dst, originPartsToCopy, opt.Concurrency)
		res.CopiedParts, res.CopiedBytes = n, size
		if err != nil {
			return nil, fmt.Errorf("从 origin 服务端拷贝 part 到目标端失败: %w", err)
		}
	}

	// ---------------------------------------------------------------
	// 4. 上传剩余 part
	// ---------------------------------------------------------------
	uploadParts := common.PartsDifference(partsToCopy, originParts)
	n, size, err := uploadPartsFrom(ctx, p.src, dst, uploadParts, opt.Concurrency)
	res.UploadedParts, res.UploadedBytes = n, size
	if err != nil {
		return nil, err
	}

	// ---------------------------------------------------------------
	// 5. 写元数据与完成标记
	// ---------------------------------------------------------------
	if err := storeMetadata(snapshotPath, dst); err != nil {
		return nil, fmt.Errorf("写入备份元数据失败: %w", err)
	}
	if err := storeMonthRange(res, opt, dst); err != nil {
		return nil, fmt.Errorf("写入月份范围说明失败: %w", err)
	}
	if err := dst.CreateFile(backupnames.BackupCompleteFilename, nil); err != nil {
		return nil, fmt.Errorf("在目标端 %s 创建完成标记失败: %w", dst, err)
	}

	res.MarkCompleted(startTime)
	logger.Infof("备份完成：%s → %s；目标端共 %d 个 part (%s)；删除 %d 个 (%s)；"+
		"服务端拷贝 %d 个 (%s)；上传 %d 个 (%s)；耗时 %.3fs",
		snapshotPath, dst, res.Stats.Kept.Parts, monthfilter.HumanizeBytes(res.Stats.Kept.Bytes),
		res.DeletedParts, monthfilter.HumanizeBytes(res.DeletedBytes),
		res.CopiedParts, monthfilter.HumanizeBytes(res.CopiedBytes),
		res.UploadedParts, monthfilter.HumanizeBytes(res.UploadedBytes),
		res.Duration.Seconds())

	return res, nil
}

// MarkCompleted 记录结束耗时。
func (r *Result) MarkCompleted(startTime time.Time) {
	r.Duration = time.Since(startTime)
}

// ---------------------------------------------------------------
// 计划构建
// ---------------------------------------------------------------

type plan struct {
	res    *Result
	src    *fslocal.FS
	filter monthfilter.Filter
	kept   []common.Part
}

func (p *plan) close() {
	if p.src != nil {
		p.src.MustStop()
	}
}

func buildPlan(snapshotPath string, maxBytesPerSecond int, opt Options) (*plan, error) {
	startTime := time.Now()

	if snapshotPath == "" {
		return nil, fmt.Errorf("快照路径不能为空")
	}
	if opt.Concurrency <= 0 {
		opt.Concurrency = 1
	}

	res := &Result{
		SnapshotPath: snapshotPath,
		SnapshotName: filepath.Base(snapshotPath),
		DryRun:       opt.DryRun,
	}

	src := &fslocal.FS{
		Dir:               snapshotPath,
		MaxBytesPerSecond: maxBytesPerSecond,
	}
	if err := src.Init(); err != nil {
		return nil, fmt.Errorf("无法初始化快照源目录 %q: %w", snapshotPath, err)
	}

	logger.Infof("正在列举快照 %q 中的 part ...", snapshotPath)
	allParts, err := src.ListParts()
	if err != nil {
		src.MustStop()
		return nil, fmt.Errorf("列举快照 %q 的 part 失败: %w", snapshotPath, err)
	}
	if len(allParts) == 0 {
		src.MustStop()
		return nil, fmt.Errorf("快照 %q 中没有任何文件；请确认 -storageDataPath 与 -snapshotName 是否正确", snapshotPath)
	}
	common.SortParts(allParts)
	res.PartsInSnapshot = len(allParts)
	logger.Infof("快照 %q 共包含 %d 个 part", snapshotPath, len(allParts))

	filter := monthfilter.Filter{
		Range:                 opt.MonthRange,
		IncludeNonPartitioned: opt.IncludeNonPartitioned,
	}
	agg := monthfilter.NewAggregator(filter)
	kept := make([]common.Part, 0, len(allParts))
	for i := range allParts {
		if _, ok := agg.Add(allParts[i].Path, allParts[i].Size); ok {
			kept = append(kept, allParts[i])
		}
	}
	stats := agg.Stats()

	res.Stats = stats
	res.PlanTable = stats.RenderPlanTable()
	res.PartsToBackup = stats.Kept.Parts
	res.BytesToBackup = stats.Kept.Bytes
	res.Duration = time.Since(startTime)

	logger.Infof("月份过滤区间 %s：保留 %d 个 part (%s)，跳过 %d 个 part (%s)",
		opt.MonthRange, stats.Kept.Parts, monthfilter.HumanizeBytes(stats.Kept.Bytes),
		stats.Dropped.Parts, monthfilter.HumanizeBytes(stats.Dropped.Bytes))

	// 安全护栏 1：期望月份必须存在
	if len(opt.ExpectMonths) > 0 {
		if missing := stats.MissingExpectedMonths(opt.ExpectMonths); len(missing) > 0 {
			src.MustStop()
			return nil, fmt.Errorf("快照 %q 中缺失期望存在的月份 %v；快照实际存在的月份为 %v。"+
				"请核对 -fromMonth/-toMonth，或先用 -dryRun 查看数据分布",
				snapshotPath, formatMonths(missing), formatMonths(stats.MonthsPresent()))
		}
	}

	// 安全护栏 2：不允许写入「没有任何月份数据」的备份。
	//
	// 注意这里判断的是「按月分区的 part 数」，而不是「通过过滤的 part 总数」：
	// 当 -includeNonPartitioned=true（默认）时，metadata/ 下的元数据文件总会被保留，
	// 若用总数判断，一个只含 metadata/ 的空备份会被误判为"非空"而通过。
	// 这种备份还原后不包含任何时间序列，属于无效备份，必须拦下。
	partitionedKept := stats.Kept.Parts - stats.NonPartitionedKept.Parts
	if partitionedKept == 0 && !opt.AllowEmptyResult {
		src.MustStop()
		return nil, fmt.Errorf("月份区间 %s 在快照 %q 中没有任何按月分区的数据 part"+
			"（仅匹配到 %d 个非分区文件，如 metadata/ 下的元数据）；快照实际存在的月份为 %v。"+
			"请核对 -fromMonth/-toMonth，确认无误可加 -allowEmptyResult",
			opt.MonthRange, snapshotPath, stats.NonPartitionedKept.Parts, formatMonths(stats.MonthsPresent()))
	}
	if partitionedKept == 0 && opt.AllowEmptyResult {
		logger.Warnf("-allowEmptyResult 已生效：快照 %q 在区间 %s 内没有按月分区的数据 part，"+
			"本次备份只包含 %d 个非分区文件（metadata/ 等），还原后不会包含任何时间序列",
			snapshotPath, opt.MonthRange, stats.NonPartitionedKept.Parts)
	}

	if opt.DryRun {
		src.MustStop()
		logger.Infof("试运行结束（未产生任何写入）：将备份 %d 个 part (%s)，跳过 %d 个 part (%s)",
			stats.Kept.Parts, monthfilter.HumanizeBytes(stats.Kept.Bytes),
			stats.Dropped.Parts, monthfilter.HumanizeBytes(stats.Dropped.Bytes))
	}

	return &plan{res: res, src: src, filter: filter, kept: kept}, nil
}

// ---------------------------------------------------------------
// 目标端元数据
// ---------------------------------------------------------------

// BackupMetadata 与官方 lib/backup/actions/backup.go 的 BackupMetadata 字段保持一致，
// 以便其它工具（如企业版 vmbackupmanager）继续解析。
type BackupMetadata struct {
	CreatedAt   string `json:"created_at"`
	CompletedAt string `json:"completed_at"`
}

// MonthRangeMetadata 记录本次备份的月份选择，便于日后审计“这个备份到底含哪些月份”。
type MonthRangeMetadata struct {
	FromMonth             string   `json:"from_month"`
	ToMonth               string   `json:"to_month"`
	MonthRange            string   `json:"month_range"`
	IncludeNonPartitioned bool     `json:"include_non_partitioned"`
	MonthsPresent         []string `json:"months_present_in_snapshot"`
	MonthsKept            []string `json:"months_kept"`
	SnapshotName          string   `json:"snapshot_name"`
	Prune                 bool     `json:"prune"`
	PartsKept             int      `json:"parts_kept"`
	BytesKept             uint64   `json:"bytes_kept"`
	PartsSkipped          int      `json:"parts_skipped"`
	BytesSkipped          uint64   `json:"bytes_skipped"`
	Tool                  string   `json:"tool"`
}

func storeMetadata(snapshotPath string, dst common.RemoteFS) error {
	snapshotName := filepath.Base(snapshotPath)
	snapshotTime, err := snapshotutil.Time(snapshotName)
	if err != nil {
		return fmt.Errorf("无法解析快照名 %q 中的时间: %w", snapshotName, err)
	}
	d := BackupMetadata{
		CreatedAt:   snapshotTime.Format(time.RFC3339),
		CompletedAt: time.Now().Format(time.RFC3339),
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("序列化备份元数据失败: %w", err)
	}
	return dst.CreateFile(backupnames.BackupMetadataFilename, raw)
}

func storeMonthRange(res *Result, opt Options, dst common.RemoteFS) error {
	kept := make([]string, 0, len(res.Stats.Months))
	for _, m := range res.Stats.Months {
		if m.Include {
			kept = append(kept, m.Month.String())
		}
	}
	d := MonthRangeMetadata{
		FromMonth:             opt.MonthRange.From.String(),
		ToMonth:               opt.MonthRange.To.String(),
		MonthRange:            opt.MonthRange.String(),
		IncludeNonPartitioned: opt.IncludeNonPartitioned,
		MonthsPresent:         formatMonths(res.Stats.MonthsPresent()),
		MonthsKept:            kept,
		SnapshotName:          res.SnapshotName,
		Prune:                 opt.Prune,
		PartsKept:             res.Stats.Kept.Parts,
		BytesKept:             res.Stats.Kept.Bytes,
		PartsSkipped:          res.Stats.Dropped.Parts,
		BytesSkipped:          res.Stats.Dropped.Bytes,
		Tool:                  "vmbackup-partition",
	}
	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return dst.CreateFile(MonthRangeFilename, raw)
}

// ---------------------------------------------------------------
// 传输
// ---------------------------------------------------------------

func deleteParts(ctx context.Context, dst common.RemoteFS, parts []common.Part, concurrency int) (int, uint64, error) {
	if len(parts) == 0 {
		return 0, 0, nil
	}
	logger.Infof("正在从 %s 删除 %d 个多余 part ...", dst, len(parts))
	var done atomic.Int64
	err := runParallel(ctx, concurrency, parts, func(p common.Part) error {
		if err := dst.DeletePart(p); err != nil {
			return fmt.Errorf("删除 %s 失败: %w", &p, err)
		}
		done.Add(1)
		return nil
	}, func(elapsed time.Duration) {
		logger.Infof("已从 %s 删除 %d/%d 个 part，耗时 %s", dst, done.Load(), len(parts), elapsed)
	})
	if err != nil {
		return int(done.Load()), sizeOf(parts), err
	}
	if err := dst.RemoveEmptyDirs(); err != nil {
		return len(parts), sizeOf(parts), fmt.Errorf("清理 %s 的空目录失败: %w", dst, err)
	}
	return len(parts), sizeOf(parts), nil
}

func copyParts(ctx context.Context, origin common.OriginFS, dst common.RemoteFS, parts []common.Part, concurrency int) (int, uint64, error) {
	if len(parts) == 0 {
		return 0, 0, nil
	}
	logger.Infof("正在服务端拷贝 %d 个 part：%s → %s ...", len(parts), origin, dst)
	var done atomic.Int64
	err := runParallel(ctx, concurrency, parts, func(p common.Part) error {
		if err := dst.CopyPart(origin, p); err != nil {
			// 跨后端类型（例如 origin 在 s3、dst 在 gcs）时 CopyPart 会失败，
			// 退化为“下载再上传”，保证数据依然被备份。
			if fallbackErr := downloadAndUpload(origin, dst, p); fallbackErr != nil {
				return fmt.Errorf("拷贝 %s 失败（CopyPart: %v；回退下载上传: %v）", &p, err, fallbackErr)
			}
		}
		done.Add(1)
		return nil
	}, func(elapsed time.Duration) {
		logger.Infof("已服务端拷贝 %d/%d 个 part，耗时 %s", done.Load(), len(parts), elapsed)
	})
	return int(done.Load()), sizeOf(parts), err
}

// downloadAndUpload 用于 origin 与 dst 后端类型不同的场景。
func downloadAndUpload(origin common.OriginFS, dst common.RemoteFS, p common.Part) error {
	srcFS, ok := origin.(common.RemoteFS)
	if !ok {
		return fmt.Errorf("origin %s 不支持下载，无法回退", origin)
	}
	pr, pw := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		bw := bufio.NewWriterSize(pw, 1024*1024)
		err := srcFS.DownloadPart(p, bw)
		if err == nil {
			err = bw.Flush()
		}
		_ = pw.CloseWithError(err)
		errCh <- err
	}()
	uploadErr := dst.UploadPart(p, pr)
	_ = pr.Close()
	downloadErr := <-errCh
	if uploadErr != nil {
		return fmt.Errorf("上传失败: %w", uploadErr)
	}
	if downloadErr != nil {
		return fmt.Errorf("下载失败: %w", downloadErr)
	}
	return nil
}

func uploadPartsFrom(ctx context.Context, src *fslocal.FS, dst common.RemoteFS, parts []common.Part, concurrency int) (int, uint64, error) {
	if len(parts) == 0 {
		logger.Infof("没有需要上传的 part")
		return 0, 0, nil
	}
	total := sizeOf(parts)
	logger.Infof("正在上传 %d 个 part (%s)：%s → %s ...", len(parts), monthfilter.HumanizeBytes(total), src, dst)

	var bytesUploaded atomic.Uint64
	var partsUploaded atomic.Int64
	err := runParallel(ctx, concurrency, parts, func(p common.Part) error {
		rc, err := src.NewReadCloser(p)
		if err != nil {
			return fmt.Errorf("读取 %s 失败: %w", &p, err)
		}
		sr := &statReader{r: rc, bytesRead: &bytesUploaded}
		uploadErr := dst.UploadPart(p, sr)
		closeErr := rc.Close()
		if uploadErr != nil {
			return fmt.Errorf("上传 %s 到 %s 失败: %w", &p, dst, uploadErr)
		}
		if closeErr != nil {
			return fmt.Errorf("关闭 %s 的读取器失败: %w", &p, closeErr)
		}
		partsUploaded.Add(1)
		return nil
	}, func(elapsed time.Duration) {
		if elapsed.Seconds() <= 0 || total == 0 {
			return
		}
		n := bytesUploaded.Load()
		prc := 100 * float64(n) / float64(total)
		var eta time.Duration
		if n > 0 {
			speed := float64(n) / elapsed.Seconds()
			estimatedTotal := time.Duration(float64(total) / speed * float64(time.Second))
			if estimatedTotal > elapsed {
				eta = estimatedTotal - elapsed
			}
		}
		logger.Infof("已上传 %d/%d 个 part，%s/%s (%.2f%%)，耗时 %s，预计剩余 %s",
			partsUploaded.Load(), len(parts),
			monthfilter.HumanizeBytes(n), monthfilter.HumanizeBytes(total), prc, elapsed, eta)
	})
	if err != nil {
		return int(partsUploaded.Load()), bytesUploaded.Load(), err
	}
	return len(parts), bytesUploaded.Load(), nil
}

type statReader struct {
	r         io.Reader
	bytesRead *atomic.Uint64
}

func (sr *statReader) Read(p []byte) (int, error) {
	n, err := sr.r.Read(p)
	sr.bytesRead.Add(uint64(n))
	return n, err
}

// ---------------------------------------------------------------
// 并发原语（语义与官方 lib/backup/actions/util.go 一致）
// ---------------------------------------------------------------

func runParallel(ctx context.Context, concurrency int, parts []common.Part, f func(p common.Part) error, progress func(elapsed time.Duration)) error {
	var err error
	runWithProgress(progress, func() {
		err = runParallelInternal(ctx, concurrency, parts, f)
	})
	return err
}

// runWithProgress 每 10 秒回调一次 progress，结束时再回调一次，
// 与官方 lib/backup/actions/util.go:runWithProgress 的间隔一致。
func runWithProgress(progress func(elapsed time.Duration), f func()) {
	if progress == nil {
		f()
		return
	}
	startTime := time.Now()
	doneCh := make(chan struct{})
	go func() {
		f()
		close(doneCh)
	}()

	tc := time.NewTicker(10 * time.Second)
	defer tc.Stop()
	for {
		select {
		case <-doneCh:
			progress(time.Since(startTime))
			return
		case <-tc.C:
			progress(time.Since(startTime))
		}
	}
}

func runParallelInternal(ctx context.Context, concurrency int, parts []common.Part, f func(p common.Part) error) error {
	if concurrency <= 0 {
		concurrency = 1
	}
	if len(parts) == 0 {
		return nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// resultCh 的缓冲容量等于任务总数，保证 worker 发送结果永不阻塞，
	// 从而避免“读端提前退出”把 worker 卡死。
	resultCh := make(chan error, len(parts))
	workCh := make(chan common.Part)

	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				var p common.Part
				var ok bool
				select {
				case p, ok = <-workCh:
					if !ok {
						return
					}
				case <-ctx.Done():
					return
				}
				if err := f(p); err != nil {
					resultCh <- err
					cancel()
					return
				}
				resultCh <- nil
			}
		}()
	}

	// 派发任务；ctx 取消时立即停止派发。
	go func() {
		defer close(workCh)
		for i := range parts {
			select {
			case workCh <- parts[i]:
			case <-ctx.Done():
				return
			}
		}
	}()

	wg.Wait()
	close(resultCh)

	var firstErr error
	for err := range resultCh {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ---------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------

func sizeOf(parts []common.Part) uint64 {
	var n uint64
	for i := range parts {
		n += parts[i].Size
	}
	return n
}

func countOutOfRange(parts []common.Part, r monthfilter.Range) int {
	n := 0
	for i := range parts {
		c := monthfilter.Classify(parts[i].Path)
		if !c.Partitioned || !r.Contains(c.Month) {
			n++
		}
	}
	return n
}

func formatMonths(months []monthfilter.Month) []string {
	out := make([]string, 0, len(months))
	for _, m := range months {
		out = append(out, m.String())
	}
	return out
}
