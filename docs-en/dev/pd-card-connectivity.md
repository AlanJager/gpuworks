# Accelerator card connectivity in PD evaluation

An accelerator compute network declares which fabric a deployment uses; it
does not guarantee that every card port reaches every other card port. In
particular, matching card indexes do not establish reachability.

Evaluation and creation share a card-pair check for cross-node PD modes
that use HCCL for KV transfer. For each pair, the check uses the newest
valid result in the selected network's retained, completed card tests.
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

<!-- translation-source-sha256: 7ddf8d3cfd79f4920eeabe639286c0a3f91f983a0a12d415a792d5f72c4b22ec # Accelerator card connectivity in PD evaluation -->
