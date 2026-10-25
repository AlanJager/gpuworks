package store

import "encoding/json"

// legacyChatOperationType is the operation type migration 80 wrote, spelled as
// that migration spelled it. It is a historical literal, not a reference to
// today's vocabulary: inferenceapi depends on this package, so the constant
// cannot be imported from there, and a migration must keep describing what was
// actually written at the time whatever the vocabulary becomes later.
const legacyChatOperationType = "openai.chat"

// IsDefaultChatOperations reports whether a model's stored operations are a
// bare chat contract that nobody chose: a single chat operation carrying
// nothing but its type.
//
// Two writers produce it. Migration 80 gave this value to every model that
// predates template API declarations, and model registration still writes it
// when the metadata infers a text-generation task (api's
// initRegisteredModelContract and modelDir.model marshal the inferred
// operations without normalizing them, and Operation.Inputs has no omitempty,
// so the chat case serializes as {"inputs":null,"type":"openai.chat"}).
// Neither is a decision: an inference template that declares its own API
// knows better than either, and may replace it.
//
// A contract saved through the model API is not this value. That path
// normalizes first (inferenceapi.NormalizeContract), which fills in the
// endpoint and the mode, so an explicit chat-only choice is distinguishable
// from an inferred one.
//
// The judgement is structural rather than a comparison against a stored
// spelling: a literal comparison would silently stop matching the day the
// serialized field order or an omitempty tag changes, leaving the repair to
// do nothing with no test able to tell.
func IsDefaultChatOperations(raw string) bool {
	var operations []map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &operations) != nil || len(operations) != 1 {
		return false
	}
	operation := operations[0]
	for key, value := range operation {
		switch key {
		case "type":
		case "inputs":
			// Migration 80's value carries no inputs at all; a writer that
			// marshals an unnormalized operation emits the key as null.
			// Anything else is a real declaration.
			if string(value) != "null" {
				return false
			}
		default:
			return false
		}
	}
	value, ok := operation["type"]
	if !ok {
		return false
	}
	var operationType string
	return json.Unmarshal(value, &operationType) == nil && operationType == legacyChatOperationType
}
