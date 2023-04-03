package storage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	bolt "go.etcd.io/bbolt"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	catalogdv1alpha1 "github.com/joelanford/catalogd/api/catalogd/v1alpha1"
)

var _ ReadOnlyStorage = &BoltStorage{}

type BoltStorage struct {
	GenericStorage
	db *bolt.DB
}

func (b *BoltStorage) SyncCatalog(_ context.Context, catalogName string, metas []Meta) error {
	return b.db.Update(func(tx *bolt.Tx) error {
		catalogsBkt, err := tx.CreateBucketIfNotExists([]byte("catalogs"))
		if err != nil {
			return err
		}
		catalogBkt, err := getEmptyBucket(catalogsBkt, []byte(catalogName))
		if err != nil {
			return err
		}
		for _, meta := range metas {
			catMeta := meta.ToCatalogMetadata(catalogName)
			val, err := json.Marshal(catMeta)
			if err != nil {
				return err
			}
			if err := catalogBkt.Put([]byte(catMeta.Name), val); err != nil {
				return err
			}
		}
		return nil
	})
}

func getEmptyBucket(parent *bolt.Bucket, bucketName []byte) (*bolt.Bucket, error) {
	if parent.Bucket(bucketName) != nil {
		if err := parent.DeleteBucket(bucketName); err != nil {
			return nil, err
		}
	}
	bkt, err := parent.CreateBucket(bucketName)
	if err != nil {
		return nil, err
	}
	return bkt, nil
}

func (b *BoltStorage) DeleteCatalog(_ context.Context, catalogName string) error {
	return b.db.Update(func(tx *bolt.Tx) error {
		catalogsBkt := tx.Bucket([]byte("catalogs"))
		if catalogsBkt == nil {
			return nil
		}
		catalogBkt := catalogsBkt.Bucket([]byte(catalogName))
		if catalogBkt == nil {
			return nil
		}
		return catalogsBkt.DeleteBucket([]byte(catalogName))
	})
}

func (b *BoltStorage) Get(_ context.Context, name string, _ *metav1.GetOptions) (runtime.Object, error) {
	gr := schema.GroupResource{Group: catalogdv1alpha1.GroupVersion.Group, Resource: "catalogmetadata"}
	var m catalogdv1alpha1.CatalogMetadata
	if err := b.db.View(func(tx *bolt.Tx) error {
		catalogsBkt := tx.Bucket([]byte("catalogs"))
		if catalogsBkt == nil {
			return apierrors.NewNotFound(gr, name)
		}

		catalogBkts := []*bolt.Bucket{}
		if err := catalogsBkt.ForEach(func(k, v []byte) error {
			bkt := catalogsBkt.Bucket(k)
			if bkt != nil {
				catalogBkts = append(catalogBkts, bkt)
			}
			return nil
		}); err != nil {
			return apierrors.NewInternalError(err)
		}

		for _, catalogBkt := range catalogBkts {
			if v := catalogBkt.Get([]byte(name)); v != nil {
				if err := json.Unmarshal(v, &m); err != nil {
					return apierrors.NewInternalError(err)
				}
				return nil
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return &m, nil
}

func (b *BoltStorage) List(_ context.Context, options *internalversion.ListOptions) (runtime.Object, error) {
	var list catalogdv1alpha1.CatalogMetadataList
	if err := b.db.View(func(tx *bolt.Tx) error {
		catalogsBkt := tx.Bucket([]byte("catalogs"))
		if catalogsBkt == nil {
			return nil
		}

		catalogBkts := []*bolt.Bucket{}
		if err := catalogsBkt.ForEach(func(k, v []byte) error {
			bkt := catalogsBkt.Bucket(k)
			if bkt != nil {
				catalogBkts = append(catalogBkts, bkt)
			}
			return nil
		}); err != nil {
			return apierrors.NewInternalError(err)
		}

		for _, catalogBkt := range catalogBkts {
			if err := catalogBkt.ForEach(func(k, v []byte) error {
				var m catalogdv1alpha1.CatalogMetadata
				if err := json.Unmarshal(v, &m); err != nil {
					return apierrors.NewInternalError(err)
				}

				if options.FieldSelector != nil {
					if !options.FieldSelector.Matches(fields.Set{"metadata.name": m.Name}) {
						return nil
					}
				}
				if options.LabelSelector != nil {
					if !options.LabelSelector.Matches(labels.Set(m.Labels)) {
						return nil
					}
				}

				list.Items = append(list.Items, m)
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return &list, nil
}

func NewBoltStorage() (*BoltStorage, error) {
	tmpDir, err := os.MkdirTemp("", "catalogd-apiserver-")
	if err != nil {
		return nil, err
	}
	file := filepath.Join(tmpDir, "cache.db")

	db, err := bolt.Open(file, 0600, nil)
	if err != nil {
		return nil, err
	}
	return &BoltStorage{db: db}, nil
}
