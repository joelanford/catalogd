package finalizer

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/finalizer"

	catalogdv1alpha1 "github.com/joelanford/catalogd/api/v1alpha1"
	"github.com/joelanford/catalogd/storage"
)

var _ finalizer.Finalizer = &RmCache{}

type RmCache struct {
	Storage storage.Syncer
}

func (r RmCache) Finalize(ctx context.Context, object client.Object) (finalizer.Result, error) {
	cat := object.(*catalogdv1alpha1.Catalog)
	if err := r.Storage.DeleteCatalog(ctx, cat.Name); err != nil {
		return finalizer.Result{}, err
	}
	return finalizer.Result{}, nil
}
