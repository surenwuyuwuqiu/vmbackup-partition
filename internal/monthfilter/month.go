// Package monthfilter 实现 VictoriaMetrics 存储层“按月分区”的识别与范围过滤。
//
// 背景：VictoriaMetrics 的存储目录按自然月切分，分区目录名为 YYYY_MM。
// 该命名由 lib/storage/time.go 的 timestampToPartitionName() 产生：
//
//	func timestampToPartitionName(timestamp int64) string {
//		t := timestampToTime(timestamp)
//		return t.Format("2006_01")
//	}
//
// 一个 vmstorage 快照的目录结构为：
//
//	<storageDataPath>/snapshots/<snapshotName>/
//	├── data/                 (dataDirname，符号链接，指向 data/{small,big,indexdb}/snapshots/<name>)
//	│   ├── small/<YYYY_MM>/<partID>/{timestamps.bin,values.bin,index.bin,metaindex.bin,parts.json}
//	│   ├── big/<YYYY_MM>/<partID>/...
//	│   └── indexdb/<YYYY_MM>/<partID>/{metaindex.bin,index.bin,values.bin,parts.json}
//	└── metadata/             (metadataDirname，实体目录，存放租户等元数据)
//
// 参见 lib/storage/filenames.go、lib/storage/storage.go:MustCreateSnapshot、
// lib/storage/table.go:MustCreateSnapshot。
package monthfilter

import (
	"fmt"
	"strings"
	"time"
)

// Month 表示一个自然月，对应存储层分区目录名 YYYY_MM。
type Month struct {
	Year  int
	Month int // 1..12
}

// Parse 解析月份字面量，接受 YYYY_MM、YYYY-MM、YYYYMM 三种写法。
func Parse(s string) (Month, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return Month{}, fmt.Errorf("月份不能为空")
	}
	s = raw
	s = strings.ReplaceAll(s, "-", "_")
	if len(s) == 6 && !strings.Contains(s, "_") {
		s = s[:4] + "_" + s[4:]
	}
	if len(s) != 7 || s[4] != '_' {
		return Month{}, fmt.Errorf("月份格式非法 %q：期望 YYYY_MM、YYYY-MM 或 YYYYMM", raw)
	}
	t, err := time.Parse("2006_01", s)
	if err != nil {
		return Month{}, fmt.Errorf("月份格式非法 %q：%w", raw, err)
	}
	return Month{Year: t.Year(), Month: int(t.Month())}, nil
}

// String 返回存储层使用的分区目录名，例如 "2026_02"。
func (m Month) String() string {
	return fmt.Sprintf("%04d_%02d", m.Year, m.Month)
}

// Index 返回用于排序比较的单调递增序号（从公元 0 年 1 月开始）。
func (m Month) Index() int {
	return m.Year*12 + (m.Month - 1)
}

// Before 报告 m 是否早于 other。
func (m Month) Before(other Month) bool {
	return m.Index() < other.Index()
}

// MonthOf 返回时间 t 所在的月份（使用 t 自身的时区，与 VM 分区逻辑保持一致，
// VM 内部使用 UTC 归一化后的时间戳分区）。
func MonthOf(t time.Time) Month {
	return Month{Year: t.Year(), Month: int(t.Month())}
}

// IsMonthDirName 判断一个目录名是否是合法的分区目录名（严格 YYYY_MM）。
//
// 注意：VM 的 part 目录名是 16 位十六进制（例如 0000000000000001），
// 不可能与 YYYY_MM 混淆，因此这里可以安全地做严格 7 字符匹配。
func IsMonthDirName(name string) bool {
	if len(name) != 7 || name[4] != '_' {
		return false
	}
	for i, c := range name {
		if i == 4 {
			continue
		}
		if c < '0' || c > '9' {
			return false
		}
	}
	month := int(name[5]-'0')*10 + int(name[6]-'0')
	if month < 1 || month > 12 {
		return false
	}
	_, err := time.Parse("2006_01", name)
	return err == nil
}

// Range 表示一个闭区间 [From, To]。
type Range struct {
	From Month
	To   Month
}

// ParseRange 解析闭区间月份范围。
func ParseRange(from, to string) (Range, error) {
	f, err := Parse(from)
	if err != nil {
		return Range{}, fmt.Errorf("解析起始月份失败：%w", err)
	}
	t, err := Parse(to)
	if err != nil {
		return Range{}, fmt.Errorf("解析结束月份失败：%w", err)
	}
	if t.Before(f) {
		return Range{}, fmt.Errorf("结束月份 %s 早于起始月份 %s", t, f)
	}
	return Range{From: f, To: t}, nil
}

// Contains 报告 m 是否落在闭区间内。
func (r Range) Contains(m Month) bool {
	return r.From.Index() <= m.Index() && m.Index() <= r.To.Index()
}

// Months 展开范围内的所有月份。
func (r Range) Months() []Month {
	n := r.To.Index() - r.From.Index() + 1
	out := make([]Month, 0, n)
	for i := r.From.Index(); i <= r.To.Index(); i++ {
		out = append(out, Month{Year: i / 12, Month: i%12 + 1})
	}
	return out
}

// String 返回人类可读的区间描述。
func (r Range) String() string {
	if r.From.Index() == r.To.Index() {
		return r.From.String()
	}
	return fmt.Sprintf("%s..%s", r.From, r.To)
}
