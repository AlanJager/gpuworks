package report

import (
	"fmt"
	"html"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
)

func warmupRows(run Run) [][]string {
	var rows [][]string
	for _, attempt := range run.Warmup {
		outcome := "success"
		if attempt.Canceled {
			outcome = "canceled"
		} else if attempt.ErrorText != "" {
			outcome = "failed"
		}
		stamp := func(t *time.Time) string {
			if t == nil {
				return "unknown"
			}
			return t.UTC().Format(time.RFC3339Nano)
		}
		status := "unknown"
		if attempt.HTTPStatus > 0 {
			status = fmt.Sprint(attempt.HTTPStatus)
		}
		rows = append(rows, []string{attempt.SessionID, fmt.Sprint(attempt.Attempt), outcome, status, attempt.ErrorStage, stamp(attempt.Metrics.StartedAt), stamp(attempt.Metrics.FinishedAt), attempt.ErrorText})
	}
	return rows
}

var warmupColumns = []string{"Warmup session", "Attempt", "Outcome", "HTTP status", "Error stage", "Started", "Finished", "Diagnostic"}

const warmupExplanation = "Warmup attempts are excluded from measured requests, points, throughput and latency. Transient failures recover for up to 6 minutes with 5 seconds between probes; permanent errors stop immediately."

func writeWarmupMarkdown(out *strings.Builder, run Run) {
	if len(run.Warmup) == 0 {
		return
	}
	out.WriteString("\n## Warmup recovery evidence\n\n" + warmupExplanation + "\n\n|")
	for _, column := range warmupColumns {
		fmt.Fprintf(out, " %s |", column)
	}
	out.WriteString("\n|")
	for range warmupColumns {
		out.WriteString("---|")
	}
	out.WriteByte('\n')
	for _, row := range warmupRows(run) {
		out.WriteByte('|')
		for _, value := range row {
			fmt.Fprintf(out, " %s |", markdownCell(value))
		}
		out.WriteByte('\n')
	}
}

func writeWarmupHTML(out *strings.Builder, run Run) {
	if len(run.Warmup) == 0 {
		return
	}
	out.WriteString("<h2>Warmup recovery evidence</h2><p>" + warmupExplanation + "</p><table><thead><tr>")
	for _, column := range warmupColumns {
		fmt.Fprintf(out, "<th>%s</th>", column)
	}
	out.WriteString("</tr></thead><tbody>")
	for _, row := range warmupRows(run) {
		out.WriteString("<tr>")
		for _, value := range row {
			fmt.Fprintf(out, "<td>%s</td>", html.EscapeString(value))
		}
		out.WriteString("</tr>")
	}
	out.WriteString("</tbody></table>")
}

func safeWarmup(records []loadgen.DetailRecord) []loadgen.DetailRecord {
	out := append([]loadgen.DetailRecord(nil), records...)
	for i := range out {
		out[i].ErrorText = loadgen.ResponseErrorText(out[i].ErrorText)
		out[i].Text, out[i].Usage, out[i].Metrics.ChunkIntervalsMs = "", nil, nil
	}
	return out
}
