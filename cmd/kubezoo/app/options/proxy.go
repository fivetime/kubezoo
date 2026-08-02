package options

import (
	"fmt"

	"github.com/spf13/pflag"

	"github.com/fivetime/kubezoo-contract/pkg/common"
)

// ProxyOptions runs a kubezoo proxy server
type ProxyOptions struct {
	// ca, cert and key file to build secure connection between kubezoo and upstream cluster
	ProxyClientCAFile   string
	ProxyClientCertFile string
	ProxyClientKeyFile  string

	ProxyClientQPS   float32
	ProxyClientBurst int

	// ClientCAFile is copied from the authentication options; it is the CA that
	// tenant client certificates are verified against. Signing them is the
	// controller's job now, so the signing key is not here and must not be:
	// this process has no reason to hold the root of the tenant trust chain.
	ClientCAFile   string
	UpstreamMaster string
	// PublicIngressClasses are the IngressClass names that reach the platform's
	// own ingress controller, and through it the public internet.
	PublicIngressClasses []string
	// PublicStorageClasses are the StorageClass names the platform publishes to
	// every tenant, read-only and under their real names.
	//
	// A tenant can already NAME a StorageClass -- pkg/convert/pvc.go passes
	// spec.storageClassName through untranslated, so dynamic provisioning works
	// -- but storage.k8s.io is not served, so it has no way to find out which
	// names exist. Empty publishes nothing, which is the safe default: an
	// operator who has not chosen classes has not offered any.
	PublicStorageClasses []string
	// MaxNamespacesPerTenant caps how many namespaces one tenant may own. Zero
	// means no cap, which is what an upgrade gets.
	MaxNamespacesPerTenant int
	ServiceAccountKeyFile  string
}

// NewProxyOptions creates a new ProxyOptions object
func NewProxyOptions() *ProxyOptions {
	return &ProxyOptions{
		ProxyClientQPS:   1000,
		ProxyClientBurst: 2000,
	}
}

func (o *ProxyOptions) AddFlags(fs *pflag.FlagSet) {
	if o == nil {
		return
	}
	fs.StringVar(&o.ProxyClientCAFile, "proxy-client-ca-file", o.ProxyClientCAFile, "proxy client ca file to verify upstream cluster apiserver.")
	fs.StringVar(&o.ProxyClientCertFile, "proxy-client-cert-file", o.ProxyClientCertFile, "proxy client cert file to prove the identity of kubezoo proxy "+
		"server to upstream cluster apiserver.")
	fs.StringVar(&o.ProxyClientKeyFile, "proxy-client-key-file", o.ProxyClientKeyFile, "proxy client key file to prove the identity of kubezoo proxy "+
		"server to upstream cluster apiserver.")
	fs.Float32Var(&o.ProxyClientQPS, "proxy-client-qps", o.ProxyClientQPS,
		fmt.Sprintf("the maximum QPS to the upstream cluster apiserver, default to %v", o.ProxyClientQPS))
	fs.IntVar(&o.ProxyClientBurst, "proxy-client-burst", o.ProxyClientBurst,
		fmt.Sprintf("the maximun burst for thorttle to the upstream cluster apiserver, default to %v", o.ProxyClientBurst))
	fs.StringVar(&o.UpstreamMaster, "proxy-upstream-master", o.UpstreamMaster, "upstream apiserver master address")
	fs.StringSliceVar(&o.PublicIngressClasses, "public-ingress-classes", o.PublicIngressClasses,
		"IngressClass names that reach the platform's own ingress controller, and so the public internet. "+
			"A tenant naming one of these is asking to be exposed; every other class it names is prefixed with "+
			"its tenant id and can only be served by a controller the tenant runs itself. "+
			"Prefer labelling the IngressClass "+common.IngressClassPublishedLabelKey+"=true, which takes "+
			"effect without a restart; this flag is unioned with those and kept so that an upgrade does not "+
			"silently un-publish anything.")
	fs.StringSliceVar(&o.PublicStorageClasses, "public-storage-classes", o.PublicStorageClasses,
		"StorageClass names offered to every tenant: visible read-only under their real names, and the "+
			"only ones a tenant may name in a PersistentVolumeClaim. A claim on anything else is refused, "+
			"so publishing is authorization and not merely discovery -- and so an upgrade must label every "+
			"class already in use BEFORE it lands, or existing tenants stop being able to create claims. "+
			"Leaving storageClassName unset still asks for the cluster default and is never refused. "+
			"Prefer labelling the StorageClass "+common.StorageClassPublishedLabelKey+"=true, which takes "+
			"effect without a restart -- this flag can only be changed by restarting the gateway, which "+
			"interrupts every tenant's API access, and a name misspelled here fails silently. Labelling it "+
			"\""+common.PublishedDeprecated+"\" instead retires it: new PersistentVolumeClaims naming it are "+
			"refused, while it stays visible and every claim that already uses it keeps working, so tenants "+
			"have a window to migrate. This flag is unioned with the labelled set and kept so that an "+
			"upgrade does not silently un-publish anything.")
	fs.IntVar(&o.MaxNamespacesPerTenant, "max-namespaces-per-tenant", o.MaxNamespacesPerTenant,
		"the most namespaces one tenant may own, or 0 for no limit, which is the default. "+
			"This is a ceiling on a shared-cluster amplifier rather than a billing control: a "+
			"cross-namespace list is assembled by reading each of the tenant's namespaces in "+
			"turn, so every `kubectl get pods` a tenant runs costs one upstream request per "+
			"namespace it owns, against the apiserver every tenant shares. Mostly-empty "+
			"namespaces are the worst case, since the walk has to reach them all before it "+
			"can fill a page. Count what tenants have today before setting it -- "+
			"`kubectl get ns -L kubezoo.io/tenant` -- because the limit refuses new "+
			"namespaces the moment it is below what a tenant already owns.")
}

func (o *ProxyOptions) Validate() []error {
	if o == nil {
		return nil
	}

	errors := []error{}

	if len(o.ProxyClientCAFile) == 0 {
		errors = append(errors, fmt.Errorf("--proxy-client-ca-file cannot be empty"))
	}
	if len(o.ProxyClientKeyFile) == 0 {
		errors = append(errors, fmt.Errorf("--proxy-client-key-file cannot be empty"))
	}
	if len(o.ProxyClientCertFile) == 0 {
		errors = append(errors, fmt.Errorf("--proxy-client-cert-file cannot be empty"))
	}
	if len(o.ClientCAFile) == 0 {
		errors = append(errors, fmt.Errorf("--client-ca-file cannot be empty"))
	}
	if len(o.UpstreamMaster) == 0 {
		errors = append(errors, fmt.Errorf("--proxy-upstream-master cannot be empty"))
	}
	return errors
}
