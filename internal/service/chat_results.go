package service

import (
	"context"
	"io"

	"github.com/k8s-ai/k8s-ai/internal/objectstore"
	"github.com/k8s-ai/k8s-ai/internal/store"
)

// chatResultStore places transient, already-redacted results in the existing
// object gateway and immediately starts the normal orphan grace period.
// Access is controlled by the Agent's current-run reference allow-list only.
type chatResultStore struct {
	gateway *BlobGateway
	store   store.Store
}

func (s chatResultStore) Put(ctx context.Context, spec objectstore.Spec, r io.Reader) (objectstore.Ref, error) {
	ref, err := s.gateway.Put(ctx, spec, r)
	if err != nil {
		return ref, err
	}
	err = s.store.WithTx(ctx, func(tx *store.Tx) error {
		if err := tx.Acquire(ctx, ref, s.gateway.Driver(), spec.CreatedBy); err != nil {
			return err
		}
		return tx.Release(ctx, ref.Key)
	})
	return ref, err
}

func (s chatResultStore) Get(ctx context.Context, key string) (io.ReadCloser, objectstore.Ref, error) {
	return s.gateway.Get(ctx, key)
}
