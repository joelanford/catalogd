package cache

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/operator-framework/operator-registry/alpha/declcfg"
)

func NewCache(baseDir string, ttl time.Duration) *Cache {
	return &Cache{
		baseDir:  baseDir,
		ttl:      ttl,
		catalogs: make(map[string]*cachedCatalog),
	}
}

type Cache struct {
	baseDir  string
	catalogs map[string]*cachedCatalog
	mu       sync.RWMutex
	ttl      time.Duration
}

func (c *Cache) Get(catalogName string) (*os.File, *Index, *time.Time, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	cc, ok := c.catalogs[catalogName]
	if !ok {
		return nil, nil, nil, false
	}
	cc.timer.Reset(c.ttl)
	f, err := os.Open(cc.File)
	if err != nil {
		delete(c.catalogs, catalogName)
		return nil, nil, nil, false
	}
	return f, cc.Index, cc.LastModified, ok
}

func (c *Cache) Delete(catalogName string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cc, ok := c.catalogs[catalogName]; ok {
		return c.deleteCachedCatalog(catalogName, cc)
	}
	return nil
}

func (c *Cache) deleteCachedCatalog(catalogName string, cc *cachedCatalog) error {
	cc.timer.Stop()
	delete(c.catalogs, catalogName)
	return os.Remove(cc.File)
}

func (c *Cache) Set(catalogName string, reader io.ReadCloser, modTime *time.Time) (*os.File, *Index, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if cc, ok := c.catalogs[catalogName]; ok {
		if err := os.Remove(cc.File); err != nil {
			return nil, nil, err
		}
		cc.timer.Stop()
		delete(c.catalogs, catalogName)
	}

	tmpFile, err := os.CreateTemp(c.baseDir, fmt.Sprintf(".%s-*.json"))
	if err != nil {
		return nil, nil, err
	}
	defer os.Remove(tmpFile.Name())

	if _, err := io.Copy(tmpFile, reader); err != nil {
		return nil, nil, fmt.Errorf("could not copy response to disk: %v", err)
	}
	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		return nil, nil, err
	}
	idx, err := newIndex(tmpFile)
	if err != nil {
		return nil, nil, err
	}
	if err := tmpFile.Close(); err != nil {
		return nil, nil, err
	}
	fileName := filepath.Join(c.baseDir, fmt.Sprintf("%s.json", catalogName))
	if err := os.Rename(tmpFile.Name(), fileName); err != nil {
		return nil, nil, err
	}
	timer := time.NewTimer(c.ttl)
	go func() {
		<-timer.C
		log.Printf("deleting cached catalog %q due to TTL expiration", catalogName)
		_ = c.Delete(catalogName)
	}()
	c.catalogs[catalogName] = &cachedCatalog{
		File:         fileName,
		Index:        idx,
		LastModified: modTime,
		timer:        timer,
	}
	f, err := os.Open(fileName)
	if err != nil {
		return nil, nil, err
	}
	return f, idx, nil
}

func (c *Cache) Purge() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	errs := make([]error, 0, len(c.catalogs))
	for name, cc := range c.catalogs {
		errs = append(errs, c.deleteCachedCatalog(name, cc))
	}
	return errors.Join(errs...)
}

type cachedCatalog struct {
	File         string
	Index        *Index
	LastModified *time.Time

	timer *time.Timer
}

type Index struct {
	keys map[string][]section
}

func (i Index) Get(r io.ReaderAt, schema, packageName, name string) (io.Reader, bool) {
	key := makeKey(schema, packageName, name)
	sections, ok := i.keys[key]
	if !ok {
		return nil, false
	}
	srs := make([]io.Reader, 0, len(sections))
	for _, s := range sections {
		sr := io.NewSectionReader(r, s.offset, s.length)
		srs = append(srs, sr)
	}
	return io.MultiReader(srs...), true
}

type section struct {
	offset int64
	length int64
}

func newIndex(r io.Reader) (*Index, error) {
	idx := &Index{
		keys: make(map[string][]section),
	}
	var meta declcfg.Meta
	dec := json.NewDecoder(r)
	for {
		i1 := dec.InputOffset()
		if err := dec.Decode(&meta); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		i2 := dec.InputOffset()
		start := i1
		length := i2 - i1

		for _, key := range []string{
			makeKey("", "", meta.Name),
			makeKey("", meta.Package, ""),
			makeKey("", meta.Package, meta.Name),
			makeKey(meta.Schema, "", ""),
			makeKey(meta.Schema, "", meta.Name),
			makeKey(meta.Schema, meta.Package, ""),
			makeKey(meta.Schema, meta.Package, meta.Name),
		} {
			idx.keys[key] = append(idx.keys[key], section{start, length})
		}
	}
	emptyKey := makeKey("", "", "")
	idx.keys[emptyKey] = []section{{0, dec.InputOffset()}}
	return idx, nil
}

func makeKey(schema, pkg, name string) string {
	return schema + "|" + pkg + "|" + name
}
