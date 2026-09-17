# Plan: Satisfy Remote Revocation Lease MFA for SSO Users with Trusted IdP MFA

**Issue**: LanternOps/breeze#5665 ("Connection Error revoked:mfa_required Relaunch it from the Breeze dashboard")  
**Branch**: `fix/revocation-lease-sso-trusted-mfa`  
**Date**: 2026-09-16  

## Context & Root Cause

1. In Breeze 0.111+, live desktop sessions maintain a fail-closed revocation lease (`apps/api/src/services/remoteRevocationLease.ts`). The agent renews the lease periodically (every 25s), which triggers `loadRevocationRecheckRow()` and `evaluateRevocationRecheck()`.
2. Upstream migration `2026-10-11-170000-partner-admin-force-mfa-reconcile.sql` sets `force_mfa = true` on the system `Partner Admin` role.
3. In `evaluateRevocationRecheck()`:
   ```typescript
   if (forceMfa && !user.mfaProtected) {
     return { ok: false, reason: 'mfa_required' };
   }
   ```
4. In `loadRevocationRecheckRow()`, `mfaProtected` was derived strictly as:
   ```typescript
   mfaProtected: found.userMfaEnabled === true || found.userHasPasskey === true,
   ```
5. When a user logs in via Single Sign-On (SSO) with `trusts_idp_mfa = true` (e.g. PocketID, Entra ID, Okta), their web session is trusted for MFA. However, because they have not configured a separate TOTP secret or registered a WebAuthn passkey directly in Breeze, `users.mfa_enabled` remains `false`.
6. As a result, 7–25 seconds into a remote desktop session, the background revocation lease detects `forceMfa === true` and `mfaProtected === false`, abruptly terminating the connection with `revoked:mfa_required`.

## Proposed Solution

1. **Schema Imports**:
   Import `ssoProviders` and `userSsoIdentities` in `apps/api/src/services/remoteRevocationLease.ts`.
2. **Recheck Query Extension**:
   In `loadRevocationRecheckRow()`, add `userHasTrustedIdpMfa` subquery:
   ```typescript
   userHasTrustedIdpMfa: sql<boolean>`EXISTS (
     SELECT 1 FROM ${userSsoIdentities}
     JOIN ${ssoProviders} ON ${userSsoIdentities.providerId} = ${ssoProviders.id}
     WHERE ${userSsoIdentities.userId} = ${users.id}
       AND ${ssoProviders.status} = 'active'
       AND ${ssoProviders.trustsIdpMfa} = true
       AND (
         ${ssoProviders.partnerId} = ${users.partnerId}
         OR (${users.orgId} IS NOT NULL AND ${ssoProviders.orgId} = ${users.orgId})
       )
   )`,
   ```
3. **MFA Protected Evaluation**:
   ```typescript
   mfaProtected:
     found.userMfaEnabled === true ||
     found.userHasPasskey === true ||
     found.userHasTrustedIdpMfa === true,
   ```
4. **Testing**:
   - Add unit tests in `apps/api/src/services/remoteRevocationLease.test.ts`.
   - Verify integration behavior against live database.
   - Deploy container image to staging LXC 129 and verify on hardware `LEGION9i`.
