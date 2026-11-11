# ADR 0062: Template components follow the primary model into its shared storage

Date: 2026-11-11 · Status: Accepted (supplements decision 4 of 0034)

## Background

An inference template can consist of several components: besides the primary model, a separately published text encoder, a speculative-decoding draft model, and so on. ADR 0034 decided that shared storage is never written automatically. Only when a deployment explicitly selects a shared storage under "Save to" does the create start one download into it, written by one node. Without a selection, a node that cannot read the model downloads it into the default storage, or into local storage when the default storage is shared.

When the primary model is already on a shared storage, the deployment form no longer asks for "Save to", so the request carries none. A template component that exists nowhere yet was then downloaded by every node into its local disk: a 66.7 GB text encoder takes over an hour on one node, and with several nodes or replicas each downloads its own copy, defeating the shared storage. In addition, even with "Save to" selected, the create started that download only for the primary model; components never entered the shared storage.

## Decision

1. When a deployment is created, every non-primary component of the template goes through the same storage validation and admission as the primary model. With "Save to" selected, the components are downloaded into the selected storage by one node, together with the primary model.
2. Without "Save to", when the primary model is already on a shared storage that covers all target nodes and accepts new files, the create downloads each component no target node can read into that storage, again written by one node. When several storages qualify, the default storage comes first, then storage ID order.
3. The deployment still records only the storage the request selected (`deployments.weight_storage_id` stays 0), and evaluation, preview and create plan against that same record. Decision 2 is a one-time action at create; it does not derive the deployment's "Save to" from where a model happens to be.
4. When the deployment has no "Save to", `store.ModelSourceOn` makes a node wait for any shared storage it can read that is being written with this model, instead of fetching another copy by the default order. With "Save to" selected (local storage included), the selection still applies. After a failed write, the usual order resumes.

## Consequences

- Shared storage is written automatically only in one case: the primary model is on shared storage, a component exists nowhere, and the request selected no "Save to". Everything else is as in ADR 0034.
- A download under way in a shared storage, whoever started it, makes deployments without "Save to" wait for it instead of each downloading. If the writer stalls, those deployments fall back to local acquisition only after the write fails.
- Component metadata checks join admission at create, so a create fails at once when a component's source cannot be read, instead of getting stuck at start.
- Not done: applying the same rule to the primary model (it has no existing shared copy to follow), and per-component acquisition forecasts in the deployment form.
