package source

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/go-containerregistry/pkg/name"
	catalogdv1alpha1 "github.com/operator-framework/catalogd/api/core/v1alpha1"
	"k8s.io/apimachinery/pkg/util/sets"
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
	// Fetch the Catalog informer from the cache and add an event handler
	// that removes all cached data for a Catalog when it is deleted from
	// the cluster.
	catalogInformer, err := gc.Cache.GetInformer(ctx, &catalogdv1alpha1.Catalog{})
	if err != nil {
		return err
	}

	_, err = catalogInformer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		DeleteFunc: func(obj interface{}) {
			catalog := obj.(*catalogdv1alpha1.Catalog)
			if catalog.Spec.Source.Type != catalogdv1alpha1.SourceTypeImage {
				return
			}
			filename := filepath.Join(gc.CachePath, catalog.Name)
			gc.Logger.Info("removing file", "path", filename)
			if err := os.RemoveAll(filename); err != nil {
				gc.Logger.Error(err, "failed to remove file", "path", filename)
			}
		},
	})
	if err != nil {
		return err
	}

	// Wait for the cache to sync to ensure that our Catalog List calls
	// in the below loop see a full view of the catalog that exist in
	// the cluster.
	if ok := gc.Cache.WaitForCacheSync(ctx); !ok {
		if ctx.Err() == nil {
			return fmt.Errorf("cache did not sync")
		}
		return fmt.Errorf("cache did not sync: %v", ctx.Err())
	}

	// Garbage collect every X amount of time, where X is the
	// provided sync interval
	ticker := time.NewTicker(gc.SyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			gc.Logger.Info("running garbage collection")

			catalogList := &catalogdv1alpha1.CatalogList{}
			if err := gc.Cache.List(ctx, catalogList); err != nil {
				gc.Logger.Error(err, "error listing catalogs for cache garbage collection")
				continue
			}

			// For each catalog get a list of files that are safe to remove
			filesToRemove := sets.NewString()
			for _, catalog := range catalogList.Items {
				cacheEntries, err := os.ReadDir(filepath.Join(gc.CachePath, catalog.Name))
				if err != nil {
					gc.Logger.Error(err, "reading cache for catalog", catalog.Name)
				}

				if catalog.Status.ResolvedSource != nil && catalog.Status.ResolvedSource.Image != nil {
					digest, err := name.NewDigest(catalog.Status.ResolvedSource.Image.Ref)
					if err != nil {
						gc.Logger.Error(err, "error parsing digest from resolved source")
						break
					}
					gc.Logger.Info("digesting....")
					digStr := strings.Split(digest.DigestStr(), ":")[1]
					for _, e := range cacheEntries {
						if e.Name() == digStr {
							continue
						}

						fInfo, err := e.Info()
						if err != nil {
							continue
						}

						if time.Since(fInfo.ModTime()) <= gc.PreserveInterval {
							gc.Logger.Info("preserving cached file", "file", e.Name())
							continue
						}

						filesToRemove.Insert(filepath.Join(gc.CachePath, catalog.Name, e.Name()))
					}
				}
			}

			for _, path := range filesToRemove.List() {
				gc.Logger.Info("removing file", "path", path)
				if err := os.RemoveAll(path); err != nil {
					gc.Logger.Error(err, "failed to remove file", "path", path)
				}
			}
		}
	}
}
