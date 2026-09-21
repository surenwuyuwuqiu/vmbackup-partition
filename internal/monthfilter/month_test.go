package monthfilter

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in      string
		want    Month
		wantErr bool
	}{
		{in: "2026_02", want: Month{2026, 2}},
		{in: "2026-02", want: Month{2026, 2}},
		{in: "202602", want: Month{2026, 2}},
		{in: " 2026_12 ", want: Month{2026, 12}},
		{in: "2026_00", wantErr: true},
		{in: "2026_13", wantErr: true},
		{in: "2026_1", wantErr: true},
		{in: "", wantErr: true},
		{in: "abc", wantErr: true},
		{in: "2026_1a", wantErr: true},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("Parse(%q): 期望报错，实际得到 %v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("Parse(%q): 意外错误 %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("Parse(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestMonthStringIndex(t *testing.T) {
	m := Month{2026, 2}
	if got := m.String(); got != "2026_02" {
		t.Fatalf("String() = %q, want %q", got, "2026_02")
	}
	// 跨年比较
	dec2026 := Month{2026, 12}
	jan2027 := Month{2027, 1}
	if !dec2026.Before(jan2027) {
		t.Fatalf("2026_12 应早于 2027_01")
	}
	feb2026 := Month{2026, 2}
	jan2026 := Month{2026, 1}
	if feb2026.Before(jan2026) {
		t.Fatalf("2026_02 不应早于 2026_01")
	}
	mar2026 := Month{2026, 3}
	if feb2026.Index() >= mar2026.Index() {
		t.Fatalf("Index 必须单调递增")
	}
}

func TestParseRange(t *testing.T) {
	r, err := ParseRange("2026_02", "2026_06")
	if err != nil {
		t.Fatalf("ParseRange: %v", err)
	}
	if r.String() != "2026_02..2026_06" {
		t.Fatalf("Range.String() = %q", r.String())
	}
	months := r.Months()
	if len(months) != 5 {
		t.Fatalf("Months() 长度 = %d, want 5", len(months))
	}
	if months[0].String() != "2026_02" || months[4].String() != "2026_06" {
		t.Fatalf("Months() 边界错误: %v .. %v", months[0], months[4])
	}
	if _, err := ParseRange("2026_06", "2026_02"); err == nil {
		t.Fatalf("起始晚于结束时应当报错")
	}
}

func TestRangeContainsAcrossYear(t *testing.T) {
	r, err := ParseRange("2025_11", "2026_02")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"2025_11", "2025_12", "2026_01", "2026_02"} {
		m, _ := Parse(s)
		if !r.Contains(m) {
			t.Errorf("%s 应落在 %s 内", s, r)
		}
	}
	for _, s := range []string{"2025_10", "2026_03"} {
		m, _ := Parse(s)
		if r.Contains(m) {
			t.Errorf("%s 不应落在 %s 内", s, r)
		}
	}
}

func TestIsMonthDirName(t *testing.T) {
	valid := []string{"2026_02", "1999_12", "2026_01"}
	for _, s := range valid {
		if !IsMonthDirName(s) {
			t.Errorf("IsMonthDirName(%q) 应为 true", s)
		}
	}
	invalid := []string{
		"2026_13", "2026_00", "2026_2", "0000000000000001", "small", "big", "indexdb",
		"metadata", "202602", "2026-02", "x026_02", "2026_0a", "", "2026_02_01",
		"000000000000000a", "ffffffffffffffff",
	}
	for _, s := range invalid {
		if IsMonthDirName(s) {
			t.Errorf("IsMonthDirName(%q) 应为 false", s)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		path        string
		partitioned bool
		month       string
		subDir      string
	}{
		// 真实布局：data/<small|big|indexdb>/<YYYY_MM>/<partID>/<file>
		{"data/small/2026_02/0000000000000001/timestamps.bin", true, "2026_02", "small"},
		{"data/small/2026_02/0000000000000001/values.bin", true, "2026_02", "small"},
		{"data/big/2026_06/0000000000000abc/index.bin", true, "2026_06", "big"},
		{"data/indexdb/2026_03/0000000000000001/metaindex.bin", true, "2026_03", "indexdb"},
		{"data/indexdb/2026_03/0000000000000001/parts.json", true, "2026_03", "indexdb"},
		// 分区目录本体（理论上不会作为文件出现，但分类必须稳健）
		{"data/small/2026_02", true, "2026_02", "small"},
		// 旧版 big 目录多一层体积分层
		{"data/big/medium/2026_04/0000000000000001/timestamps.bin", true, "2026_04", "big"},
		// 非分区路径
		{"metadata/tenant/1/metadata.json", false, "", ""},
		{"metadata/metadata.json", false, "", ""},
		{"data/small", false, "", ""},
		{"data/indexdb", false, "", ""},
		{"data", false, "", ""},
		{"snapshots/x", false, "", ""},
		{"", false, "", ""},
		{"data/small/2026_2/0000000000000001/x", false, "", ""},
		{"data/small/2026_13/0000000000000001/x", false, "", ""},
		// 中间多插一层目录的变体：仍然识别为分区路径（深度放宽的稳健性保证）。
		// 真实布局中不可能出现该形状（part 目录名是 16 位十六进制，其下只有文件），
		// 因此这里的判定只会影响人工构造的异常样本。
		{"data/small/0000000000000001/2026_02/x", true, "2026_02", "small"},
	}
	for _, c := range cases {
		got := Classify(c.path)
		if got.Partitioned != c.partitioned {
			t.Errorf("Classify(%q).Partitioned = %v, want %v", c.path, got.Partitioned, c.partitioned)
			continue
		}
		if !c.partitioned {
			continue
		}
		if got.Month.String() != c.month {
			t.Errorf("Classify(%q).Month = %s, want %s", c.path, got.Month, c.month)
		}
		if got.SubDir != c.subDir {
			t.Errorf("Classify(%q).SubDir = %q, want %q", c.path, got.SubDir, c.subDir)
		}
	}
}

func newTestFilter(t *testing.T, from, to string, includeNonPartitioned bool) Filter {
	t.Helper()
	r, err := ParseRange(from, to)
	if err != nil {
		t.Fatal(err)
	}
	return Filter{Range: r, IncludeNonPartitioned: includeNonPartitioned}
}

func TestFilterDecide(t *testing.T) {
	f := newTestFilter(t, "2026_02", "2026_06", true)

	keep := []string{
		"data/small/2026_02/0000000000000001/timestamps.bin",
		"data/small/2026_06/0000000000000001/timestamps.bin",
		"data/big/2026_02/0000000000000001/index.bin",
		"data/indexdb/2026_03/0000000000000001/index.bin",
		"metadata/metadata.json",
		"metadata/tenant/0/metadata.json",
	}
	for _, p := range keep {
		if c, ok := f.Decide(p); !ok {
			t.Errorf("Decide(%q) = false（分类 %+v），期望保留", p, c)
		}
	}

	drop := []string{
		"data/small/2026_01/0000000000000001/timestamps.bin",
		"data/small/2026_07/0000000000000001/timestamps.bin",
		"data/small/2026_09/0000000000000001/timestamps.bin",
		"data/big/2026_08/0000000000000001/index.bin",
		"data/indexdb/2026_01/0000000000000001/index.bin",
		"data/indexdb/2026_09/0000000000000001/index.bin",
	}
	for _, p := range drop {
		if _, ok := f.Decide(p); ok {
			t.Errorf("Decide(%q) = true，期望丢弃", p)
		}
	}
}

func TestFilterNonPartitionedPolicy(t *testing.T) {
	f := newTestFilter(t, "2026_02", "2026_06", false)
	if _, ok := f.Decide("metadata/metadata.json"); ok {
		t.Fatalf("IncludeNonPartitioned=false 时 metadata 应被丢弃")
	}
	f.IncludeNonPartitioned = true
	if _, ok := f.Decide("metadata/metadata.json"); !ok {
		t.Fatalf("IncludeNonPartitioned=true 时 metadata 应被保留")
	}
}

func TestAggregator(t *testing.T) {
	f := newTestFilter(t, "2026_02", "2026_06", true)
	agg := NewAggregator(f)

	add := func(path string, size uint64) bool {
		_, ok := agg.Add(path, size)
		return ok
	}

	if !add("data/small/2026_02/p1/timestamps.bin", 100) {
		t.Fatal("2026_02 应保留")
	}
	if !add("data/small/2026_02/p2/timestamps.bin", 200) {
		t.Fatal("2026_02 应保留")
	}
	if add("data/small/2026_01/p1/timestamps.bin", 1000) {
		t.Fatal("2026_01 应丢弃")
	}
	if !add("data/indexdb/2026_06/p1/index.bin", 50) {
		t.Fatal("2026_06 应保留")
	}
	if add("data/big/2026_09/p1/index.bin", 5000) {
		t.Fatal("2026_09 应丢弃")
	}
	if !add("metadata/metadata.json", 10) {
		t.Fatal("metadata 应保留")
	}

	s := agg.Stats()
	if s.Total.Parts != 6 {
		t.Fatalf("Total.Parts = %d, want 6", s.Total.Parts)
	}
	if s.Total.Bytes != 6360 {
		t.Fatalf("Total.Bytes = %d, want 6360", s.Total.Bytes)
	}
	if s.Kept.Parts != 4 {
		t.Fatalf("Kept.Parts = %d, want 4", s.Kept.Parts)
	}
	if s.Kept.Bytes != 360 {
		t.Fatalf("Kept.Bytes = %d, want 360", s.Kept.Bytes)
	}
	if s.Dropped.Parts != 2 {
		t.Fatalf("Dropped.Parts = %d, want 2", s.Dropped.Parts)
	}
	if s.NonPartitioned.Parts != 1 || s.NonPartitionedKept.Parts != 1 {
		t.Fatalf("非分区统计错误: %+v", s.NonPartitioned)
	}
	if len(s.Months) != 4 {
		t.Fatalf("Months 长度 = %d, want 4 (2026_01,02,06,09)", len(s.Months))
	}
	// 月份必须按升序
	for i := 1; i < len(s.Months); i++ {
		if s.Months[i-1].Month.Index() >= s.Months[i].Month.Index() {
			t.Fatalf("Months 未按升序排列: %v", s.Months)
		}
	}

	expected := f.Range.Months() // 2026_02..2026_06 共 5 个月
	if missing := s.MissingExpectedMonths(expected); len(missing) != 3 {
		t.Fatalf("期望缺失 2026_03/04/05 共 3 个月，实际缺失 %v", missing)
	} else {
		for i, want := range []string{"2026_03", "2026_04", "2026_05"} {
			if missing[i].String() != want {
				t.Fatalf("缺失月份[%d] = %s, want %s", i, missing[i], want)
			}
		}
	}
	// 2026_01 / 2026_02 / 2026_06 / 2026_09 在快照中存在
	present := s.MonthsPresent()
	if len(present) != 4 {
		t.Fatalf("MonthsPresent 长度 = %d", len(present))
	}
	if present[0].String() != "2026_01" || present[3].String() != "2026_09" {
		t.Fatalf("MonthsPresent 内容错误: %v", present)
	}
}

func TestHumanizeBytes(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{0, "0B"},
		{512, "512B"},
		{1024, "1.00KiB"},
		{1536, "1.50KiB"},
		{1048576, "1.00MiB"},
	}
	for _, c := range cases {
		if got := HumanizeBytes(c.in); got != c.want {
			t.Errorf("HumanizeBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRenderPlanTable(t *testing.T) {
	f := newTestFilter(t, "2026_02", "2026_06", true)
	agg := NewAggregator(f)
	agg.Add("data/small/2026_02/p1/timestamps.bin", 100)
	agg.Add("data/small/2026_09/p1/timestamps.bin", 100)
	agg.Add("metadata/metadata.json", 10)
	out := agg.Stats().RenderPlanTable()
	for _, want := range []string{"MONTH", "2026_02", "2026_09", "keep", "drop", "(non-partitioned)", "snapshot total"} {
		if !strings.Contains(out, want) {
			t.Errorf("计划表缺少 %q，实际输出：\n%s", want, out)
		}
	}
}

func TestEncodePlanJSON(t *testing.T) {
	f := newTestFilter(t, "2026_02", "2026_06", true)
	agg := NewAggregator(f)
	agg.Add("data/small/2026_02/p1/timestamps.bin", 100)
	agg.Add("data/small/2026_09/p1/timestamps.bin", 100)

	raw, err := agg.Stats().EncodePlanJSON(true)
	if err != nil {
		t.Fatal(err)
	}
	var p PlanJSON
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("计划 JSON 无法反序列化: %v\n%s", err, raw)
	}
	if p.FromMonth != "2026_02" || p.ToMonth != "2026_06" {
		t.Fatalf("月份区间错误: %s..%s", p.FromMonth, p.ToMonth)
	}
	if len(p.Months) != 2 {
		t.Fatalf("Months 长度 = %d, want 2", len(p.Months))
	}
	if p.Months[0].Decision != "keep" || p.Months[1].Decision != "drop" {
		t.Fatalf("决策错误: %+v", p.Months)
	}
	if p.TotalKept.Parts != 1 || p.TotalDropped.Parts != 1 {
		t.Fatalf("汇总错误: kept=%+v dropped=%+v", p.TotalKept, p.TotalDropped)
	}
}
