package catalog

import (
	"io/fs"
	"net/http"
	"os"
)

func Handler(wwwRoot string) http.Handler {
	return http.FileServer(http.FS(&noDirsFS{os.DirFS(wwwRoot)}))
}

type noDirsFS struct {
	fsys fs.FS
}

func (n noDirsFS) Open(name string) (fs.File, error) {
	stat, err := fs.Stat(n.fsys, name)
	if err != nil {
		return nil, err
	}
	if stat.IsDir() {
		return nil, os.ErrNotExist
	}
	return n.fsys.Open(name)
}
