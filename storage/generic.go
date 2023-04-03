package storage

import (
	"context"
	"encoding/json"
	"fmt"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	catalogdv1alpha1 "github.com/joelanford/catalogd/api/catalogd/v1alpha1"
)

type GenericStorage struct{}

func (_ GenericStorage) New() runtime.Object {
	return &catalogdv1alpha1.CatalogMetadata{}
}

func (_ GenericStorage) NewList() runtime.Object {
	return &catalogdv1alpha1.CatalogMetadataList{}
}

func (_ GenericStorage) Destroy() {}

func (_ GenericStorage) ConvertToTable(_ context.Context, object runtime.Object, tableOptions runtime.Object) (*metav1.Table, error) {
	table := &metav1.Table{
		ColumnDefinitions: []metav1.TableColumnDefinition{
			{Name: "Name", Type: "string", Format: "name"},
			{Name: "Catalog", Type: "string"},
			{Name: "Schema", Type: "string"},
			{Name: "Package", Type: "string"},
			{Name: "Name", Type: "string"},
		},
	}
	if list, ok := object.(*catalogdv1alpha1.CatalogMetadataList); ok {
		for _, item := range list.Items {
			item := item
			table.Rows = append(table.Rows, metav1.TableRow{
				Object: runtime.RawExtension{Object: &item},
				Cells: []interface{}{
					item.Name,
					item.Spec.Catalog.Name,
					item.Spec.Schema,
					item.Spec.Package,
					item.Spec.Name,
				},
			})
		}
		return table, nil
	}
	if item, ok := object.(*catalogdv1alpha1.CatalogMetadata); ok {
		table.Rows = append(table.Rows, metav1.TableRow{
			Object: runtime.RawExtension{Object: item},
			Cells: []interface{}{
				item.Name,
				item.Spec.Catalog.Name,
				item.Spec.Schema,
				item.Spec.Package,
				item.Spec.Name,
			},
		})
		return table, nil
	}
	return nil, apierrors.NewInternalError(fmt.Errorf("unable to convert object type %T to Table", object))
}

func (_ GenericStorage) NamespaceScoped() bool {
	return false
}

type Meta struct {
	Schema  string
	Package string
	Name    string

	Blob json.RawMessage
}

func (m Meta) MarshalJSON() ([]byte, error) {
	return m.Blob, nil
}

func (m *Meta) UnmarshalJSON(blob []byte) error {
	type tmp struct {
		Schema  string `json:"schema"`
		Package string `json:"package,omitempty"`
		Name    string `json:"name,omitempty"`
	}
	var t tmp
	if err := json.Unmarshal(blob, &t); err != nil {
		return err
	}
	m.Schema = t.Schema
	m.Package = t.Package
	m.Name = t.Name
	m.Blob = make([]byte, len(blob))
	copy(m.Blob, blob)
	return nil
}
func (m Meta) ToCatalogMetadata(catalogName string) catalogdv1alpha1.CatalogMetadata {
	packageOrName := m.Package
	if packageOrName == "" {
		packageOrName = m.Name
	}

	objName := fmt.Sprintf("%s-%s", catalogName, m.Schema)
	if m.Package != "" {
		objName = fmt.Sprintf("%s-%s", objName, m.Package)
	}
	if m.Name != "" {
		objName = fmt.Sprintf("%s-%s", objName, m.Name)
	}

	return catalogdv1alpha1.CatalogMetadata{
		ObjectMeta: metav1.ObjectMeta{
			Name: objName,
			Labels: map[string]string{
				"catalogd.operatorframework.io/catalog":       catalogName,
				"catalogd.operatorframework.io/schema":        m.Schema,
				"catalogd.operatorframework.io/package":       m.Package,
				"catalogd.operatorframework.io/name":          m.Name,
				"catalogd.operatorframework.io/packageOrName": packageOrName,
			},
		},
		Spec: catalogdv1alpha1.CatalogMetadataSpec{
			Catalog: v1.LocalObjectReference{
				Name: catalogName,
			},
			Schema:  m.Schema,
			Package: m.Package,
			Name:    m.Name,
			Content: m.Blob,
		},
	}
}
