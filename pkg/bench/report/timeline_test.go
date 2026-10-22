package report

import (
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
)

func timedRecord(start time.Time, session string, turn int) loadgen.DetailRecord {
	first, visible, last, finish := start.Add(time.Second), start.Add(2*time.Second), start.Add(3*time.Second), start.Add(4*time.Second)
	ttft, tpot, latency, completion := 1000.0, 20.0, 3000.0, 4000.0
	return loadgen.DetailRecord{Version: 1, SessionID: session, Turn: turn, PlannedTurns: 13, Attempt: 1, FinishReason: "stop", Metrics: loadgen.DetailMetrics{StartedAt: &start, FirstOutputAt: &first, FirstContentAt: &visible, LastOutputAt: &last, FinishedAt: &finish, TTFTMs: &ttft, TPOTMs: &tpot, LatencyMs: &latency, CompletionMs: &completion, InputTokens: 1000, OutputTokens: 101, Chunks: 3, ChunkIntervalsMs: []float64{1000, 1000}}, Usage: []byte(`{"prompt_tokens":1000,"completion_tokens":101}`), InputSource: "server_usage", OutputSource: "server_usage"}
}
func TestTimelineLinksRequestsAndPreservesTimingAndErrors(t *testing.T) {
	e := sampleEvidence()
	point := evidencePoint(e)
	point.DetailStatus = "truncated"
	point.DetailNote = "read limit reached"
	one := timedRecord(*e.Timing.StartedAt, "session-1", 1)
	two := timedRecord(e.Timing.StartedAt.Add(500*time.Millisecond), "session-2", 1)
	two.ErrorText = "stream interrupted <script>bad()</script>"
	two.ErrorStage = "stream"
	two.HTTPStatus = 200
	two.Text = "</pre><script>bad()</script>"
	point.Details = []loadgen.DetailRecord{one, two}
	run := Run{Points: []Point{point}}
	doc, err := Render("html", run)
	if err != nil {
		t.Fatal(err)
	}
	body := string(doc.Body)
	for _, want := range []string{`href="#request-1-1"`, `id="request-1-1"`, `href="#request-1-2"`, `id="request-1-2" open`, `aria-label="Session request timeline"`, `aria-label="Observed in-flight requests"`, `aria-label="Observed active sessions"`, `maximum 2`, `first visible content`, `First visible content`, `First output (including reasoning)`, `Stream finished`, `truncated`, `read limit reached`, `retained records=2`, `&lt;script&gt;bad()&lt;/script&gt;`, `server_usage`, `1000.000`, `4000.000`} {
		if !strings.Contains(body, want) {
			t.Errorf("timeline missing %q", want)
		}
	}
	if strings.Contains(body, "<script>") || strings.Contains(body, "%!") {
		t.Fatal("unsafe or malformed report")
	}
	md, _ := Render("markdown", run)
	if strings.Contains(string(md.Body), "<script>") || !strings.Contains(string(md.Body), "stream interrupted") {
		t.Fatal("Markdown lost error or contains injected HTML")
	}
}
func TestTimelineNeverInventsMissingEventsAndBoundsChart(t *testing.T) {
	point := Point{Seq: 1, Details: []loadgen.DetailRecord{{SessionID: "old"}}}
	if !strings.Contains(timelineSVG(point), "timestamps are incomplete") {
		t.Fatal("missing timestamps generated timeline")
	}
	start := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	point.Details = nil
	for i := 0; i < 201; i++ {
		point.Details = append(point.Details, timedRecord(start.Add(time.Duration(i)*time.Second), "session", i+1))
	}
	body := timelineSVG(point)
	if !strings.Contains(body, "displayed 200 / 201 records") || strings.Contains(body, `href="#request-1-201"`) {
		t.Fatal("chart retention bound missing")
	}
}
func TestNoDetailsDoesNotEraseAggregateEvidence(t *testing.T) {
	point := evidencePoint(sampleEvidence())
	point.DetailStatus = "unavailable"
	point.DetailNote = "artifact missing"
	doc, _ := Render("html", Run{Points: []Point{point}})
	body := string(doc.Body)
	if !strings.Contains(body, "75.00%") || !strings.Contains(body, "artifact missing") || !strings.Contains(body, "retained records=0") || strings.Contains(body, `aria-label="Session request timeline"`) {
		t.Fatal("missing artifact concealed or fabricated observations")
	}
}
