// Connecting to the cluster, and the scheme the tool reads it through. The
// scheme carries both API versions and the core kinds, because the migration
// reads objects at v1alpha1, writes them at v1alpha2, and reparents core
// objects that belong to neither.

package main

import (
	"fmt"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// newScheme builds the scheme the tool reads and writes through.
//
// CustomResourceDefinition is in it because the upgrade applies CRDs, waits for
// them to be established, and later switches their storage version, and none of
// that is reachable through the typed clients for the group being upgraded.
func newScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(apiextensionsv1.AddToScheme(scheme))
	utilruntime.Must(simplyblockv1alpha1.AddToScheme(scheme))
	utilruntime.Must(simplyblockv1alpha2.AddToScheme(scheme))
	return scheme
}

// newClient connects to the cluster the flags name.
//
// It is a direct client rather than a cached one. A cache would serve a
// migration reads that are a reconcile behind the writes it just made, and the
// verification after every step is the one thing that must not be stale.
func newClient(global *globalOptions) (client.Client, error) {
	config, err := restConfig(global)
	if err != nil {
		return nil, err
	}
	c, err := client.New(config, client.Options{Scheme: newScheme()})
	if err != nil {
		return nil, fmt.Errorf("connecting to the cluster: %w", err)
	}
	return c, nil
}

// restConfig resolves the connection from the flags, the in-cluster
// configuration, and the default loading rules, in that order.
func restConfig(global *globalOptions) (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if global.Kubeconfig != "" {
		rules.ExplicitPath = global.Kubeconfig
	}

	overrides := &clientcmd.ConfigOverrides{}
	if global.Context != "" {
		overrides.CurrentContext = global.Context
	}

	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("no cluster to connect to: %w", err)
	}
	return config, nil
}
