---
section: Fixed
---

- A session request for a cluster pattern (for example `*`) or for a cluster
  without a registered `ClusterConfig` is now rejected with `422`. Before, an
  escalation that allows `*` accepted a session for the literal cluster `*`,
  which could never grant access. The escalation list now returns
  `clusterOptions` with the registered clusters that a pattern matches, and
  the UI shows a cluster picker for such escalations.
