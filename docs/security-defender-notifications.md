<!--
SPDX-FileCopyrightText: 2026 Deutsche Telekom AG
SPDX-License-Identifier: Apache-2.0
-->

# Session request notification privacy

Request emails go only to approvers of the first ready escalation matching the
requested group. Other eligible escalations do not contribute recipients.

`notificationExclusions.groups` always denotes groups, even when a group name
looks like an email address. If an exclusion or hidden group's membership cannot
be resolved and no successful request-time membership snapshot is available, all
request notifications are suppressed. Session creation and approval remain
available. Successfully resolved empty groups are distinct from unavailable
membership. Complete request-time membership is retained for filtering even when
notification recipient limits truncate the candidate list.

`approvers.hiddenFromUI` removes hidden group names from email content as well as
removing their members from recipients. An item explicitly listed in
`approvers.users` can be hidden directly without group resolution, unless it is
also a configured or request-resolved group. Ambiguous items are resolved as
groups; failed resolution suppresses notifications. Exclusions apply to members
even when those members also appear as explicit users or in visible groups.

These rules apply to session request emails and do not change approval rights.

Approver provider restrictions use `allowedIdentityProvidersForApprovers`, falling
back to legacy `allowedIdentityProviders` only when the role-specific list is
empty. Restricted groups use `status.idpGroupMemberships` for those providers;
the aggregate membership map and default provider resolver cannot add recipients.
Until every allowed provider has a resolved snapshot for a group, that group
contributes no recipients. A successfully resolved empty group stays empty.

The same provider scope applies to exclusion-only and hidden-only groups. Their
request snapshot or complete allowed-provider status snapshot must be known;
otherwise all request emails are suppressed. A successful empty lookup from the
default provider cannot establish that a restricted-provider group is empty.
Explicitly configured hidden users still require no group lookup.
