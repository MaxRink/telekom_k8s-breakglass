<!--
SPDX-FileCopyrightText: 2026 Deutsche Telekom AG

SPDX-License-Identifier: Apache-2.0
-->

# Small, behavior-preserving simplifications

Prefer direct standard-library calls and expressions when they remove local
machinery without hiding security decisions. In particular:

- Namespace selector membership uses `slices.Contains` for **exact** values;
  `*` remains a literal label value. Do not substitute this for wildcard-aware
  deny-policy matching. Missing keys retain distinct `In` and `NotIn` semantics.
- Keep deny-before-allow guards explicit. Nil label maps can be read without
  first constructing an empty map.
- `GlobMatch` forwards `path.Match` results, retaining its universal-wildcard
  and exact-literal fast paths and invalid-pattern behavior.
- Logger defaults are selected once; the shared build path retains stacktrace
  settings and contextual error wrapping.
- Session enrichment checks existence with `some`, not a filtered collection.
  Backend-provided and stored approval reasons keep precedence; older records
  still receive best-effort enrichment without mutation of input records.

The unused frontend `testButton` and `validateBreakglassRequest` methods and
their method-only tests were removed. No controller endpoint, approval token
validation, CRD, or supported session action was removed. The development mock
server is unchanged.

Focused checks:

```sh
go test ./pkg/utils
cd frontend && npm test
```

Also run the normal repository lint, type-check, and integration checks before
merging. A reduction in source lines is not a measured runtime speedup.
