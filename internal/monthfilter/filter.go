package monthfilter

import (
	"fmt"
	"sort"
	"strings"
)

// VictoriaMetrics 存储层目录名常量，与 lib/storage/filenames.go 逐字对应：
//
//	smallDirname     = "small"
//	bigDirname       = "big"
//	indexdbDirname   = "indexdb"
//	dataDirname      = "data"
//	metadataDirname  = "metadata"
//	snapshotsDirname = "snapshots"
const (
	DirData     = "data"
	DirSmall    = "small"
	DirBig      = "big"
	DirIndexDB  = "indexdb"
	DirMetadata = "metadata"
)

// maxPartitionProbeDepth 是在 data/ 之下探测分区目录名的最大深度。
//
// 当前权威布局为 data/<small|big|indexdb>/<YYYY_MM>/...（深度 1），
// 见官方文档中的实际日志样例（docs/victoriametrics/vmctl/thanos.md）：
//
//	creating a partition "2025_04" with smallPartsPath=".../data/small/2025_04",
//	bigPartsPath=".../data/big/2025_04"
//
// 这里把探测深度放宽到 3，是为了对“中间多插一层目录”的变体保持稳健：
// 一旦某层目录名严格匹配 YYYY_MM，就认定为分区路径。
//
// 放宽不会误判：data/<subdir>/ 之下只可能是分区目录，分区目录之下只可能是
// 16 位十六进制命名的 part 目录，part 目录之下只有数据文件。
// 因此除“分区目录本身”外，不可能再出现严格形如 YYYY_MM 的目录名。
const maxPartitionProbeDepth = 3

// Classification 描述某个快照内 part 路径的归类结果。
type Classification struct {
	// Path 是相对快照根的规范化路径，以 / 分隔，例如
	// data/small/2026_02/0000000000000001/timestamps.bin
	Path string

	// Partitioned 表示该路径位于按月分区目录内。
	Partitioned bool

	// Month 仅在 Partitioned 为 true 时有效，是路径所属的自然月分区。
	Month Month

	// SubDir 是 data/ 之下的一级目录名（small / big / indexdb）。
	SubDir string
}

// Classify 解析 part 路径，判断其是否落在按月分区目录内以及属于哪个月。
//
// 判定规则（严格对齐 lib/storage 的目录构造逻辑）：
//   - 路径首段必须是 "data"，否则视为非分区路径（例如 metadata/ 下的元数据文件）；
//   - 在 data/ 之后的 1..maxPartitionProbeDepth 层内，寻找严格匹配 YYYY_MM 的目录名；
//     找到即视为分区路径，并记录该月；未找到则视为非分区路径。
func Classify(path string) Classification {
	c := Classification{Path: path}

	segs := strings.Split(path, "/")
	if len(segs) < 3 || segs[0] != DirData {
		return c
	}

	for i := 1; i <= maxPartitionProbeDepth && i < len(segs); i++ {
		if !IsMonthDirName(segs[i]) {
			continue
		}
		m, err := Parse(segs[i])
		if err != nil {
			// IsMonthDirName 已保证名字合法，这里不会发生。
			return c
		}
		c.Partitioned = true
		c.Month = m
		c.SubDir = segs[1]
		return c
	}

	return c
}

// Filter 按月份闭区间筛选 part 路径。
type Filter struct {
	// Range 是保留的月份区间（闭区间）。
	Range Range

	// IncludeNonPartitioned 决定非分区路径（如 metadata/ 下的元数据文件）是否保留。
	//
	// 默认应为 true：metadata/ 体积很小且与租户、索引元信息相关，
	// 丢弃它会导致还原后的存储缺失元数据。
	IncludeNonPartitioned bool
}

// Decide 对单个 part 路径作出过滤决策。
//
// 返回的 Classification 便于调用方做进一步统计或日志输出。
func (f Filter) Decide(path string) (Classification, bool) {
	c := Classify(path)
	if !c.Partitioned {
		return c, f.IncludeNonPartitioned
	}
	return c, f.Range.Contains(c.Month)
}

// Total 表示一组 part 的数量与字节数。
type Total struct {
	Parts int
	Bytes uint64
}

// Add 累加另一个 Total。
func (t *Total) Add(other Total) {
	t.Parts += other.Parts
	t.Bytes += other.Bytes
}

// MonthStat 是单个自然月的 part 统计。
type MonthStat struct {
	Month   Month
	Total   Total
	Include bool
	// SubDirs 记录该月出现的具体存储子目录（small / big / indexdb）。
	SubDirs []string
}

// Stats 汇总一次过滤的整体结果。
type Stats struct {
	Range Range

	Total   Total // 快照中的全部 part
	Kept    Total // 通过过滤的 part
	Dropped Total // 被过滤掉的 part

	Months []MonthStat // 按月份升序，覆盖快照中实际出现的所有月份

	NonPartitioned       Total    // 非分区 part 合计
	NonPartitionedKept   Total    // 其中被保留的
	NonPartitionedSample []string // 非分区路径样例，便于人工核对
	UnknownMonths        []Month  // 出现在快照中、但不在期望集合内的月份（由 CLI 填充）
}

// Aggregator 在流式遍历 part 列表时做增量聚合，避免为海量 part 保存额外状态。
type Aggregator struct {
	filter Filter

	total   Total
	kept    Total
	dropped Total

	monthOrder []int // Month.Index()，保持首次出现顺序之后统一排序
	byMonth    map[int]*MonthStat

	nonPartitioned     Total
	nonPartitionedKept Total
	nonPartitionedSmp  []string
}

const maxNonPartitionedSample = 16

// NewAggregator 构造聚合器。
func NewAggregator(f Filter) *Aggregator {
	return &Aggregator{
		filter:  f,
		byMonth: make(map[int]*MonthStat),
	}
}

// Add 记录一个 part 并返回其是否被保留。
func (a *Aggregator) Add(path string, size uint64) (Classification, bool) {
	one := Total{Parts: 1, Bytes: size}

	c, include := a.filter.Decide(path)
	a.total.Add(one)
	if include {
		a.kept.Add(one)
	} else {
		a.dropped.Add(one)
	}

	if !c.Partitioned {
		a.nonPartitioned.Add(one)
		if include {
			a.nonPartitionedKept.Add(one)
		}
		if len(a.nonPartitionedSmp) < maxNonPartitionedSample {
			a.nonPartitionedSmp = append(a.nonPartitionedSmp, path)
		}
		return c, include
	}

	idx := c.Month.Index()
	st, ok := a.byMonth[idx]
	if !ok {
		st = &MonthStat{Month: c.Month, Include: a.filter.Range.Contains(c.Month)}
		a.byMonth[idx] = st
		a.monthOrder = append(a.monthOrder, idx)
	}
	st.Total.Add(one)
	if c.SubDir != "" && !containsString(st.SubDirs, c.SubDir) {
		st.SubDirs = append(st.SubDirs, c.SubDir)
	}
	return c, include
}

// Stats 返回聚合结果。
func (a *Aggregator) Stats() *Stats {
	sort.Ints(a.monthOrder)
	months := make([]MonthStat, 0, len(a.monthOrder))
	for _, idx := range a.monthOrder {
		st := a.byMonth[idx]
		sort.Strings(st.SubDirs)
		months = append(months, *st)
	}
	return &Stats{
		Range:                a.filter.Range,
		Total:                a.total,
		Kept:                 a.kept,
		Dropped:              a.dropped,
		Months:               months,
		NonPartitioned:       a.nonPartitioned,
		NonPartitionedKept:   a.nonPartitionedKept,
		NonPartitionedSample: a.nonPartitionedSmp,
	}
}

// MonthsPresent 返回快照中实际出现的月份集合。
func (s *Stats) MonthsPresent() []Month {
	out := make([]Month, 0, len(s.Months))
	for _, m := range s.Months {
		out = append(out, m.Month)
	}
	return out
}

// MissingExpectedMonths 返回期望月份中在快照里缺失的部分。
func (s *Stats) MissingExpectedMonths(expected []Month) []Month {
	present := make(map[int]bool, len(s.Months))
	for _, m := range s.Months {
		present[m.Month.Index()] = true
	}
	var missing []Month
	for _, m := range expected {
		if !present[m.Index()] {
			missing = append(missing, m)
		}
	}
	return missing
}

// HumanizeBytes 是本地实现的字节数人类可读格式化，避免为文档/CLI 引入额外依赖。
func HumanizeBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
