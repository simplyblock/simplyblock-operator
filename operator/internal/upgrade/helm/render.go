// Rendering the chart that would replace what is deployed, which is one half of
// §12.4's difference.
//
// The other half is the release that is already there, and that one needs no
// Helm at all: it is a Secret this repository decodes in upgrade/release. Only
// the rendering needs the template engine, which is why the SDK is here and not
// everywhere the handover touches.
//
// It renders rather than upgrades. §12 annotates the survivors before any
// upgrade runs, so what the handover needs is what the new chart would contain,
// and asking Helm for that without letting it write is a dry run.

package helm

import (
	"fmt"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
)

// LoadChart reads a chart from a directory or a packaged archive.
//
// §11 argues that the CRDs ship embedded so their version matches the code that
// converts them, and the same holds for the chart: one fetched at run time can
// be a different chart from the one this binary was tested against. Until it is
// embedded, the path is where it comes from.
func LoadChart(path string) (*chart.Chart, error) {
	loaded, err := loader.Load(path)
	if err != nil {
		return nil, fmt.Errorf("loading the chart at %s: %w", path, err)
	}
	return loaded, nil
}

// RenderUpgrade returns the manifest a Helm upgrade of this release would
// produce, without performing one.
//
// It goes through the upgrade action rather than the install one because the
// two render differently: an upgrade knows the release it is replacing, so the
// manifest it produces is the one §12.4's difference has to be taken against.
func (c *Client) RenderUpgrade(release string, loaded *chart.Chart, values map[string]any) (string, error) {
	upgrade := action.NewUpgrade(c.config)
	upgrade.Namespace = c.namespace
	upgrade.DryRun = true

	// A dry run that reaches the cluster reports what the API server would
	// accept, and one that does not reports what the templates produce.
	// §12.4's difference is about the templates, and reaching the cluster here
	// would make a read-only command depend on admission.
	upgrade.DryRunOption = "client"

	rendered, err := upgrade.Run(release, loaded, values)
	if err != nil {
		return "", fmt.Errorf("rendering the upgrade of %s: %w", release, err)
	}
	return rendered.Manifest, nil
}
