package source

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/archive"
	"github.com/google/go-containerregistry/pkg/authn/k8schain"
	gcrkube "github.com/google/go-containerregistry/pkg/authn/kubernetes"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	catalogdv1alpha1 "github.com/operator-framework/catalogd/api/core/v1alpha1"
)

// TODO: Add garbage collection to remove any unused
// images/SHAs that exist in the cache

// TODO: Make asynchronous

type ImageRegistry struct {
	BaseCachePath string
	AuthNamespace string
}

const configDirLabel = "operators.operatorframework.io.index.configs.v1"

func (i *ImageRegistry) Unpack(ctx context.Context, catalog *catalogdv1alpha1.Catalog) (*Result, error) {
	if catalog.Spec.Source.Type != catalogdv1alpha1.SourceTypeImage {
		panic(fmt.Sprintf("programmer error: source type %q is unable to handle specified catalog source type %q", catalogdv1alpha1.SourceTypeImage, catalog.Spec.Source.Type))
	}

	if catalog.Spec.Source.Image == nil {
		return nil, fmt.Errorf("error parsing catalog, catalog %s has a nil image source", catalog.Name)
	}

	imgRef, err := name.ParseReference(catalog.Spec.Source.Image.Ref)
	if err != nil {
		return nil, NewUnrecoverableError(fmt.Errorf("error parsing image reference: %w", err))
	}

	remoteOpts := []remote.Option{}
	if catalog.Spec.Source.Image.PullSecret != "" {
		chainOpts := k8schain.Options{
			ImagePullSecrets: []string{catalog.Spec.Source.Image.PullSecret},
			Namespace:        i.AuthNamespace,
			// TODO: Do we want to use any secrets that are included in the catalogd service account?
			// If so, we will need to add the permission to get service accounts and specify
			// the catalogd service account name here.
			ServiceAccountName: gcrkube.NoServiceAccount,
		}
		authChain, err := k8schain.NewInCluster(ctx, chainOpts)
		if err != nil {
			return nil, fmt.Errorf("error getting auth keychain: %w", err)
		}

		remoteOpts = append(remoteOpts, remote.WithAuthFromKeychain(authChain))
	}

	// always fetch the hash
	imgDesc, err := remote.Head(imgRef, remoteOpts...)
	if err != nil {
		return nil, fmt.Errorf("error fetching image descriptor: %w", err)
	}

	dirToUnpack := imgDesc.Annotations[configDirLabel]
	if dirToUnpack == "" {
		dirToUnpack = "configs"
	}

	unpackPath := filepath.Join(i.BaseCachePath, catalog.Name, imgDesc.Digest.Hex)
	if _, err = os.Stat(unpackPath); errors.Is(err, os.ErrNotExist) {
		if err = os.MkdirAll(unpackPath, 0700); err != nil {
			return nil, fmt.Errorf("error creating unpack path: %w", err)
		}

		if err = unpackImage(ctx, imgRef, unpackPath, dirToUnpack, remoteOpts...); err != nil {
			return nil, fmt.Errorf("error unpacking image: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("error checking if image is in filesystem cache: %w", err)
	}

	fsys := os.DirFS(unpackPath)

	return &Result{
		FS: fsys,
		ResolvedSource: &catalogdv1alpha1.CatalogSource{
			Type: catalogdv1alpha1.SourceTypeImage,
			Image: &catalogdv1alpha1.ImageSource{
				Ref:        fmt.Sprintf("%s@sha256:%s", imgRef.Context().Name(), imgDesc.Digest.Hex),
				PullSecret: catalog.Spec.Source.Image.PullSecret,
			},
		},
		State: StateUnpacked,
	}, nil
}

// unpackImage unpacks a catalog image reference to the provided unpackPath,
// returning an error if any errors are encountered along the way.
func unpackImage(ctx context.Context, imgRef name.Reference, unpackPath, dirToUnpack string, remoteOpts ...remote.Option) error {
	img, err := remote.Image(imgRef, remoteOpts...)
	if err != nil {
		return fmt.Errorf("error fetching remote image %q: %w", imgRef.Name(), err)
	}

	layers, err := img.Layers()
	if err != nil {
		return fmt.Errorf("error getting image layers: %w", err)
	}

	dirBase := filepath.Base(dirToUnpack)
	for _, layer := range layers {
		layerRc, err := layer.Uncompressed()
		if err != nil {
			return fmt.Errorf("error getting uncompressed layer data: %w", err)
		}

		// Apply the layer contents, but filter on the directory that contains catalog contents so we only cache the
		// catalog contents and nothing else. This filter ensures that the files created have the proper UID and GID
		// for the filesystem they will be stored on to ensure no permission errors occur when attempting to create the
		// files.
		_, err = archive.Apply(ctx, unpackPath, layerRc, archive.WithFilter(func(th *tar.Header) (bool, error) {
			th.Uid = os.Getuid()
			th.Gid = os.Getgid()
			dir, file := filepath.Split(th.Name)
			return (dir == "" && file == dirBase) || strings.HasPrefix(dir, fmt.Sprintf("%s/", dirBase)), nil
		}))
		if err != nil {
			return fmt.Errorf("error applying layer to archive: %w", err)
		}
	}

	return nil
}
