---
section: Fixed
---

- The periodic AuditConfig resync (every 10 minutes) no longer rebuilds the
  audit pipeline when the AuditConfigs and the Secrets their sinks read are
  unchanged. Before, every resync stopped the running pipeline, and events it
  was still retrying during a sink outage were dropped with reason
  `shutdown`. A real change still rebuilds the pipeline; the replaced pipeline
  now finishes the events it holds, bounded by its `queue.retryTimeoutSeconds`,
  before it closes. New value
  `breakglass_audit_config_reloads_total{status="unchanged"}`.
- `AuditConfig` `status.eventsDropped` now includes drops in the per-sink
  queues (for example `retry_exhausted`), not only in the shared ingress
  queue. The counters describe the current pipeline and restart at zero when
  it is rebuilt.
