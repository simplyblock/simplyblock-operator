// Where the control plane is and how to authenticate against it, in the shape
// atlas-lib's client takes.
//
// It lives in this package because resolving the endpoint is what this package
// already does: the URL comes from an environment variable or a default, the
// scheme follows SB_TLS_SERVE, and getting either wrong in a second place would
// point half the operator at a control plane the other half is not talking to.
// The client it configures is atlas-lib's, which is where the typed calls are
// and where this package's own hand-rolled requests are headed.

package webapi

import (
	"fmt"

	"github.com/simplyblock/atlas/controlplane"
	atlaskube "github.com/simplyblock/atlas/kube"
)

// ControlPlaneConfig resolves the endpoint and the bearer token a
// control-plane client needs.
//
// A token that cannot be read is an error rather than an empty string, for the
// same reason the stream config treats it that way: starting unauthenticated
// pushes the failure to the first call, where it arrives as a 401 that says
// nothing about the token.
func ControlPlaneConfig() (controlplane.Config, error) {
	token, err := atlaskube.ServiceAccountToken()
	if err != nil {
		return controlplane.Config{}, fmt.Errorf("read the service-account token: %w", err)
	}
	return controlplane.Config{Endpoint: NewClient().BaseURL, Token: token}, nil
}
