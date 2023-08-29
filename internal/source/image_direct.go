package source

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/containerd/containerd/archive"
	"github.com/containerd/containerd/archive/compression"
	"github.com/containerd/containerd/images"
	"github.com/docker/cli/cli/config/configfile"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/operator-framework/operator-registry/alpha/declcfg"
	corev1 "k8s.io/api/core/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/operator-framework/catalogd/api/core/v1alpha1"
	"github.com/operator-framework/catalogd/internal/version"
)

var _ Unpacker = &ImageDirect{}

type ImageDirect struct {
	GetSecret  func(context.Context, string) (*corev1.Secret, error)
	ImageCache content.Storage

	// TODO: Increment the retry count for each image that fails to unpack.
	//   On subsequent calls to Unpack, use "Retrying" instead of "Unpacking"
	//   if the retry count is > 0.
	RetryCount map[string]int

	// CatalogGoRoutineMap map[catalogName]*goroutine
	// type goroutine struct {
	//     cancel func()
	//     imageRef string
	//     refCount int
	// }
	// func (g *goroutine) Run() { incrementRefCount(); if refCount == 1 { go g.run() }}
	// func (g *goroutine) Close() { decrementRefCount(); if refCount == 0 { g.cancel() }}
	// func (g *goroutine) Result() *Result { return <-g.resultChan }
	//
	// NOTES:
	//  1. When unpack is called for a catalog, we need to be able to lookup the current goroutine for it, regardless of what the current catalog's reference is.
	//  2. Ideally its the catalog image reference that is used as the key. That way multiple catalogs that reference the same image can share the same goroutine.

	CatalogsRoot string
	TmpRoot      string

	UseCache bool
}

type unpackResult struct {
	result *Result
	err    error
}

func (i ImageDirect) Unpack(ctx context.Context, catalog *v1alpha1.Catalog) (*Result, error) {
	if catalog.Spec.Source.Type != v1alpha1.SourceTypeImage {
		panic("source type image is unable to handle specified catalog source type " + string(catalog.Spec.Source.Type))
	}

	// TODO: move this to struct field
	regClient := &auth.Client{
		Client: retry.DefaultClient,
		Header: http.Header{
			"User-Agent": []string{"catalogd/" + version.Version().GitVersion},
		},
		Cache: auth.DefaultCache,
	}
	if catalog.Spec.Source.Image.PullSecret != "" {
		pullSecret, err := i.GetSecret(ctx, catalog.Spec.Source.Image.PullSecret)
		if err != nil {
			return nil, err
		}
		if pullSecret.Type != corev1.SecretTypeDockerConfigJson {
			return nil, fmt.Errorf("pull secret %q is not of type %q", catalog.Spec.Source.Image.PullSecret, string(corev1.SecretTypeDockerConfigJson))
		}
		cf := configfile.ConfigFile{}
		if err := json.Unmarshal(pullSecret.Data[corev1.DockerConfigJsonKey], &cf); err != nil {
			return nil, err
		}
		regClient.Credential = func(ctx context.Context, imageRegistryHost string) (auth.Credential, error) {
			authConfig, err := cf.GetAuthConfig(imageRegistryHost)
			if err != nil {
				return auth.Credential{}, err
			}
			return auth.Credential{
				Username:     authConfig.Username,
				Password:     authConfig.Password,
				RefreshToken: authConfig.IdentityToken,
				AccessToken:  authConfig.RegistryToken,
			}, nil
		}
	}

	ref, err := registry.ParseReference(catalog.Spec.Source.Image.Ref)
	if err != nil {
		return nil, err
	}
	repo := &remote.Repository{
		Client:    regClient,
		Reference: ref,
		ManifestMediaTypes: []string{
			// Do not support manifest lists or image indexes.
			// TODO: not sure what this actually does. Using a manifest list
			//   for the image ref seems to work fine.
			ocispec.MediaTypeImageManifest,
			images.MediaTypeDockerSchema2Manifest,
		},
	}

	// TODO: this resultChan stuff is a bit of a mess still. In it's current state, this function
	//   is just a plain old synchronous function that happens to use a goroutine to do the work.
	//   The intent is to make this function asynchronous, but that will require some refactoring
	//   and bookkeeping.
	resultChan := make(chan unpackResult)
	go func() {
		desc, err := repo.Resolve(ctx, ref.String())
		if err != nil {
			handleError(resultChan, fmt.Errorf("unable to resolve image %q: %w", ref.String(), err))
			return
		}

		contentDir := filepath.Join(i.CatalogsRoot, desc.Digest.String())
		contentTmpDir := filepath.Join(i.TmpRoot, desc.Digest.String())
		resolvedRef := fmt.Sprintf("%s/%s@%s", ref.Registry, ref.Repository, desc.Digest.String())
		if _, err := os.Stat(contentDir); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				handleError(resultChan, fmt.Errorf("unable to stat content file %q: %w", contentDir, err))
				return
			}

			var fetcher content.Fetcher = repo
			if i.UseCache {
				fetcher = i.ImageCache
				if err := oras.CopyGraph(ctx, repo, i.ImageCache, desc, oras.CopyGraphOptions{
					Concurrency: runtime.NumCPU(),
				}); err != nil {
					handleError(resultChan, fmt.Errorf("unable to download image %q: %w", resolvedRef, err))
					return
				}

			}
			rc, err := fetcher.Fetch(ctx, desc)
			if err != nil {
				handleError(resultChan, fmt.Errorf("unable to fetch image %q from cache: %w", resolvedRef, err))
				return
			}
			defer rc.Close()
			var image ocispec.Manifest
			if err := json.NewDecoder(rc).Decode(&image); err != nil {
				handleError(resultChan, fmt.Errorf("unable to decode image manifest %q: %w", resolvedRef, err))
				return
			}

			if err := os.MkdirAll(contentTmpDir, 0700); err != nil {
				handleError(resultChan, fmt.Errorf("unable to create temporary content directory %q: %w", contentTmpDir, err))
				return
			}
			defer os.RemoveAll(contentTmpDir)

			for _, layerDesc := range image.Layers {
				if err := i.unpackLayer(ctx, contentTmpDir, layerDesc, fetcher); err != nil {
					handleError(resultChan, fmt.Errorf("unable to unpack layer %q for image %q: %w", layerDesc.Digest.String(), resolvedRef, err))
					return
				}
			}

			configsFS, err := fs.Sub(os.DirFS(contentTmpDir), "configs")
			if err != nil {
				handleError(resultChan, fmt.Errorf("unable to create sub filesystem for configs directory of image %q: %w", resolvedRef, err))
				return
			}

			if err := i.writeCatalogJSON(configsFS, contentDir); err != nil {
				handleError(resultChan, fmt.Errorf("unable to write catalog JSON for image %q: %w", resolvedRef, err))
				return
			}
		}
		result := &Result{
			FS: os.DirFS(contentDir),
			ResolvedSource: &v1alpha1.CatalogSource{
				Type: v1alpha1.SourceTypeImage,
				Image: &v1alpha1.ImageSource{
					Ref:        resolvedRef,
					PullSecret: catalog.Spec.Source.Image.PullSecret,
				},
			},
			State:   StateUnpacked,
			Message: fmt.Sprintf("successfully unpacked catalog image %s", resolvedRef),
		}
		resultChan <- unpackResult{result, nil}
	}()
	r := <-resultChan
	return r.result, r.err
}

func handleError(resultChan chan<- unpackResult, err error) {
	resultChan <- unpackResult{nil, err}
}

func (i ImageDirect) unpackLayer(ctx context.Context, root string, desc ocispec.Descriptor, fetcher content.Fetcher) error {
	compressedTarReader, err := fetcher.Fetch(ctx, desc)
	if err != nil {
		return err
	}
	defer compressedTarReader.Close()
	tarReader, err := compression.DecompressStream(compressedTarReader)
	if err != nil {
		return err
	}
	defer tarReader.Close()

	_, err = archive.Apply(ctx, root, tarReader, archive.WithFilter(func(h *tar.Header) (bool, error) {
		h.Uid = os.Getuid()
		h.Gid = os.Getgid()
		dir, file := filepath.Split(h.Name)
		return (dir == "" && file == "configs") || strings.HasPrefix(dir, "configs/"), nil
	}))
	return err
}

func (i *ImageDirect) writeCatalogJSON(fsys fs.FS, contentDir string) error {
	if err := os.MkdirAll(contentDir, 0700); err != nil {
		return fmt.Errorf("unable to create content directory %q: %w", contentDir, err)
	}
	if err := func() error {
		contentFilePath := filepath.Join(contentDir, "all.json")
		contentFile, err := os.Create(contentFilePath)
		if err != nil {
			return fmt.Errorf("unable to create temporary catalog file: %w", err)
		}
		defer contentFile.Close()
		if err := declcfg.WalkMetasFS(fsys, func(path string, meta *declcfg.Meta, err error) error {
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(contentFile, bytes.NewReader(meta.Blob))
			return copyErr
		}); err != nil {
			return fmt.Errorf("unable to write catalog file: %w", err)
		}
		return nil
	}(); err != nil {
		os.RemoveAll(contentDir)
		return err
	}
	return nil
}
