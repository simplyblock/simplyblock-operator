/*
Copyright 2025.

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

package subscriptions

import (
	"github.com/simplyblock/atlas/kube"

	"github.com/simplyblock/simplyblock-operator/internal/cpinformer"
	"github.com/simplyblock/simplyblock-operator/internal/webapi"
)

// ResolveStreamConfig builds the shared control-plane stream config used by the
// SubscriptionManager: endpoint and TLS client from webapi, bearer token from
// the pod service account. It is resource-agnostic — only the per-resource path
// (from each Subscription) differs.
func ResolveStreamConfig() (cpinformer.StreamConfig, error) {
	streamClient, err := webapi.NewStreamClient()
	if err != nil {
		return cpinformer.StreamConfig{}, err
	}
	token, _ := kube.ServiceAccountToken()
	return cpinformer.StreamConfig{
		Endpoint: streamClient.BaseURL,
		Token:    token,
		Client:   streamClient.Client,
	}, nil
}
