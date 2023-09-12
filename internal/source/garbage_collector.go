package source

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/go-logr/logr"
	catalogdv1alpha1 "github.com/operator-framework/catalogd/api/core/v1alpha1"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// TODO: Clean this up some more

var _ manager.Runnable = &CatalogCacheGarbageCollector{}

// CatalogCacheGarbageCollector is a garbage collector
// for ensuring that caches created for a Catalog resource
// don't grow unbounded and are cleaned up periodically
type CatalogCacheGarbageCollector struct {
	// Cache is the cache used to get informers and
	// configure garbage collection on deletion events
	Cache cache.Cache
	// SyncInterval is how long the garbage collector
	// should wait between cleanup runs
	SyncInterval time.Duration
	// PreserveInterval is how long files should persist
	// in the cache before being garbage collected
	PreserveInterval time.Duration
	// CachePath is the path to the cache that needs
	// to be garbage collected
	CachePath string
	// Logger is the logger to be used by the garbage collector
	Logger logr.Logger
}

// Start implements the manager.Runnable interface so that the garbage collector
// can be run with a controller-runtime manager as its own process
func (gc *CatalogCacheGarbageCollector) Start(ctx context.Context) error {
	// Create a channel to use as a queue for performing cache deletion
	deleteChannel := make(chan *catalogdv1alpha1.Catalog)

	// Fetch the Catalog informer from the cache and add an event handler
	// that removes all cached data for a Catalog when it is deleted from
	// the cluster.
	catalogInformer, err := gc.Cache.GetInformer(ctx, &catalogdv1alpha1.Catalog{})
	if err != nil {
		return err
	}

	_, err = catalogInformer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		DeleteFunc: func(obj interface{}) {
			var objToCast interface{}
			dfu, ok := obj.(*toolscache.DeletedFinalStateUnknown)
			if ok {
				objToCast = dfu.Obj
			} else {
				objToCast = obj
			}
			catalog, ok := objToCast.(*catalogdv1alpha1.Catalog)
			if !ok || catalog.Spec.Source.Type != catalogdv1alpha1.SourceTypeImage {
				return
			}
			deleteChannel <- catalog
		},
	})
	if err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case catalog := <-deleteChannel:
			pathToGc := filepath.Join(gc.CachePath, catalog.Name)
			gc.Logger.Info("running garbage collection",
				"catalog",
				catalog.Name,
				"path",
				pathToGc)

			if err := os.RemoveAll(pathToGc); err != nil {
				gc.Logger.Error(err, "removing file", "path", pathToGc)
			}
		}
	}
}
