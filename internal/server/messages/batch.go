package messages

// Batch codes about the models an input file names, when the batch was not
// given one model to send every request to. Both carry the models as a list
// for the client to join in its own language: {models} the first few names,
// {more} how many further ones the server found (the lookups stop at a few
// dozen unknown models, so it may be a lower bound).
const (
	// ErrorBatchFileModelsUnknown: no active deployment serves {models} (and
	// {more} more) that the input file's requests name.
	ErrorBatchFileModelsUnknown = "BATCH_FILE_MODELS_UNKNOWN"
	// WarningBatchFileModelsSleepHeld: the deployments serving {models} (and
	// {more} more) of the input file sleep by an operator's or a schedule's
	// order, which requests do not wake.
	WarningBatchFileModelsSleepHeld = "BATCH_FILE_MODELS_SLEEP_HELD"
)
