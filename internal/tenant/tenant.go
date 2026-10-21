// Package tenant carries the tenant scope through request and job contexts.
//
// BeaconX v1 runs as a single-tenant deployment, but the tenant identifier is
// part of every request context, every store call and every on-disk path from
// day one. Enabling real isolation later is then a matter of resolving the
// tenant from the authenticated principal instead of the configured default.
package tenant

import "context"

// ID is a tenant identifier.
type ID int64

// DefaultID is used when no explicit tenant is configured.
const DefaultID ID = 1

type ctxKey struct{}

// With returns a child context carrying the tenant identifier.
func With(ctx context.Context, id ID) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// From extracts the tenant identifier from a context.
func From(ctx context.Context) (ID, bool) {
	id, ok := ctx.Value(ctxKey{}).(ID)
	return id, ok
}

// MustFrom extracts the tenant identifier or falls back to DefaultID.
//
// Callers that cannot proceed without a tenant must not rely on this; it exists
// only so leaf helpers stay ergonomic while middleware guarantees presence.
func MustFrom(ctx context.Context) ID {
	if id, ok := From(ctx); ok {
		return id
	}
	return DefaultID
}
