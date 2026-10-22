package gpu

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// Reading npu-smi output: values inside the card table and per-card health
// details. The table itself is read in ascend_chips.go; running the commands
// is in ascend.go.

var bdfRE = regexp.MustCompile(`[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-9a-fA-F]`)
var (
	slashValueRE = regexp.MustCompile(`(?i)([0-9]+(?:\.[0-9]+)?)\s*/\s*([0-9]+(?:\.[0-9]+)?)\s*([KMGT]i?B|[KMGT]B)?`)
	percentRE    = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)\s*%`)
	wattsRE      = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)\s*W\b`)
	celsiusRE    = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)\s*C\b`)
)

// ascendHealthStatus finds npu-smi's Health verdict among a table row's
// fields. npu-smi documents four levels — OK, Warning, Alarm, Critical; the
// other spellings are accepted from older or customised builds.
func ascendHealthStatus(fields []string) string {
	for _, f := range fields {
		value := strings.Trim(f, ",;")
		if ascendHealthCode(value) != "" || ascendHealthOK(value) {
			return value
		}
	}
	return ""
}

func ascendHealthOK(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "ok", "healthy", "normal":
		return true
	}
	return false
}

// ascendHealthCode maps npu-smi's Health verdict to a finding: Warning keeps
// the chip schedulable at a lower preference, Alarm and Critical (and the
// failure spellings) stop scheduling, anything else is not a finding.
func ascendHealthCode(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "warning", "warn":
		return accelerator.HealthVendorWarning
	case "alarm", "critical", "fault", "fail", "failed", "abnormal", "error":
		return accelerator.HealthVendorFault
	}
	return ""
}

type ascendCommandRunner func(context.Context, ...string) (string, error)

func parseAscendHealthDetail(out string) (status, code, message string) {
	collectMessage := false
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(strings.Trim(strings.TrimSpace(raw), "|"))
		if line == "" || strings.Trim(line, "+-=") == "" {
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if found {
			recognized := true
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "health status":
				status = strings.TrimSpace(value)
				collectMessage = false
			case "error code":
				code = strings.TrimSpace(value)
				collectMessage = false
			case "error information":
				message = strings.TrimSpace(value)
				collectMessage = true
			default:
				recognized = false
			}
			if recognized {
				continue
			}
		}
		if collectMessage {
			if message != "" {
				message += " "
			}
			message += line
		}
	}
	return status, code, strings.TrimSpace(message)
}

func ascendMemoryBytes(fields []string) int64 {
	for _, f := range fields {
		v := strings.ToLower(strings.Trim(f, ",;"))
		mult := int64(0)
		switch {
		case strings.HasSuffix(v, "gib"):
			mult = 1 << 30
			v = strings.TrimSuffix(v, "gib")
		case strings.HasSuffix(v, "gb"):
			mult = 1 << 30
			v = strings.TrimSuffix(v, "gb")
		case strings.HasSuffix(v, "mib"):
			mult = 1 << 20
			v = strings.TrimSuffix(v, "mib")
		case strings.HasSuffix(v, "mb"):
			mult = 1 << 20
			v = strings.TrimSuffix(v, "mb")
		default:
			continue
		}
		n, err := strconv.ParseFloat(v, 64)
		if err == nil && n > 0 {
			return int64(n * float64(mult))
		}
	}
	return 0
}

func ascendBDF(line string) string {
	return bdfRE.FindString(line)
}

// ascendTableRow is a chip's text in the card table: its line, and the
// Bus-Id line under it when the table spans two lines per chip. A line that
// already carries the Bus-Id is a whole row by itself.
func ascendTableRow(lines []string, i int) string {
	row := lines[i]
	if !bdfRE.MatchString(row) && i+1 < len(lines) && bdfRE.MatchString(lines[i+1]) {
		row += "\n" + lines[i+1]
	}
	return row
}

func parseSlashBytes(line string) (float64, float64, bool) {
	return parseSlashBytesWithDefaultUnit(line, 0)
}

func parseSlashBytesWithDefaultUnit(line string, defaultScale float64) (float64, float64, bool) {
	matches := slashValueRE.FindAllStringSubmatch(line, -1)
	for _, m := range matches {
		used, err1 := strconv.ParseFloat(m[1], 64)
		total, err2 := strconv.ParseFloat(m[2], 64)
		if err1 != nil || err2 != nil || total <= 0 || used > total {
			continue
		}
		unit := strings.ToLower(strings.ReplaceAll(m[3], " ", ""))
		scale := defaultScale
		if scale == 0 {
			scale = float64(1 << 20) // npu-smi memory is commonly MB/MiB when unit omitted.
		}
		switch unit {
		case "gib", "gb":
			scale = 1 << 30
		case "mib", "mb", "":
			scale = 1 << 20
		case "kib", "kb":
			scale = 1 << 10
		}
		return used * scale, total * scale, true
	}
	return 0, 0, false
}

// ascendColumnValue parses power and temperature from the first half of a 26.x
// two-line table row. Separators are not stable enough for byte offsets, but
// these two columns immediately follow NPU, name and health.
func ascendColumnValue(line, column string) (float64, bool) {
	first, _, _ := strings.Cut(line, "\n")
	fields := strings.Fields(strings.NewReplacer("|", " ", "/", " ").Replace(first))
	if len(fields) < 5 || !isAscendIndex(fields[0]) {
		return 0, false
	}
	idx := -1
	switch column {
	case "Power(W)":
		idx = 3
	case "Temp(C)":
		idx = 4
	}
	if idx < 0 || idx >= len(fields) {
		return 0, false
	}
	v := strings.TrimSuffix(strings.TrimSuffix(fields[idx], "%"), "W")
	v = strings.TrimSuffix(v, "C")
	n, err := strconv.ParseFloat(v, 64)
	return n, err == nil
}

func ascendContinuationAICore(row string) (float64, bool) {
	_, continuation, ok := strings.Cut(row, "\n")
	if !ok || !bdfRE.MatchString(continuation) {
		return 0, false
	}
	fields := strings.Fields(strings.NewReplacer("|", " ", "/", " ").Replace(continuation))
	for i, field := range fields {
		if bdfRE.MatchString(field) && i+1 < len(fields) {
			v, err := strconv.ParseFloat(strings.TrimSuffix(fields[i+1], "%"), 64)
			return v, err == nil
		}
	}
	return 0, false
}

func isAscendIndex(s string) bool {
	_, err := strconv.Atoi(strings.TrimSpace(s))
	return err == nil
}

func firstRegexFloat(re *regexp.Regexp, line string) (float64, bool) {
	m := re.FindStringSubmatch(line)
	if len(m) < 2 {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	return v, err == nil
}
