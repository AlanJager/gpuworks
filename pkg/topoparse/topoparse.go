// Package topoparse parses `nvidia-smi topo -m` output into a topology model
// used for scheduling and topology visualization.
package topoparse

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// LinkClass ranks interconnect quality (higher = better/faster).
type LinkClass int

const (
	LinkUnknown LinkClass = iota
	LinkSYS               // cross-NUMA PCIe (worst)
	LinkPHB               // host bridge
	LinkNODE              // within-NUMA PCIe
	LinkPXB               // multiple PCIe bridges
	LinkPIX               // single PCIe bridge
	LinkNVLink            // NV# (best)
	LinkSelf              // X (diagonal)
)

// Link is one pairwise connection.
type Link struct {
	Raw         string // e.g. "NV12", "SYS", "PIX"
	Class       LinkClass
	NVLinkBonds int // # for NV#, else 0
}

// GPU holds per-GPU topology facts.
type GPU struct {
	Index       int
	CPUAffinity string // e.g. "0-23"
	NUMANode    int    // -1 if unknown
}

// Topology is the parsed model.
type Topology struct {
	GPUs          []GPU
	Matrix        [][]Link      // Matrix[i][j] link between GPU i and j
	FastCliques   [][]int       // maximal groups fully fast-interconnect-linked
	NVLinkCliques [][]int       // maximal groups fully NVLink-interconnected
	NUMAGroups    map[int][]int // numa node -> gpu indexes
}

func (t *Topology) UnmarshalJSON(b []byte) error {
	type alias Topology
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*t = Topology(a)
	t.Normalize()
	return nil
}

// Normalize backfills neutral and legacy clique fields from one another.
func (t *Topology) Normalize() {
	if len(t.FastCliques) == 0 && len(t.NVLinkCliques) > 0 {
		t.FastCliques = cloneCliques(t.NVLinkCliques)
	}
	if len(t.NVLinkCliques) == 0 && len(t.FastCliques) > 0 {
		t.NVLinkCliques = cloneCliques(t.FastCliques)
	}
}

func cloneCliques(in [][]int) [][]int {
	if len(in) == 0 {
		return nil
	}
	out := make([][]int, len(in))
	for i := range in {
		out[i] = append([]int(nil), in[i]...)
	}
	return out
}

// ClassOf maps a raw cell token ("X","SYS","NV12","HCCS","PIX",...) to
// (LinkClass, bonds). LinkNVLink is kept as the legacy enum name for the fastest
// accelerator interconnect class.
func ClassOf(token string) (LinkClass, int) {
	t := strings.ToUpper(strings.TrimSpace(token))
	switch t {
	case "X":
		return LinkSelf, 0
	case "SYS":
		return LinkSYS, 0
	case "PHB":
		return LinkPHB, 0
	case "NODE":
		return LinkNODE, 0
	case "PXB":
		return LinkPXB, 0
	case "PIX":
		return LinkPIX, 0
	}
	if strings.HasPrefix(t, "NV") {
		bonds := 0
		if n, err := strconv.Atoi(t[2:]); err == nil {
			bonds = n
		}
		return LinkNVLink, bonds
	}
	if strings.HasPrefix(t, "HCCS") || strings.HasPrefix(t, "XGMI") {
		return LinkNVLink, 0
	}
	// SIO joins the two chips of one Ascend 910C card on the package.
	if t == "SIO" {
		return LinkNVLink, 0
	}
	if strings.HasPrefix(t, "ICN") {
		bonds := 0
		if n, err := strconv.Atoi(t[3:]); err == nil {
			bonds = n
		}
		return LinkNVLink, bonds
	}
	return LinkUnknown, 0
}

// isGPULabel reports whether a header/row label denotes an accelerator
// (e.g. GPU3 from nvidia-smi, NPU4 from npu-smi, PPU7 from ppu-smi, or
// Phy-ID14 from npu-smi on cards with several chips, which numbers chips by
// physical id).
func isGPULabel(label string) (int, bool) {
	label = strings.ToUpper(strings.TrimSpace(label))
	prefix := ""
	switch {
	case strings.HasPrefix(label, "GPU"):
		prefix = "GPU"
	case strings.HasPrefix(label, "NPU"):
		prefix = "NPU"
	case strings.HasPrefix(label, "PPU"):
		prefix = "PPU"
	case strings.HasPrefix(label, "PHY-ID"):
		prefix = "PHY-ID"
	default:
		return 0, false
	}
	idx, err := strconv.Atoi(label[len(prefix):])
	if err != nil {
		return 0, false
	}
	return idx, true
}

// ParseAuto parses supported accelerator topology matrix outputs. It accepts
// nvidia-smi topo -m and npu-smi HCCS matrix-like output.
func ParseAuto(output string) (*Topology, error) {
	return ParseMatrix(output)
}

// ParseMatrix parses `nvidia-smi topo -m` output.
// ansiEscape matches ANSI SGR escape sequences (nvidia-smi underlines the header
// row, e.g. "\x1b[4mGPU0...\x1b[0m", even when piped to a non-tty).
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func ParseMatrix(output string) (*Topology, error) {
	output = ansiEscape.ReplaceAllString(output, "")
	lines := strings.Split(output, "\n")

	// Locate the header row: the first non-empty line whose fields are all
	// column labels (GPU#, NIC#, mlx*, CPU Affinity, ...). The header has no
	// leading row label, so its first field is a column label, not "GPU#" acting
	// as a row key. We detect it as the line preceding the first data row.
	var header []string
	headerIdx := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "Legend:") {
			break
		}
		fields := strings.Fields(trimmed)
		if len(fields) == 0 {
			continue
		}
		// The header is indented (leading tab) and its first field is a column
		// label (GPU#/NIC#/mlx*); data rows begin with a row label at column 0.
		if looksLikeHeader(fields) && startsWithWhitespace(line) {
			header = fields
			headerIdx = i
			break
		}
	}

	if header == nil {
		// Fallback: find a line containing at least one "GPU#" that looks like a
		// header of column labels.
		for i, line := range lines {
			fields := strings.Fields(line)
			if looksLikeHeader(fields) {
				header = fields
				headerIdx = i
				break
			}
		}
	}
	if header == nil {
		return nil, fmt.Errorf("topoparse: no header row found")
	}

	// Map column position -> GPU index for GPU columns only.
	// colGPU[k] is the GPU index for column label k, or -1 if not a GPU column.
	colGPU := make([]int, len(header))
	maxGPU := -1
	for k, label := range header {
		if idx, ok := isGPULabel(label); ok {
			colGPU[k] = idx
			if idx > maxGPU {
				maxGPU = idx
			}
		} else {
			colGPU[k] = -1
		}
	}
	if maxGPU < 0 {
		return nil, fmt.Errorf("topoparse: header has no GPU columns")
	}
	numGPU := maxGPU + 1

	topo := &Topology{
		GPUs:       make([]GPU, numGPU),
		Matrix:     make([][]Link, numGPU),
		NUMAGroups: map[int][]int{},
	}
	for i := range topo.Matrix {
		topo.Matrix[i] = make([]Link, numGPU)
		topo.GPUs[i] = GPU{Index: i, NUMANode: -1}
	}
	seen := make([]bool, numGPU)

	// Parse data rows.
	for i := headerIdx + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			break
		}
		if strings.HasPrefix(trimmed, "Legend:") {
			break
		}
		fields := strings.Fields(trimmed)
		if len(fields) == 0 {
			break
		}
		rowIdx, ok := isGPULabel(fields[0])
		if !ok {
			// NIC row (e.g. "NIC0" / "mlx5_0") or unknown; skip but don't crash.
			continue
		}
		if rowIdx < 0 || rowIdx >= numGPU {
			continue
		}
		seen[rowIdx] = true

		// Cells align to header columns: fields[1..] correspond to header[0..].
		cells := fields[1:]
		for k := range header {
			gpuIdx := colGPU[k]
			if gpuIdx < 0 {
				continue // NIC column or affinity column handled below
			}
			if k >= len(cells) {
				continue
			}
			cls, bonds := ClassOf(cells[k])
			topo.Matrix[rowIdx][gpuIdx] = Link{
				Raw:         cells[k],
				Class:       cls,
				NVLinkBonds: bonds,
			}
		}

		// Affinity columns: the columns after all device (GPU/NIC) columns are
		// "CPU Affinity" and "NUMA Affinity". Device columns count = number of
		// non-affinity labels in header. We locate the CPU/NUMA affinity by
		// finding the first header index where the label is an affinity keyword.
		cpuAff, numaNode := extractAffinity(header, cells)
		topo.GPUs[rowIdx].CPUAffinity = cpuAff
		topo.GPUs[rowIdx].NUMANode = numaNode
	}

	// Build NUMA groups from parsed GPUs.
	for _, g := range topo.GPUs {
		if !seen[g.Index] {
			continue
		}
		if g.NUMANode >= 0 {
			topo.NUMAGroups[g.NUMANode] = append(topo.NUMAGroups[g.NUMANode], g.Index)
		}
	}
	for k := range topo.NUMAGroups {
		sort.Ints(topo.NUMAGroups[k])
	}
	topo.GPUs = seenGPUs(topo.GPUs, seen)

	topo.FastCliques = computeFastCliques(topo.Matrix)
	topo.NVLinkCliques = cloneCliques(topo.FastCliques)
	return topo, nil
}

func seenGPUs(gpus []GPU, seen []bool) []GPU {
	out := make([]GPU, 0, len(gpus))
	for i, g := range gpus {
		if i < len(seen) && seen[i] {
			out = append(out, g)
		}
	}
	return out
}

// extractAffinity pulls CPU affinity string and NUMA node from the trailing
// columns of a data row, using the header to know how many device columns
// precede the affinity columns.
func extractAffinity(header, cells []string) (cpuAff string, numaNode int) {
	numaNode = -1
	// Count device columns (GPU# / NIC# / mlx*) in header.
	deviceCols := 0
	for _, label := range header {
		if isDeviceLabel(label) {
			deviceCols++
		}
	}
	// Trailing cells after device columns are affinity info.
	if deviceCols >= len(cells) {
		return "", -1
	}
	trailing := cells[deviceCols:]
	if len(trailing) >= 1 {
		cpuAff = trailing[0] // CPU Affinity is the first trailing column (a range)
	}
	// NUMA Affinity is the first pure-integer trailing column after CPU affinity.
	// (Later columns like "GPU NUMA ID" may be "N/A"; CPU affinity is a range.)
	for _, t := range trailing[1:] {
		if n, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
			numaNode = n
			break
		}
	}
	return cpuAff, numaNode
}

func isDeviceLabel(label string) bool {
	if _, ok := isGPULabel(label); ok {
		return true
	}
	if strings.HasPrefix(label, "NIC") {
		return true
	}
	if strings.HasPrefix(label, "mlx") {
		return true
	}
	return false
}

func looksLikeHeader(fields []string) bool {
	if len(fields) == 0 {
		return false
	}
	gpuCount := 0
	for _, f := range fields {
		if _, ok := isGPULabel(f); ok {
			gpuCount++
		}
	}
	// A header lists at least one GPU column and its first field is a device
	// label (not a cell token).
	return gpuCount >= 1 && isDeviceLabel(fields[0])
}

func startsWithWhitespace(line string) bool {
	if line == "" {
		return false
	}
	return line[0] == ' ' || line[0] == '\t'
}

// computeFastCliques finds maximal sets of GPUs where every pair has the
// fastest known accelerator interconnect class.
func computeFastCliques(matrix [][]Link) [][]int {
	n := len(matrix)
	// Build adjacency: edge iff Matrix[i][j].Class == LinkNVLink (both dirs).
	adj := make([]map[int]bool, n)
	for i := range adj {
		adj[i] = map[int]bool{}
	}
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			if i == j {
				continue
			}
			if j < len(matrix[i]) && matrix[i][j].Class == LinkNVLink {
				adj[i][j] = true
			}
		}
	}

	var cliques [][]int
	// Bron–Kerbosch (without pivoting; graphs are tiny).
	var bk func(r, p, x map[int]bool)
	bk = func(r, p, x map[int]bool) {
		if len(p) == 0 && len(x) == 0 {
			if len(r) >= 2 {
				clique := make([]int, 0, len(r))
				for v := range r {
					clique = append(clique, v)
				}
				sort.Ints(clique)
				cliques = append(cliques, clique)
			}
			return
		}
		// Iterate over a snapshot of P.
		pv := make([]int, 0, len(p))
		for v := range p {
			pv = append(pv, v)
		}
		for _, v := range pv {
			if !p[v] {
				continue
			}
			nr := copySet(r)
			nr[v] = true
			np := intersect(p, adj[v])
			nx := intersect(x, adj[v])
			bk(nr, np, nx)
			delete(p, v)
			x[v] = true
		}
	}

	p := map[int]bool{}
	for i := 0; i < n; i++ {
		p[i] = true
	}
	bk(map[int]bool{}, p, map[int]bool{})

	// Deterministic ordering: sort cliques by their first (smallest) member.
	sort.Slice(cliques, func(a, b int) bool {
		return cliques[a][0] < cliques[b][0]
	})
	return cliques
}

func copySet(s map[int]bool) map[int]bool {
	c := make(map[int]bool, len(s))
	for k, v := range s {
		if v {
			c[k] = true
		}
	}
	return c
}

func intersect(a map[int]bool, b map[int]bool) map[int]bool {
	out := map[int]bool{}
	for k := range a {
		if a[k] && b[k] {
			out[k] = true
		}
	}
	return out
}
