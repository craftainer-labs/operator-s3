// Package v1alpha1 holds the validating admission webhooks of the
// s3.craftainer.dev API. Defaults and nearly every rule live in the CRD
// schema (OpenAPI and CEL); the webhooks check only what CEL cannot do well:
// that a URL parses and has a host, and that a quantity is at least one.
// Updates of an object that is being deleted are always admitted, so
// finalizer removal is never blocked, for example while the webhook's own pod
// is down.
package v1alpha1
