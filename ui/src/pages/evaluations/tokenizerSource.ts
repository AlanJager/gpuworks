export type TokenizerSource = "auto" | "estimate" | "huggingface" | "modelscope" | "upload";

export type TokenizerFormState = {
	source: TokenizerSource;
	repository?: string;
	reference?: string;
};

// tokenizerFormState restores the source a rerun starts from. A run stores only
// the reference it resolved to, so the source is derived back from its shape —
// except on a managed target, where a bare path is a machine-local fact: the
// node that ran the previous run is not necessarily the node this one will use,
// so it is left to be resolved and re-validated again.
export function tokenizerFormState(reference?: string, managedTarget = false): TokenizerFormState {
	const value = reference?.trim();
	if (!value) return { source: "estimate" };
	if (value.startsWith("hf://")) return { source: "huggingface", repository: value.slice("hf://".length), reference: value };
	if (value.startsWith("ms://")) return { source: "modelscope", repository: value.slice("ms://".length), reference: value };
	return managedTarget ? { source: "auto", reference: value } : { source: "upload", reference: value };
}

// tokenizerSourceOrDefault is the source a form starts on: the one a rerun is
// restored to, or the managed-target default for a fresh run. Token-length workloads
// cannot estimate, so a source that would leave the tokenizer unresolved is
// replaced by one that resolves it.
export function tokenizerSourceOrDefault(managedTarget: boolean, tokenLengthControlled: boolean, reference?: string, rerun = true): TokenizerSource {
	const source = rerun ? tokenizerFormState(reference, managedTarget).source : managedTarget ? "auto" : "estimate";
	if (tokenLengthControlled && source === "estimate") return managedTarget ? "auto" : "huggingface";
	return source;
}

export function tokenizerReference(source: TokenizerSource, repository: string | undefined, uploadedReference: string | undefined, schemes: Record<string, string>): string | undefined {
	// auto and estimate carry no reference: the first is resolved on the server,
	// and the second means "no tokenizer at all".
	if (source === "auto" || source === "estimate") return undefined;
	if (source === "upload") return uploadedReference?.trim() || undefined;
	const value = repository?.trim();
	return value ? `${schemes[source]}://${value}` : undefined;
}

export function tokenizerReferenceName(reference?: string): string | undefined {
	const value = reference?.trim().replace(/[\\/]+$/, "");
	return value?.split(/[\\/]/).pop() || undefined;
}
