package storage

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	"codeberg.org/meta/gzipped/v2"

	"github.com/operator-framework/operator-registry/alpha/declcfg"
)

// LocalDir is a storage Instance. When Storing a new FBC contained in
// fs.FS, the content is first written to a temporary file, after which
// it is copied to it's final destination in RootDir/catalogName/. This is
// done so that clients accessing the content stored in RootDir/catalogName have
// atomic view of the content for a catalog.
type LocalDir struct {
	RootDir string
	BaseURL *url.URL
}

func (s LocalDir) Store(ctx context.Context, catalog string, fsys fs.FS) error {
	fbcDir := filepath.Join(s.RootDir, catalog)
	if err := os.MkdirAll(fbcDir, 0700); err != nil {
		return err
	}

	tmpDir, err := os.MkdirTemp(s.RootDir, fmt.Sprintf(".%s-*.tmp", catalog))
	if err != nil {
		return err
	}
	jsonFile, err := os.Create(filepath.Join(tmpDir, "all.json"))
	if err != nil {
		return err
	}
	jsonGzFile, err := os.Create(filepath.Join(tmpDir, "all.json.gz"))
	if err != nil {
		return err
	}

	if err := func() error {
		var (
			gzipMu     sync.Mutex
			gzipWriter = gzip.NewWriter(jsonGzFile)
			mw         = io.MultiWriter(jsonFile, gzipWriter)
		)

		defer func() {
			_ = gzipWriter.Close()
			_ = jsonFile.Close()
			_ = jsonGzFile.Close()
		}()

		return declcfg.WalkMetasFS(ctx, fsys, func(path string, meta *declcfg.Meta, err error) error {
			if err != nil {
				return fmt.Errorf("error in parsing catalog content files in the filesystem: %w", err)
			}
			gzipMu.Lock()
			defer gzipMu.Unlock()
			_, err = mw.Write(meta.Blob)
			return err
		})
	}(); err != nil {
		return err
	}

	// Rename the temporary directory to the final directory
	if err := os.RemoveAll(fbcDir); err != nil {
		return err
	}
	return os.Rename(tmpDir, fbcDir)
}

func (s LocalDir) Delete(catalog string) error {
	return os.RemoveAll(filepath.Join(s.RootDir, catalog))
}

func (s LocalDir) ContentURL(catalog string) string {
	return fmt.Sprintf("%s%s/all.json", s.BaseURL, catalog)
}

func (s LocalDir) StorageServerHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle(s.BaseURL.Path, http.StripPrefix(s.BaseURL.Path, gzipped.FileServer(gzipped.FS(&filesOnlyFilesystem{os.DirFS(s.RootDir)}))))
	return mux
}

func (s LocalDir) Exists(catalog string) (bool, error) {
	exists := func(path string) (bool, error) {
		s, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				return false, nil
			}
			return false, err
		}
		return !s.IsDir(), nil
	}

	var (
		paths = []string{
			filepath.Join(s.RootDir, catalog, "all.json"),
			filepath.Join(s.RootDir, catalog, "all.json.gz"),
		}
		errs          = make([]error, 0, len(paths))
		allFilesExist = true
	)

	for _, path := range paths {
		exist, err := exists(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		allFilesExist = allFilesExist && exist
	}
	return allFilesExist, errors.Join(errs...)
}

// filesOnlyFilesystem is a file system that can open only regular
// files from the underlying filesystem. All other file types result
// in os.ErrNotExists
type filesOnlyFilesystem struct {
	FS fs.FS
}

// Open opens a named file from the underlying filesystem. If the file
// is not a regular file, it return os.ErrNotExists. Callers are resposible
// for closing the file returned.
func (f *filesOnlyFilesystem) Open(name string) (fs.File, error) {
	file, err := f.FS.Open(name)
	if err != nil {
		return nil, err
	}
	stat, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !stat.Mode().IsRegular() {
		_ = file.Close()
		return nil, os.ErrNotExist
	}
	return file, nil
}
