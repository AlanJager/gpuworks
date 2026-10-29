package experience

import (
	"encoding/json"
	"strings"
)

// tokenizerWords is the vocabulary of the simulated checkpoints' tokenizer:
// one common word per token, so a benchmark's sampled prompts read as text
// and the engine's word count matches the tokens the client planned.
const tokenizerWords = `the of and to in is that for it as with was on be by at this from or an are
have not but they which one you were all we can her has there been if more when will would who
so no out up into than them only some could time these two may then do first any my now such like
other over even most made after also did many before must through back years where much your way
well down should because each just those people how too little state good very make world still own
see men work long get here between both life being under never day same another know while last might
us great old year off come since against go came right used take three model data system service compute
cluster node memory network storage request token latency throughput batch cache prefill decode inference
deploy schedule replica engine image driver kernel tensor parallel pipeline expert weight context prompt
answer question summary report plan design review test release version update config policy metric
monitor alert trace log user team product market customer price cost value quality speed scale`

func tokenizerJSON() []byte {
	vocab := map[string]int{}
	for i, w := range strings.Fields(tokenizerWords) {
		if _, ok := vocab[w]; !ok {
			vocab[w] = i
		}
	}
	raw, _ := json.Marshal(map[string]any{"version": "1.0", "model": map[string]any{"type": "WordLevel", "vocab": vocab}})
	return raw
}
