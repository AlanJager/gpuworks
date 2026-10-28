package gpu

import (
	"strconv"
	"strings"
)

// hygonOverviewMetrics maps a bare `hy-smi` overview header token to the
// metric its column feeds and the unit suffix its cells carry. Columns not
// listed here (Perf, PwrCap, VRAM%, Dec%, Enc%, Mode, ...) vary across DTK
// releases and are tolerated without being consumed.
var hygonOverviewMetrics = map[string]struct {
	metric string
	unit   string
}{
	"Temp":   {"gpuworks_gpu_temperature_celsius", "C"},
	"AvgPwr": {"gpuworks_gpu_power_watts", "W"},
	"HCU%":   {"gpuworks_gpu_utilization", "%"},
}

type hygonOverviewColumn struct {
	position int
	metric   string
	unit     string
}

// parseHygonOverview reads the bare `hy-smi` table. Column positions come
// from the header row itself, so extra columns on newer DTK builds never
// shift the readings. A row shorter than a needed column skips just that
// metric, and "N/A" or unparseable cells yield no sample.
func parseHygonOverview(out string) map[int]map[string]float64 {
	rows := make(map[int]map[string]float64)
	var columns []hygonOverviewColumn
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if columns == nil {
			columns = hygonOverviewColumns(fields)
			continue
		}
		if len(fields) == 0 {
			continue
		}
		index, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		values := map[string]float64{}
		for _, column := range columns {
			if column.position >= len(fields) {
				continue
			}
			if value, ok := parseHygonOverviewCell(fields[column.position], column.unit); ok {
				values[column.metric] = value
			}
		}
		if len(values) > 0 {
			rows[index] = values
		}
	}
	return rows
}

// hygonOverviewColumns resolves metric-bearing column positions from a
// candidate header row. It returns nil unless the row is the overview header
// (leading HCU plus at least one known metric column), so banner and trailer
// lines around the table are skipped.
func hygonOverviewColumns(header []string) []hygonOverviewColumn {
	if len(header) == 0 || header[0] != "HCU" {
		return nil
	}
	var columns []hygonOverviewColumn
	for position, token := range header {
		column, ok := hygonOverviewMetrics[token]
		if !ok {
			continue
		}
		columns = append(columns, hygonOverviewColumn{position: position, metric: column.metric, unit: column.unit})
	}
	return columns
}

func parseHygonOverviewCell(raw, unit string) (float64, bool) {
	if strings.EqualFold(raw, "N/A") {
		return 0, false
	}
	value, err := strconv.ParseFloat(strings.TrimSuffix(raw, unit), 64)
	if err != nil {
		return 0, false
	}
	return value, true
}
