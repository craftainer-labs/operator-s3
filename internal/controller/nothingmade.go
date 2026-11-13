package controller

import (
	"context"

	"github.com/craftainer-labs/lib-operator-go/pkg/binding"
	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"
)

// nothingMade is the External of a claim that is being deleted and never issued
// a create (status.externalId is empty): nothing of it is on the store, so
// there is nothing to observe or delete, and the store is not contacted (it may
// be gone). A status that predates the record, or lost it, reads the same, which
// is the safe reading: the object is treated as not ours, so it is left alone.
type nothingMade[S, O any] struct{}

// Observe implements lifecycle.External: nothing exists.
func (nothingMade[S, O]) Observe(context.Context, S) (lifecycle.Observation[O], error) {
	return lifecycle.Observation[O]{}, nil
}

// Create implements lifecycle.External; it is never called, because nothing exists.
func (nothingMade[S, O]) Create(context.Context, S) (binding.Data, error) { return nil, nil }

// Update implements lifecycle.External; it is never called, because nothing exists.
func (nothingMade[S, O]) Update(context.Context, S, O) (binding.Data, error) { return nil, nil }

// Delete implements lifecycle.External: there is nothing to delete.
func (nothingMade[S, O]) Delete(context.Context, O) error { return nil }
