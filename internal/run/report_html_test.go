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
			Name:    "ostester-port-" + strconv.Itoa(i),
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
