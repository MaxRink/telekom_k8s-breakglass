<!--
SPDX-FileCopyrightText: 2026 Deutsche Telekom AG
SPDX-License-Identifier: Apache-2.0
-->

# Durable session quota admission

API session requests first create a provisional object to obtain a Kubernetes
UID. The controller records that UID in the shared
`breakglass-session-quota-v1` ConfigMap in the configured controller namespace.
One resource-version compare-and-swap reserves every applicable scope together.
Only then does the API complete admission and send successful creation responses
or request notifications. Provisional objects are visible but cannot grant
breakglass access or progress to debug workloads without admission.

The ledger covers regular-session tuple uniqueness (user, cluster, granted
group), global per-user limits, escalation-UID totals, debug template-UID totals,
binding-UID totals, and binding per-user limits. Regular user identity preserves
the existing canonical `spec.user` quota semantics across escalations and
providers. Debug per-user limits preserve username/email alias matching. Limits
from IDP group overrides use the authenticated group snapshot captured at request
time; policy resources are read from the API server when admission is attempted.
Template concurrency limits span session namespaces and bindings. A binding may
further restrict template concurrency.

Reservations include pending, approval-waiting, scheduled, and active sessions.
They have no process lease or clock expiration. A crashed worker, failed initial
status update, slow workload deployment, or canceled request cannot release its
slot. Retries for the same UID are idempotent; existing reservations are
preserved when limits tighten. A request uses the live policy snapshot read for
its admission attempt; policy changes are not an atomic transaction with
admission already in flight.

Quota denial records a terminal rejection/failure with an optimistic status
write. Cleanup of ledger entries happens lazily during later admissions, only
after an authoritative GET proves that exact UID terminal or deleted. Missing
entries in a list never prove that a reservation is free. Expiry timestamps
alone do not release slots before lifecycle cleanup records terminal state.
Status writes for managed sessions use resource-version fencing so stale
activation cannot revive a terminal session. Spoke resource cleanup remains the
responsibility of the debug lifecycle controller.

The regular cleanup loop completes empty-status provisional admissions after a
crash. Debug reconciliation retries admission before approval-waiting or
activation, including sessions created directly as Kubernetes resources.
Failed API calls can therefore leave recoverable provisional objects; inspect
the session list before submitting a replacement request. Explicit quota denials
are terminal and do not later activate. Nonterminal legacy sessions are counted
conservatively during bootstrap, including resolved bindings on sessions without
an explicit binding reference. Already recorded reservations use their immutable
ledger scopes even if their template or binding is later deleted. A legacy
session whose scopes have never been recorded and whose policy is missing blocks
bootstrap until an operator restores its policy or completes its lifecycle.
Auto-discovery rejects failed, missing, or ambiguous cluster-label data rather
than silently dropping selector-based binding limits.

## Deployment and recovery

Stop old API/controller writers before enabling this version, then restart every
writer with the same configured controller namespace before reopening traffic.
Mixed old/new replicas cannot provide atomic quotas because old writers bypass
the admission protocol. Existing nonterminal sessions remain counted during
migration; an already over-limit population can temporarily prevent admission.
Privileged writers of session spec/status, admission annotations, or ledger
ConfigMaps remain in the controller's trust boundary.

All API and lifecycle roles need ConfigMap get/create/update and session
get/list/patch/status-update permissions. The shipped controller role already
includes these operations; review custom API-only roles. The ledger uses version
1 JSON and refuses unsupported/corrupt data or serialized size above 512 KiB.
It fails closed when storage/read/CAS retries fail. This deliberately bounds
storage; deployments approaching that ceiling need a sharded reservation
protocol before increasing capacity.

Do not delete or edit the ledger to clear quota errors. Process timeout is not
proof that a session stopped. Resolve the owning session through its terminal
lifecycle or delete that exact session with normal cleanup, then retry admission.
Ledger restoration/rebuild requires stopping all writers and accounting for all
nonterminal sessions and provisional reservations before service resumes.
