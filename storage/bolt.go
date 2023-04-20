package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

func (b *BoltStorage) SyncCatalog(_ context.Context, catalogName string, metas <-chan Meta) error {
	return b.db.Update(func(tx *bolt.Tx) error {
		metasBkt, err := tx.CreateBucketIfNotExists([]byte("metas"))
		if err != nil {
			return err
		}
		if err := b.deleteCatalogMetasTx(tx, metasBkt, catalogName); err != nil {
			return err
		}
		for meta := range metas {
			catMeta := meta.ToCatalogMetadata(catalogName)
			val, err := json.Marshal(catMeta)
			if err != nil {
				return err
			}
			if err := metasBkt.Put([]byte(catMeta.Name), val); err != nil {
				return err
			}
		}
		return nil
	})
}

func (b *BoltStorage) DeleteCatalog(_ context.Context, catalogName string) error {
	return b.db.Update(func(tx *bolt.Tx) error {
		return b.deleteCatalogMetasTx(tx, tx.Bucket([]byte("metas")), catalogName)
	})
}

func (b *BoltStorage) deleteCatalogMetasTx(tx *bolt.Tx, metasBkt *bolt.Bucket, catalogName string) error {
	if metasBkt == nil {
		return nil
	}
	prefix := []byte(fmt.Sprintf("%s-", catalogName))
	c := metasBkt.Cursor()
	for k, _ := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
		if err := metasBkt.Delete(k); err != nil {
			return err
		}
	}
	return nil
}

func (b *BoltStorage) Get(_ context.Context, name string, _ *metav1.GetOptions) (runtime.Object, error) {
	gr := schema.GroupResource{Group: catalogdv1alpha1.GroupVersion.Group, Resource: "catalogmetadata"}
	var m catalogdv1alpha1.CatalogMetadata
	if err := b.db.View(func(tx *bolt.Tx) error {
		metasBkt := tx.Bucket([]byte("metas"))
		if metasBkt == nil {
			return apierrors.NewNotFound(gr, name)
		}
		v := metasBkt.Get([]byte(name))
		if v == nil {
			return apierrors.NewNotFound(gr, name)
		}
		if err := json.Unmarshal(v, &m); err != nil {
			return apierrors.NewInternalError(err)
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
		metasBkt := tx.Bucket([]byte("metas"))
		if metasBkt == nil {
			return nil
		}
		if err := metasBkt.ForEach(func(k, v []byte) error {
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
