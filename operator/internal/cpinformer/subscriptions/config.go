// The connection half of a subscription: where the control plane is and how to
// authenticate against it. It is resource-agnostic and shared by every
// subscription, which is why it lives beside them rather than in any one of
// them — only the per-resource path differs, and each Subscription supplies its
// own.

package subscriptions

import (
	"fmt"

	"github.com/simplyblock/atlas/kube"

	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// ResolveStreamConfig builds the shared control-plane stream config used by the
// SubscriptionManager: endpoint and TLS client from webapi, bearer token from
// the pod service account.
//
// A token that cannot be read is an error rather than an empty string. Starting
// unauthenticated would push the failure to the first stream, where it arrives
// as a 401 that reconnects forever and says nothing about the token.
func ResolveStreamConfig() (cpinformer.StreamConfig, error) {
	streamClient, err := webapi.NewStreamClient()
	if err != nil {
		return cpinformer.StreamConfig{}, err
	}
	token, err := kube.ServiceAccountToken()
	if err != nil {
		return cpinformer.StreamConfig{}, fmt.Errorf("read the service-account token: %w", err)
	}
	return cpinformer.StreamConfig{
		Endpoint: streamClient.BaseURL,
		Token:    token,
		Client:   streamClient.Client,
	}, nil
}
