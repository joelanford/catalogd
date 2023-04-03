/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/joelanford/ignore"
	"github.com/operator-framework/operator-registry/pkg/image"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/finalizer"
	"sigs.k8s.io/controller-runtime/pkg/log"

	catalogdv1alpha1 "github.com/joelanford/catalogd/api/v1alpha1"
	"github.com/joelanford/catalogd/storage"
)

// CatalogReconciler reconciles a Catalog object
type CatalogReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	Storage    storage.Syncer
	Registry   image.Registry
	Finalizers finalizer.Finalizers
}

//+kubebuilder:rbac:groups=operatorframework.io,resources=catalogs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=operatorframework.io,resources=catalogs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=operatorframework.io,resources=catalogs/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Catalog object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.1/pkg/reconcile
func (r *CatalogReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx).WithName("catalog-controller")
	l.V(1).Info("starting")
	defer l.V(1).Info("ending")

	existingCat := &catalogdv1alpha1.Catalog{}
	if err := r.Get(ctx, req.NamespacedName, existingCat); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	reconciledCat := existingCat.DeepCopy()
	res, reconcileErr := r.reconcile(ctx, reconciledCat)

	// Do checks before any Update()s, as Update() may modify the resource structure!
	updateStatus := !equality.Semantic.DeepEqual(existingCat.Status, reconciledCat.Status)
	updateFinalizers := !equality.Semantic.DeepEqual(existingCat.Finalizers, reconciledCat.Finalizers)
	unexpectedFieldsChanged := checkForUnexpectedFieldChange(*existingCat, *reconciledCat)

	if updateStatus {
		if updateErr := r.Status().Update(ctx, reconciledCat); updateErr != nil {
			return res, errors.Join(reconcileErr, updateErr)
		}
	}

	if unexpectedFieldsChanged {
		panic("spec or metadata changed by reconciler")
	}

	if updateFinalizers {
		if updateErr := r.Update(ctx, reconciledCat); updateErr != nil {
			return res, errors.Join(reconcileErr, updateErr)
		}
	}

	return res, reconcileErr
}

// Compare resources - ignoring status & metadata.finalizers
func checkForUnexpectedFieldChange(a, b catalogdv1alpha1.Catalog) bool {
	a.Status, b.Status = catalogdv1alpha1.CatalogStatus{}, catalogdv1alpha1.CatalogStatus{}
	a.Finalizers, b.Finalizers = []string{}, []string{}
	return !equality.Semantic.DeepEqual(a, b)
}

func (r *CatalogReconciler) reconcile(ctx context.Context, cat *catalogdv1alpha1.Catalog) (ctrl.Result, error) {
	l := log.FromContext(ctx).WithName("catalog-controller")

	if _, err := r.Finalizers.Finalize(ctx, cat); err != nil {
		err := fmt.Errorf("error managing finalizers: %v", err)
		meta.SetStatusCondition(&cat.Status.Conditions, metav1.Condition{
			Type:    catalogdv1alpha1.ConditionTypeSynced,
			Status:  metav1.ConditionFalse,
			Reason:  catalogdv1alpha1.SyncReasonError,
			Message: err.Error(),
		})
		return ctrl.Result{}, err
	}

	if cat.Spec.Source.Type != "image" {
		err := fmt.Errorf("unsupported source type %q", cat.Spec.Source.Type)
		meta.SetStatusCondition(&cat.Status.Conditions, metav1.Condition{
			Type:    catalogdv1alpha1.ConditionTypeSynced,
			Status:  metav1.ConditionFalse,
			Reason:  catalogdv1alpha1.SyncReasonError,
			Message: err.Error(),
		})
		return ctrl.Result{}, err
	}

	if cat.Spec.Source.Image == nil {
		err := fmt.Errorf("image source type requires image field")
		meta.SetStatusCondition(&cat.Status.Conditions, metav1.Condition{
			Type:    catalogdv1alpha1.ConditionTypeSynced,
			Status:  metav1.ConditionFalse,
			Reason:  catalogdv1alpha1.SyncReasonError,
			Message: err.Error(),
		})
		return ctrl.Result{}, err
	}

	l.Info("pulling image")
	if err := r.Registry.Pull(ctx, image.SimpleReference(cat.Spec.Source.Image.Ref)); err != nil {
		err := fmt.Errorf("error pulling image %q: %v", cat.Spec.Source.Image.Ref, err)
		meta.SetStatusCondition(&cat.Status.Conditions, metav1.Condition{
			Type:    catalogdv1alpha1.ConditionTypeSynced,
			Status:  metav1.ConditionFalse,
			Reason:  catalogdv1alpha1.SyncReasonError,
			Message: err.Error(),
		})
		return ctrl.Result{}, err
	}

	tmpDir, err := os.MkdirTemp("", fmt.Sprintf("catalogd-unpack-%s-*", cat.Name))
	if err != nil {
		err := fmt.Errorf("error creating temp directory for unpacking catalog: %v", err)
		meta.SetStatusCondition(&cat.Status.Conditions, metav1.Condition{
			Type:    catalogdv1alpha1.ConditionTypeSynced,
			Status:  metav1.ConditionFalse,
			Reason:  catalogdv1alpha1.SyncReasonError,
			Message: err.Error(),
		})
		return ctrl.Result{}, err
	}
	defer os.RemoveAll(tmpDir)

	l.Info("unpacking image")
	if err := r.Registry.Unpack(ctx, image.SimpleReference(cat.Spec.Source.Image.Ref), tmpDir); err != nil {
		err := fmt.Errorf("error unpacking image %q: %v", cat.Spec.Source.Image.Ref, err)
		meta.SetStatusCondition(&cat.Status.Conditions, metav1.Condition{
			Type:    catalogdv1alpha1.ConditionTypeSynced,
			Status:  metav1.ConditionFalse,
			Reason:  catalogdv1alpha1.SyncReasonError,
			Message: err.Error(),
		})
		return ctrl.Result{}, err
	}

	var allMetas []storage.Meta
	l.Info("loading metas")
	if err := walkFS(os.DirFS(filepath.Join(tmpDir, "configs")), func(path string, metas []storage.Meta, err error) error {
		if err != nil {
			return err
		}
		allMetas = append(allMetas, metas...)
		return nil
	}); err != nil {
		err := fmt.Errorf("error walking configs: %v", err)
		meta.SetStatusCondition(&cat.Status.Conditions, metav1.Condition{
			Type:    catalogdv1alpha1.ConditionTypeSynced,
			Status:  metav1.ConditionFalse,
			Reason:  catalogdv1alpha1.SyncReasonError,
			Message: err.Error(),
		})
		return ctrl.Result{}, err
	}

	l.Info("syncing catalog")
	if err := r.Storage.SyncCatalog(ctx, cat.Name, allMetas); err != nil {
		err := fmt.Errorf("error syncing catalog: %v", err)
		meta.SetStatusCondition(&cat.Status.Conditions, metav1.Condition{
			Type:    catalogdv1alpha1.ConditionTypeSynced,
			Status:  metav1.ConditionFalse,
			Reason:  catalogdv1alpha1.SyncReasonError,
			Message: err.Error(),
		})
		return ctrl.Result{}, err
	}
	meta.SetStatusCondition(&cat.Status.Conditions, metav1.Condition{
		Type:    catalogdv1alpha1.ConditionTypeSynced,
		Status:  metav1.ConditionTrue,
		Reason:  catalogdv1alpha1.SyncedReasonSuccess,
		Message: "Catalog synced successfully",
	})
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *CatalogReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&catalogdv1alpha1.Catalog{}).
		Complete(r)
}

type walkFunc func(path string, metas []storage.Meta, err error) error

// WalkFS walks root using a gitignore-style filename matcher to skip files
// that match patterns found in .indexignore files found throughout the filesystem.
// It calls walkFn for each declarative config file it finds. If WalkFS encounters
// an error loading or parsing any file, the error will be immediately returned.
func walkFS(root fs.FS, walkFn walkFunc) error {
	const indexIgnoreFilename = ".indexignore"
	if root == nil {
		return fmt.Errorf("no declarative config filesystem provided")
	}

	matcher, err := ignore.NewMatcher(root, indexIgnoreFilename)
	if err != nil {
		return err
	}

	return fs.WalkDir(root, ".", func(path string, info fs.DirEntry, err error) error {
		if err != nil {
			return walkFn(path, nil, err)
		}
		// avoid validating a directory, an .indexignore file, or any file that matches
		// an ignore pattern outlined in a .indexignore file.
		if info.IsDir() || info.Name() == indexIgnoreFilename || matcher.Match(path, false) {
			return nil
		}

		metas, err := loadMetas(root, path)
		if err != nil {
			return walkFn(path, metas, err)
		}

		return walkFn(path, metas, err)
	})
}

func loadMetas(root fs.FS, path string) ([]storage.Meta, error) {
	f, err := root.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := yaml.NewYAMLToJSONDecoder(f)

	var metas []storage.Meta
	for {
		var meta storage.Meta
		if err := dec.Decode(&meta); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		metas = append(metas, meta)
	}
	return metas, nil
}
