package apiservers

import (
	"context"
	_ "embed"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apiserver/pkg/registry/rest"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/server/options"

	catalogdv1alpha1 "github.com/joelanford/catalogd/api/catalogd/v1alpha1"
	"github.com/joelanford/catalogd/storage"
)

type CatalogMetadataAPIServer struct {
	CertFile string
	KeyFile  string
	Storage  storage.ReadOnlyStorage
}

func (s *CatalogMetadataAPIServer) Start(ctx context.Context) error {
	sch := runtime.NewScheme()
	metav1.AddToGroupVersion(sch, schema.GroupVersion{Version: "v1"})
	catalogdv1alpha1.AddToScheme(sch)

	cf := serializer.NewCodecFactory(sch)
	conf := genericapiserver.NewRecommendedConfig(cf)

	sso := options.NewSecureServingOptions().WithLoopback()
	sso.BindPort = 9443
	sso.ServerCert = options.GeneratableKeyCert{CertKey: options.CertKey{CertFile: s.CertFile, KeyFile: s.KeyFile}}
	if err := sso.ApplyTo(&conf.SecureServing, &conf.LoopbackClientConfig); err != nil {
		return err
	}

	c := conf.Complete()
	srv, err := c.New("catalogmetadata-apiserver", genericapiserver.NewEmptyDelegate())
	if err != nil {
		return err
	}
	apiGroupInfo := genericapiserver.NewDefaultAPIGroupInfo(catalogdv1alpha1.GroupVersion.Group, sch, metav1.ParameterCodec, cf)
	apiGroupInfo.VersionedResourcesStorageMap["v1alpha1"] = map[string]rest.Storage{
		"catalogmetadata": s.Storage,
	}
	apiGroupInfo.PrioritizedVersions = []schema.GroupVersion{catalogdv1alpha1.GroupVersion}
	if err := srv.InstallAPIGroup(&apiGroupInfo); err != nil {
		return err
	}
	return srv.PrepareRun().Run(ctx.Done())
}
