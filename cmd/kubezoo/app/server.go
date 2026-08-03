/*
Copyright 2022 The KubeZoo Authors.

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

// Package app does all of the work necessary to create a Kubernetes
// APIServer by binding together the API, master and APIServer infrastructure.
// It can be configured and called directly or via the hyperkube framework.
package app

import (
	"context"
	stdx509 "crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/managedfields"
	openapibuilder3 "k8s.io/kube-openapi/pkg/builder3"
	openapiutil "k8s.io/kube-openapi/pkg/util"

	"github.com/fivetime/kubezoo-gateway/pkg/apiconfig"
	"github.com/fivetime/kubezoo-gateway/pkg/publishedclass"

	"github.com/spf13/cobra"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	extensionsapiserver "k8s.io/apiextensions-apiserver/pkg/apiserver"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	externalinformer "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	util_net "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/sets"
	utilwait "k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/request/union"
	"k8s.io/apiserver/pkg/authentication/request/x509"
	"k8s.io/apiserver/pkg/authentication/user"
	genericapifilters "k8s.io/apiserver/pkg/endpoints/filters"
	openapinamer "k8s.io/apiserver/pkg/endpoints/openapi"
	"k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/server"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/server/filters"
	genericfilters "k8s.io/apiserver/pkg/server/filters"
	serveroptions "k8s.io/apiserver/pkg/server/options"
	serverstorage "k8s.io/apiserver/pkg/server/storage"
	"k8s.io/apiserver/pkg/storage/etcd3/preflight"
	"k8s.io/apiserver/pkg/util/webhook"
	clidiscovery "k8s.io/client-go/discovery"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/keyutil"
	cliflag "k8s.io/component-base/cli/flag"
	utilflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/cli/globalflag"
	"k8s.io/component-base/metrics"
	_ "k8s.io/component-base/metrics/prometheus/workqueue" // for workqueue metric registration
	"k8s.io/component-base/term"
	"k8s.io/component-base/version"
	"k8s.io/component-base/version/verflag"
	"k8s.io/klog"
	aggregatorapiserver "k8s.io/kube-aggregator/pkg/apiserver"
	aggregatorscheme "k8s.io/kube-aggregator/pkg/apiserver/scheme"
	openapicommon "k8s.io/kube-openapi/pkg/common"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	"k8s.io/kubernetes/pkg/capabilities"
	master "k8s.io/kubernetes/pkg/controlplane"
	controlplaneapiserver "k8s.io/kubernetes/pkg/controlplane/apiserver"
	controlplaneoptions "k8s.io/kubernetes/pkg/controlplane/apiserver/options"
	"k8s.io/kubernetes/pkg/controlplane/reconcilers"
	"k8s.io/kubernetes/pkg/kubeapiserver"
	kubeoptions "k8s.io/kubernetes/pkg/kubeapiserver/options"
	"k8s.io/kubernetes/pkg/routes"
	"k8s.io/kubernetes/pkg/serviceaccount"

	ownedopenapi "github.com/fivetime/kubezoo-contract/pkg/apis/generated/openapi"
	quotav1alpha1 "github.com/fivetime/kubezoo-contract/pkg/apis/quota/v1alpha1"
	_ "github.com/fivetime/kubezoo-contract/pkg/apis/tenant/install"
	"github.com/fivetime/kubezoo-contract/pkg/common"
	"github.com/fivetime/kubezoo-contract/pkg/dynamic"
	"github.com/fivetime/kubezoo-contract/pkg/generated/clientset/versioned"
	quotaclient "github.com/fivetime/kubezoo-contract/pkg/generated/clientset/versioned/typed/quota/v1alpha1"
	"github.com/fivetime/kubezoo-contract/pkg/generated/informers/externalversions"
	tenantlister "github.com/fivetime/kubezoo-contract/pkg/generated/listers/tenant/v1alpha1"
	"github.com/fivetime/kubezoo-contract/pkg/util"

	"github.com/fivetime/kubezoo-gateway/cmd/kubezoo/app/options"
	proxiedopenapi "github.com/fivetime/kubezoo-gateway/pkg/apis/openapi"
	"github.com/fivetime/kubezoo-gateway/pkg/convert"
	tenantfilters "github.com/fivetime/kubezoo-gateway/pkg/filters"
	"github.com/fivetime/kubezoo-gateway/pkg/proxy"
	tenantrest "github.com/fivetime/kubezoo-gateway/pkg/rest"
)

const (
	etcdRetryLimit    = 60
	etcdRetryInterval = 1 * time.Second
)

// NewAPIServerCommand creates a *cobra.Command object with default parameters
func NewAPIServerCommand() *cobra.Command {
	s := options.NewServerRunOptions()
	cmd := &cobra.Command{
		Use: "kube-zoo",
		Long: `The Kubernetes API server validates and configures data
for the api objects which include pods, services, replicationcontrollers, and
others. The API Server services REST operations and provides the frontend to the
cluster's shared state through which all other components interact.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			verflag.PrintAndExitIfRequested()
			utilflag.PrintFlags(cmd.Flags())

			// set default options
			completedOptions, err := Complete(s)
			if err != nil {
				return err
			}

			// validate options
			if errs := completedOptions.Validate(); len(errs) != 0 {
				return utilerrors.NewAggregate(errs)
			}

			return Run(completedOptions, genericapiserver.SetupSignalHandler())
		},
	}

	fs := cmd.Flags()
	namedFlagSets := s.Flags()
	verflag.AddFlags(namedFlagSets.FlagSet("global"))
	globalflag.AddGlobalFlags(namedFlagSets.FlagSet("global"), cmd.Name())
	// AddCustomGlobalFlags used to re-register flags that internal packages
	// pushed into the global "flag" flagset. Nothing does that any more: the
	// in-tree GCE provider was deleted, and default-{not-ready,unreachable}-
	// toleration-seconds moved into AdmissionOptions.AddFlags, which s.Flags()
	// above already calls. Re-registering them here now panics, because they
	// are no longer in the global flagset to look up.
	for _, f := range namedFlagSets.FlagSets {
		fs.AddFlagSet(f)
	}

	usageFmt := "Usage:\n  %s\n"
	cols, _, _ := term.TerminalSize(cmd.OutOrStdout())
	cmd.SetUsageFunc(func(cmd *cobra.Command) error {
		fmt.Fprintf(cmd.OutOrStderr(), usageFmt, cmd.UseLine())
		cliflag.PrintSections(cmd.OutOrStderr(), namedFlagSets, cols)
		return nil
	})
	cmd.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		fmt.Fprintf(cmd.OutOrStdout(), "%s\n\n"+usageFmt, cmd.Long, cmd.UseLine())
		cliflag.PrintSections(cmd.OutOrStdout(), namedFlagSets, cols)
	})

	return cmd
}

// openAPIDefinitions is the union of the two generated definition sets: the
// Kubernetes types kubezoo proxies, and the types this repository owns such as
// Tenant. Both are served, so both have to be here.
//
// Only the proxied set used to be wired up. That was survivable while the config
// only fed the /openapi endpoint, but 1.36 builds a field-management type
// converter over every installed resource, and refuses to start when a model is
// missing -- which for tenants it was.
func openAPIDefinitions(ref openapicommon.ReferenceCallback) map[string]openapicommon.OpenAPIDefinition {
	defs := proxiedopenapi.GetOpenAPIDefinitions(ref)
	for name, def := range ownedopenapi.GetOpenAPIDefinitions(ref) {
		defs[name] = def
	}
	return defs
}

// applyTypeConverter reads objects against the schemas kubezoo serves, so that
// the fields a server-side apply owns can be lifted back out of a converted
// object and forwarded upstream as an apply. See pkg/proxy/apply.go.
//
// Built from the same definitions the OpenAPI config uses, so a resource kubezoo
// serves is a resource this knows the shape of.
func applyTypeConverter() (managedfields.TypeConverter, error) {
	namer := openapinamer.NewDefinitionNamer(legacyscheme.Scheme, extensionsapiserver.Scheme, aggregatorscheme.Scheme)
	config := genericapiserver.DefaultOpenAPIV3Config(openAPIDefinitions, namer)

	// The generated definitions on their own are not enough: the converter finds
	// a type by its group, version and kind, and that mapping lives in an
	// extension the builder adds rather than in the generated code. Handing it
	// the raw definitions produced "no corresponding type for /v1, Kind=ConfigMap"
	// for every apply.
	//
	// The names are the canonical Go type names of the versioned objects, and
	// they come from what kubezoo serves rather than from everything the scheme
	// knows: the scheme carries types no resource is installed for, such as
	// AdmissionReview, and the builder fails on the first one it has no
	// definition for.
	seen := map[string]bool{}
	names := make([]string, 0)
	collect := func(group apiconfig.APIGroupConfig) {
		for _, resources := range group.StorageConfigs {
			for _, config := range resources {
				if config == nil || config.IsConnecter || config.Kind.Empty() {
					continue
				}
				object, err := legacyscheme.Scheme.New(config.Kind)
				if err != nil {
					continue
				}
				name := openapiutil.GetCanonicalTypeName(object)
				if seen[name] {
					continue
				}
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	collect(legacyGroup)
	for i := range nonLegacyGroups {
		collect(nonLegacyGroups[i])
	}

	schemas, err := openapibuilder3.BuildOpenAPIDefinitionsForResources(config, names...)
	if err != nil {
		return nil, err
	}
	return managedfields.NewTypeConverter(schemas, false)
}

// Run runs the specified APIServer.  This should never exit.
func Run(completeOptions completedServerRunOptions, stopCh <-chan struct{}) error {
	// To help debugging, immediately log version
	klog.Infof("Version: %+v", version.Get())

	server, err := CreateServerChain(completeOptions, stopCh)
	if err != nil {
		return err
	}

	prepared := server.ControlPlane.GenericAPIServer.PrepareRun()

	return prepared.Run(stopCh)
}

// CreateServerChain creates the apiservers connected via delegation.
func CreateServerChain(completedOptions completedServerRunOptions, stopCh <-chan struct{}) (*master.Instance, error) {
	kubeAPIServerConfig, _, serviceResolver, proxyConfig, controlPlaneConfig, err := CreateKubeAPIServerConfig(completedOptions)
	if err != nil {
		return nil, err
	}

	// If additional API servers are added, they should be gated.
	apiExtensionsConfig, err := createAPIExtensionsConfig(*kubeAPIServerConfig.ControlPlane.Generic, kubeAPIServerConfig.ControlPlane.VersionedInformers, completedOptions.ServerRunOptions, completedOptions.MasterCount,
		serviceResolver, webhook.NewDefaultAuthenticationInfoResolverWrapper(nil, kubeAPIServerConfig.ControlPlane.Generic.EgressSelector, kubeAPIServerConfig.ControlPlane.Generic.LoopbackClientConfig,
			kubeAPIServerConfig.ControlPlane.Generic.TracerProvider))
	if err != nil {
		return nil, err
	}
	apiExtensionsServer, err := createKubeAPIExtensionsServer(apiExtensionsConfig, genericapiserver.NewEmptyDelegate(), proxyConfig)
	if err != nil {
		return nil, err
	}

	kubeZooServer, err := CreateKubeZooServer(kubeAPIServerConfig, apiExtensionsServer.GenericAPIServer, proxyConfig, controlPlaneConfig)
	if err != nil {
		return nil, err
	}
	return kubeZooServer, nil
}

// InstallLegacyAPI will install the legacy APIs if they are enabled.
func InstallLegacyAPI(m *master.Instance,
	apiResourceConfigSource serverstorage.APIResourceConfigSource,
	c *master.CompletedConfig,
	restOptionsGetter generic.RESTOptionsGetter,
	legacyConfig apiconfig.APIGroupConfig) error {
	legacyProviders, err := proxy.NewRESTStorageProviders(legacyConfig)
	if err != nil {
		return err
	}

	restStorageBuilder := legacyProviders[0]
	groupName := restStorageBuilder.GroupName()
	if !apiResourceConfigSource.AnyResourceForGroupEnabled(groupName) {
		klog.V(1).Infof("Skipping disabled API group %q.", groupName)
		return nil
	}
	apiGroupInfo, err := restStorageBuilder.NewRESTStorage(
		apiResourceConfigSource, restOptionsGetter)
	if err != nil {
		return fmt.Errorf("problem initializing API group %q : %v",
			groupName, err)
	}
	klog.V(1).Infof("Enabling API group %q.", groupName)

	if postHookProvider, ok := restStorageBuilder.(genericapiserver.PostStartHookProvider); ok {
		name, hook, err := postHookProvider.PostStartHook()
		if err != nil {
			klog.Fatalf("Error building PostStartHook: %v", err)
		}
		m.ControlPlane.GenericAPIServer.AddPostStartHookOrDie(name, hook)
	}

	if err := m.ControlPlane.GenericAPIServer.InstallLegacyAPIGroup(
		genericapiserver.DefaultLegacyAPIPrefix, &apiGroupInfo); err != nil {
		return fmt.Errorf("error in registering group versions: %v", err)
	}
	return nil
}

// CreateKubeZooServer creates and wires a workable kube-zoo-apiserver
func CreateKubeZooServer(kubeAPIServerConfig *master.Config,
	delegateAPIServer genericapiserver.DelegationTarget,
	proxyConfig *ProxyConfig,
	controlPlaneConfig *ControlPlaneConfig) (*master.Instance, error) {
	c := kubeAPIServerConfig.Complete()
	// disable admission
	c.ControlPlane.Generic.AdmissionControl = nil
	s, err := c.ControlPlane.Generic.New("kube-zoo-server", delegateAPIServer)
	if err != nil {
		return nil, err
	}

	if c.ControlPlane.EnableLogsSupport {
		routes.Logs{}.Install(s.Handler.GoRestfulContainer)
	}
	m := &master.Instance{
		ControlPlane: &controlplaneapiserver.Server{
			GenericAPIServer:          s,
			ClusterAuthenticationInfo: c.ControlPlane.ClusterAuthenticationInfo,
			// InstallAPIs reads these off the server now instead of taking them
			// as arguments.
			APIResourceConfigSource: kubeAPIServerConfig.ControlPlane.APIResourceConfigSource,
			RESTOptionsGetter:       c.ControlPlane.Generic.RESTOptionsGetter,
		},
	}

	proxyConfig.ApplyToGroup(&legacyGroup)
	if err := InstallLegacyAPI(
		m, kubeAPIServerConfig.ControlPlane.APIResourceConfigSource,
		&c, c.ControlPlane.Generic.RESTOptionsGetter, legacyGroup); err != nil {
		return nil, err
	}

	for i := range nonLegacyGroups {
		proxyConfig.ApplyToGroup(&nonLegacyGroups[i])
	}
	providers, err := proxy.NewRESTStorageProviders(nonLegacyGroups...)
	if err != nil {
		return nil, err
	}

	providers = append(providers, tenantrest.RESTStorageProvider{})

	if err := m.ControlPlane.InstallAPIs(providers...); err != nil {
		return nil, err
	}

	// The tenant controller used to be started here. It is its own binary now --
	// kubezoo-controller -- because an apiserver is all-active and a controller
	// is not: every replica of this process would have run one, all reconciling
	// the same tenants. Splitting them is what makes the replica count of each
	// a separate decision.
	//
	// ⚠️ Deployment consequence: kubezoo alone no longer creates a tenant's
	// namespaces or issues its RoleBindings. A cluster running only this will
	// accept Tenant objects and do nothing with them.
	// ⭐ Start it here. Nothing else does any more, and nothing used to either --
	// the informer got started as a side effect of being handed to
	// controller.Run, which did `go tc.tenantInformer.Run(stopCh)`. Taking the
	// controller out of this process took the gateway's own informer with it.
	//
	// What that broke is not the controller's work, which moved wholesale and is
	// fine. It is WithTenantSuspension: its lister was permanently empty, so
	// suspensionFor found no tenant and treated every one as not suspended. The
	// filter fails open by design, so nothing logged and nothing errored -- a
	// frozen tenant's kubectl was simply refused by upstream RBAC instead, which
	// looks close enough to working to pass a casual read. The hook below waits
	// for a sync that was never going to happen, so even the readiness gate said
	// nothing.
	m.ControlPlane.GenericAPIServer.AddPostStartHookOrDie("start-tenant-informer", func(context genericapiserver.PostStartHookContext) error {
		controlPlaneConfig.tenantInformers.Start(context.Done())
		return nil
	})
	m.ControlPlane.GenericAPIServer.AddPostStartHookOrDie("tenant-informer-synced", func(context genericapiserver.PostStartHookContext) error {
		return utilwait.PollImmediateUntil(100*time.Millisecond, func() (bool, error) {
			return controlPlaneConfig.tenantInformers.Tenant().V1alpha1().Tenants().Informer().HasSynced(), nil
		}, context.Done())
	})

	// The published-class caches, and a readiness gate on them. Without the
	// gate a tenant listing storage classes during the first seconds after a
	// restart is told, truthfully as far as the cache knows, that there are
	// none -- and a tenant cannot tell that answer from "the platform published
	// nothing". Reporting healthy only once the caches are filled is the same
	// reasoning the CRD informer's gate is written for.
	if proxyConfig.classInformers != nil {
		m.ControlPlane.GenericAPIServer.AddPostStartHookOrDie("start-published-class-informers",
			func(context genericapiserver.PostStartHookContext) error {
				proxyConfig.classInformers.Start(context.Done())
				proxyConfig.ingressClassInformers.Start(context.Done())
				proxyConfig.volumeAttributesClassInformers.Start(context.Done())
				return nil
			})
		m.ControlPlane.GenericAPIServer.AddPostStartHookOrDie("published-class-informers-synced",
			func(context genericapiserver.PostStartHookContext) error {
				return utilwait.PollImmediateUntil(100*time.Millisecond, func() (bool, error) {
					return proxyConfig.publishedStorageClasses.HasSynced() &&
						proxyConfig.publishedIngressClasses.HasSynced() &&
						proxyConfig.publishedVolumeAttributesClasses.HasSynced(), nil
				}, context.Done())
			})
	}

	return m, nil
}

// CreateKubeAPIServerConfig creates all the resources for running the API server, but runs none of them
func CreateKubeAPIServerConfig(
	s completedServerRunOptions,
) (
	*master.Config,
	*genericapiserver.DeprecatedInsecureServingInfo,
	aggregatorapiserver.ServiceResolver,
	*ProxyConfig,
	*ControlPlaneConfig,
	error,
) {
	genericConfig, insecureServingInfo, serviceResolver, _, storageFactory, proxyConfig, controlPlaneConfig, err := buildGenericConfig(s.ServerRunOptions)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}

	schemes := []string{"http", "https"}
	etcdEndpoint := s.Etcd.StorageConfig.Transport.ServerList[0]
	for _, scheme := range schemes {
		etcdEndpoint = strings.TrimPrefix(etcdEndpoint, scheme+"://")
	}

	if _, port, err := net.SplitHostPort(etcdEndpoint); err == nil && port != "0" && len(port) != 0 {
		etcdConnection := preflight.EtcdConnection{ServerList: s.Etcd.StorageConfig.Transport.ServerList}
		if err := utilwait.PollImmediate(etcdRetryInterval, etcdRetryLimit*etcdRetryInterval, etcdConnection.CheckEtcdServers); err != nil {
			return nil, nil, nil, nil, nil, fmt.Errorf("error waiting for etcd connection: %v", err)
		}

	}

	capabilities.Initialize(capabilities.Capabilities{
		AllowPrivileged: s.AllowPrivileged,
		PrivilegedSources: capabilities.PrivilegedSources{
			HostNetworkSources: []string{},
			HostPIDSources:     []string{},
			HostIPCSources:     []string{},
		},
		PerConnectionBandwidthLimitBytesPerSec: s.MaxConnectionBytesPerSec,
	})

	if len(s.ShowHiddenMetricsForVersion) > 0 {
		metrics.SetShowHidden()
	}

	serviceIPRange, apiServerServiceIP, err := controlplaneoptions.ServiceIPRange(s.PrimaryServiceClusterIPRange)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}

	// defaults to empty range and ip
	var secondaryServiceIPRange net.IPNet
	// process secondary range only if provided by user
	if s.SecondaryServiceClusterIPRange.IP != nil {
		secondaryServiceIPRange, _, err = controlplaneoptions.ServiceIPRange(s.SecondaryServiceClusterIPRange)
		if err != nil {
			return nil, nil, nil, nil, nil, err
		}
	}

	config := &master.Config{
		ControlPlane: controlplaneapiserver.Config{
			Generic: genericConfig,
			Extra: controlplaneapiserver.Extra{
				APIResourceConfigSource: storageFactory.APIResourceConfigSource,
				StorageFactory:          storageFactory,
				EventTTL:                s.EventTTL,
				EnableLogsSupport:       s.EnableLogsHandler,

				ServiceAccountIssuer:        s.ServiceAccountIssuer,
				ServiceAccountMaxExpiration: s.ServiceAccountTokenMaxExpiration,
			},
		},
		Extra: master.Extra{
			KubeletClientConfig:     s.KubeletConfig,
			ServiceIPRange:          serviceIPRange,
			APIServerServiceIP:      apiServerServiceIP,
			SecondaryServiceIPRange: secondaryServiceIPRange,

			APIServerServicePort: 443,

			ServiceNodePortRange:      s.ServiceNodePortRange,
			KubernetesServiceNodePort: s.KubernetesServiceNodePort,

			EndpointReconcilerType: reconcilers.Type(s.EndpointReconcilerType),
			MasterCount:            s.MasterCount,
		},
	}

	clientCAProvider, err := s.Authentication.ClientCert.GetClientCAContentProvider()
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	config.ControlPlane.ClusterAuthenticationInfo.ClientCA = clientCAProvider

	requestHeaderConfig, err := s.Authentication.RequestHeader.ToAuthenticationRequestHeaderConfig()
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	if requestHeaderConfig != nil {
		config.ControlPlane.ClusterAuthenticationInfo.RequestHeaderCA = requestHeaderConfig.CAContentProvider
		config.ControlPlane.ClusterAuthenticationInfo.RequestHeaderAllowedNames = requestHeaderConfig.AllowedClientNames
		config.ControlPlane.ClusterAuthenticationInfo.RequestHeaderExtraHeaderPrefixes = requestHeaderConfig.ExtraHeaderPrefixes
		config.ControlPlane.ClusterAuthenticationInfo.RequestHeaderGroupHeaders = requestHeaderConfig.GroupHeaders
		config.ControlPlane.ClusterAuthenticationInfo.RequestHeaderUsernameHeaders = requestHeaderConfig.UsernameHeaders
	}

	// Load the public keys.
	var pubKeys []interface{}
	for _, f := range s.Authentication.ServiceAccounts.KeyFiles {
		keys, err := keyutil.PublicKeysFromFile(f)
		if err != nil {
			return nil, nil, nil, nil, nil, fmt.Errorf("failed to parse key file %q: %v", f, err)
		}
		pubKeys = append(pubKeys, keys...)
	}
	// Plumb the required metadata through ExtraConfig.
	config.ControlPlane.ServiceAccountIssuerURL = s.Authentication.ServiceAccounts.Issuers[0]
	config.ControlPlane.ServiceAccountJWKSURI = s.Authentication.ServiceAccounts.JWKSURI
	publicKeysGetter, err := serviceaccount.StaticPublicKeysGetter(pubKeys)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	config.ControlPlane.ServiceAccountPublicKeysGetter = publicKeysGetter

	return config, insecureServingInfo, serviceResolver, proxyConfig, controlPlaneConfig, nil
}

type ControlPlaneConfig struct {
	tenantClient    versioned.Interface
	tenantInformers externalversions.SharedInformerFactory
}

func buildControlPlaneConfig(loopConfig *rest.Config) (*ControlPlaneConfig, error) {
	tenantClient, err := versioned.NewForConfig(loopConfig)
	if err != nil {
		return nil, err
	}
	tenantInformers := externalversions.NewSharedInformerFactory(tenantClient, 5*time.Minute)
	return &ControlPlaneConfig{
		tenantClient:    tenantClient,
		tenantInformers: tenantInformers,
	}, nil
}

type ProxyConfig struct {
	dynamicClient   dynamic.Interface
	discoveryClient *clidiscovery.DiscoveryClient
	crdClient       *apiextensions.Clientset
	typedClientSet  kubernetes.Interface
	quotaClient     quotaclient.QuotaV1alpha1Interface

	crdInformers externalinformer.SharedInformerFactory

	nativeConvertor common.ObjectConvertor
	customConvertor common.ObjectConvertor

	typeConverter managedfields.TypeConverter

	proxyTransport http.RoundTripper
	upstreamMaster *url.URL

	// publishedStorageClasses answers which storage classes the platform offers,
	// from a label on the objects plus whatever --public-storage-classes named.
	// Never nil once the config is built: publishing nothing is a real answer,
	// and a nil would send storageclasses down the tenant-proxy path, which would
	// prefix names that belong to the platform.
	publishedStorageClasses publishedclass.Set
	// publishedIngressClasses is the same for ingress classes, consumed by
	// pkg/convert's ingress transformer rather than by a storage: a tenant may
	// have IngressClasses of its own, so what the label decides there is which
	// names pass through unprefixed.
	publishedIngressClasses publishedclass.Set
	// publishedVolumeAttributesClasses is the third: consumed both by the
	// read-only view and by the PVC endpoint, which refuses a claim naming one
	// that is not published.
	publishedVolumeAttributesClasses publishedclass.Set
	// maxNamespacesPerTenant is a ceiling on the fan-out amplifier: a tenant's
	// cross-namespace list costs one upstream request per namespace it owns.
	maxNamespacesPerTenant int
	// maxClusterRoleBindingsPerTenant is the second multiplier: each binding is
	// stored once per namespace the tenant owns.
	maxClusterRoleBindingsPerTenant int
	// classInformers backs both. Started and waited for in a post-start hook, so
	// that no tenant is told a published class does not exist because the cache
	// was still filling.
	classInformers                 informers.SharedInformerFactory
	ingressClassInformers          informers.SharedInformerFactory
	volumeAttributesClassInformers informers.SharedInformerFactory
}

func (c *ProxyConfig) ApplyToGroup(group *apiconfig.APIGroupConfig) {
	for version := range group.StorageConfigs {
		for resource := range group.StorageConfigs[version] {
			c.ApplyToStorage(group.StorageConfigs[version][resource])
		}
	}
}

func (c *ProxyConfig) ApplyToStorage(config *apiconfig.StorageConfig) {
	config.DynamicClient = c.dynamicClient
	// The platform's own classes, published read-only. Set even when the operator
	// published none: a non-nil Set is what makes the storage serve the resource
	// and show nothing, rather than falling through to the tenant proxy and
	// prefixing names that are not the tenant's.
	if config.Kind.Group == "storage.k8s.io" && config.Resource == "storageclasses" {
		config.PublishedClasses = c.publishedStorageClasses
	}
	if config.Kind.Group == "storage.k8s.io" && config.Resource == "volumeattributesclasses" {
		config.PublishedClasses = c.publishedVolumeAttributesClasses
	}
	// ⚠️ A different field, on a different resource, doing a different thing:
	// this leaves the PVC endpoint an ordinary tenant proxy and only lets it
	// refuse a CREATE that names a retired class. Assigning PublishedClasses here
	// instead would replace the PVC endpoint with a read-only list of storage
	// classes, and it would compile.
	// The subresource check is not decoration: persistentvolumeclaims/status
	// carries the same Resource string, and a status write is not a claim.
	if config.Kind.Group == "" && config.Resource == "namespaces" && config.Subresource == "" {
		config.MaxNamespaces = c.maxNamespacesPerTenant
	}
	if config.Kind.Group == "rbac.authorization.k8s.io" &&
		config.Resource == "clusterrolebindings" && config.Subresource == "" {
		config.MaxClusterRoleBindings = c.maxClusterRoleBindingsPerTenant
	}
	if config.Kind.Group == "" && config.Resource == "persistentvolumeclaims" &&
		config.Subresource == "" {
		config.PublishedStorageClasses = c.publishedStorageClasses
		config.PublishedVolumeAttributesClasses = c.publishedVolumeAttributesClasses
	}
	config.TypeConverter = c.typeConverter
	config.ProxyTransport = c.proxyTransport
	config.UpstreamMaster = c.upstreamMaster
	if config.IsCustomResource {
		config.Convertor = c.customConvertor
	} else {
		config.Convertor = c.nativeConvertor
	}
}

func buildProxyConfig(o *options.ProxyOptions) (*ProxyConfig, error) {
	upstreamConfig, err := clientcmd.BuildConfigFromFlags(o.UpstreamMaster, "")
	if err != nil {
		return nil, err
	}
	upstreamConfig.CAFile = o.ProxyClientCAFile
	upstreamConfig.KeyFile = o.ProxyClientKeyFile
	upstreamConfig.CertFile = o.ProxyClientCertFile
	upstreamConfig.QPS = o.ProxyClientQPS
	upstreamConfig.Burst = o.ProxyClientBurst
	dynamicClient, err := dynamic.NewForConfig(upstreamConfig)
	if err != nil {
		return nil, err
	}
	discoveryClient, err := clidiscovery.NewDiscoveryClientForConfig(upstreamConfig)
	if err != nil {
		return nil, err
	}
	crdClient, err := apiextensions.NewForConfig(upstreamConfig)
	if err != nil {
		return nil, err
	}
	typedClientSet, err := kubernetes.NewForConfig(upstreamConfig)
	if err != nil {
		return nil, err
	}

	crdInformers := externalinformer.NewSharedInformerFactory(crdClient, 5*time.Minute)
	crdLister := crdInformers.Apiextensions().V1().CustomResourceDefinitions().Lister()

	var clusterQuotaClient quotaclient.QuotaV1alpha1Interface
	apiResourceList, err := discoveryClient.ServerResourcesForGroupVersion(quotav1alpha1.GroupVersion.String())
	if err == nil {
		for _, resource := range apiResourceList.APIResources {
			klog.Infof("find resource group=%v verions=%v  kind=%v", resource.Group, resource.Version, resource.Kind)
			if resource.Name == "clusterresourcequotas" {
				var nerr error
				clusterQuotaClient, nerr = quotaclient.NewForConfig(upstreamConfig)
				if nerr != nil {
					klog.Warningf("failed to init cluster quota client with error %v", err)
					return nil, err
				}
				break
			}
		}
		if clusterQuotaClient == nil {
			klog.Warningf("upstream cluster does not have a resource 'clusterresourcequotas'")
		}
	} else if errors.IsNotFound(err) {
		klog.Warningf("upstream cluster does not have a resource 'clusterresourcequotas'")
	} else {
		klog.Warningf("failed to init cluster quota client with discovery error %v", err)
		return nil, err
	}

	checkGroupKind := util.NewCheckGroupKindFunc(crdLister)
	listTenantCRDs := convert.ListTenantCRDsFunc(func(tenantID string) ([]*apiextensionsv1.CustomResourceDefinition, error) {
		return util.ListCRDsForTenant(tenantID, crdLister)
	})
	// Label-selected, so each store holds exactly the published objects. The
	// resync is long: the label is watched, and a resync is only a backstop.
	classInformers := informers.NewSharedInformerFactoryWithOptions(typedClientSet, 10*time.Minute,
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.LabelSelector = publishedclass.PublishedSelector(
				common.StorageClassPublishedLabelKey).String()
		}))
	storageClassInformer := classInformers.Storage().V1().StorageClasses().Informer()
	// A second factory: the two resources carry different labels, and one
	// factory has one tweak.
	ingressClassInformers := informers.NewSharedInformerFactoryWithOptions(typedClientSet, 10*time.Minute,
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.LabelSelector = publishedclass.PublishedSelector(
				common.IngressClassPublishedLabelKey).String()
		}))
	ingressClassInformer := ingressClassInformers.Networking().V1().IngressClasses().Informer()
	// A third, for the same reason as the second: one factory carries one tweak,
	// and these three resources carry three different labels.
	volumeAttributesClassInformers := informers.NewSharedInformerFactoryWithOptions(typedClientSet, 10*time.Minute,
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.LabelSelector = publishedclass.PublishedSelector(
				common.VolumeAttributesClassPublishedLabelKey).String()
		}))
	volumeAttributesClassInformer := volumeAttributesClassInformers.Storage().V1().VolumeAttributesClasses().Informer()

	publishedStorageClasses := publishedclass.New("storageclass",
		common.StorageClassPublishedLabelKey,
		storageClassInformer.GetStore(), storageClassInformer.HasSynced, o.PublicStorageClasses)
	publishedIngressClasses := publishedclass.New("ingressclass",
		common.IngressClassPublishedLabelKey,
		ingressClassInformer.GetStore(), ingressClassInformer.HasSynced, o.PublicIngressClasses)
	// ⭐ No flag counterpart, deliberately: there is nothing to stay compatible
	// with, because nothing validated this field before. Publishing none is the
	// default and means no tenant can set spec.volumeAttributesClassName at all.
	publishedVolumeAttributesClasses := publishedclass.New("volumeattributesclass",
		common.VolumeAttributesClassPublishedLabelKey,
		volumeAttributesClassInformer.GetStore(), volumeAttributesClassInformer.HasSynced, nil)

	nativeConvertor, customConvertor := convert.InitConvertors(checkGroupKind, listTenantCRDs, publishedIngressClasses)

	// construct transport for connect proxy round trip
	proxyTransport, err := rest.TransportFor(upstreamConfig)
	if err != nil {
		return nil, err
	}
	tlsConfig, err := util_net.TLSClientConfig(proxyTransport)
	if err == nil && tlsConfig != nil {
		// since http2 doesn't support websocket, we need to disable http2 when using websocket
		if supportsHTTP11(tlsConfig.NextProtos) {
			tlsConfig.NextProtos = []string{"http/1.1"}
		}
	}
	upstreamMaster, err := url.Parse(o.UpstreamMaster)
	if err != nil {
		return nil, err
	}

	typeConverter, err := applyTypeConverter()
	if err != nil {
		return nil, fmt.Errorf("building the type converter server-side apply needs: %w", err)
	}

	return &ProxyConfig{
		typeConverter:   typeConverter,
		dynamicClient:   dynamicClient,
		discoveryClient: discoveryClient,
		crdClient:       crdClient,
		typedClientSet:  typedClientSet,
		crdInformers:    crdInformers,
		quotaClient:     clusterQuotaClient,
		nativeConvertor: nativeConvertor,
		customConvertor: customConvertor,
		proxyTransport:  proxyTransport,
		upstreamMaster:  upstreamMaster,

		publishedStorageClasses:          publishedStorageClasses,
		publishedIngressClasses:          publishedIngressClasses,
		publishedVolumeAttributesClasses: publishedVolumeAttributesClasses,
		maxNamespacesPerTenant:           o.MaxNamespacesPerTenant,
		maxClusterRoleBindingsPerTenant:  o.MaxClusterRoleBindingsPerTenant,
		classInformers:                   classInformers,
		ingressClassInformers:            ingressClassInformers,
		volumeAttributesClassInformers:   volumeAttributesClassInformers,
	}, nil
}

// copy from https://github.com/kubernetes/apimachinery/blob/master/pkg/util/proxy/dial.go.
func supportsHTTP11(nextProtos []string) bool {
	if len(nextProtos) == 0 {
		return true
	}
	for _, proto := range nextProtos {
		if proto == "http/1.1" {
			return true
		}
	}
	return false
}

// BuildGenericConfig takes the master server options and produces the genericapiserver.Config associated with it
func buildGenericConfig(
	s *options.ServerRunOptions,
) (
	genericConfig *genericapiserver.Config,
	insecureServingInfo *genericapiserver.DeprecatedInsecureServingInfo,
	serviceResolver aggregatorapiserver.ServiceResolver,
	admissionPostStartHook genericapiserver.PostStartHookFunc,
	storageFactory *serverstorage.DefaultStorageFactory,
	proxyConfig *ProxyConfig,
	controlPlaneConfig *ControlPlaneConfig,
	lastErr error,
) {
	genericConfig = genericapiserver.NewConfig(legacyscheme.Codecs)
	// install resource config without any resource
	genericConfig.MergedResourceConfig = serverstorage.NewResourceConfig()

	proxyConfig, lastErr = buildProxyConfig(s.Proxy)
	if lastErr != nil {
		return
	}

	var discoveryProxy proxy.DiscoveryProxy
	// The served groups go in so that discovery advertises what is installed
	// rather than everything the scheme knows about.
	discoveryProxy, lastErr = proxy.NewDiscoveryProxy(proxyConfig.discoveryClient,
		proxyConfig.crdInformers.Apiextensions().V1().CustomResourceDefinitions().Lister(),
		ServedAPIGroups())
	if lastErr != nil {
		return
	}
	if lastErr = s.GenericServerRunOptions.ApplyTo(genericConfig); lastErr != nil {
		return
	}

	if lastErr = s.SecureServing.ApplyTo(&genericConfig.SecureServing, &genericConfig.LoopbackClientConfig); lastErr != nil {
		return
	}
	// In 1.24 FeatureOptions.ApplyTo only copied the profiling flags. In 1.36 it
	// also builds the priority-and-fairness filter, for which it needs a core
	// client and an informer factory someone will start. kubezoo has never had
	// P&F -- genericConfig.FlowControl is set nowhere in this tree -- and wiring
	// it would mean pointing a client at our own loopback, which proxies every
	// request upstream and rewrites flowschema names per tenant. Keep the old
	// behaviour explicitly rather than half-wiring it.
	s.Features.EnablePriorityAndFairness = false
	if lastErr = s.Features.ApplyTo(genericConfig, nil, nil); lastErr != nil {
		return
	}
	if lastErr = s.APIEnablement.ApplyTo(genericConfig, master.DefaultAPIResourceConfigSource(), legacyscheme.Scheme); lastErr != nil {
		return
	}
	// enable kubezoo
	genericConfig.MergedResourceConfig.EnableVersions(tenantrest.SchemeGroupVersion)
	if lastErr = s.EgressSelector.ApplyTo(genericConfig); lastErr != nil {
		return
	}
	// ⛔ Without this the audit flags are a lie. AddFlags registers every one of
	// them and Validate checks them, so --audit-policy-file and --audit-log-path
	// are accepted, validated, and then nothing is recorded: ApplyTo is what
	// builds the policy evaluator and the backend, and it was never called. An
	// audit control that silently does nothing is worse than none at all,
	// because somebody is relying on it being there.
	//
	// ⭐ And kubezoo is the only place some of it can be recorded. It
	// impersonates the tenant on everything it forwards, so what a tenant
	// MANAGES to do is attributable in the upstream audit log -- but what
	// pkg/proxy REFUSES never reaches upstream, and neither do writes to Tenant
	// objects, which live in kubezoo's own etcd. Blocked attempts, and the
	// decision to freeze a tenant, exist here or nowhere.
	if lastErr = s.Audit.ApplyTo(genericConfig); lastErr != nil {
		return
	}

	namer := openapinamer.NewDefinitionNamer(legacyscheme.Scheme, extensionsapiserver.Scheme, aggregatorscheme.Scheme)
	genericConfig.OpenAPIConfig = genericapiserver.DefaultOpenAPIConfig(openAPIDefinitions, namer)
	genericConfig.OpenAPIConfig.Info.Title = "Kubernetes"
	// Required since server-side apply went GA: getOpenAPIModels builds the type
	// converter from the V3 config and refuses to start without it. Nothing here
	// dereferences it at compile time, so leaving it nil built and tested clean
	// and only failed when the server actually came up.
	genericConfig.OpenAPIV3Config = genericapiserver.DefaultOpenAPIV3Config(openAPIDefinitions, namer)
	genericConfig.OpenAPIV3Config.Info.Title = "Kubernetes"
	genericConfig.LongRunningFunc = filters.BasicLongRunningRequestCheck(
		sets.NewString("watch", "proxy"),
		sets.NewString("attach", "exec", "proxy", "log", "portforward"),
	)

	// genericConfig.Version is gone in 1.36; the version now travels as
	// genericConfig.EffectiveVersion, which GenericServerRunOptions.ApplyTo has
	// already set above. Upstream's buildGenericConfig no longer sets it here.

	storageFactoryConfig := kubeapiserver.NewStorageFactoryConfig()
	storageFactoryConfig.APIResourceConfig = genericConfig.MergedResourceConfig
	storageFactory, lastErr = storageFactoryConfig.Complete(s.Etcd).New()
	if lastErr != nil {
		return
	}

	if genericConfig.EgressSelector != nil {
		storageFactory.StorageConfig.Transport.EgressLookup = genericConfig.EgressSelector.Lookup
	}
	if lastErr = s.Etcd.ApplyWithStorageFactoryTo(storageFactory, genericConfig); lastErr != nil {
		return
	}

	// Use protobufs for self-communication.
	// Since not every generic apiserver has to support protobufs, we
	// cannot default to it in generic apiserver and need to explicitly
	// set it in kube-apiserver.
	genericConfig.LoopbackClientConfig.ContentConfig.ContentType = "application/vnd.kubernetes.protobuf"
	// Disable compression for self-communication, since we are going to be
	// on a fast local network
	genericConfig.LoopbackClientConfig.DisableCompression = true

	controlPlaneConfig, lastErr = buildControlPlaneConfig(genericConfig.LoopbackClientConfig)
	if lastErr != nil {
		return
	}

	// Built here rather than beside the discovery proxy above, because the
	// suspension filter needs the tenant lister and the informers do not exist
	// until the control plane config does. The chain function is not called
	// until the server is assembled, so its position here is free.
	genericConfig.BuildHandlerChainFunc = NewBuildHandlerChanFunc(discoveryProxy,
		controlPlaneConfig.tenantInformers.Tenant().V1alpha1().Tenants().Lister())

	// The upstream client goes in so that ServiceAccount tokens can be
	// authenticated: validating a bound token means reading the pod and service
	// account it names, and those live upstream under their real names.
	if lastErr = applyAuthenticationOptions(utilwait.ContextForChannel(genericConfig.DrainedNotify()),
		s.Authentication, genericConfig, proxyConfig.typedClientSet); lastErr != nil {
		return
	}

	ac, _ := s.Authentication.ToAuthenticationConfig()
	if ac.ClientCAContentProvider != nil {
		// append the authentication handler that will extract tenant ID from
		// the x509 certificate
		ta := x509.NewDynamic(ac.ClientCAContentProvider.VerifyOptions,
			CommonNameUserConversion)
		genericConfig.Authentication.Authenticator = union.New(ta,
			genericConfig.Authentication.Authenticator)
	}

	// ToAuthorizationConfig gained an error return in 1.36, and New now takes a
	// server-lifecycle context plus the apiserver ID. The nil informer factory
	// is pre-existing: kubezoo authorizes with AlwaysAllow and defers real
	// authorization to the upstream apiserver, so no RBAC informers are needed.
	authorizationConfig, err := s.Authorization.ToAuthorizationConfig(nil)
	if err != nil {
		lastErr = fmt.Errorf("invalid authorization config: %v", err)
		return
	}
	if authorizationConfig != nil {
		ctx := utilwait.ContextForChannel(genericConfig.DrainedNotify())
		genericConfig.Authorization.Authorizer, genericConfig.RuleResolver, err = authorizationConfig.New(ctx, genericConfig.APIServerID)
		if err != nil {
			lastErr = fmt.Errorf("invalid authorization config: %v", err)
			return
		}
	}
	return
}

func applyAuthenticationOptions(ctx context.Context, o *kubeoptions.BuiltInAuthenticationOptions,
	genericConfig *server.Config, upstreamClient kubernetes.Interface) error {
	authenticatorConfig, err := o.ToAuthenticationConfig()
	if err != nil {
		return err
	}

	// Without this, kubezoo cannot authenticate a ServiceAccount token at all.
	// ToAuthenticationConfig leaves the getter nil, and upstream fills it in
	// from its informers; every token the kubelet projects is bound to a pod,
	// and validating a bound token needs to read that pod. Missing, the
	// validator returns "authentication failed unexpectedly" and a tenant's own
	// workload cannot reach kubezoo -- which is the only way a workload gets the
	// tenant's view of the API rather than the upstream one.
	//
	// The tenant is then derived from the ServiceAccount's namespace by
	// WithTenantInfo, which has been waiting for this.
	if upstreamClient != nil {
		authenticatorConfig.ServiceAccountTokenGetter = newUpstreamTokenGetter(upstreamClient)
		authenticatorConfig.SecretsWriter = upstreamClient.CoreV1()
	}

	authInfo := &genericConfig.Authentication
	secureServing := genericConfig.SecureServing

	if authenticatorConfig.ClientCAContentProvider != nil {
		if err = authInfo.ApplyClientCert(authenticatorConfig.ClientCAContentProvider, secureServing); err != nil {
			return fmt.Errorf("unable to load client CA file: %v", err)
		}
	}
	if authenticatorConfig.RequestHeaderConfig != nil && authenticatorConfig.RequestHeaderConfig.CAContentProvider != nil {
		if err = authInfo.ApplyClientCert(authenticatorConfig.RequestHeaderConfig.CAContentProvider, secureServing); err != nil {
			return fmt.Errorf("unable to load client CA file: %v", err)
		}
	}

	authInfo.APIAudiences = o.APIAudiences
	if o.ServiceAccounts != nil && len(o.ServiceAccounts.Issuers) > 0 && o.ServiceAccounts.Issuers[0] != "" && len(o.APIAudiences) == 0 {
		authInfo.APIAudiences = authenticator.Audiences{o.ServiceAccounts.Issuers[0]}
	}
	// The handler chain now needs the request-header config to strip inbound
	// X-Remote-* headers, so carry it over the way upstream's Authentication
	// .ApplyTo does.
	authInfo.RequestHeaderConfig = authenticatorConfig.RequestHeaderConfig

	// New took no arguments and returned 3 values in 1.24; it now takes a
	// server-lifecycle context and also returns the config reloader and the
	// OpenAPI v3 security schemes, neither of which kubezoo uses.
	authInfo.Authenticator, _, _, _, err = authenticatorConfig.New(ctx)
	return err
}

// completedServerRunOptions is a private wrapper that enforces a call of Complete() before Run can be invoked.
type completedServerRunOptions struct {
	*options.ServerRunOptions
}

// Complete set default ServerRunOptions.
// Should be called after kube-apiserver flags parsed.
func Complete(s *options.ServerRunOptions) (completedServerRunOptions, error) {
	var options completedServerRunOptions
	// set defaults
	if err := s.GenericServerRunOptions.DefaultAdvertiseAddress(s.SecureServing.SecureServingOptions); err != nil {
		return options, err
	}
	if err := s.GenericServerRunOptions.DefaultAdvertiseAddress(s.SecureServing.SecureServingOptions); err != nil {
		return options, err
	}

	// process s.ServiceClusterIPRange from list to Primary and Secondary
	// we process secondary only if provided by user
	apiServerServiceIP, primaryServiceIPRange, secondaryServiceIPRange, err := getServiceIPAndRanges(s.ServiceClusterIPRanges)
	if err != nil {
		return options, err
	}
	s.PrimaryServiceClusterIPRange = primaryServiceIPRange
	s.SecondaryServiceClusterIPRange = secondaryServiceIPRange

	if err := s.SecureServing.MaybeDefaultWithSelfSignedCerts(s.GenericServerRunOptions.AdvertiseAddress.String(), []string{"kubernetes.default.svc", "kubernetes.default", "kubernetes"}, []net.IP{apiServerServiceIP}); err != nil {
		return options, fmt.Errorf("error creating self-signed certificates: %v", err)
	}

	if len(s.GenericServerRunOptions.ExternalHost) == 0 {
		if len(s.GenericServerRunOptions.AdvertiseAddress) > 0 {
			s.GenericServerRunOptions.ExternalHost = s.GenericServerRunOptions.AdvertiseAddress.String()
		} else {
			if hostname, err := os.Hostname(); err == nil {
				s.GenericServerRunOptions.ExternalHost = hostname
			} else {
				return options, fmt.Errorf("error finding host name: %v", err)
			}
		}
		klog.Infof("external host was not specified, using %v", s.GenericServerRunOptions.ExternalHost)
	}

	s.Authentication.ApplyAuthorization(s.Authorization)

	// Use (ServiceAccountSigningKeyFile != "") as a proxy to the user enabling
	// TokenRequest functionality. This defaulting was convenient, but messed up
	// a lot of people when they rotated their serving cert with no idea it was
	// connected to their service account keys. We are taking this opportunity to
	// remove this problematic defaulting.
	if s.ServiceAccountSigningKeyFile == "" {
		// Default to the private server key for service account token signing
		if len(s.Authentication.ServiceAccounts.KeyFiles) == 0 && s.SecureServing.ServerCert.CertKey.KeyFile != "" {
			// kubeauthenticator.IsValidServiceAccountKeyFile is gone in 1.36; it
			// was a one-line wrapper over keyutil.PublicKeysFromFile.
			if _, keyErr := keyutil.PublicKeysFromFile(s.SecureServing.ServerCert.CertKey.KeyFile); keyErr == nil {
				s.Authentication.ServiceAccounts.KeyFiles = []string{s.SecureServing.ServerCert.CertKey.KeyFile}
			} else {
				klog.Warning("No TLS key provided, service account token authentication disabled")
			}
		}
	}

	if s.ServiceAccountSigningKeyFile != "" && len(s.Authentication.ServiceAccounts.Issuers) > 0 &&
		s.Authentication.ServiceAccounts.Issuers[0] != "" {
		sk, err := keyutil.PrivateKeyFromFile(s.ServiceAccountSigningKeyFile)
		if err != nil {
			return options, fmt.Errorf("failed to parse service-account-issuer-key-file: %v", err)
		}
		if s.Authentication.ServiceAccounts.MaxExpiration != 0 {
			lowBound := time.Hour
			upBound := time.Duration(1<<32) * time.Second
			if s.Authentication.ServiceAccounts.MaxExpiration < lowBound ||
				s.Authentication.ServiceAccounts.MaxExpiration > upBound {
				return options, fmt.Errorf("the serviceaccount max expiration must be between 1 hour to 2^32 seconds")
			}
		}

		s.ServiceAccountIssuer, err = serviceaccount.JWTTokenGenerator(s.Authentication.ServiceAccounts.Issuers[0], sk)
		if err != nil {
			return options, fmt.Errorf("failed to build token generator: %v", err)
		}
		s.ServiceAccountTokenMaxExpiration = s.Authentication.ServiceAccounts.MaxExpiration
	}

	if s.Etcd.EnableWatchCache {
		sizes := kubeapiserver.DefaultWatchCacheSizes()
		if userSpecified, err := serveroptions.ParseWatchCacheSizes(s.Etcd.WatchCacheSizes); err == nil {
			for resource, size := range userSpecified {
				sizes[resource] = size
			}
		}
		s.Etcd.WatchCacheSizes, err = serveroptions.WriteWatchCacheSizes(sizes)
		if err != nil {
			return options, err
		}
	}

	if s.APIEnablement.RuntimeConfig != nil {
		for key, value := range s.APIEnablement.RuntimeConfig {
			if key == "v1" || strings.HasPrefix(key, "v1/") ||
				key == "api/v1" || strings.HasPrefix(key, "api/v1/") {
				delete(s.APIEnablement.RuntimeConfig, key)
				s.APIEnablement.RuntimeConfig["/v1"] = value
			}
			if key == "api/legacy" {
				delete(s.APIEnablement.RuntimeConfig, key)
			}
		}
	}
	options.ServerRunOptions = s

	// currently, we use the same ca and ca-key files for tenant and admin
	options.Proxy.ClientCAFile = options.Authentication.ClientCert.ClientCA
	return options, nil
}

// buildServiceResolver used to live here, copied from kube-apiserver and never
// called: kubezoo runs no aggregator and no admission webhooks, so nothing ever
// needed to resolve a Service to a URL. In 1.36 NewEndpointServiceResolver takes
// an EndpointSlice getter instead of an Endpoints lister, so keeping it would
// have meant migrating code no test and no code path exercises -- which is how
// the CRD handler ended up stranded at its fork point. Removed instead; upstream
// buildServiceResolver in cmd/kube-apiserver/app/server.go is the reference if
// aggregation is ever added.

func getServiceIPAndRanges(serviceClusterIPRanges string) (net.IP, net.IPNet, net.IPNet, error) {
	serviceClusterIPRangeList := []string{}
	if serviceClusterIPRanges != "" {
		serviceClusterIPRangeList = strings.Split(serviceClusterIPRanges, ",")
	}

	var apiServerServiceIP net.IP
	var primaryServiceIPRange net.IPNet
	var secondaryServiceIPRange net.IPNet
	var err error
	// nothing provided by user, use default range (only applies to the Primary)
	if len(serviceClusterIPRangeList) == 0 {
		var primaryServiceClusterCIDR net.IPNet
		primaryServiceIPRange, apiServerServiceIP, err = controlplaneoptions.ServiceIPRange(primaryServiceClusterCIDR)
		if err != nil {
			return net.IP{}, net.IPNet{}, net.IPNet{}, fmt.Errorf("error determining service IP ranges: %v", err)
		}
		return apiServerServiceIP, primaryServiceIPRange, net.IPNet{}, nil
	}

	if len(serviceClusterIPRangeList) > 0 {
		_, primaryServiceClusterCIDR, err := net.ParseCIDR(serviceClusterIPRangeList[0])
		if err != nil {
			return net.IP{}, net.IPNet{}, net.IPNet{}, fmt.Errorf("service-cluster-ip-range[0] is not a valid cidr")
		}

		primaryServiceIPRange, apiServerServiceIP, err = controlplaneoptions.ServiceIPRange(*(primaryServiceClusterCIDR))
		if err != nil {
			return net.IP{}, net.IPNet{}, net.IPNet{}, fmt.Errorf("error determining service IP ranges for primary service cidr: %v", err)
		}
	}

	// user provided at least two entries
	// note: validation asserts that the list is max of two dual stack entries
	if len(serviceClusterIPRangeList) > 1 {
		_, secondaryServiceClusterCIDR, err := net.ParseCIDR(serviceClusterIPRangeList[1])
		if err != nil {
			return net.IP{}, net.IPNet{}, net.IPNet{}, fmt.Errorf("service-cluster-ip-range[1] is not an ip net")
		}
		secondaryServiceIPRange = *secondaryServiceClusterCIDR
	}
	return apiServerServiceIP, primaryServiceIPRange, secondaryServiceIPRange, nil
}

func NewBuildHandlerChanFunc(discoveryProxy proxy.DiscoveryProxy,
	tenants tenantlister.TenantLister) func(apiHandler http.Handler, c *server.Config) (secure http.Handler) {
	return func(handler http.Handler, c *genericapiserver.Config) (secure http.Handler) {
		failedHandler := genericapifilters.Unauthorized(c.Serializer)
		handler = tenantfilters.WithDiscoveryProxy(handler, discoveryProxy)
		// Outside the discovery proxy so that a suspended tenant can still
		// discover the API and read the refusal, and inside WithTenantInfo,
		// which is what puts the tenant on the context.
		handler = tenantfilters.WithTenantSuspension(handler, tenants)
		// ⚠️ Inside WithTenantInfo, because it needs the tenant on the context,
		// and it is not optional: this chain installs no authorization filter, so
		// the authorizer built below is never consulted and --authorization-mode
		// does nothing. That is defensible for everything kubezoo proxies, since
		// upstream authorizes those. tenant.kubezoo.io and quota.kubezoo.io have
		// no upstream -- they are served from kubezoo's own etcd -- so without
		// this a tenant could read every other tenant's kubeconfig, private key
		// included, and delete any tenant it liked.
		handler = tenantfilters.WithPlatformAPIGuard(handler)
		handler = tenantfilters.WithTenantInfo(handler)
		// Carries the apply force flag, which is a query parameter the storage
		// layer never sees. See tenantfilters.WithApplyForce.
		handler = tenantfilters.WithApplyForce(handler)
		// Inside authentication and outside everything else, which is where
		// upstream puts it (config.go, WithAudit at 1064 and WithAuthentication
		// at 1075): the audit event has to carry the authenticated identity, and
		// it has to be emitted for requests the filters below go on to refuse.
		handler = genericapifilters.WithAudit(handler, c.AuditBackend, c.AuditPolicyRuleEvaluator, c.LongRunningFunc)
		// A request that fails to authenticate takes the failed handler and never
		// reaches the line above, so it needs its own audit wrapper or repeated
		// attempts with a bad or withdrawn credential leave no trace.
		failedHandler = genericapifilters.WithFailedAuthenticationAudit(failedHandler, c.AuditBackend, c.AuditPolicyRuleEvaluator)
		handler = genericapifilters.WithAuthentication(handler, c.Authentication.Authenticator, failedHandler, c.Authentication.APIAudiences, c.Authentication.RequestHeaderConfig)
		handler = genericfilters.WithCORS(handler, c.CorsAllowedOriginList, nil, nil, nil, "true")
		// Records the warnings the request path emits -- without it AddWarning is
		// a no-op and a tenant is never told that a field it set was dropped.
		// Upstream places it exactly here, inside the timeout handler, so that
		// adding a header stays threadsafe when the timeout fires.
		handler = genericapifilters.WithWarningRecorder(handler)
		handler = genericfilters.WithTimeoutForNonLongRunningRequests(handler, c.LongRunningFunc)
		// HandlerChainWaitGroup was split in 1.29 into a wait group for
		// non-long-running requests and a rate-limited one for watches.
		handler = genericfilters.WithWaitGroup(handler, c.LongRunningFunc, c.NonLongRunningRequestWaitGroup)
		handler = genericapifilters.WithRequestInfo(handler, c.RequestInfoResolver)
		// Upstream's DefaultBuildHandlerChain always installs this, even with
		// auditing switched off, because the audit helpers dereference the
		// context's AuditContext without checking it. This chain is hand-built
		// and omitted it, so any request that logs its body -- PATCH does, via
		// audit.LogRequestPatch -- panicked on a nil pointer. kubectl annotate,
		// label, patch, scale and set image all take that path.
		handler = genericapifilters.WithAuditInit(handler)
		if c.SecureServing != nil && !c.SecureServing.DisableHTTP2 && c.GoawayChance > 0 {
			handler = genericfilters.WithProbabilisticGoaway(handler, c.GoawayChance)
		}
		handler = genericapifilters.WithCacheControl(handler)
		handler = genericfilters.WithPanicRecovery(handler, c.RequestInfoResolver)
		return handler
	}
}

var CommonNameUserConversion = x509.UserConversionFunc(func(chain []*stdx509.Certificate) (*authenticator.Response, bool, error) {
	if len(chain[0].Subject.CommonName) == 0 {
		return nil, false, nil
	}

	OrganizationalUnit := chain[0].Subject.OrganizationalUnit
	CommonName := chain[0].Subject.CommonName

	u := user.DefaultInfo{
		Name:   CommonName,
		Groups: chain[0].Subject.Organization,
	}
	tenantIDLength := 6
	if len(OrganizationalUnit) > 0 {
		if len(OrganizationalUnit[0]) == tenantIDLength && len(CommonName) > tenantIDLength {
			if OrganizationalUnit[0] == CommonName[:tenantIDLength] && CommonName[tenantIDLength] == '-' {
				tenantName := OrganizationalUnit[0]
				u.Extra = map[string][]string{"tenant": {tenantName}}
			}
		}
	}

	return &authenticator.Response{
		User: &u,
	}, true, nil
})
