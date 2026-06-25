package run

import (
	_ "embed"
	"fmt"
	"html/template"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/B42Labs/openstack-tester/internal/metrics"
)

// htmlTemplateSrc is the single-file HTML report template, embedded so the
// binary carries it and the renderer needs no working-directory access.
//
//go:embed report.tmpl.html
var htmlTemplateSrc string

// htmlTemplate is the parsed report template. It is data-only: the view model
// arrives fully precomputed (durations humanized, chart geometry pre-scaled to
// pixels, counts pre-aggregated and sorted), so the template stays declarative
// and the output is deterministic.
var htmlTemplate = template.Must(template.New("report").Parse(htmlTemplateSrc))

// Chart geometry, in user-space SVG pixels. Horizontal bar charts (latency,
// volume, errors) share one row layout; the chaos time series uses its own
// vertical layout. All charts share the template's 720px viewBox width.
const (
	// Horizontal row charts.
	hLabel = 150.0 // left label gutter
	hPlot  = 500.0 // plot-area width
	hBarH  = 14.0  // bar thickness
	hRow   = 24.0  // row pitch
	hTop   = 8.0   // top padding

	// Vertical chaos time-series charts.
	tsLeft  = 46.0  // left axis gutter
	tsPlotW = 658.0 // 720 viewBox - 46 left - 16 right margin
	tsTop   = 10.0
	tsPlotH = 150.0
)

// WriteHTML renders the run record as a single self-contained HTML file: inline
// CSS, hand-rolled inline SVG charts, and a little static JavaScript for table
// sorting and collapsibles. It references no external resources, so the report
// opens offline, and its output is byte-identical for a given record. All
// cloud-derived strings flow through html/template auto-escaping.
func WriteHTML(w io.Writer, r *Record) error {
	if err := htmlTemplate.Execute(w, buildHTMLView(r)); err != nil {
		return fmt.Errorf("writing html report: %w", err)
	}
	return nil
}

// htmlView is the fully precomputed model the template renders.
type htmlView struct {
	Header    headerView
	KPIs      []kpi
	Columns   []string
	PerType   []typeRow
	Latency   latencyChart
	Volume    volumeChart
	Errors    *barChart // nil when the run had no errors
	Readiness []readyRow
	Inventory inventoryView
	Chaos     *chaosView // nil for an apply run
}

type headerView struct {
	RunID      string
	Scenario   string
	Seed       string
	StartedAt  string
	FinishedAt string
	Wall       string
	Error      string
}

type kpi struct {
	Label string
	Value string
}

// cell is one per-type table cell: Text is shown, Sort is the value the column
// sorter compares (numeric string for metric columns, the label for the type).
type cell struct {
	Text string
	Sort string
}

type typeRow struct {
	Cells []cell
}

// latencyBar is one resource type's latency range, pre-scaled to pixel x-coords
// against the chart's shared maximum: a thin line spans min→max and a band spans
// the p50→p99 tail.
type latencyBar struct {
	Label   string
	Caption string
	Y       string
	TextY   string
	LineX1  string
	LineX2  string
	BandX   string
	BandW   string
}

type latencyChart struct {
	Height  string
	AxisMax string
	Bars    []latencyBar
}

// volumeBar is one resource type's attempted volume as a stacked bar: a
// succeeded segment followed by a failed segment, together spanning attempted.
type volumeBar struct {
	Label   string
	Caption string
	Y       string
	TextY   string
	OKX     string
	OKW     string
	FailX   string
	FailW   string
}

type volumeChart struct {
	Height string
	Bars   []volumeBar
}

// barRow is one row of a simple single-series horizontal bar chart.
type barRow struct {
	Label   string
	Caption string
	Y       string
	TextY   string
	W       string
}

type barChart struct {
	Height string
	Rows   []barRow
}

type readyRow struct {
	Type   string
	Ready  string
	Median string
	Max    string
}

type inventoryView struct {
	Total     int
	Counts    []kindCount
	Items     []invItem
	Truncated int // created resources beyond maxInvRows, omitted from Items
}

type kindCount struct {
	Kind  string
	Count int
}

type invItem struct {
	Kind    string
	Logical string
	Name    string
	ID      string
}

type chaosView struct {
	Creates    int
	Deletes    int
	Cycles     int
	PopMin     int
	PopMax     int
	PopMean    string
	TargetFill string
	HasBuckets bool
	Volume     tsChart
	Latency    tsChart
}

// tsChart is a chaos time-series sub-chart: vertical bars and/or polylines over
// the buckets, all pre-scaled to pixels, plus the axis captions.
type tsChart struct {
	Height    string
	AxisMax   string
	FirstTime string
	LastTime  string
	Baseline  string
	Bars      []tsBar
	P50Points string
	P99Points string
}

type tsBar struct {
	X       string
	W       string
	Y       string
	H       string
	FailY   string
	FailH   string
	HasFail bool
}

// buildHTMLView converts a record into the render-ready view model.
func buildHTMLView(r *Record) htmlView {
	m := r.Metrics
	v := htmlView{
		Header:    buildHeader(r),
		KPIs:      buildKPIs(m.Overall),
		Columns:   []string{"type", "ops", "ok", "failed", "success", "ops/s", "min", "mean", "p50", "p90", "p95", "p99", "max"},
		PerType:   buildPerType(m.ByType),
		Latency:   buildLatencyChart(m.ByType),
		Volume:    buildVolumeChart(m.ByType),
		Errors:    buildErrorChart(m.Errors),
		Readiness: buildReadiness(m.Readiness),
		Inventory: buildInventory(r),
	}
	if r.Chaos != nil {
		v.Chaos = buildChaosView(r.Chaos)
	}
	return v
}

func buildHeader(r *Record) headerView {
	return headerView{
		RunID:      r.RunID,
		Scenario:   r.Scenario,
		Seed:       strconv.FormatInt(r.Seed, 10),
		StartedAt:  r.StartedAt.UTC().Format(time.RFC3339),
		FinishedAt: r.FinishedAt.UTC().Format(time.RFC3339),
		Wall:       humanizeDuration(r.Metrics.Wall),
		Error:      r.Error,
	}
}

func buildKPIs(o metrics.Stats) []kpi {
	return []kpi{
		{Label: "total ops", Value: strconv.Itoa(o.Attempted)},
		{Label: "success rate", Value: pct(o.Succeeded, o.Attempted)},
		{Label: "failed", Value: strconv.Itoa(o.Failed)},
		{Label: "throughput", Value: f2(o.Throughput) + " ops/s"},
		{Label: "p50 latency", Value: humanizeDuration(o.Latency.Median)},
		{Label: "p99 latency", Value: humanizeDuration(o.Latency.P99)},
	}
}

func buildPerType(byType []metrics.Stats) []typeRow {
	rows := make([]typeRow, 0, len(byType))
	for _, s := range byType {
		rows = append(rows, typeRow{Cells: []cell{
			{Text: s.Type, Sort: s.Type},
			{Text: strconv.Itoa(s.Attempted), Sort: strconv.Itoa(s.Attempted)},
			{Text: strconv.Itoa(s.Succeeded), Sort: strconv.Itoa(s.Succeeded)},
			{Text: strconv.Itoa(s.Failed), Sort: strconv.Itoa(s.Failed)},
			{Text: pct(s.Succeeded, s.Attempted), Sort: sortPct(s.Succeeded, s.Attempted)},
			{Text: f2(s.Throughput), Sort: f2(s.Throughput)},
			latencyCell(s.Latency.Min),
			latencyCell(s.Latency.Mean),
			latencyCell(s.Latency.Median),
			latencyCell(s.Latency.P90),
			latencyCell(s.Latency.P95),
			latencyCell(s.Latency.P99),
			latencyCell(s.Latency.Max),
		}})
	}
	return rows
}

// latencyCell renders a duration cell sorted by its nanosecond value so the
// humanized text (ms/s) still sorts numerically.
func latencyCell(d time.Duration) cell {
	return cell{Text: humanizeDuration(d), Sort: strconv.FormatInt(int64(d), 10)}
}

func buildLatencyChart(byType []metrics.Stats) latencyChart {
	var max time.Duration
	for _, s := range byType {
		if s.Latency.Max > max {
			max = s.Latency.Max
		}
	}
	scale := scaler(float64(max), hPlot)

	c := latencyChart{
		Height:  f1(hTop + float64(len(byType))*hRow + 4),
		AxisMax: humanizeDuration(max),
		Bars:    make([]latencyBar, 0, len(byType)),
	}
	for i, s := range byType {
		base := hTop + float64(i)*hRow
		bandX := hLabel + scale(float64(s.Latency.Median))
		bandW := scale(float64(s.Latency.P99)) - scale(float64(s.Latency.Median))
		c.Bars = append(c.Bars, latencyBar{
			Label:   s.Type,
			Caption: "p99 " + humanizeDuration(s.Latency.P99),
			Y:       f1(base + hBarH/2),
			TextY:   f1(base + hBarH),
			LineX1:  f1(hLabel + scale(float64(s.Latency.Min))),
			LineX2:  f1(hLabel + scale(float64(s.Latency.Max))),
			BandX:   f1(bandX),
			BandW:   f1(bandW),
		})
	}
	return c
}

func buildVolumeChart(byType []metrics.Stats) volumeChart {
	max := 0
	for _, s := range byType {
		if s.Attempted > max {
			max = s.Attempted
		}
	}
	scale := scaler(float64(max), hPlot)

	c := volumeChart{
		Height: f1(hTop + float64(len(byType))*hRow + 4),
		Bars:   make([]volumeBar, 0, len(byType)),
	}
	for i, s := range byType {
		base := hTop + float64(i)*hRow
		okW := scale(float64(s.Succeeded))
		c.Bars = append(c.Bars, volumeBar{
			Label:   s.Type,
			Caption: fmt.Sprintf("ok %d / fail %d", s.Succeeded, s.Failed),
			Y:       f1(base),
			TextY:   f1(base + hBarH),
			OKX:     f1(hLabel),
			OKW:     f1(okW),
			FailX:   f1(hLabel + okW),
			FailW:   f1(scale(float64(s.Failed))),
		})
	}
	return c
}

func buildErrorChart(errs []metrics.ErrorCount) *barChart {
	if len(errs) == 0 {
		return nil
	}
	max := 0
	for _, e := range errs {
		if e.Count > max {
			max = e.Count
		}
	}
	scale := scaler(float64(max), hPlot)

	c := &barChart{
		Height: f1(hTop + float64(len(errs))*hRow + 4),
		Rows:   make([]barRow, 0, len(errs)),
	}
	for i, e := range errs {
		base := hTop + float64(i)*hRow
		c.Rows = append(c.Rows, barRow{
			Label:   e.Kind,
			Caption: strconv.Itoa(e.Count),
			Y:       f1(base),
			TextY:   f1(base + hBarH),
			W:       f1(scale(float64(e.Count))),
		})
	}
	return c
}

func buildReadiness(stats []metrics.ReadinessStats) []readyRow {
	rows := make([]readyRow, 0, len(stats))
	for _, s := range stats {
		rows = append(rows, readyRow{
			Type:   s.Type,
			Ready:  fmt.Sprintf("%d/%d", s.OK, s.Count),
			Median: humanizeDuration(s.Latency.Median),
			Max:    humanizeDuration(s.Latency.Max),
		})
	}
	return rows
}

// maxInvRows caps the number of per-resource rows materialized into the
// inventory table. A high-churn soak run creates tens of thousands of
// resources; inlining every one into the single self-contained DOM grows the
// file without bound and can stall the browser tab (and a crafted record with a
// huge Created slice would amplify memory here). The per-kind chips still
// reflect the full Total; only the detail table is bounded.
const maxInvRows = 2000

func buildInventory(r *Record) inventoryView {
	counts := make(map[string]int)
	items := make([]invItem, 0, min(len(r.Created), maxInvRows))
	for _, res := range r.Created {
		counts[string(res.Kind)]++
		if len(items) < maxInvRows {
			items = append(items, invItem{
				Kind:    string(res.Kind),
				Logical: res.Logical,
				Name:    res.Name,
				ID:      res.ID,
			})
		}
	}

	kinds := make([]kindCount, 0, len(counts))
	for k, n := range counts {
		kinds = append(kinds, kindCount{Kind: k, Count: n})
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i].Kind < kinds[j].Kind })

	return inventoryView{Total: len(r.Created), Counts: kinds, Items: items, Truncated: len(r.Created) - len(items)}
}

func buildChaosView(c *ChaosStats) *chaosView {
	v := &chaosView{
		Creates:    c.Creates,
		Deletes:    c.Deletes,
		Cycles:     c.Cycles,
		PopMin:     c.PopMin,
		PopMax:     c.PopMax,
		PopMean:    f2(c.PopMean),
		TargetFill: f2(c.TargetFill),
		HasBuckets: len(c.Buckets) > 0,
	}
	if v.HasBuckets {
		v.Volume = buildBucketVolume(c.Buckets)
		v.Latency = buildBucketLatency(c.Buckets)
	}
	return v
}

func buildBucketVolume(buckets []ChaosBucket) tsChart {
	maxAtt := 0
	var maxStart time.Duration
	for _, b := range buckets {
		if b.Stats.Attempted > maxAtt {
			maxAtt = b.Stats.Attempted
		}
		if b.Start > maxStart {
			maxStart = b.Start
		}
	}
	scale := scaler(float64(maxAtt), tsPlotH)
	slot := tsPlotW / float64(len(buckets))
	barW := slot * 0.7

	c := tsChart{
		Height:    f1(tsTop + tsPlotH + 24),
		AxisMax:   strconv.Itoa(maxAtt),
		FirstTime: humanizeDuration(buckets[0].Start),
		LastTime:  humanizeDuration(maxStart),
		Baseline:  f1(tsTop + tsPlotH),
		Bars:      make([]tsBar, 0, len(buckets)),
	}
	for i, b := range buckets {
		x := tsLeft + float64(i)*slot + (slot-barW)/2
		h := scale(float64(b.Stats.Attempted))
		failH := scale(float64(b.Stats.Failed))
		c.Bars = append(c.Bars, tsBar{
			X:       f1(x),
			W:       f1(barW),
			Y:       f1(tsTop + tsPlotH - h),
			H:       f1(h),
			FailY:   f1(tsTop + tsPlotH - failH),
			FailH:   f1(failH),
			HasFail: b.Stats.Failed > 0,
		})
	}
	return c
}

func buildBucketLatency(buckets []ChaosBucket) tsChart {
	var maxLat, maxStart time.Duration
	for _, b := range buckets {
		if b.Stats.Latency.P99 > maxLat {
			maxLat = b.Stats.Latency.P99
		}
		if b.Start > maxStart {
			maxStart = b.Start
		}
	}
	scale := scaler(float64(maxLat), tsPlotH)
	slot := tsPlotW / float64(len(buckets))

	c := tsChart{
		Height:    f1(tsTop + tsPlotH + 24),
		AxisMax:   humanizeDuration(maxLat),
		FirstTime: humanizeDuration(buckets[0].Start),
		LastTime:  humanizeDuration(maxStart),
		Baseline:  f1(tsTop + tsPlotH),
	}
	p50 := make([]string, 0, len(buckets))
	p99 := make([]string, 0, len(buckets))
	for i, b := range buckets {
		x := tsLeft + float64(i)*slot + slot/2
		y50 := tsTop + tsPlotH - scale(float64(b.Stats.Latency.Median))
		y99 := tsTop + tsPlotH - scale(float64(b.Stats.Latency.P99))
		p50 = append(p50, f1(x)+","+f1(y50))
		p99 = append(p99, f1(x)+","+f1(y99))
	}
	c.P50Points = strings.Join(p50, " ")
	c.P99Points = strings.Join(p99, " ")
	return c
}

// scaler returns a function mapping a value in [0, max] to a pixel offset in
// [0, span]. The divisor is clamped to ≥1 so an all-zero series renders flat
// rather than producing NaN/Inf coordinates.
func scaler(max, span float64) func(float64) float64 {
	if max < 1 {
		max = 1
	}
	return func(v float64) float64 { return v / max * span }
}

// humanizeDuration renders a nanosecond duration as milliseconds or seconds, so
// the int64-nanosecond durations the record carries read naturally in the
// report. The zero duration renders as "0ms".
func humanizeDuration(d time.Duration) string {
	if d == 0 {
		return "0ms"
	}
	if d < time.Second {
		return f2(float64(d)/float64(time.Millisecond)) + "ms"
	}
	return f2(float64(d)/float64(time.Second)) + "s"
}

// pct renders num/den as a percentage, or "n/a" when den is zero.
func pct(num, den int) string {
	if den == 0 {
		return "n/a"
	}
	return f2(float64(num)/float64(den)*100) + "%"
}

// sortPct returns a numeric sort key for a success percentage, -1 when there
// were no attempts so empty rows sort below real ones.
func sortPct(num, den int) string {
	if den == 0 {
		return "-1"
	}
	return f2(float64(num) / float64(den) * 100)
}

// f1 and f2 format floats with fixed precision so chart geometry and metrics
// are byte-identical across renders.
func f1(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }
func f2(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }
