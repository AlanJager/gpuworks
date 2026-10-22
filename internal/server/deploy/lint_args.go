// lint_args.go asks of an argv the platform did not write - a catalog spec's
// args - the one question the dictionary can answer on its own: does every flag
// a dictionary key owns carry a value that key can hold?
//
// ReverseArgs answers by omission. It is a reader, so a value it cannot decode
// stays in the residual and reaches the engine verbatim, which is the right
// answer for a running deployment and the wrong one for an import: a spec that
// writes --max-model-len abc has a typo the author should hear about, not a
// passthrough token. Both answers come out of the same walk in reverse_args.go,
// which is why the importer can never name a flag the reader would have read
// successfully, or miss one the reader had to give up on.

package deploy

import (
	"fmt"
	"slices"
	"strings"

	enginenames "gpuworks.ai/gpuworks/internal/engines"
)

// ArgProblem is one argv token that names a dictionary parameter and states a
// value that parameter cannot hold. Value is empty when the flag was the last
// token and carried no value at all.
type ArgProblem struct {
	Flag   string
	Value  string
	Key    string
	Reason string
}

// String renders a problem as the phrase an import error embeds.
func (p ArgProblem) String() string {
	if p.Value == "" {
		return fmt.Sprintf("%s (%s) %s", p.Flag, p.Key, p.Reason)
	}
	return fmt.Sprintf("%s (%s) value %q %s", p.Flag, p.Key, p.Value, p.Reason)
}

// LintArgs reports the tokens of an already-tokenized engine argv that name a
// dictionary parameter and cannot be decoded into it.
//
// What it deliberately stays quiet about is as much of the contract as what it
// reports, because each silence is a case where the dictionary has no opinion:
//
//   - a flag no key owns is the author's to spell however the engine spells it;
//   - a flag several keys share (vLLM's --speculative-config, which carries
//     three keys, and --kv-transfer-config, which carries four) cannot be
//     attributed to one key, so no key's type applies to its value;
//   - a boolean written as a bare switch states its own value;
//   - an engine with no parameter dictionary (callable, whose launch argv
//     belongs to a catalog-owned adapter) has no key to judge against.
//
// The first three are silences of the shared walk. The last one is this
// function's own: reverseArgs reads any registered engine against the vLLM
// column, which is harmless for a reader asked about an engine it knows and
// meaningless as a judgement, so an engine outside the dictionary is refused a
// verdict here rather than given a wrong one.
func LintArgs(engine string, args []string) []ArgProblem {
	selected, err := SelectEngine(engine)
	if err != nil || !slices.Contains(enginenames.DialectNames(), selected.Name()) {
		return nil
	}
	_, _, _, problems, err := reverseArgs(selected.Name(), args)
	if err != nil {
		return nil
	}
	return problems
}

// undecodableReason says what the key would have accepted. An enum names its
// options because the spelling is the whole of the author's mistake.
func undecodableReason(spec engineParam) string {
	switch spec.Type {
	case "boolean":
		return "is not a boolean"
	case "integer":
		return "is not an integer"
	case "number":
		return "is not a number"
	case "json":
		return "is not valid JSON"
	}
	if spec.Type == "enum" && len(spec.Enum) > 0 {
		return "is not one of " + strings.Join(spec.Enum, ", ")
	}
	return "cannot be read as " + spec.Type
}
