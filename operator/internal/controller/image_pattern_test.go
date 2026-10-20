// What the apiserver accepts in the v1alpha1 image fields. The pattern is a
// CRD marker, so only an envtest apiserver applies it; a fake client would
// accept any value. Mirrored references must be admitted for air-gapped
// installations, and malformed ones must still be refused.

package controller

import (
	"context"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	simplyblockv1alpha1 "github.com/simplyblock/simplyblock-operator/api/v1alpha1"
)

var mirroredImages = []string{
	"registry.airgap.local:5000/simplyblock/simplyblock:26.3.2",
	"mirror.example.com/a/b/c/spdk:v1.2.3@sha256:" + strings.Repeat("a", 64),
	"localhost/ultra@sha256:" + strings.Repeat("0", 64),
	"ultra:main-latest",
}

var malformedImages = []string{
	"docker.io/simplyblock/ultra",
	"docker.io/Simplyblock/ultra:main",
	"docker.io/simplyblock/ultra:",
	"docker.io/simplyblock/ultra:main@sha256:abc",
	"https://docker.io/simplyblock/ultra:main",
	"docker.io/simplyblock/ultra main",
}

func createControlPlaneWithImage(name, image string) error {
	return k8sClient.Create(context.Background(), &simplyblockv1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       simplyblockv1alpha1.ControlPlaneSpec{Image: image},
	})
}

func createStorageNodeSetWithImages(name, image string) error {
	return k8sClient.Create(context.Background(), &simplyblockv1alpha1.StorageNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: simplyblockv1alpha1.StorageNodeSetSpec{
			ClusterName:    "cluster",
			ClusterImage:   image,
			SpdkImage:      image,
			SpdkProxyImage: image,
		},
	})
}

var _ = Describe("Image reference validation", func() {
	It("accepts images from any registry, mirrors included", func() {
		for i, image := range mirroredImages {
			Expect(createControlPlaneWithImage(fmt.Sprintf("cp-mirror-%d", i), image)).
				To(Succeed(), "ControlPlane image %q", image)
			Expect(createStorageNodeSetWithImages(fmt.Sprintf("sns-mirror-%d", i), image)).
				To(Succeed(), "StorageNodeSet image %q", image)
		}
	})

	It("refuses malformed image references", func() {
		for i, image := range malformedImages {
			err := createControlPlaneWithImage(fmt.Sprintf("cp-malformed-%d", i), image)
			Expect(err).To(MatchError(ContainSubstring("should match")), "ControlPlane image %q", image)
			err = createStorageNodeSetWithImages(fmt.Sprintf("sns-malformed-%d", i), image)
			Expect(err).To(MatchError(ContainSubstring("should match")), "StorageNodeSet image %q", image)
		}
	})
})
