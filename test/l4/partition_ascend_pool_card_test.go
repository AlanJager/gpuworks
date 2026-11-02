//go:build cloudgpu && partition

package l4

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// ascendPoolMaxIdleHBMMB is the most HBM an "idle" pool card may already hold.
// A bare 910B4 sits near 2.7 GB with its context resident; the platform case
// asks vLLM for 80% of the 32 GB HBM, so anything much above that cannot fit.
func ascendPoolMaxIdleHBMMB() int {
	if v, err := strconv.Atoi(hostEnv("GPUWORKS_ASCEND_L4_MAX_IDLE_HBM_MB", "")); err == nil && v > 0 {
		return v
	}
	return 4096
}

// ascendIdleCardSelection is a shell fragment that reads $info (npu-smi info)
// and sets $selected to an idle card: the preferred one when idle, otherwise
// the first idle one, empty when none is.
//
// "No running processes found in NPU n" is not enough. npu-smi only lists
// processes it can attribute, and workloads in Kubernetes pods or vNPU slices
// (another tenant's vllm serve on this shared host) hold HBM without appearing
// there. The 2026-10-27 run picked such a card, asked vLLM for 80% of the HBM
// and every replica died with ERR99999 after a ten minute wait. A card also
// has to be under the HBM budget, which is read from the chip row.
func ascendIdleCardSelection(preferred string) string {
	return fmt.Sprintf(`preferred=%s
idle=$(printf '%%s\n' "$info" | awk -v max=%d '
  # NPU row: "| 1     910B4   | OK  | ..."; process rows have a numeric third field.
  /^\| +[0-9]+ +[A-Za-z0-9]+ +\| +[A-Za-z]+ +\|/ { id = $2; next }
  # Chip row follows it: "| 0   | 0000:C2:00.0 | 0  0 / 0   9835 / 32768 |"; HBM is the last used/total pair.
  /^\| +[0-9]+ +\| +[0-9A-Fa-f:.]+ +\|/ && id != "" {
    n = split($0, cell, "|")
    if (match(cell[4], /[0-9]+ *\/ *[0-9]+ *$/)) {
      pair = substr(cell[4], RSTART, RLENGTH); gsub(/ /, "", pair); split(pair, hbm, "/")
      used[id] = hbm[1] + 0
    }
    id = ""; next
  }
  match($0, /No running processes found in NPU [0-9]+/) {
    card = substr($0, RSTART + 34, RLENGTH - 34) + 0
    # An unreadable HBM column keeps the old process-only rule.
    if (!(card in used) || used[card] <= max) print card
  }')
if printf '%%s\n' "$idle" | grep -Fxq "$preferred"; then
  selected=$preferred
else
  selected=$(printf '%%s\n' "$idle" | head -n 1)
fi`, shellQuoteL4(preferred), ascendPoolMaxIdleHBMMB())
}

// npu-smi output of the shared pool host on 2026-10-27: card 0 has a visible
// sglang scheduler, card 1 holds 9.8 GB for a Kubernetes vNPU pod yet reports
// "No running processes", card 2 is genuinely idle.
const ascendNPUSMIShared = `+---------------------------+---------------+----------------------------------------------------------------------+
| NPU   Name                | Health        | Power(W)             Temp(C)                 Hugepages-Usage(page)   |
| Chip                      | Bus-Id        | AICore(%)            Memory-Usage(MB)        HBM-Usage(MB)           |
+===========================+===============+======================================================================+
| 0     910B4               | OK            | 78.1                 32                      0    / 0                |
| 0                         | 0000:C1:00.0  | 0                    0    / 0                30871/ 32768            |
+===========================+===============+======================================================================+
| 1     910B4               | OK            | 78.9                 32                      0    / 0                |
| 0                         | 0000:C2:00.0  | 0                    0    / 0                9835 / 32768            |
+===========================+===============+======================================================================+
| 2     910B4               | Warning       | 81.3                 30                      0    / 0                |
| 0                         | 0000:81:00.0  | 0                    0    / 0                2658 / 32768            |
+===========================+===============+======================================================================+
+---------------------------+---------------+----------------------------------------------------------------------+
| NPU     Chip              | Process id    | Process name       | Process memory(MB)    | Process id in container |
+===========================+===============+======================================================================+
| 0       0                 | 2701582       | sglangschedul      | 28004                 | NA                      |
| 0       0                 | 2696486       | python             | 112                   | NA                      |
+===========================+===============+======================================================================+
| No running processes found in NPU 1                                                                              |
+===========================+===============+======================================================================+
| No running processes found in NPU 2                                                                              |
+===========================+===============+======================================================================+
`

func runIdleCardSelection(t *testing.T, info, preferred string) string {
	t.Helper()
	script := "info=$1\n" + ascendIdleCardSelection(preferred) + "\nprintf '%s' \"$selected\""
	out, err := exec.Command("bash", "-c", script, "bash", info).CombinedOutput()
	if err != nil {
		t.Fatalf("selection script: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestAscendIdleCardSelectionSkipsCardsHoldingHiddenHBM(t *testing.T) {
	t.Run("hidden HBM user is skipped even though it lists no process", func(t *testing.T) {
		// Before the HBM check this returned 1, the failing 2026-10-27 pick.
		if got := runIdleCardSelection(t, ascendNPUSMIShared, "0"); got != "2" {
			t.Fatalf("selected card %q, want 2 (0 runs sglang, 1 holds 9.8 GB of another tenant's HBM)", got)
		}
	})
	t.Run("preferred idle card wins", func(t *testing.T) {
		if got := runIdleCardSelection(t, ascendNPUSMIShared, "2"); got != "2" {
			t.Fatalf("selected card %q, want preferred 2", got)
		}
	})
	t.Run("no idle card selects nothing", func(t *testing.T) {
		busy := strings.Replace(ascendNPUSMIShared, "2658 / 32768", "30000/ 32768", 1)
		if got := runIdleCardSelection(t, busy, "0"); got != "" {
			t.Fatalf("selected card %q, want none", got)
		}
	})
	t.Run("unreadable HBM column keeps the process-only rule", func(t *testing.T) {
		noHBM := strings.NewReplacer("9835 / 32768", "N/A", "2658 / 32768", "N/A").Replace(ascendNPUSMIShared)
		if got := runIdleCardSelection(t, noHBM, "0"); got != "1" {
			t.Fatalf("selected card %q, want 1", got)
		}
	})
}
