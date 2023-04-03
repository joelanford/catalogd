package storage

import (
	"context"

	"k8s.io/apiserver/pkg/registry/rest"
)

type Syncer interface {
	SyncCatalog(ctx context.Context, catalogName string, metas []Meta) error
	DeleteCatalog(ctx context.Context, catalogName string) error
}

type ReadOnlyStorage interface {
	rest.Storage
	rest.Getter
	rest.Lister
	rest.Scoper
}
