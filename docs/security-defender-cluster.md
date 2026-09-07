# Security hardening: cluster and identity paths

These checks protect cluster credentials and escalation policy updates.

## OIDC redirects

OIDC discovery and token grant requests use a client that refuses HTTP redirects. This prevents a 307 or 308 response from replaying a refresh token or client secret to another origin. The discovered token endpoint is still required to match the issuer origin before a grant is attempted.

## OIDC Secret rotation

When a `ClusterConfig` inherits OIDC settings from an `IdentityProvider` and omits an explicit client secret, the controller may use the IdentityProvider's Keycloak service-account Secret as fallback credentials. That Secret is included in the cache dependency index, so updates and deletes evict cached REST configuration and token state. Eviction is local cache invalidation; it does not revoke tokens at the identity provider.

## Issuer binding

An explicit `IdentityProvider.spec.issuer` is authoritative. Authority fallback is used only when the explicit issuer is empty, so a signed token whose issuer differs from the configured issuer cannot select that provider through the fallback path.

## Escalation readiness

An escalation is ready only when its `Ready=True` condition was observed for the current object generation. A prior successful status cannot authorize a changed specification while the new validation status is pending or failed. Kubernetes assigns a positive generation when an escalation is created, so a newly created escalation must complete validation before it becomes available.
