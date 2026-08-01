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
	PublicStorageClasses  []string
	ServiceAccountKeyFile string
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
		"StorageClass names published to every tenant, read-only and under their real names, so that a "+
			"tenant can discover what it may put in a PersistentVolumeClaim's storageClassName. The "+
			"reference already works without this; what it adds is the ability to find out. "+
			"Prefer labelling the StorageClass "+common.StorageClassPublishedLabelKey+"=true, which takes "+
			"effect without a restart -- this flag can only be changed by restarting the gateway, which "+
			"interrupts every tenant's API access, and a name misspelled here fails silently. Labelling it "+
			"\""+common.PublishedDeprecated+"\" instead announces that the class is going away: it stays "+
			"visible, so a tenant can see why its own PersistentVolumeClaim names it. This flag is unioned "+
			"with the labelled set and kept so that an upgrade does not silently un-publish anything.")
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
