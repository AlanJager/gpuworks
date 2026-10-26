# Observability Limits of Per-Request Engine Queue Time

A monitored request's `engine_queue_ms` must come from timing reported in that same inference response.
Queue counts in `/metrics` are instantaneous gauges, and service histograms contain aggregate samples.
Neither identifies an individual request's queue duration. Missing fields and JSON `null` remain unknown;
only an explicit numeric `0` means measured zero. If only queue time is reported, retain that segment and
leave the remainder as waiting rather than inventing zero Prefill time.

GWKS-93 checked the built-in Strata image version 0.1.38. The pinned upstream source,
[`09817da3979abfeb5fbdef7c8f1223d120c1ad39/serve/server.py`](https://github.com/Niko1221/Strata/blob/09817da3979abfeb5fbdef7c8f1223d120c1ad39/serve/server.py),
shows that:

- `request_timings` (line 1624) returns known stages such as prompt/decode; standard responses omit queue time.
- Optional `begin_request` (line 1232) and `/api/requests` (line 2147) retain internal `queue_s`, but the internal ID
  is generated independently at line 1237. The OpenAI completion ID is generated separately at line 1751, without a verifiable correlation.
- `api_monitor` is disabled by default. Enabling it retains input/output bodies for the latest 100 requests (line 970), so the platform should not enable it automatically.

Current Strata responses can therefore provide Prefill and Decode timing, but cannot reliably recover an individual
request's 20.3 seconds of queueing. Completing this requires queue timing in the same response or a supported stable
request correlation; matching by timestamp or body is insufficient. vLLM's `--enable-per-request-metrics`, available
since 0.25.0, has separate existing support, and the platform reads its reported fields.
For field declarations and measurement semantics, see [Inference Metric Vocabulary](../reference/inference-metrics.md#per-request-statistics).
