package enginelog

import (
	"math"
	"os"
	"strings"
	"testing"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// sglangNextNStartup is a production SGLang startup, verbatim from the
// container's first line to "The server is fired up": GLM with NextN speculative
// decoding over TP=4, hybrid (Mamba) layers, and progress bars written with
// carriage returns.
func sglangNextNStartup(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("testdata/sglang-nextn-tp4.log")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// One GPU's share: rank 0 alone, with the draft model's weights, KV pool and
// graphs added to the target model's, and the Mamba state counted with the
// cache. The four ranks print the same figures; none is counted twice.
func TestParseSGLangSumsOneRankOfASpeculativeStartup(t *testing.T) {
	m := ParseDialect(DialectSGLang, sglangNextNStartup(t))
	want := Measured{
		ModelWeightsGiB: 44.79 + 3.94,
		KVCacheGiB:      19.85 + 1.80 + 0.28 + 8.03 + 1.23 + 0.04,
		KVCacheTokens:   2936064,
		GraphCaptureGiB: 0.86 + 0.21 + 0.17,
		Found:           true,
	}
	if !approx(m.ModelWeightsGiB, want.ModelWeightsGiB) || !approx(m.KVCacheGiB, want.KVCacheGiB) ||
		!approx(m.GraphCaptureGiB, want.GraphCaptureGiB) || m.KVCacheTokens != want.KVCacheTokens || !m.Found {
		t.Fatalf("ParseDialect = %+v, want %+v", m, want)
	}
}

// The agent reads a bounded tail. Whatever the tail has lost of the summary,
// a memory figure is either the whole sum or absent, never a part that reads
// as whole; the token count, a single figure, survives as long as its line.
func TestParseSGLangTailNeverGivesAPartialSum(t *testing.T) {
	full := ParseDialect(DialectSGLang, sglangNextNStartup(t))
	lines := strings.Split(sglangNextNStartup(t), "\n")
	serving := strings.Repeat("[2026-10-24 02:17:42 TP0 EP0] Decode batch, #running-req: 1, #token: 2048, gen throughput (token/s): 80.00\n", 50)
	for cut := range lines {
		m := ParseDialect(DialectSGLang, strings.Join(lines[cut:], "\n")+"\n"+serving)
		for name, pair := range map[string][2]float64{
			"weights": {m.ModelWeightsGiB, full.ModelWeightsGiB},
			"kv":      {m.KVCacheGiB, full.KVCacheGiB},
			"graph":   {m.GraphCaptureGiB, full.GraphCaptureGiB},
		} {
			if pair[0] != 0 && !approx(pair[0], pair[1]) {
				t.Fatalf("tail from line %d: %s = %v, want %v or absent", cut, name, pair[0], pair[1])
			}
		}
		if m.KVCacheTokens != 0 && m.KVCacheTokens != full.KVCacheTokens {
			t.Fatalf("tail from line %d: tokens = %d", cut, m.KVCacheTokens)
		}
		if m.Found != (m.KVCacheTokens != 0 || m.ModelWeightsGiB != 0) {
			t.Fatalf("tail from line %d: Found = %v for %+v", cut, m.Found, m)
		}
	}
	if m := ParseDialect(DialectSGLang, serving); m != (Measured{}) {
		t.Fatalf("serving lines alone = %+v, want nothing", m)
	}
}

// Under the platform's logging config SGLang stamps a process id instead of
// rank tags, and older releases print K and V sizes apart and a single graph
// capture: one process is read, its K and V added.
func TestParseSGLangProcessStampsAndSplitKV(t *testing.T) {
	log := `[2026-09-19 10:29:50 4200] INFO server_args=ServerArgs(model_path='/model', tp_size=2)
[2026-09-19 10:30:01 4242] INFO Load weight begin. avail mem=78.50 GB
[2026-09-19 10:30:01 4243] INFO Load weight begin. avail mem=78.50 GB
[2026-09-19 10:30:30 4243] INFO Load weight end. type=Qwen2ForCausalLM, dtype=torch.bfloat16, avail mem=70.80 GB, mem usage=7.70 GB.
[2026-09-19 10:30:31 4242] INFO Load weight end. type=Qwen2ForCausalLM, dtype=torch.bfloat16, avail mem=70.80 GB, mem usage=7.70 GB.
[2026-09-19 10:30:32 4242] INFO KV Cache is allocated. #tokens: 1,048,576, K size: 28.00 GB, V size: 28.00 GB
[2026-09-19 10:30:32 4243] INFO KV Cache is allocated. #tokens: 1,048,576, K size: 28.00 GB, V size: 28.00 GB
[2026-09-19 10:30:40 4242] INFO Capture cuda graph end. Time elapsed: 7.81 s. mem usage=0.62 GB. avail mem=13.90 GB.
[2026-09-19 10:30:40 4243] INFO Capture cuda graph end. Time elapsed: 7.90 s. mem usage=0.62 GB. avail mem=13.90 GB.`
	m := ParseDialect(DialectSGLang, log)
	if !approx(m.ModelWeightsGiB, 7.70) || !approx(m.KVCacheGiB, 56.00) || !approx(m.GraphCaptureGiB, 0.62) || m.KVCacheTokens != 1048576 || !m.Found {
		t.Fatalf("ParseDialect = %+v", m)
	}
}

// A single-GPU engine stamps neither ranks nor a process.
func TestParseSGLangSingleGPU(t *testing.T) {
	log := `[2026-10-01 07:59:51] server_args=ServerArgs(model_path='/model', tp_size=1)
[2026-10-01 08:00:00] Load weight begin. avail mem=23.30 GB
[2026-10-01 08:00:09] Load weight end. type=LlamaForCausalLM, dtype=torch.bfloat16, avail mem=8.20 GB, mem usage=15.10 GB.
[2026-10-01 08:00:10] KV Cache is allocated. #tokens: 52,000, KV size: 6.35 GiB
[2026-10-01 08:00:16] Capture cuda graph end. Time elapsed: 5.40 s. mem usage=0.40 GB. avail mem=1.10 GB.`
	m := ParseDialect(DialectSGLang, log)
	if !approx(m.ModelWeightsGiB, 15.10) || !approx(m.KVCacheGiB, 6.35) || !approx(m.GraphCaptureGiB, 0.40) || m.KVCacheTokens != 52000 {
		t.Fatalf("ParseDialect = %+v", m)
	}
}

// A node past the first of a multi-node engine holds only nonzero ranks; its
// lowest rank is read, once.
func TestParseSGLangLaterNodeReadsItsLowestRank(t *testing.T) {
	log := `[2026-10-02 09:00:00] server_args=ServerArgs(model_path='/model', tp_size=4, nnodes=2, node_rank=1)
[2026-10-02 09:00:30 TP3] Load weight end. type=Qwen3MoeForCausalLM, dtype=torch.bfloat16, avail mem=40.00 GB, mem usage=30.00 GB.
[2026-10-02 09:00:30 TP2] Load weight end. type=Qwen3MoeForCausalLM, dtype=torch.bfloat16, avail mem=40.00 GB, mem usage=30.00 GB.
[2026-10-02 09:00:31 TP2] KV Cache is allocated. #tokens: 400000, K size: 12.00 GB, V size: 12.00 GB
[2026-10-02 09:00:31 TP3] KV Cache is allocated. #tokens: 400000, K size: 12.00 GB, V size: 12.00 GB`
	m := ParseDialect(DialectSGLang, log)
	if !approx(m.ModelWeightsGiB, 30.00) || !approx(m.KVCacheGiB, 24.00) || m.KVCacheTokens != 400000 {
		t.Fatalf("ParseDialect = %+v", m)
	}
}
