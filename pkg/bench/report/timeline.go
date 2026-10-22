package report

import (
	"fmt"
	"html"
	"sort"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
)

func requestAnchor(pointSeq, index int) string {
	return fmt.Sprintf("request-%d-%d", pointSeq, index+1)
}

func detailProperties(record loadgen.DetailRecord) [][2]string {
	m := record.Metrics
	return [][2]string{
		{"Session / turn / planned turns / attempt", fmt.Sprintf("%s / %d / %d / %d", record.SessionID, record.Turn, record.PlannedTurns, record.Attempt)},
		{"Sent", unknown(timeValue(m.StartedAt))}, {"First output (including reasoning)", unknown(timeValue(m.FirstOutputAt))}, {"First visible content", unknown(timeValue(m.FirstContentAt))}, {"Last output", unknown(timeValue(m.LastOutputAt))}, {"Stream finished", unknown(timeValue(m.FinishedAt))},
		{"TTFT ms", optionalNumber(m.TTFTMs)}, {"TPOT ms", optionalNumber(m.TPOTMs)}, {"Last output latency ms", optionalNumber(m.LatencyMs)}, {"Stream completion ms", optionalNumber(m.CompletionMs)},
		{"Selected input / output tokens", fmt.Sprintf("%d / %d", m.InputTokens, m.OutputTokens)}, {"Input / output sources", record.InputSource + " / " + record.OutputSource},
		{"Local input / output estimates", optionalIntPointer(record.LocalInput) + " / " + optionalIntPointer(record.LocalOutput)},
		{"Output budget", strconv.Itoa(record.OutputBudget)}, {"Finish reason", unknown(record.FinishReason)}, {"HTTP status", unknown(optionalInt(record.HTTPStatus))}, {"Error stage", unknown(record.ErrorStage)}, {"Error text", record.ErrorText},
		{"Raw service usage", unknown(string(record.Usage))}, {"Stream chunks / chunk intervals ms", fmt.Sprintf("%d / %s", m.Chunks, jsonValue(m.ChunkIntervalsMs))},
	}
}

func optionalNumber(value *float64) string {
	if value == nil {
		return "unknown"
	}
	return number(*value)
}
func optionalIntPointer(value *int) string {
	if value == nil {
		return "unknown"
	}
	return strconv.Itoa(*value)
}

func writeDetailMarkdown(out *strings.Builder, point Point) {
	for i, record := range point.Details {
		fmt.Fprintf(out, "<a id=\"%s\"></a>\n\n#### Request %s / turn %d\n\n| Field | Value |\n|---|---|\n", requestAnchor(point.Seq, i), markdownCell(record.SessionID), record.Turn)
		for _, property := range detailProperties(record) {
			fmt.Fprintf(out, "| %s | %s |\n", property[0], markdownCell(html.EscapeString(property[1])))
		}
		if record.Text != "" {
			// JSON escaping preserves all text without allowing an output string to
			// terminate a Markdown fence or inject raw HTML into the report.
			fmt.Fprintf(out, "\nOutput (JSON string):\n\n    %s\n\n", jsonValue(record.Text))
		}
	}
}

func writeEvidenceHTML(out *strings.Builder, rows []pointRow) {
	out.WriteString("<h2>Request and session evidence</h2>")
	for _, row := range rows {
		fmt.Fprintf(out, "<section id=\"point-%d\"><h3>Point %d (%s)</h3>", row.Seq, row.Seq, html.EscapeString(pointLabel(row.Point)))
		if row.Evidence == nil {
			out.WriteString("<p>Evidence version 1 unavailable: session outcomes, round groups, effective policies and sample counts are unknown. Historical values are not backfilled.</p>")
		} else {
			writePropertiesHTML(out, evidenceSummary(row))
			for _, group := range evidenceGroups(row.Evidence) {
				fmt.Fprintf(out, "<details><summary>%s: attempted %d, successful %d; TTFT n=%d, TPOT n=%d</summary>", html.EscapeString(group.Name), group.Group.Attempted, group.Group.Success, group.Group.TTFT.Count, group.Group.TPOT.Count)
				writePropertiesHTML(out, groupProperties(group.Group))
				out.WriteString("</details>")
			}
			out.WriteString("<details><summary>Session outcomes and execution evidence</summary>")
			writePropertiesHTML(out, executionProperties(row))
			out.WriteString("<table><thead><tr><th>Session</th><th>State</th><th>Planned</th><th>Attempted</th><th>Success</th><th>Failed</th><th>Canceled</th><th>Skipped</th><th>Started</th><th>Finished</th></tr></thead><tbody>")
			for _, session := range row.Evidence.Sessions {
				fmt.Fprintf(out, "<tr><td>%s</td><td>%s</td><td>%d</td><td>%d</td><td>%d</td><td>%d</td><td>%d</td><td>%d</td><td>%s</td><td>%s</td></tr>", html.EscapeString(session.SessionID), html.EscapeString(session.State), session.PlannedTurns, session.Attempted, session.Success, session.Failed, session.Canceled, session.Skipped, html.EscapeString(unknown(timeValue(session.StartedAt))), html.EscapeString(unknown(timeValue(session.FinishedAt))))
			}
			out.WriteString("</tbody></table></details>")
			if row.Evidence.SessionsTruncated {
				out.WriteString("<p>Session summaries truncated; aggregate counts remain complete.</p>")
			}
		}
		fmt.Fprintf(out, "<p>Request detail: <strong>%s</strong>; retained records=%d. %s</p>", html.EscapeString(detailStatus(row.Point)), len(row.Details), html.EscapeString(row.DetailNote))
		if len(row.Details) > 0 {
			out.WriteString("<details open><summary>Session request timeline — click a bar to inspect its request</summary><p>Blue: send to first generated output (including reasoning). Green: first to last output. Gray: stream completion. Orange marker: first visible content. Unobserved events have no bar. Client timing does not isolate server queue or prefill time.</p><div class=\"timeline\">")
			out.WriteString(timelineSVG(row.Point))
			out.WriteString("</div><p>Actual in-flight requests from retained request timestamps; configured concurrency is not observed concurrency. Partial detail represents only the retained subset.</p>")
			out.WriteString(concurrencySVG(row.Point))
			if row.Evidence != nil {
				var sessions []loadgen.DetailRecord
				for _, session := range row.Evidence.Sessions {
					sessions = append(sessions, loadgen.DetailRecord{Metrics: loadgen.DetailMetrics{StartedAt: session.StartedAt, FinishedAt: session.FinishedAt}})
				}
				out.WriteString(activitySVG(sessions, "Observed active sessions", "retained session summary subset"))
			}
			out.WriteString("</details>")
		}
		for i, record := range row.Details {
			open := ""
			if record.ErrorText != "" {
				open = " open"
			}
			fmt.Fprintf(out, "<details id=\"%s\"%s><summary>Request %s / turn %d / attempt %d: %s</summary>", requestAnchor(row.Seq, i), open, html.EscapeString(record.SessionID), record.Turn, record.Attempt, html.EscapeString(first(record.ErrorText, record.FinishReason, "finish reason unknown")))
			writePropertiesHTML(out, detailProperties(record))
			if record.Text != "" {
				fmt.Fprintf(out, "<h4>Retained output</h4><pre>%s</pre>", html.EscapeString(record.Text))
			}
			out.WriteString("</details>")
		}
		out.WriteString("</section>")
	}
}

func writePropertiesHTML(out *strings.Builder, properties [][2]string) {
	out.WriteString("<table><tbody>")
	for _, property := range properties {
		fmt.Fprintf(out, "<tr><th>%s</th><td>%s</td></tr>", html.EscapeString(property[0]), html.EscapeString(property[1]))
	}
	out.WriteString("</tbody></table>")
}

func timelineWindow(records []loadgen.DetailRecord) (*time.Time, *time.Time) {
	var start, end *time.Time
	for _, record := range records {
		m := record.Metrics
		if m.StartedAt != nil && (start == nil || m.StartedAt.Before(*start)) {
			start = m.StartedAt
		}
		if m.FinishedAt != nil && (end == nil || m.FinishedAt.After(*end)) {
			end = m.FinishedAt
		}
	}
	return start, end
}

func timelineSVG(point Point) string {
	start, end := timelineWindow(point.Details)
	if start == nil || end == nil || !end.After(*start) {
		return "<p>Timeline unavailable: request timestamps are incomplete.</p>"
	}
	n := len(point.Details)
	if n > 200 {
		n = 200
	}
	const left, width = 200.0, 750.0
	var out strings.Builder
	fmt.Fprintf(&out, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 980 %d" role="img" aria-label="Session request timeline">`, n*28+45)
	x := func(t *time.Time) float64 { return left + t.Sub(*start).Seconds()/end.Sub(*start).Seconds()*width }
	for i, record := range point.Details[:n] {
		y := float64(i*28 + 22)
		fmt.Fprintf(&out, `<text x="2" y="%.1f" font-size="11">%s / turn %d</text><a href="#%s"><title>%s</title>`, y+11, html.EscapeString(record.SessionID), record.Turn, requestAnchor(point.Seq, i), html.EscapeString(first(record.ErrorText, record.FinishReason, "finish reason unknown")))
		segment := func(a, b *time.Time, color string) {
			if a != nil && b != nil && !b.Before(*a) {
				w := x(b) - x(a)
				if w < 1 {
					w = 1
				}
				fmt.Fprintf(&out, `<rect x="%.2f" y="%.1f" width="%.2f" height="16" fill="%s"/>`, x(a), y, w, color)
			}
		}
		m := record.Metrics
		segment(m.StartedAt, m.FirstOutputAt, "#1677ff")
		segment(m.FirstOutputAt, m.LastOutputAt, "#16a34a")
		segment(m.LastOutputAt, m.FinishedAt, "#94a3b8")
		if m.FirstOutputAt == nil {
			segment(m.StartedAt, m.FinishedAt, "#dc2626")
		}
		if m.FirstContentAt != nil {
			fmt.Fprintf(&out, `<line x1="%.2f" x2="%.2f" y1="%.1f" y2="%.1f" stroke="#f97316" stroke-width="2"/>`, x(m.FirstContentAt), x(m.FirstContentAt), y-2, y+18)
		}
		out.WriteString("</a>")
	}
	fmt.Fprintf(&out, `<text x="200" y="%d" font-size="11">0 → %.3f seconds since first retained send; displayed %d / %d records</text></svg>`, n*28+40, end.Sub(*start).Seconds(), n, len(point.Details))
	return out.String()
}

func concurrencySVG(point Point) string {
	return activitySVG(point.Details, "Observed in-flight requests", "retained request subset")
}

func activitySVG(records []loadgen.DetailRecord, label, scope string) string {
	type event struct {
		at    time.Time
		delta int
	}
	var events []event
	for _, record := range records {
		m := record.Metrics
		if m.StartedAt != nil && m.FinishedAt != nil && m.FinishedAt.After(*m.StartedAt) {
			events = append(events, event{*m.StartedAt, 1}, event{*m.FinishedAt, -1})
		}
	}
	if len(events) == 0 {
		return ""
	}
	sort.Slice(events, func(i, j int) bool { return events[i].at.Before(events[j].at) })
	start, end := events[0].at, events[len(events)-1].at
	if !end.After(start) {
		return ""
	}
	active, maximum := 0, 0
	for i := 0; i < len(events); {
		at := events[i].at
		for i < len(events) && events[i].at.Equal(at) {
			active += events[i].delta
			i++
		}
		if active > maximum {
			maximum = active
		}
	}
	if maximum == 0 {
		return ""
	}
	var path strings.Builder
	path.WriteString("M 50 150")
	active = 0
	for i := 0; i < len(events); {
		at := events[i].at
		for i < len(events) && events[i].at.Equal(at) {
			active += events[i].delta
			i++
		}
		fmt.Fprintf(&path, " H %.2f V %.2f", 50+at.Sub(start).Seconds()/end.Sub(start).Seconds()*850, 150-float64(active)/float64(maximum)*120)
	}
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 940 190" role="img" aria-label="%s"><path d="%s" fill="none" stroke="#1677ff" stroke-width="2"/><text x="50" y="180" font-size="12">%s: maximum %d; %.3f seconds; %s</text></svg>`, html.EscapeString(label), path.String(), html.EscapeString(label), maximum, end.Sub(start).Seconds(), html.EscapeString(scope))
}
