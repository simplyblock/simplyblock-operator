// The hub's site works checked against what OCM's API server admits: every
// ManifestWork the StorageSiteDeployment and StretchedCluster reconcilers write
// is validated against the ManifestWork CRD schema that open-cluster-management.io/api
// publishes, the schema the hub's API server enforces. The fake client enforces
// none of it, and envtest does not load OCM's CRDs.

package controller

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	apiextensionsinternal "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	workv1 "open-cluster-management.io/api/work/v1"
	"sigs.k8s.io/yaml"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// ocmWorkValidator builds a validator from the ManifestWork CRD in the
// open-cluster-management.io/api module this operator builds against.
func ocmWorkValidator(t *testing.T) validation.SchemaCreateValidator {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "open-cluster-management.io/api").Output()
	if err != nil {
		t.Fatalf("locate open-cluster-management.io/api: %v", err)
	}
	path := filepath.Join(strings.TrimSpace(string(out)), "work", "v1",
		"0000_00_work.open-cluster-management.io_manifestworks.crd.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the ManifestWork CRD: %v", err)
	}
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal(raw, crd); err != nil {
		t.Fatalf("parse the ManifestWork CRD: %v", err)
	}
	var schema *apiextensionsv1.JSONSchemaProps
	for _, v := range crd.Spec.Versions {
		if v.Name == workv1.GroupVersion.Version && v.Schema != nil {
			schema = v.Schema.OpenAPIV3Schema
		}
	}
	if schema == nil {
		t.Fatalf("the ManifestWork CRD has no %s schema", workv1.GroupVersion.Version)
	}
	internal := &apiextensionsinternal.JSONSchemaProps{}
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(schema, internal, nil); err != nil {
		t.Fatalf("convert the ManifestWork schema: %v", err)
	}
	validator, _, err := validation.NewSchemaValidator(internal)
	if err != nil {
		t.Fatalf("build the ManifestWork validator: %v", err)
	}
	return validator
}

// expectOCMAdmits fails the test with every field OCM's schema refuses in w.
func expectOCMAdmits(t *testing.T, validator validation.SchemaCreateValidator, w *workv1.ManifestWork) {
	t.Helper()
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(w)
	if err != nil {
		t.Fatalf("convert work %s/%s: %v", w.Namespace, w.Name, err)
	}
	obj["apiVersion"] = workv1.GroupVersion.String()
	obj["kind"] = "ManifestWork"
	if errs := validation.ValidateCustomResource(field.NewPath(""), obj, validator); len(errs) > 0 {
		t.Errorf("OCM refuses ManifestWork %s/%s: %v", w.Namespace, w.Name, errs.ToAggregate())
	}
}

// Regression: 2026-10-22-work-fieldmanager-pattern: once a site's draft named
// nodes, the hub put the draft's sizing into the work as a server-side apply under
// the field manager hub-deploy, and OCM's API server refused the whole work
// (fieldManager must match ^work-agent). The reconcile failed on that site before
// reading the other, so the StretchedCluster stayed in Discovering with both
// drafts written, and no site was ever sized or approved.
func TestTheStretchedClustersSiteWorksAreAdmittedByOCM(t *testing.T) {
	validator := ocmWorkValidator(t)
	r, cl, _ := newStretchedReconciler(t, newStretched(nil))
	got := reconcileStretched(t, r, cl, 2)
	for _, site := range got.Spec.Sites {
		expectOCMAdmits(t, validator, siteWork(t, cl, got, site.Cluster, site.Site))
	}

	for _, site := range got.Spec.Sites {
		project(t, cl, got, site.Cluster, site.Site, "draft", stretchedDraft(got, false))
	}
	got = reconcileStretched(t, r, cl, 2)
	if got.Status.Phase != simplyblockv1alpha2.StretchedClusterPhaseDrafted {
		t.Fatalf("phase = %q (%s), want Drafted", got.Status.Phase, got.Status.Message)
	}
	for _, site := range got.Spec.Sites {
		expectOCMAdmits(t, validator, siteWork(t, cl, got, site.Cluster, site.Site))
	}
}

// Regression: 2026-10-22-work-fieldmanager-pattern: the single-site flow writes
// the same server-side apply when it carries sizing, so its work was refused the
// same way once its draft named nodes.
func TestTheStorageSiteDeploymentsWorkIsAdmittedByOCM(t *testing.T) {
	validator := ocmWorkValidator(t)
	data, parity := int32(1), int32(1)
	r, cl := newStorageSiteDeploymentReconciler(t, newSiteDeployment(func(sd *simplyblockv1alpha2.StorageSiteDeployment) {
		sd.Spec.Sizing = &simplyblockv1alpha2.StorageSiteSizing{
			Stripe: &simplyblockv1alpha2.StripeSpec{DataChunks: &data, ParityChunks: &parity},
		}
	}))
	sd := reconcileSD(t, r, cl, 2)
	expectOCMAdmits(t, validator, getSiteWork(t, cl, sd))

	projectSiteObject(t, cl, sd, "draft", siteDraft(false, simplyblockv1alpha2.ClusterDeploymentConfigPhaseDraft, nil))
	sd = reconcileSD(t, r, cl, 2)
	if sd.Status.Phase != simplyblockv1alpha2.StorageSiteDeploymentPhaseDrafted {
		t.Fatalf("phase = %q (%s), want Drafted", sd.Status.Phase, sd.Status.Message)
	}
	expectOCMAdmits(t, validator, getSiteWork(t, cl, sd))
}
