// Command vmbackup-partition 是 VictoriaMetrics 官方 vmbackup 的“按月份分区筛选”增强版。
//
// 背景：开源版 vmbackup 只能整份备份一个 vmstorage 的数据目录，无法只备份指定月份分区。
// 官方文档确认 vmbackupmanager（企业版）才提供按时间范围选择的能力：
//
//	https://docs.victoriametrics.com/victoriametrics/vmbackupmanager/
//
// 本工具在保持与 vmbackup 完全一致的命令行、快照机制、备份格式与并发语义的前提下，
// 新增 -fromMonth/-toMonth 两个参数，只把落在指定月份闭区间内的分区写入目标端。
//
// 由于 VictoriaMetrics 的备份产物是“按 part 路径寻址的远端对象集合”（不依赖 parts.json），
// 过滤只发生在源端，因此官方 vmrestore 可以直接还原本工具产出的备份，无需任何改动。
package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/backup/actions"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/backup/common"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/backup/fsnil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/buildinfo"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/envflag"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/flagutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/httpserver"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/procutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/pushmetrics"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/snapshot"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/snapshot/snapshotutil"

	"github.com/surenwuyuwuqiu/vmbackup-partition/internal/backupaction"
	"github.com/surenwuyuwuqiu/vmbackup-partition/internal/monthfilter"
)

var (
	httpListenAddr = flag.String("httpListenAddr", ":8420", "Address for exposing metrics at /metrics page. "+
		"Use unix:/path/to/socket to listen on Unix domain socket")
	storageDataPath = flag.String("storageDataPath", "victoria-metrics-data", "Path to VictoriaMetrics data. "+
		"Must match -storageDataPath from VictoriaMetrics or vmstorage")
	snapshotName = flag.String("snapshotName", "", "Name for the snapshot to backup. "+
		"There is no need in setting -snapshotName if -snapshot.createURL is set")
	snapshotCreateURL = flag.String("snapshot.createURL", "", "VictoriaMetrics create snapshot url. "+
		"When this is given a snapshot will automatically be created during backup. "+
		"Example: http://victoriametrics:8428/snapshot/create . "+
		"There is no need in setting -snapshotName if -snapshot.createURL is set")
	snapshotDeleteURL = flag.String("snapshot.deleteURL", "", "VictoriaMetrics delete snapshot url. Optional. "+
		"Will be generated from -snapshot.createURL if not provided. "+
		"All created snapshots will be automatically deleted. Example: http://victoriametrics:8428/snapshot/delete")
	dst = flag.String("dst", "", "Where to put the backup on the remote storage. "+
		"Example: gs://bucket/path/to/backup, s3://bucket/path/to/backup, azblob://container/path/to/backup or fs:///path/to/local/backup/dir\n"+
		"-dst can point to the previous backup. In this case incremental backup is performed, i.e. only changed data is uploaded\n"+
		"Note: If custom S3 endpoint is used, URL should contain only name of the bucket, while hostname of S3 server must be specified via the -customS3Endpoint command-line flag.")
	origin            = flag.String("origin", "", "Optional origin directory on the remote storage with old backup for server-side copying when performing full backup. This speeds up full backups")
	concurrency       = flag.Int("concurrency", 10, "The number of concurrent workers. Higher concurrency may reduce backup duration")
	maxBytesPerSecond = flagutil.NewBytes("maxBytesPerSecond", 0, "The maximum upload speed. There is no limit if it is set to 0")

	// ------------------------------------------------------------------
	// 以下为本工具新增的参数
	// ------------------------------------------------------------------
	fromMonth = flag.String("fromMonth", "", "【必填】起始月份（闭区间），格式 YYYY_MM / YYYY-MM / YYYYMM。\n"+
		"VictoriaMetrics 的存储数据按自然月切分为 data/<small|big|indexdb>/<YYYY_MM>/ 目录，\n"+
		"本工具只备份落在 [fromMonth, toMonth] 区间内的分区。例如 -fromMonth=2026_02")
	toMonth               = flag.String("toMonth", "", "【必填】结束月份（闭区间），格式同 -fromMonth。例如 -toMonth=2026_06")
	includeNonPartitioned = flag.Bool("includeNonPartitioned", true, "是否保留非月份分区文件（主要是 metadata/ 目录下的租户与索引元数据）。\n"+
		"强烈建议保持默认值 true：这些文件体积很小，但缺失会导致还原后的存储元数据不完整")
	prune = flag.Bool("prune", true, "是否删除目标端存在、而本次备份范围内不存在的 part，语义与官方 vmbackup 保持一致（把目标端对齐到本次备份的子集）。\n"+
		"注意：如果目标端 -dst 中已经存在其它月份的数据（例如你打算把多批不同月份累积到同一个目录），\n"+
		"必须显式设置 -prune=false，否则那些月份会被删除")
	dryRun = flag.Bool("dryRun", false, "只列举快照并输出备份计划（各月份的 part 数量与体积、保留/跳过决策），不产生任何远端写入。\n"+
		"该模式下不需要 -dst，也不需要远端凭据")
	allowEmptyResult = flag.Bool("allowEmptyResult", false, "当月份区间内没有任何『按月分区的 part』时，是否允许继续执行。\n"+
		"默认拒绝：因为此时备份只可能包含 metadata/ 之类的非分区文件，还原后不含任何时间序列，属于无效备份")
	expectMonths = flag.String("expectMonths", "", "可选安全护栏：以逗号分隔的月份列表（如 2026_02,2026_03）。\n"+
		"若设置，则要求快照中确实存在这些月份的 part，否则立即报错退出，防止月份记错导致备份了错误的数据集")
	planOut = flag.String("planOut", "", "可选：将备份计划以 JSON 写入指定文件，便于审计与自动化比对")
)

func main() {
	// 把 flag 与帮助信息写到 stdout，便于 grep 与管道处理（与官方 vmbackup 一致）。
	flag.CommandLine.SetOutput(os.Stdout)
	flag.Usage = usage
	envflag.Parse()
	initSecretFlags()
	buildinfo.Init()
	logger.Init()

	opt, err := buildOptions()
	if err != nil {
		logger.Fatalf("参数校验失败: %s", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		procutil.WaitForSigterm()
		logger.Infof("收到停止信号，正在取消备份操作")
		cancel()
	}()

	// 快照删除函数在出错时也要被调用，因为 logger.Fatalf 会直接退出进程，
	// 不会执行 defer。参见 https://github.com/VictoriaMetrics/VictoriaMetrics/issues/2055
	deleteSnapshot := func() {}

	if len(*snapshotCreateURL) > 0 {
		createURL, err := url.Parse(*snapshotCreateURL)
		if err != nil {
			logger.Fatalf("无法解析 -snapshot.createURL: %s", err)
		}
		if len(*snapshotName) > 0 {
			logger.Fatalf("-snapshot.createURL 与 -snapshotName 不能同时设置，因为设置前者时快照会自动创建")
		}
		logger.Infof("快照创建地址 %s", createURL.Redacted())
		if len(*snapshotDeleteURL) <= 0 {
			if err := flag.Set("snapshot.deleteURL", strings.Replace(*snapshotCreateURL, "/create", "/delete", 1)); err != nil {
				logger.Fatalf("设置 -snapshot.deleteURL 失败: %v", err)
			}
		}
		deleteURL, err := url.Parse(*snapshotDeleteURL)
		if err != nil {
			logger.Fatalf("无法解析 -snapshot.deleteURL: %s", err)
		}
		logger.Infof("快照删除地址 %s", deleteURL.Redacted())

		name, err := snapshot.Create(ctx, createURL.String())
		if err != nil {
			logger.Fatalf("创建快照失败: %s", err)
		}
		if err := flag.Set("snapshotName", name); err != nil {
			logger.Fatalf("设置 -snapshotName 失败: %v", err)
		}
		deleteSnapshot = func() {
			// 这里不使用 ctx：进程被中断时 ctx 可能已被取消。
			if err := snapshot.Delete(context.Background(), deleteURL.String(), name); err != nil {
				logger.Fatalf("删除快照失败: %s", err)
			}
		}
	}

	if len(*snapshotName) == 0 {
		logger.Fatalf("必须设置 -snapshotName，或设置 -snapshot.createURL 以便自动创建快照")
	}
	if err := snapshotutil.Validate(*snapshotName); err != nil {
		logger.Fatalf("-snapshotName=%q 非法: %s", *snapshotName, err)
	}
	snapshotPath := filepath.Join(*storageDataPath, "snapshots", *snapshotName)
	if err := checkSnapshotDir(snapshotPath); err != nil {
		logger.Fatalf("%s", err)
	}

	// --------------------------------------------------------------
	// 试运行：纯本地计算，不起 HTTP 服务、不连远端
	// --------------------------------------------------------------
	if *dryRun {
		res, err := backupaction.Plan(snapshotPath, opt)
		deleteSnapshot()
		if err != nil {
			logger.Fatalf("试运行失败: %s", err)
		}
		printPlan(snapshotPath, res, opt)
		return
	}

	listenAddrs := []string{*httpListenAddr}
	go httpserver.Serve(listenAddrs, nil, httpserver.ServeOptions{})
	pushmetrics.Init()

	runErr := runBackup(ctx, snapshotPath, opt)
	deleteSnapshot()
	if runErr != nil {
		logger.Fatalf("备份失败: %s", runErr)
	}

	pushmetrics.StopAndPush()
	startTime := time.Now()
	logger.Infof("正在优雅关闭指标 HTTP 服务 %q", listenAddrs)
	if err := httpserver.Stop(listenAddrs); err != nil {
		logger.Fatalf("关闭指标 HTTP 服务失败: %s", err)
	}
	logger.Infof("指标 HTTP 服务已在 %.3f 秒内关闭", time.Since(startTime).Seconds())
}

func runBackup(ctx context.Context, snapshotPath string, opt backupaction.Options) error {
	dstFS, err := newDstFS(ctx)
	if err != nil {
		return err
	}
	originFS, err := newOriginFS(ctx)
	if err != nil {
		dstFS.MustStop()
		return err
	}

	_, err = backupaction.Run(ctx, snapshotPath, dstFS, originFS, opt)
	dstFS.MustStop()
	originFS.MustStop()
	return err
}

// buildOptions 解析并校验本工具特有参数。
func buildOptions() (backupaction.Options, error) {
	if *fromMonth == "" || *toMonth == "" {
		return backupaction.Options{}, fmt.Errorf("-fromMonth 与 -toMonth 均为必填参数，" +
			"例如 -fromMonth=2026_02 -toMonth=2026_06")
	}
	r, err := monthfilter.ParseRange(*fromMonth, *toMonth)
	if err != nil {
		return backupaction.Options{}, err
	}

	var expect []monthfilter.Month
	if strings.TrimSpace(*expectMonths) != "" {
		for _, s := range strings.Split(*expectMonths, ",") {
			if strings.TrimSpace(s) == "" {
				continue
			}
			m, err := monthfilter.Parse(s)
			if err != nil {
				return backupaction.Options{}, fmt.Errorf("解析 -expectMonths=%q 失败: %w", *expectMonths, err)
			}
			expect = append(expect, m)
		}
	}

	opt := backupaction.Options{
		MonthRange:            r,
		IncludeNonPartitioned: *includeNonPartitioned,
		Concurrency:           *concurrency,
		MaxBytesPerSecond:     maxBytesPerSecond.IntN(),
		Prune:                 *prune,
		DryRun:                *dryRun,
		AllowEmptyResult:      *allowEmptyResult,
		ExpectMonths:          expect,
	}
	if opt.Concurrency <= 0 {
		return opt, fmt.Errorf("-concurrency 必须大于 0，当前为 %d", opt.Concurrency)
	}
	return opt, nil
}

func checkSnapshotDir(snapshotPath string) error {
	f, err := os.Open(snapshotPath)
	if err != nil {
		return fmt.Errorf("无法打开快照目录 %q: %w（请确认 -storageDataPath 与 -snapshotName 是否正确）", snapshotPath, err)
	}
	fi, err := f.Stat()
	_ = f.Close()
	if err != nil {
		return fmt.Errorf("无法 stat 快照目录 %q: %w", snapshotPath, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("快照路径 %q 不是目录", snapshotPath)
	}
	return nil
}

func printPlan(snapshotPath string, res *backupaction.Result, opt backupaction.Options) {
	out := os.Stdout
	fmt.Fprintf(out, "\n")
	fmt.Fprintf(out, "================ 备份计划（试运行，未写入任何数据）================\n")
	fmt.Fprintf(out, "快照目录      : %s\n", snapshotPath)
	fmt.Fprintf(out, "快照名        : %s\n", res.SnapshotName)
	fmt.Fprintf(out, "月份过滤区间  : %s\n", opt.MonthRange)
	fmt.Fprintf(out, "保留非分区文件: %v\n", opt.IncludeNonPartitioned)
	fmt.Fprintf(out, "目标端删除策略: -prune=%v\n", opt.Prune)
	fmt.Fprintf(out, "\n")
	fmt.Fprint(out, res.PlanTable)
	fmt.Fprintf(out, "\n")

	if len(res.Stats.NonPartitionedSample) > 0 {
		fmt.Fprintf(out, "非分区文件样例（最多 16 个）:\n")
		for _, s := range res.Stats.NonPartitionedSample {
			fmt.Fprintf(out, "  - %s\n", s)
		}
		fmt.Fprintf(out, "\n")
	}
	fmt.Fprintf(out, "预期上传体积  : %s（%d 个 part）\n",
		monthfilter.HumanizeBytes(res.Stats.Kept.Bytes), res.Stats.Kept.Parts)
	fmt.Fprintf(out, "预计跳过体积  : %s（%d 个 part）\n",
		monthfilter.HumanizeBytes(res.Stats.Dropped.Bytes), res.Stats.Dropped.Parts)
	fmt.Fprintf(out, "================================================================\n\n")

	if *planOut != "" {
		raw, err := res.Stats.EncodePlanJSON(opt.IncludeNonPartitioned)
		if err != nil {
			logger.Fatalf("生成计划 JSON 失败: %s", err)
		}
		if err := os.WriteFile(*planOut, raw, 0644); err != nil {
			logger.Fatalf("写入 -planOut=%q 失败: %s", *planOut, err)
		}
		fmt.Fprintf(out, "计划 JSON 已写入 %s\n", *planOut)
	}
}

func usage() {
	const s = `
vmbackup-partition 是 VictoriaMetrics 官方 vmbackup 的增强版：
它保留了 vmbackup 的全部命令行参数、快照机制、备份格式与并发语义，
额外支持只备份指定月份区间的分区数据。

VictoriaMetrics 的存储数据按自然月切分为目录
    <storageDataPath>/data/<small|big|indexdb>/<YYYY_MM>/<partID>/...
本工具通过 -fromMonth / -toMonth 选定一个闭区间，只把区间内的月份分区写入目标端，
区间外的月份（以及可选的非分区文件）不会出现在备份中。

产出的备份是标准 VictoriaMetrics 备份格式，可直接用官方 vmrestore 还原，无需任何改动。

示例：
  # 1) 先看计划（不需要 -dst，也不需要远端凭据）
  vmbackup-partition -storageDataPath=/vm/data \
      -snapshot.createURL=http://127.0.0.1:8482/snapshot/create \
      -fromMonth=2026_02 -toMonth=2026_06 -dryRun

  # 2) 备份到本地目录
  vmbackup-partition -storageDataPath=/vm/data \
      -snapshot.createURL=http://127.0.0.1:8482/snapshot/create \
      -fromMonth=2026_02 -toMonth=2026_06 \
      -dst=fs:///backup/vmstorage-0

  # 3) 备份到 S3/MinIO
  vmbackup-partition -storageDataPath=/vm/data \
      -snapshot.createURL=http://127.0.0.1:8482/snapshot/create \
      -fromMonth=2026_02 -toMonth=2026_06 \
      -dst=s3://bucket/vmstorage-0 -customS3Endpoint=http://minio:9000

  # 4) 已有快照时直接备份（不自动创建快照）
  vmbackup-partition -storageDataPath=/vm/data \
      -snapshotName=20260921120000-1A2B3C4D \
      -fromMonth=2026_02 -toMonth=2026_06 -dst=fs:///backup/vmstorage-0

完整文档见 README.md 与 docs/ 目录。
`
	flagutil.Usage(s)
}

func newDstFS(ctx context.Context) (common.RemoteFS, error) {
	if len(*dst) == 0 {
		return nil, fmt.Errorf("-dst 不能为空")
	}
	fs, err := actions.NewRemoteFS(ctx, *dst, nil)
	if err != nil {
		return nil, fmt.Errorf("无法解析 -dst=%q: %w", *dst, err)
	}
	if hasFilepathPrefix(*dst, *storageDataPath) {
		return nil, fmt.Errorf("-dst=%q 不能位于 VictoriaMetrics 数据目录内部（-storageDataPath=%q）", *dst, *storageDataPath)
	}
	return fs, nil
}

func newOriginFS(ctx context.Context) (common.OriginFS, error) {
	if len(*origin) == 0 {
		return &fsnil.FS{}, nil
	}
	fs, err := actions.NewRemoteFS(ctx, *origin, nil)
	if err != nil {
		return nil, fmt.Errorf("无法解析 -origin=%q: %w", *origin, err)
	}
	return fs, nil
}

// hasFilepathPrefix 判断 path（fs:// 形式）是否位于 prefix 目录内部。
// 逻辑与官方 app/vmbackup/main.go 的 hasFilepathPrefix 一致，
// 用于阻止把备份写进 vmstorage 的数据目录。
func hasFilepathPrefix(path, prefix string) bool {
	if !strings.HasPrefix(path, "fs://") {
		return false
	}
	path = path[len("fs://"):]
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	prefixAbs, err := filepath.Abs(prefix)
	if err != nil {
		return false
	}
	if prefixAbs == pathAbs {
		return true
	}
	rel, err := filepath.Rel(prefixAbs, pathAbs)
	if err != nil {
		return false
	}
	if i := strings.Index(rel, "."); i == 0 {
		return false
	}
	return true
}

// initSecretFlags 必须在 flag 解析之后、logger 初始化之前调用。
func initSecretFlags() {
	flagutil.RegisterSecretFlag("snapshot.createURL")
	flagutil.RegisterSecretFlag("snapshot.deleteURL")
	pushmetrics.InitSecretFlags()
}
