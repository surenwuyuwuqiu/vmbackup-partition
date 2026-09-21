package monthfilter

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// PlanJSON 是 -dryRun -planOut 输出的机器可读备份计划。
type PlanJSON struct {
	Range                 string      `json:"month_range"`
	FromMonth             string      `json:"from_month"`
	ToMonth               string      `json:"to_month"`
	IncludeNonPartitioned bool        `json:"include_non_partitioned"`
	Months                []MonthPlan `json:"months"`
	NonPartitioned        TotalJSON   `json:"non_partitioned"`
	TotalSnapshot         TotalJSON   `json:"total_snapshot"`
	TotalKept             TotalJSON   `json:"total_kept"`
	TotalDropped          TotalJSON   `json:"total_dropped"`
}

// MonthPlan 是计划中单个自然月的条目。
type MonthPlan struct {
	Month    string    `json:"month"`
	Dirs     []string  `json:"dirs,omitempty"`
	Total    TotalJSON `json:"total"`
	Decision string    `json:"decision"` // keep | drop
}

// TotalJSON 是 part 数量与字节数的 JSON 表示。
type TotalJSON struct {
	Parts int    `json:"parts"`
	Bytes uint64 `json:"bytes"`
	Human string `json:"human_bytes"`
}

func toTotalJSON(t Total) TotalJSON {
	return TotalJSON{Parts: t.Parts, Bytes: t.Bytes, Human: HumanizeBytes(t.Bytes)}
}

// EncodePlanJSON 把统计结果编码为备份计划 JSON。
func (s *Stats) EncodePlanJSON(includeNonPartitioned bool) ([]byte, error) {
	p := PlanJSON{
		Range:                 s.Range.String(),
		FromMonth:             s.Range.From.String(),
		ToMonth:               s.Range.To.String(),
		IncludeNonPartitioned: includeNonPartitioned,
		NonPartitioned:        toTotalJSON(s.NonPartitioned),
		TotalSnapshot:         toTotalJSON(s.Total),
		TotalKept:             toTotalJSON(s.Kept),
		TotalDropped:          toTotalJSON(s.Dropped),
	}
	for _, m := range s.Months {
		p.Months = append(p.Months, MonthPlan{
			Month:    m.Month.String(),
			Dirs:     m.SubDirs,
			Total:    toTotalJSON(m.Total),
			Decision: decisionWord(m.Include),
		})
	}
	return json.MarshalIndent(p, "", "  ")
}

func decisionWord(include bool) string {
	if include {
		return "keep"
	}
	return "drop"
}

// RenderPlanTable 渲染人类可读的备份计划表。
func (s *Stats) RenderPlanTable() string {
	const (
		wMonth = 18 // 首列宽度，需容纳 "(non-partitioned)"
		wDirs  = 24
		wParts = 12
		wSize  = 12
	)

	var b strings.Builder
	months := append([]MonthStat(nil), s.Months...)
	sort.Slice(months, func(i, j int) bool { return months[i].Month.Index() < months[j].Month.Index() })

	b.WriteString(fmt.Sprintf("%-*s  %-*s  %*s  %*s  %s\n", wMonth, "MONTH", wDirs, "DIRS", wParts, "PARTS", wSize, "SIZE", "DECISION"))
	b.WriteString(strings.Repeat("-", wMonth+wDirs+wParts+wSize+12) + "\n")
	for _, m := range months {
		b.WriteString(fmt.Sprintf("%-*s  %-*s  %*d  %*s  %s\n",
			wMonth, m.Month.String(),
			wDirs, strings.Join(m.SubDirs, ","),
			wParts, m.Total.Parts,
			wSize, HumanizeBytes(m.Total.Bytes),
			decisionWord(m.Include)))
	}
	if s.NonPartitioned.Parts > 0 {
		b.WriteString(fmt.Sprintf("%-*s  %-*s  %*d  %*s  %s\n",
			wMonth, "(non-partitioned)",
			wDirs, "-",
			wParts, s.NonPartitioned.Parts,
			wSize, HumanizeBytes(s.NonPartitioned.Bytes),
			decisionWord(s.NonPartitionedKept.Parts == s.NonPartitioned.Parts)))
	}
	b.WriteString(strings.Repeat("-", wMonth+wDirs+wParts+wSize+12) + "\n")

	summaryRow := func(label string, t Total, decision string) {
		b.WriteString(fmt.Sprintf("%-*s  %-*s  %*d  %*s  %s\n",
			wMonth, label, wDirs, "-", wParts, t.Parts, wSize, HumanizeBytes(t.Bytes), decision))
	}
	summaryRow("snapshot total", s.Total, "")
	summaryRow("will backup", s.Kept, "keep")
	summaryRow("will skip", s.Dropped, "drop")
	return b.String()
}
