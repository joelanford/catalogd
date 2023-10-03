package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/blang/semver/v4"
	olmv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	"github.com/operator-framework/operator-registry/alpha/declcfg"
	"github.com/operator-framework/operator-registry/alpha/property"
	kappctrl "github.com/vmware-tanzu/carvel-kapp-controller/pkg/apis/kappctrl/v1alpha1"
	"github.com/vmware-tanzu/carvel-kapp-controller/pkg/apiserver/apis/datapackaging/v1alpha1"
	"golang.org/x/net/context"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/yaml"

	catalogdv1alpha1 "github.com/operator-framework/catalogd/api/core/v1alpha1"
)

var (
	log = zap.New(zap.UseDevMode(false)).WithName("catdapter")
)

/*
------ NOTES ------
1. Carvel uses CRDs to store packages and package metadata. This result in quite long sync times for large catalogs.
   There may be other disadvantages to this approach as well (need to research more). For example, are packages and
   package metadata atomically synced when a catalog is updated? If not, then there may be a period of time where
   the view is inconsistent.
2. For some reason, the Carvel API server returns a maximum of 500 packages with `kubectl get packages`. Possibly need
   implement some paging logic?
3. Carvel has no concept of channels.
4. Carvel only supports SVG icons. FBC is extensible because its icon field includes a media type. OLM currently
   supports SVG and PNG icons.
5. FBC still does not actually put all of the package metadata in the olm.package schema. We should add the missing
   fields, which would make it _much_ easier (and more efficient) to convert FBC to Carvel packages and package
   metadata.
6. Similarities with OLMv0
   - PackageRepository is a namespaced object.
   - There is a designated namespace for global package repositories.
   - The Packages and PackageMetadata APIs are served from an API service, which handles returning the correct packages
     and package metadata from the global repositories AND the repositories in the current namespace.
7. Differences with OLMv0
   - Carvel has a namespace annotation that allows a namespace to opt-out of global repositories.
-------------------
*/

func main() {
	var catalogsDir string
	flag.StringVar(&catalogsDir, "catalogs-dir", "/var/cache/catalog", "Directory to write catalogs to")
	flag.Parse()

	logf.SetLogger(log)
	klog.SetLogger(log)

	if err := os.RemoveAll(catalogsDir); err != nil {
		fatalErr(err, "Failed to clear catalogs dir")
	}

	sch := runtime.NewScheme()
	if err := catalogdv1alpha1.AddToScheme(sch); err != nil {
		fatalErr(err, "Failed to add catalogd to scheme")
	}

	restConfig := config.GetConfigOrDie()
	cl, err := client.New(restConfig, client.Options{Scheme: sch})
	if err != nil {
		fatalErr(err, "Failed to create client")
	}

	httpClient := &http.Client{
		Timeout: 10 * time.Second,
	}

	log.Info("Starting server at :8080")
	if err := http.ListenAndServe(":8080", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		catalogName := strings.TrimPrefix(r.URL.Path, "/")

		catalog := &catalogdv1alpha1.Catalog{}
		if err := cl.Get(r.Context(), client.ObjectKey{Name: catalogName}, catalog); err != nil {
			log.Error(err, "catalog request error")
			k8sHttpError(w, err)
			return
		}

		if catalog.Status.ContentURL == "" {
			msg := fmt.Sprintf("Catalog %s is not unpacked", catalogName)
			unpackedCondition := meta.FindStatusCondition(catalog.Status.Conditions, catalogdv1alpha1.TypeUnpacked)
			if unpackedCondition != nil && unpackedCondition.Status == metav1.ConditionFalse {
				msg = fmt.Sprintf("%s: %s", msg, unpackedCondition.Message)
			}
			http.Error(w, msg, http.StatusServiceUnavailable)
			return
		}

		ref := catalog.Status.ResolvedSource.Image.Ref
		catalogDir := filepath.Join(catalogsDir, catalogName)
		refDir := filepath.Join(catalogDir, ref)
		if refDirStat, err := os.Stat(refDir); err == nil && refDirStat.IsDir() {
			log.Info("Using cached packages", "ref", ref)
			http.ServeFile(w, r, filepath.Join(refDir, "catalog.tar.gz"))
			return
		}

		resp, err := httpClient.Get(catalog.Status.ContentURL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			http.Error(w, resp.Status, resp.StatusCode)
			return
		}

		prefix := fmt.Sprintf("olm.%s", catalogName)
		if p := catalog.Annotations["catalogd.operatorframework.io/domain-prefix"]; p != "" {
			prefix = p
		}

		if err := os.MkdirAll(refDir, 0755); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if err := func() error {
			tarFile, err := os.Create(filepath.Join(refDir, "catalog.tar.gz"))
			if err != nil {
				return err
			}
			defer tarFile.Close()

			gzipWriter := gzip.NewWriter(tarFile)
			defer gzipWriter.Close()

			tarWriter := tar.NewWriter(gzipWriter)
			defer tarWriter.Close()

			type versionBundle struct {
				Version semver.Version
				Bundle  declcfg.Bundle
			}
			highestBundle := map[string]versionBundle{}

			metadatas := map[string]v1alpha1.PackageMetadata{}
			if err := declcfg.WalkMetasReader(resp.Body, func(meta *declcfg.Meta, err error) error {
				if err != nil {
					return err
				}

				refName := fmt.Sprintf("%s.%s", prefix, meta.Package)
				if meta.Package == "" {
					refName = fmt.Sprintf("%s.%s", prefix, meta.Name)
				}

				if meta.Schema == "olm.package" {
					pkg := declcfg.Package{}
					if err := json.Unmarshal(meta.Blob, &pkg); err != nil {
						return err
					}

					iconSVGBase64 := ""
					if pkg.Icon != nil && pkg.Icon.MediaType == "image/svg+xml" {
						iconSVGBase64 = base64.StdEncoding.EncodeToString(pkg.Icon.Data)
					}
					pkgMeta := v1alpha1.PackageMetadata{
						TypeMeta: metav1.TypeMeta{
							Kind:       "PackageMetadata",
							APIVersion: schema.GroupVersion{Group: v1alpha1.GroupName, Version: "v1alpha1"}.String(),
						},
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"catalogd.operatorframework.io/catalog": catalogName},
							Name:   refName,
						},
						Spec: v1alpha1.PackageMetadataSpec{
							LongDescription: pkg.Description,
							IconSVGBase64:   iconSVGBase64,
						},
					}
					metadatas[refName] = pkgMeta
				}

				if meta.Schema == "olm.bundle" {
					bundle := declcfg.Bundle{}
					if err := json.Unmarshal(meta.Blob, &bundle); err != nil {
						return err
					}

					thisBundle := versionBundle{
						Bundle: bundle,
					}
					for _, p := range bundle.Properties {
						if p.Type == "olm.package" {
							var pkgProp property.Package
							if err := json.Unmarshal(p.Value, &pkgProp); err != nil {
								return err
							}
							thisBundle.Version, err = semver.Parse(pkgProp.Version)
							if err != nil {
								return err
							}
							break
						}
					}

					if curHighest, ok := highestBundle[refName]; (!ok || curHighest.Version.LT(thisBundle.Version)) && len(thisBundle.Version.Pre) == 0 {
						highestBundle[refName] = thisBundle
					}

					name := fmt.Sprintf("%s.%s", refName, thisBundle.Version)
					pkg := v1alpha1.Package{
						TypeMeta: metav1.TypeMeta{
							Kind:       "Package",
							APIVersion: schema.GroupVersion{Group: v1alpha1.GroupName, Version: "v1alpha1"}.String(),
						},
						ObjectMeta: metav1.ObjectMeta{
							Labels:    map[string]string{"catalogd.operatorframework.io/catalog": catalogName},
							Name:      name,
							Namespace: "todo",
						},
						Spec: v1alpha1.PackageSpec{
							RefName:                         refName,
							Version:                         thisBundle.Version.String(),
							Licenses:                        nil,
							ReleasedAt:                      metav1.Time{},
							CapactiyRequirementsDescription: "",
							ReleaseNotes:                    "",
							Template: v1alpha1.AppTemplateSpec{
								Spec: &kappctrl.AppSpec{
									Fetch: []kappctrl.AppFetch{
										{Image: &kappctrl.AppFetchImage{
											URL: bundle.Image,
										}},
									},
									Template: []kappctrl.AppTemplate{
										{OLMRegistry: &kappctrl.AppTemplateOLMRegistry{}},
									},
									Deploy: []kappctrl.AppDeploy{
										{Kapp: &kappctrl.AppDeployKapp{}},
									},
								},
							},
						},
					}

					pkgYAML, err := yaml.Marshal(pkg)
					if err != nil {
						return err
					}
					if err := tarWriter.WriteHeader(&tar.Header{
						Name: path.Join("packages", refName, fmt.Sprintf("%s.yaml", thisBundle.Version)),
						Size: int64(len(pkgYAML)),
						Mode: 0644,
					}); err != nil {
						return err
					}
					if _, err := tarWriter.Write(pkgYAML); err != nil {
						return err
					}
					log.Info("Wrote package", "package", refName, "version", thisBundle.Version)
				}
				return nil
			}); err != nil {
				return err
			}
			pkgsSorted := sets.List(sets.KeySet(metadatas))
			for _, pkgName := range pkgsSorted {
				pkgMeta := metadatas[pkgName]
				if highestBundle, ok := highestBundle[pkgName]; ok {
					if err := updatePkgMeta(&pkgMeta, highestBundle.Bundle); err != nil {
						return err
					}
				}
				pkgMetaYAML, err := yaml.Marshal(pkgMeta)
				if err != nil {
					return err
				}
				if err := tarWriter.WriteHeader(&tar.Header{
					Name: path.Join("packages", pkgMeta.Name, "metadata.yaml"),
					Size: int64(len(pkgMetaYAML)),
					Mode: 0644,
				}); err != nil {
					return err
				}
				if _, err := tarWriter.Write(pkgMetaYAML); err != nil {
					return err
				}
				log.Info("Wrote package metadata", "package", pkgMeta.Name)
			}
			return nil
		}(); err != nil {
			os.RemoveAll(catalogDir)
			log.Error(err, "Failed to unpack catalog")
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		http.ServeFile(w, r, filepath.Join(refDir, "catalog.tar.gz"))
	})); err != nil {
		fatalErr(err, "Failed to run server")
	}
}

func updatePkgMeta(pkgMeta *v1alpha1.PackageMetadata, highestBundle declcfg.Bundle) error {
	var csvMetadata *property.CSVMetadata
	for _, p := range highestBundle.Properties {
		if p.Type == "olm.csv.metadata" {
			var csv property.CSVMetadata
			if err := json.Unmarshal(p.Value, &csv); err != nil {
				return err
			}
			csvMetadata = &csv
			break
		}
	}

	if csvMetadata != nil {
		pkgMeta.Spec.DisplayName = csvMetadata.DisplayName
		pkgMeta.Spec.ProviderName = csvMetadata.Provider.Name
		pkgMeta.Spec.Categories = csvMetadata.Keywords
		pkgMeta.Spec.Maintainers = func(in []olmv1alpha1.Maintainer) []v1alpha1.Maintainer {
			out := make([]v1alpha1.Maintainer, 0, len(in))
			for _, m := range in {
				out = append(out, v1alpha1.Maintainer{
					Name: m.Name,
				})
			}
			return out
		}(csvMetadata.Maintainers)
	}
	return nil
}

func fatalErr(err error, msg string, keysAndValues ...interface{}) {
	log.Error(err, msg, keysAndValues...)
	os.Exit(1)
}

func k8sHttpError(w http.ResponseWriter, err error) {
	if status, ok := err.(apierrors.APIStatus); ok || errors.As(err, &status) {
		http.Error(w, status.Status().Message, int(status.Status().Code))
	} else if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		http.Error(w, err.Error(), http.StatusGatewayTimeout)
	} else {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
