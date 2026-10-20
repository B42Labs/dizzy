package run

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/B42Labs/dizzy/internal/metrics"
	"github.com/B42Labs/dizzy/internal/neutron"
)

var update = flag.Bool("update", false, "update golden files")

// applyHTMLRecord is an apply record (Chaos nil) with a non-empty Error (so the
// failed banner renders) and time-to-ready stats (so that section renders),
// exercising every apply-side section of the HTML report.
func applyHTMLRecord() *Record {
	r := sampleRecord()
	r.Metrics.Readiness = []metrics.ReadinessStats{
		{Type: "network", Count: 1, OK: 1, Latency: metrics.Latency{Median: 2 * time.Second, Max: 3 * time.Second}},
	}
	return r
}

// checkGolden compares got against the golden file at testdata/golden/name,
// rewriting it under -update.
func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("writing golden file: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden file (run with -update to create it): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("HTML report differs from golden file %s; run with -update if the change is intended", path)
	}
}

// TestWriteHTMLGoldenApply locks the rendered HTML for an apply record,
// including the failed banner, KPI tiles, per-type table, latency/volume/error
// charts, time-to-ready, and the created inventory.
func TestWriteHTMLGoldenApply(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteHTML(&buf, applyHTMLRecord()); err != nil {
		t.Fatalf("WriteHTML: %v", err)
	}
	checkGolden(t, "report-apply.html", buf.Bytes())
}

// TestWriteHTMLGoldenChaos locks the rendered HTML for a chaos record: the ok
// banner (Error cleared), the churn summary, and the per-bucket time-series
// charts.
func TestWriteHTMLGoldenChaos(t *testing.T) {
	rec := chaosRecord()
	rec.Error = ""
	var buf bytes.Buffer
	if err := WriteHTML(&buf, rec); err != nil {
		t.Fatalf("WriteHTML: %v", err)
	}
	checkGolden(t, "report-chaos.html", buf.Bytes())
}

// TestWriteHTMLDeterministic confirms rendering the same record twice produces
// byte-identical output, the property the golden-file tests rely on.
func TestWriteHTMLDeterministic(t *testing.T) {
	var a, b bytes.Buffer
	if err := WriteHTML(&a, chaosRecord()); err != nil {
		t.Fatalf("WriteHTML(a): %v", err)
	}
	if err := WriteHTML(&b, chaosRecord()); err != nil {
		t.Fatalf("WriteHTML(b): %v", err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Error("two renders of the same record differ; output is not deterministic")
	}
}

// TestWriteHTMLOffline confirms the report references no external resources, so
// it opens offline and is safe to archive.
func TestWriteHTMLOffline(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteHTML(&buf, chaosRecord()); err != nil {
		t.Fatalf("WriteHTML: %v", err)
	}
	out := buf.String()
	for _, bad := range []string{"http://", "https://", "<link ", "src=", "url(http", "@import"} {
		if strings.Contains(out, bad) {
			t.Errorf("report contains external reference %q, expected a self-contained file", bad)
		}
	}
}

// TestWriteHTMLFailedRunFlagged confirms a failed run (Error set) is flagged
// with the failure banner and the error text, and a clean run is not.
func TestWriteHTMLFailedRunFlagged(t *testing.T) {
	var failed bytes.Buffer
	if err := WriteHTML(&failed, sampleRecord()); err != nil {
		t.Fatalf("WriteHTML(failed): %v", err)
	}
	out := failed.String()
	if !strings.Contains(out, "banner fail") || !strings.Contains(out, "Run failed") {
		t.Errorf("failed run not flagged with the failure banner:\n%s", out)
	}

	ok := sampleRecord()
	ok.Error = ""
	var clean bytes.Buffer
	if err := WriteHTML(&clean, ok); err != nil {
		t.Fatalf("WriteHTML(ok): %v", err)
	}
	if got := clean.String(); strings.Contains(got, "banner fail") {
		t.Errorf("clean run unexpectedly flagged as failed:\n%s", got)
	}
}

// TestWriteHTMLEscapesCloudStrings confirms a cloud-derived string carrying a
// script payload is HTML-escaped, the XSS analog of the CSV-injection guard.
func TestWriteHTMLEscapesCloudStrings(t *testing.T) {
	rec := sampleRecord()
	rec.Created = []neutron.Resource{
		{Kind: neutron.KindNetwork, Logical: "net-0001", Name: "<script>alert(1)</script>", ID: "net-id-1"},
	}

	var buf bytes.Buffer
	if err := WriteHTML(&buf, rec); err != nil {
		t.Fatalf("WriteHTML: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "<script>alert(1)</script>") {
		t.Error("cloud-derived name rendered without escaping (XSS)")
	}
	if !strings.Contains(out, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("cloud-derived name not HTML-escaped:\n%s", out)
	}
}

// TestWriteHTMLInventoryCapped confirms a record whose created inventory exceeds
// maxInvRows renders only maxInvRows detail rows and reports the dropped count in
// the truncation notice, while the per-kind total still counts every resource —
// so a high-churn soak run cannot produce an unbounded, unopenable report.
func TestWriteHTMLInventoryCapped(t *testing.T) {
	const extra = 5
	rec := sampleRecord()
	rec.Created = make([]neutron.Resource, maxInvRows+extra)
	for i := range rec.Created {
		rec.Created[i] = neutron.Resource{
			Kind:    neutron.KindPort,
			Logical: "port-" + strconv.Itoa(i),
			Name:    "dizzy-port-" + strconv.Itoa(i),
			ID:      "port-id-" + strconv.Itoa(i),
		}
	}

	var buf bytes.Buffer
	if err := WriteHTML(&buf, rec); err != nil {
		t.Fatalf("WriteHTML: %v", err)
	}
	out := buf.String()

	// The uuid cell's "mono" class is unique to inventory rows, so its count is
	// the number of rendered detail rows.
	if got := strings.Count(out, `class="mono"`); got != maxInvRows {
		t.Errorf("inventory rendered %d detail rows, want the %d-row cap", got, maxInvRows)
	}
	if want := "and " + strconv.Itoa(extra) + " more not shown"; !strings.Contains(out, want) {
		t.Errorf("truncation notice %q missing from report", want)
	}
	if want := "Created resources (" + strconv.Itoa(maxInvRows+extra) + ")"; !strings.Contains(out, want) {
		t.Errorf("reported total %q missing; the cap must not change the counted total", want)
	}
}

// TestWriteHTMLApplyOmitsChaos confirms the chaos section appears only for a
// chaos record, mirroring the table renderer's apply/chaos separation.
func TestWriteHTMLApplyOmitsChaos(t *testing.T) {
	var apply, chaos bytes.Buffer
	if err := WriteHTML(&apply, sampleRecord()); err != nil {
		t.Fatalf("WriteHTML(apply): %v", err)
	}
	if strings.Contains(apply.String(), "Chaos churn") {
		t.Error("apply report unexpectedly contains the chaos section")
	}
	if err := WriteHTML(&chaos, chaosRecord()); err != nil {
		t.Fatalf("WriteHTML(chaos): %v", err)
	}
	if !strings.Contains(chaos.String(), "Chaos churn") {
		t.Error("chaos report missing the chaos section")
	}
}

// TestWriteHTMLIncompleteBanner confirms the banner is three-way: a checkpoint
// shows the incomplete banner, a complete record the ok banner, and a failed
// run the fail banner even when it is also incomplete.
func TestWriteHTMLIncompleteBanner(t *testing.T) {
	tests := []struct {
		name       string
		incomplete bool
		err        string
		want       []string
		notWant    []string
	}{
		{"incomplete", true, "", []string{"banner warn", "Run incomplete", "checkpoint written at 2026-06-24T10:01:30Z"}, []string{"banner ok", "banner fail"}},
		{"complete", false, "", []string{"banner ok"}, []string{"banner warn", "banner fail"}},
		{"failed takes precedence", true, "boom", []string{"banner fail"}, []string{"banner warn", "banner ok"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := chaosRecord()
			rec.Incomplete = tc.incomplete
			rec.Error = tc.err

			var buf bytes.Buffer
			if err := WriteHTML(&buf, rec); err != nil {
				t.Fatalf("WriteHTML: %v", err)
			}
			out := buf.String()
			// The stylesheet spells the classes as ".banner.ok", so "banner ok"
			// only matches a rendered banner.
			for _, s := range tc.want {
				if !strings.Contains(out, s) {
					t.Errorf("report lacks %q", s)
				}
			}
			for _, s := range tc.notWant {
				if strings.Contains(out, s) {
					t.Errorf("report unexpectedly has %q", s)
				}
			}
		})
	}
}

// TestWriteHTMLGoldenMix locks the rendered HTML for a mix record: the overall
// sections, then one section per persona with its chips and KPIs, and the
// time-series charts for the persona that has churn buckets.
func TestWriteHTMLGoldenMix(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteHTML(&buf, mixRecord()); err != nil {
		t.Fatalf("WriteHTML: %v", err)
	}
	checkGolden(t, "report-mix.html", buf.Bytes())
}

// TestWriteHTMLPersonaWithoutChaos confirms a persona with no churn statistics
// renders its heading, chips and KPIs and no chart, while the persona with
// buckets gets both charts.
func TestWriteHTMLPersonaWithoutChaos(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteHTML(&buf, mixRecord()); err != nil {
		t.Fatalf("WriteHTML: %v", err)
	}
	out := buf.String()
	ci := strings.Index(out, "<h2>Persona ci</h2>")
	legacy := strings.Index(out, "<h2>Persona legacy</h2>")
	end := strings.Index(out, "<script>")
	if ci < 0 || legacy < ci || end < legacy {
		t.Fatalf("persona sections missing or out of order (ci %d, legacy %d, script %d)", ci, legacy, end)
	}
	ciSection, legacySection := out[ci:legacy], out[legacy:end]
	if got := strings.Count(ciSection, "<svg"); got != 2 {
		t.Errorf("ci section has %d charts, want 2", got)
	}
	for _, want := range []string{"project -", "share 40%", "servers 4", "total ops"} {
		if !strings.Contains(legacySection, want) {
			t.Errorf("legacy section lacks %q:\n%s", want, legacySection)
		}
	}
	if strings.Contains(legacySection, "<svg") {
		t.Errorf("legacy section without churn statistics renders a chart:\n%s", legacySection)
	}
}

// TestWriteHTMLEscapesPersonaName confirms a persona name carrying a script
// payload is HTML-escaped.
func TestWriteHTMLEscapesPersonaName(t *testing.T) {
	rec := mixRecord()
	rec.Personas[0].Name = "<script>alert(1)</script>"
	var buf bytes.Buffer
	if err := WriteHTML(&buf, rec); err != nil {
		t.Fatalf("WriteHTML: %v", err)
	}
	if strings.Contains(buf.String(), "<script>alert(1)</script>") {
		t.Error("persona name rendered without escaping (XSS)")
	}
}

// mixOneLaneRecord is a mix record with the ci persona and the keystone lane.
func mixOneLaneRecord() *Record {
	r := mixLanesRecord()
	r.Personas = r.Personas[:1]
	r.Lanes = r.Lanes[:1]
	return r
}

// TestWriteHTMLGoldenMixLanes locks the rendered HTML for a mix record with
// one persona and one lane: the persona section, then the lane section with
// its chips, KPIs and time-series charts.
func TestWriteHTMLGoldenMixLanes(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteHTML(&buf, mixOneLaneRecord()); err != nil {
		t.Fatalf("WriteHTML: %v", err)
	}
	checkGolden(t, "report-mix-lanes.html", buf.Bytes())
}

// TestWriteHTMLLaneWithoutChaos confirms a lane with no churn statistics
// renders its heading, chips and KPIs and no chart, while the lane with
// buckets gets both charts, after the persona sections.
func TestWriteHTMLLaneWithoutChaos(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteHTML(&buf, mixLanesRecord()); err != nil {
		t.Fatalf("WriteHTML: %v", err)
	}
	out := buf.String()
	legacy := strings.Index(out, "<h2>Persona legacy</h2>")
	keystone := strings.Index(out, "<h2>Lane keystone</h2>")
	glance := strings.Index(out, "<h2>Lane glance</h2>")
	end := strings.Index(out, "<script>")
	if legacy < 0 || keystone < legacy || glance < keystone || end < glance {
		t.Fatalf("sections missing or out of order (legacy %d, keystone %d, glance %d, script %d)", legacy, keystone, glance, end)
	}
	keystoneSection, glanceSection := out[keystone:glance], out[glance:end]
	if got := strings.Count(keystoneSection, "<svg"); got != 2 {
		t.Errorf("keystone section has %d charts, want 2", got)
	}
	for _, want := range []string{"project -", "scenario small/glance", "total ops"} {
		if !strings.Contains(glanceSection, want) {
			t.Errorf("glance section lacks %q:\n%s", want, glanceSection)
		}
	}
	if strings.Contains(glanceSection, "<svg") {
		t.Errorf("glance section without churn statistics renders a chart:\n%s", glanceSection)
	}
}

// TestWriteHTMLEscapesLaneName confirms a lane name carrying markup is
// HTML-escaped.
func TestWriteHTMLEscapesLaneName(t *testing.T) {
	rec := mixLanesRecord()
	rec.Lanes[0].Name = "<script>alert(1)</script>"
	var buf bytes.Buffer
	if err := WriteHTML(&buf, rec); err != nil {
		t.Fatalf("WriteHTML: %v", err)
	}
	if strings.Contains(buf.String(), "<script>alert(1)</script>") {
		t.Error("lane name rendered without escaping (XSS)")
	}
	if !strings.Contains(buf.String(), "<h2>Lane &lt;script&gt;alert(1)&lt;/script&gt;</h2>") {
		t.Error("lane heading lacks the escaped name")
	}
}

// TestWriteHTMLGoldenPressure locks the rendered HTML for a pressure record:
// the pressure section with its chips and per-operation table after the
// inventory, and one heading with both time-series charts per operation.
func TestWriteHTMLGoldenPressure(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteHTML(&buf, pressureRecord()); err != nil {
		t.Fatalf("WriteHTML: %v", err)
	}
	checkGolden(t, "report-pressure.html", buf.Bytes())

	out := buf.String()
	if !strings.Contains(out, "<h2>Pressure</h2>") {
		t.Error("pressure report lacks the pressure heading")
	}
	for _, op := range []string{"revoke", "validate"} {
		heading := "<h2>Pressure: " + op + "</h2>"
		if got := strings.Count(out, heading); got != 1 {
			t.Fatalf("pressure report has %d %q headings, want 1", got, heading)
		}
		series := out[strings.Index(out, heading)+len(heading):]
		if end := strings.Index(series, "<h2"); end >= 0 {
			series = series[:end]
		} else {
			series = series[:strings.Index(series, "<script>")]
		}
		if got := strings.Count(series, "<svg"); got != 2 {
			t.Errorf("%s series has %d charts, want 2", op, got)
		}
	}
}

// TestWriteHTMLGoldenPressureStages locks the rendered HTML for a staged
// pressure record: the stage table under its own heading between the
// per-operation table and the per-operation charts.
func TestWriteHTMLGoldenPressureStages(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteHTML(&buf, pressureStagesRecord()); err != nil {
		t.Fatalf("WriteHTML: %v", err)
	}
	checkGolden(t, "report-pressure-stages.html", buf.Bytes())
}

// TestWriteHTMLPressureStagesTable confirms a staged record's pressure
// section carries one "Pressure stages" heading after the per-operation table
// and before the first per-operation chart, over a sortable table with the
// stage columns and one row per stage and operation, "all" first, with "-"
// sorted as -1 for a stage without an override and the window sorted by
// nanoseconds; and that a record without stages has no such heading.
func TestWriteHTMLPressureStagesTable(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteHTML(&buf, pressureStagesRecord()); err != nil {
		t.Fatalf("WriteHTML: %v", err)
	}
	section := pressureSection(buf.String())
	const heading = "<h2>Pressure stages</h2>"
	if got := strings.Count(section, heading); got != 1 {
		t.Fatalf("pressure section has %d %q headings, want 1:\n%s", got, heading, section)
	}
	at := strings.Index(section, heading)
	if ops := strings.Index(section, "<th>operation</th><th>ops</th>"); ops < 0 || ops > at {
		t.Errorf("stage heading at %d does not follow the per-operation table at %d", at, ops)
	}
	if series := strings.Index(section, "<h2>Pressure: "); series < at {
		t.Errorf("stage heading at %d does not precede the first per-operation heading at %d", at, series)
	}
	table := section[at+len(heading):]
	table = table[:strings.Index(table, "</table>")]
	if !strings.HasPrefix(table, "\n<table class=\"sortable\">\n  <thead><tr><th>stage</th><th>fan</th><th>start</th>") {
		t.Errorf("stage heading is not followed by the sortable stage table:\n%s", table)
	}
	if !strings.Contains(table, "<th>end</th><th>operation</th><th>ops</th><th>ok</th><th>failed</th><th>ops/s</th><th>p50</th><th>p95</th><th>p99</th></tr></thead>") {
		t.Errorf("stage table header lacks the remaining columns:\n%s", table)
	}
	if got := strings.Count(table, "<tr><td"); got != 6 {
		t.Errorf("stage table has %d rows, want 6 (two stages, each all, effective and validate):\n%s", got, table)
	}
	for _, want := range []string{
		`<tr><td data-sort="1">1</td><td data-sort="-1">-</td><td data-sort="0">0ms</td><td data-sort="60000000000">60.00s</td><td data-sort="all">all</td>`,
		`<tr><td data-sort="2">2</td><td data-sort="1000">1000</td><td data-sort="61500400000">61.50s</td><td data-sort="120000000000">120.00s</td><td data-sort="validate">validate</td>`,
	} {
		if !strings.Contains(table, want) {
			t.Errorf("stage table lacks the row %q:\n%s", want, table)
		}
	}

	var plain bytes.Buffer
	if err := WriteHTML(&plain, pressureRecord()); err != nil {
		t.Fatalf("WriteHTML(without stages): %v", err)
	}
	if strings.Contains(plain.String(), "Pressure stages") {
		t.Error("report of a run without stages has a stage heading")
	}
}

// pressureSection returns the pressure section of a rendered report, from its
// heading up to the script, or "" when the report has none.
func pressureSection(out string) string {
	start := strings.Index(out, "<h2>Pressure</h2>")
	if start < 0 {
		return ""
	}
	return out[start:strings.Index(out, "<script>")]
}

// TestWriteHTMLPressureSection confirms the pressure section's chips, with
// the churn users only when the run reserved any, and its sortable table with
// "operation" as the first column and one row per operation.
func TestWriteHTMLPressureSection(t *testing.T) {
	tests := []struct {
		name       string
		churnUsers int
		want       []string
		notWant    []string
	}{
		{"with churn users", 1,
			[]string{"workers 4", "token pool 3", "churn users 1", "mix revoke=1, validate=9", "revocations 3"}, nil},
		{"without churn users", 0,
			[]string{"workers 4", "token pool 3", "mix revoke=1, validate=9", "revocations 3"}, []string{"churn users"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := pressureRecord()
			rec.Pressure.ChurnUsers = tc.churnUsers
			var buf bytes.Buffer
			if err := WriteHTML(&buf, rec); err != nil {
				t.Fatalf("WriteHTML: %v", err)
			}
			section := pressureSection(buf.String())
			for _, s := range tc.want {
				if !strings.Contains(section, `<span class="chip">`+s+`</span>`) {
					t.Errorf("pressure section lacks the chip %q:\n%s", s, section)
				}
			}
			for _, s := range tc.notWant {
				if strings.Contains(section, s) {
					t.Errorf("pressure section unexpectedly has %q:\n%s", s, section)
				}
			}
			if !strings.Contains(section, `<table class="sortable">`) || !strings.Contains(section, "<thead><tr><th>operation</th><th>ops</th>") {
				t.Errorf("pressure section lacks the sortable operation table:\n%s", section)
			}
			for _, op := range []string{"revoke", "validate"} {
				if !strings.Contains(section, `<tr><td data-sort="`+op+`">`+op+`</td>`) {
					t.Errorf("pressure table lacks the %s row:\n%s", op, section)
				}
			}
		})
	}
}

// TestWriteHTMLApplyOmitsPressure confirms the pressure section appears only
// for a pressure record: an apply and a chaos report have no pressure heading.
func TestWriteHTMLApplyOmitsPressure(t *testing.T) {
	for name, build := range map[string]func() *Record{"apply": sampleRecord, "chaos": chaosRecord} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteHTML(&buf, build()); err != nil {
				t.Fatalf("WriteHTML: %v", err)
			}
			if strings.Contains(buf.String(), "<h2>Pressure") {
				t.Errorf("%s report unexpectedly contains a pressure heading", name)
			}
		})
	}
}

// TestWriteHTMLPressureWithoutBuckets confirms a pressure record without
// buckets, a checkpoint taken before the first one, renders the chips and the
// table and no per-operation heading or chart.
func TestWriteHTMLPressureWithoutBuckets(t *testing.T) {
	rec := pressureRecord()
	rec.Pressure.Buckets = nil
	var buf bytes.Buffer
	if err := WriteHTML(&buf, rec); err != nil {
		t.Fatalf("WriteHTML: %v", err)
	}
	out := buf.String()
	section := pressureSection(out)
	if section == "" {
		t.Fatal("pressure report without buckets lacks the pressure section")
	}
	for _, want := range []string{`<span class="chip">workers 4</span>`, `<span class="chip">revocations 3</span>`, `<td data-sort="validate">validate</td>`} {
		if !strings.Contains(section, want) {
			t.Errorf("pressure section lacks %q:\n%s", want, section)
		}
	}
	if strings.Contains(out, "Pressure: ") {
		t.Error("pressure report without buckets has a per-operation heading")
	}
	if strings.Contains(section, "<svg") {
		t.Errorf("pressure section without buckets renders a chart:\n%s", section)
	}
}

// TestWriteHTMLPressureZeroOperationBucket confirms an operation without an
// entry in a bucket charts that bucket as zero rather than skipping it, so
// every operation's series has one point per bucket.
func TestWriteHTMLPressureZeroOperationBucket(t *testing.T) {
	p := pressureRecord().Pressure
	p.Buckets[1].Operations.ByType = p.Buckets[1].Operations.ByType[1:] // no revoke in the second bucket
	series := operationBuckets(p.Buckets, "revoke")
	if len(series) != 2 {
		t.Fatalf("revoke series has %d buckets, want 2", len(series))
	}
	if series[0].Start != 0 || series[0].Stats.Attempted != 2 {
		t.Errorf("first revoke bucket = %+v, want start 0 and 2 attempted", series[0])
	}
	if series[1].Start != time.Minute || series[1].Stats != (metrics.Stats{}) {
		t.Errorf("second revoke bucket = %+v, want start 1m and zero stats", series[1])
	}
}

// TestWriteHTMLEscapesPressureOperation confirms an operation name carrying
// markup is HTML-escaped in the table and the series heading.
func TestWriteHTMLEscapesPressureOperation(t *testing.T) {
	rec := pressureRecord()
	const name = "<script>alert(1)</script>"
	rec.Pressure.Operations.ByType[0].Type = name
	rec.Pressure.Buckets[0].Operations.ByType[0].Type = name
	var buf bytes.Buffer
	if err := WriteHTML(&buf, rec); err != nil {
		t.Fatalf("WriteHTML: %v", err)
	}
	if strings.Contains(buf.String(), name) {
		t.Error("operation name rendered without escaping (XSS)")
	}
	if !strings.Contains(buf.String(), "<h2>Pressure: &lt;script&gt;alert(1)&lt;/script&gt;</h2>") {
		t.Error("series heading lacks the escaped operation name")
	}
}

// TestWriteHTMLGoldenPressureServices locks the rendered HTML for a staged
// pressure record with probes, bad tokens and the propagation probe: the
// probe and bad-token tables by service and the propagation chips between
// the per-operation table and the stage table.
func TestWriteHTMLGoldenPressureServices(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteHTML(&buf, pressureServicesRecord()); err != nil {
		t.Fatalf("WriteHTML: %v", err)
	}
	checkGolden(t, "report-pressure-services.html", buf.Bytes())
}

// TestWriteHTMLPressureServicesTables confirms the pressure section of a run
// with probes, bad tokens and the propagation probe carries, in this order
// after the per-operation table and before the stage table, a "Probes by
// service" and a "Bad tokens by service" heading, each over a sortable table
// with "service" as the first column and one row per service, and a
// "Propagation" heading with its chips, the delays humanized; that a run
// whose propagation probe completed nothing shows "-" for every delay; that
// the page stays self-contained; and that a run without them has none of the
// headings.
func TestWriteHTMLPressureServicesTables(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteHTML(&buf, pressureServicesRecord()); err != nil {
		t.Fatalf("WriteHTML: %v", err)
	}
	section := pressureSection(buf.String())
	headings := []string{"<h2>Probes by service</h2>", "<h2>Bad tokens by service</h2>", "<h2>Propagation</h2>"}
	prev := strings.Index(section, "<thead><tr><th>operation</th><th>ops</th>")
	if prev < 0 {
		t.Fatalf("pressure section lacks the per-operation table:\n%s", section)
	}
	for _, h := range headings {
		if got := strings.Count(section, h); got != 1 {
			t.Fatalf("pressure section has %d %q headings, want 1:\n%s", got, h, section)
		}
		at := strings.Index(section, h)
		if at < prev {
			t.Errorf("%q at %d does not follow the previous table or heading at %d", h, at, prev)
		}
		prev = at
	}
	if stages := strings.Index(section, "<h2>Pressure stages</h2>"); stages < prev {
		t.Errorf("stage heading at %d does not follow the propagation heading at %d", stages, prev)
	}

	for i, tc := range []struct {
		services []string
	}{
		{[]string{"glance", "nova"}},
		{[]string{"nova"}},
	} {
		table := section[strings.Index(section, headings[i])+len(headings[i]):]
		table = table[:strings.Index(table, "</table>")]
		if !strings.HasPrefix(table, "\n<table class=\"sortable\">\n  <thead><tr><th>service</th><th>ops</th><th>ok</th><th>failed</th><th>success</th>") {
			t.Errorf("%q is not followed by the sortable service table:\n%s", headings[i], table)
		}
		if got := strings.Count(table, "<tr><td"); got != len(tc.services) {
			t.Errorf("%q table has %d rows, want %d:\n%s", headings[i], got, len(tc.services), table)
		}
		for _, svc := range tc.services {
			if !strings.Contains(table, `<tr><td data-sort="`+svc+`">`+svc+`</td>`) {
				t.Errorf("%q table lacks the %s row:\n%s", headings[i], svc, table)
			}
		}
	}
	probes := section[strings.Index(section, headings[0]):strings.Index(section, headings[1])]
	if want := `<td data-sort="1200000">1.20ms</td>`; !strings.Contains(probes, want) {
		t.Errorf("probe table lacks nova's p50 cell %q:\n%s", want, probes)
	}

	propagation := section[strings.Index(section, headings[2]):strings.Index(section, "<h2>Pressure stages</h2>")]
	for _, chip := range []string{"every 30s", "timeout 10m0s", "probes 12", "timed out 0", "p50 2.10s", "p95 4.80s", "p99 6.20s", "max 6.20s"} {
		if !strings.Contains(propagation, `<span class="chip">`+chip+`</span>`) {
			t.Errorf("propagation section lacks the chip %q:\n%s", chip, propagation)
		}
	}
	for _, bad := range []string{"http://", "https://", "<link ", "src=", "url(http", "@import"} {
		if strings.Contains(buf.String(), bad) {
			t.Errorf("report contains external reference %q, expected a self-contained file", bad)
		}
	}

	none := pressureServicesRecord()
	none.Pressure.Propagation.Probes, none.Pressure.Propagation.Delay = 0, metrics.Latency{}
	buf.Reset()
	if err := WriteHTML(&buf, none); err != nil {
		t.Fatalf("WriteHTML(no probe completed): %v", err)
	}
	for _, chip := range []string{"probes 0", "p50 -", "p95 -", "p99 -", "max -"} {
		if !strings.Contains(buf.String(), `<span class="chip">`+chip+`</span>`) {
			t.Errorf("propagation section without a completed probe lacks the chip %q", chip)
		}
	}

	buf.Reset()
	if err := WriteHTML(&buf, pressureStagesRecord()); err != nil {
		t.Fatalf("WriteHTML(without services): %v", err)
	}
	for _, h := range headings {
		if strings.Contains(buf.String(), h) {
			t.Errorf("report of a run without probes, bad tokens or propagation has %q", h)
		}
	}
}

// TestWriteHTMLEscapesProbeService confirms a service name carrying markup is
// HTML-escaped in the probe and bad-token tables.
func TestWriteHTMLEscapesProbeService(t *testing.T) {
	rec := pressureServicesRecord()
	const name = "<b>x</b>"
	rec.Pressure.Probes[0].Type = name
	rec.Pressure.BadTokens[0].Type = name
	var buf bytes.Buffer
	if err := WriteHTML(&buf, rec); err != nil {
		t.Fatalf("WriteHTML: %v", err)
	}
	if strings.Contains(buf.String(), name) {
		t.Error("service name rendered without escaping")
	}
	const escaped = `<tr><td data-sort="&lt;b&gt;x&lt;/b&gt;">&lt;b&gt;x&lt;/b&gt;</td>`
	if got := strings.Count(buf.String(), escaped); got != 2 {
		t.Errorf("report has %d rows with the escaped service name, want 2, one per table", got)
	}
}
