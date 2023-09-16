package source

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/operator-framework/catalogd/api/core/v1alpha1"
)

var _ manager.Runnable = &CatalogCacheGarbageCollector{}

// CatalogCacheGarbageCollector is a manager runnable that deletes
// unused catalog cache directories when the manager starts.
type CatalogCacheGarbageCollector struct {
	// Cache is the cache used to get informers and
	// configure garbage collection on deletion events
	Cache cache.Cache
	// CachePath is the path to the cache that needs
	// to be garbage collected
	CachePath string
	// Logger is the logger to be used by the garbage collector
	Logger logr.Logger
}

// Start runs a one-shot garbage collection of the cache directory to clean up
// any directories that are not associated with an active catalog.
func (gc *CatalogCacheGarbageCollector) Start(ctx context.Context) error {
	if ok := gc.Cache.WaitForCacheSync(ctx); !ok {
		if ctx.Err() == nil {
			return fmt.Errorf("cache did not sync")
		}
		return ctx.Err()
	}

	var catalogList v1alpha1.CatalogList
	if err := gc.Cache.List(ctx, &catalogList); err != nil {
		return fmt.Errorf("error listing catalogs: %w", err)
	}

	expectedCatalogs := sets.New[string]()
	for _, catalog := range catalogList.Items {
		expectedCatalogs.Insert(catalog.Name)
	}

	cacheDirEntries, err := os.ReadDir(gc.CachePath)
	if err != nil {
		return fmt.Errorf("error reading cache directory: %w", err)
	}
	for _, cacheDirEntry := range cacheDirEntries {
		if cacheDirEntry.IsDir() && expectedCatalogs.Has(cacheDirEntry.Name()) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(gc.CachePath, cacheDirEntry.Name())); err != nil {
			gc.Logger.Error(err, "error removing cache directory entry", "path", cacheDirEntry.Name(), "isDir", cacheDirEntry.IsDir())
		} else {
			gc.Logger.Info("deleted unexpected cache directory entry", "path", cacheDirEntry.Name(), "isDir", cacheDirEntry.IsDir())
		}
	}
	return nil
}
