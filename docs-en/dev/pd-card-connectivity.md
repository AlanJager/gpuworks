# Accelerator card connectivity in PD evaluation

An accelerator compute network declares which fabric a deployment uses; it
does not guarantee that every card port reaches every other card port. In
particular, matching card indexes do not establish reachability.

Evaluation and creation share a card-pair check for cross-node PD modes
that use HCCL for KV transfer. Each network separately retains its 10 newest
completed card tests, with card pings and full card matrices sharing that limit.
For each pair, the check uses the newest valid result in those tests. Host tests
do not count toward the card-test limit.
The result must be within 24 hours and match the two nodes, ports, and
current RoCE or HCCS addresses. A narrower test, tool failure, or retest
in progress leaves the pair's last valid result in effect.
A card ping includes a reply, so the card tester stores
one round-trip observation per unordered node pair; either stored orientation
is accepted. Host TCP and host RDMA tests are not
accelerator-port evidence.

| Code | Type | Meaning |
| --- | --- | --- |
| `PD_CARD_NETWORK_BLOCKED` | Error | Prefill and decode each have one single-node instance with one pinned card, and the measured cross-node pair is unreachable. Evaluation returns `ok: false` and creation returns HTTP 400. |
| `PD_CARD_NETWORK_UNREACHABLE` | Warning | A measured pair is unreachable in another layout, such as one with multiple cards, multiple instances, or followers. |
| `PD_CARD_NETWORK_UNTESTED` | Warning | The selection lacks complete valid evidence, for example because cards are selected automatically, the pair has no valid result, the addresses changed, or the test is older than 24 hours. |

The messages include the network, selected port pairs, and up to eight
alternatives that are reachable according to their latest valid results.
Placement checks the cards' availability separately. Warnings allow submission.
Verify KV handoff after the engines start. Pin the intended cards and run a
card matrix test before deployment. Connectivity evidence cannot diagnose every local Mooncake
initialization failure, and does not replace engine startup validation.

<!-- translation-source-sha256: 7aa3b1741f7dffff78f5ab708066ca7143e32b4262cea0a6f2e7770ea59b9f66 # Accelerator card connectivity in PD evaluation -->
