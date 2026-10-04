// Probing a worker's storage-node API.
//
// It is the one read the node's own reconcile makes that has no streamed
// counterpart: a Kubernetes-side check against a pod rather than a question about
// a control-plane object, so nothing delivers it and a request is what answers it
// (design-storagenode.md §4.4).
//
// It lives in utils rather than beside either caller because two of them ask. The
// node's provisioning holds until the worker answers, and a maintenance window's
// AwaitingHost step waits for the same answer after a reboot.

package utils

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/simplyblock/simplyblock-operator/internal/tlsutil"
)

// storageNodeAPIProbeTimeout bounds one probe. It is short because the question is
// whether the host is answering now, and a caller that holds until it does looks
// again on its own schedule rather than waiting inside one request.
const storageNodeAPIProbeTimeout = 3 * time.Second

// StorageNodeAPIReachable reports nil when the worker's storage-node API answers
// its info endpoint, and an error describing why not otherwise.
//
// The scheme follows the deployment's TLS settings rather than being probed for: a
// deployment serving TLS refuses a plaintext request, and a refused request is not
// the same answer as an unreachable host.
func StorageNodeAPIReachable(
	ctx context.Context, worker, namespace string, tlsEnabled, tlsMutualEnabled bool,
) error {
	scheme := "http"
	httpClient := &http.Client{Timeout: storageNodeAPIProbeTimeout}
	if tlsEnabled {
		scheme = "https"
		certPath, keyPath := "", ""
		if tlsMutualEnabled {
			certPath = tlsutil.ServiceClientCertificatePath
			keyPath = tlsutil.ServiceClientKeyPath
		}
		built, err := tlsutil.BuildStorageNodeSetAPIClient(
			namespace, tlsutil.ServiceCABundlePath, certPath, keyPath)
		if err != nil {
			return fmt.Errorf("build the storage-node TLS client: %w", err)
		}
		httpClient = built
	}

	url := fmt.Sprintf("%s://%s/snode/info", scheme, StorageNodeSetAPIAddress(worker, namespace))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	response, err := httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("the storage-node API on worker %s does not answer: %w", worker, err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("the storage-node API on worker %s answered %d",
			worker, response.StatusCode)
	}
	return nil
}
