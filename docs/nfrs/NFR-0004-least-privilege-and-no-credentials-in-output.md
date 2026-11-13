# NFR-0004. Least-privilege RBAC, an audit trail, and no credential outside its Secret

## Status

Implemented

## Attribute

Security (access control, audit, secrets, plan rows EB-2, EB-4 and EB-9).

## Description

- The manager's ClusterRole lists exact resources and verbs: the group's kinds,
  their `status` and `finalizers`, `namespaces` (get, list, watch),
  `configmaps` (get), `secrets` and `events`; no wildcard. The manager's Secret
  cache holds only the Secrets labelled `app.kubernetes.io/managed-by`; the
  admin Secrets and CA bundles of stores are read from the API server on
  demand. A cluster store's admin Secret is read from the operator namespace
  only (`server.Resolve`); a namespaced store's from its own namespace, which is
  why the `secrets` rule is a ClusterRole rule. **For the user to review:** the
  `secrets` rule grants create, update, patch and delete on every Secret the
  cache cannot narrow by namespace, because namespaced stores keep their admin
  Secret in tenant namespaces.
- Each state change is an Event on the claim and a structured JSON log line (who,
  what, when, the store and the names of the changed fields, never their
  values), written by `lifecycle.Reconcile`.
- Admin credentials, service-account secret keys and proxy URLs never appear in
  Events, logs, conditions or status. A secret key is generated per issue
  (`crypto/rand`, 40 characters, the longest RustFS accepts) and reaches only
  the binding Secret. The IAM user's own secret key is generated, sent once and
  discarded: the keys of the binding are service accounts of the user, so
  nothing holds a login for the user itself. The request and response bodies
  of the admin API carry secret keys, so they are never logged; the log line
  of a refused request holds the start of the answer's body only.
- A rotation is two-phase: the next service account is issued, the binding
  Secret is written, and the previous account is deleted when the grace period
  ends (`gracePeriod`, one hour by default).

## Source

Plan rows EB-2, EB-4 and EB-9; [ADR 0002 of lib-operator-go](https://github.com/craftainer-labs/lib-operator-go/blob/main/docs/adrs/0002-operator-crd-conventions.md), sections 8, 11 and 12.

## Verification

`config/rbac/role.yaml` is generated from the markers in `internal/app/app.go`
and checked for drift by the `generated-current` hook (the chart's copy by
`scripts/check-chart-sync.sh`). `TestAccessLifecycleAndRotation` asserts that
no Event or condition holds a secret key and that a rotation keeps the
previous key valid until its grace period ends; `TestBucketOnAnInvalidStore`
asserts that no Event or condition holds a proxy credential;
`TestAccessRotationSteps` checks that a retried rotation issues the same key.
