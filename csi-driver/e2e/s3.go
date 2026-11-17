// The S3 specs: an ObjectStore gateway created for the run, then a fixed
// lifecycle (S1), a randomized workload checked against a model (S2), MinIO's
// mint suites (S3), and a warp load run (S4) against it.
//
// The environment provides what is infrastructure: the gateway image in
// GATEWAY_IMAGE and the Vault-backed IAM configuration in Secret
// simplyblock-s3-iam. The specs create everything else and remove it again.

package e2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	ginkgo "github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/simplyblock/atlas/kube"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
)

const (
	// s3IAMSecret is the gateway's IAM configuration, which the environment
	// provides: versitygw's Vault backend needs a Vault with KV-v2 and an
	// AppRole, and setting that up is not the suite's job.
	s3IAMSecret = "simplyblock-s3-iam"

	// s3StoreName is the ObjectStore the suite creates. Its Service, its
	// certificate Secret, and its gateway Deployment carry the same name.
	s3StoreName = "e2e-s3"
	s3Port      = 9000

	// s3BucketCapacity is every bucket's volume size. warp's objects and the
	// randomized spec's largest bucket stay well below it.
	s3BucketCapacity = 2 << 30
)

var (
	objectStoreGVR = schema.GroupVersionResource{
		Group: "storage.simplyblock.io", Version: "v1alpha2", Resource: "objectstores",
	}
	s3BucketGVR = schema.GroupVersionResource{
		Group: "storage.simplyblock.io", Version: "v1alpha2", Resource: "s3buckets",
	}
)

// s3Env is what BeforeAll sets up and every S3 spec uses.
type s3Env struct {
	c   kubernetes.Interface
	dyn dynamic.Interface
	ns  string

	// prefix starts every bucket and user name the suite creates. Other
	// ObjectStores on the cluster share the bucket mount on each node and the
	// Vault path, so their buckets and users can appear in this gateway's
	// listings. Comparisons look only at names with this prefix.
	prefix string

	root   s3Account
	caPEM  []byte
	gw     *gatewayEndpoint
	scName string

	// users are the accounts created through the admin API, removed in
	// AfterAll if a spec did not remove them itself.
	users map[string]bool
}

// s3Keep leaves the gateway, its buckets, and its users in place after the
// run, so a failure can be examined on the cluster it happened on. Set
// S3_E2E_KEEP=1 to keep them. The next run needs them removed by hand first.
var s3Keep = os.Getenv("S3_E2E_KEEP") == "1"

// s3Cleanup registers fn to run after the S3 specs, unless S3_E2E_KEEP is set.
func s3Cleanup(what string, fn func()) {
	ginkgo.DeferCleanup(func() {
		if s3Keep {
			framework.Logf("S3_E2E_KEEP=1: leaving %s in place", what)
			return
		}
		fn()
	})
}

var _ = ginkgo.Describe("SPDKCSI-S3", ginkgo.Ordered, ginkgo.ContinueOnFailure, func() {
	f := newTestFramework("spdkcsi")
	env := &s3Env{users: map[string]bool{}}

	ginkgo.BeforeAll(func() {
		// BeforeAll can run before the framework's BeforeEach builds
		// f.ClientSet, so it builds its own clients.
		cfg, err := framework.LoadConfig()
		framework.ExpectNoError(err, "load the kubeconfig")
		env.c, err = kubernetes.NewForConfig(cfg)
		framework.ExpectNoError(err, "build a clientset")
		env.dyn, err = dynamic.NewForConfig(cfg)
		framework.ExpectNoError(err, "build a dynamic client")
		env.ns = systemNamespace

		framework.ExpectNoError(s3Prerequisites(env.c, env.dyn, env.ns))

		env.prefix = "e2e-" + randomHex(3)
		ginkgo.By("bucket and user names in this run start with " + env.prefix)

		env.scName = env.prefix + "-pnfs-sc"
		ginkgo.By("create StorageClass " + env.scName + " with fstype pnfs and Immediate binding")
		createS3StorageClass(f, env.c, env.scName)
		s3Cleanup("StorageClass "+env.scName, func() { deleteStorageClass(env.c, env.scName) })

		env.root = s3Account{access: env.prefix + "-root", secret: randomHex(20)}
		rootSecret := s3StoreName + "-root-credentials"
		ginkgo.By("create Secret " + rootSecret)
		_, err = env.c.CoreV1().Secrets(env.ns).Create(context.Background(), &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: rootSecret},
			StringData: map[string]string{
				"ROOT_ACCESS_KEY_ID":     env.root.access,
				"ROOT_SECRET_ACCESS_KEY": env.root.secret,
			},
		}, metav1.CreateOptions{})
		framework.ExpectNoError(err, "create Secret %s", rootSecret)
		s3Cleanup("Secret "+rootSecret, func() {
			if err := env.c.CoreV1().Secrets(env.ns).
				Delete(context.Background(), rootSecret, metav1.DeleteOptions{}); err != nil {
				framework.Logf("failed to delete Secret %s: %v", rootSecret, err)
			}
		})

		ginkgo.By("create ObjectStore " + s3StoreName)
		framework.ExpectNoError(createObjectStore(env, rootSecret), "create ObjectStore %s", s3StoreName)
		// Teardown also removes the endpoint, because it deletes buckets and
		// users through it first.
		s3Cleanup("ObjectStore "+s3StoreName+", its buckets, and its users",
			func() { teardownObjectStore(env) })

		ginkgo.By("wait for ObjectStore " + s3StoreName + " to be Ready")
		framework.ExpectNoError(waitForObjectStoreReady(env.dyn, env.ns, 10*time.Minute))

		secret, err := env.c.CoreV1().Secrets(env.ns).Get(context.Background(), s3StoreName, metav1.GetOptions{})
		framework.ExpectNoError(err, "read the gateway's certificate Secret %s", s3StoreName)
		env.caPEM = secret.Data["ca.crt"]
		gomega.Expect(env.caPEM).NotTo(gomega.BeEmpty(),
			"Secret %s has no ca.crt, so the gateway's certificate cannot be verified", s3StoreName)

		ginkgo.By("expose the gateway to the test process on a node port")
		env.gw, err = exposeGateway(cfg, env.c, env.ns, s3StoreName, env.caPEM)
		framework.ExpectNoError(err)
	})

	ginkgo.AfterEach(func() {
		if report := ginkgo.CurrentSpecReport(); report.Failed() && env.c != nil {
			logGatewayOutput(env, report.StartTime)
		}
	})

	ginkgo.It("bucket and object lifecycle", func() {
		runS3Lifecycle(env)
	})

	ginkgo.It("users, roles, isolation, and bucket ownership", func() {
		runS3IAMLifecycle(env)
	})

	ginkgo.It("a randomized workload matches its model", func() {
		runS3Randomized(env)
	})

	ginkgo.It("mint's SDK and tool suites pass, apart from known failures", func() {
		runMint(f, env)
	})

	ginkgo.It("warp's mixed load completes without errors", func() {
		runWarp(f, env)
	})
})

// s3Prerequisites reports everything that keeps this run from creating a
// gateway, together, so one run names everything that has to change.
func s3Prerequisites(c kubernetes.Interface, dyn dynamic.Interface, ns string) error {
	var problems []string
	if !operatorMode {
		problems = append(problems, "OPERATOR_MODE is not true, and an ObjectStore needs the operator")
	}
	if os.Getenv("GATEWAY_IMAGE") == "" {
		problems = append(problems, "GATEWAY_IMAGE is unset, and ObjectStore.spec.image has no default")
	}
	_, err := c.CoreV1().Secrets(ns).Get(context.Background(), s3IAMSecret, metav1.GetOptions{})
	switch {
	case k8serrors.IsNotFound(err):
		problems = append(problems, fmt.Sprintf("Secret %s/%s does not exist; it holds the gateway's "+
			"Vault IAM configuration (VGW_IAM_VAULT_*), see deploy.sh --s3", ns, s3IAMSecret))
	case err != nil:
		problems = append(problems, fmt.Sprintf("read Secret %s/%s: %v", ns, s3IAMSecret, err))
	}
	if err := pnfsPrerequisites(c, dyn); err != nil {
		problems = append(problems, "every bucket is a pNFS volume: "+err.Error())
	}
	if len(problems) == 0 {
		return nil
	}
	return errors.New("this cluster cannot run the S3 specs: " + strings.Join(problems, "; "))
}

// createS3StorageClass writes the pNFS class every bucket is provisioned from.
//
// Binding is Immediate rather than WaitForFirstConsumer, which createStorageClass
// uses: no pod ever mounts a bucket's claim, so it would never get a first
// consumer and never bind.
func createS3StorageClass(f *framework.Framework, c kubernetes.Interface, scName string) {
	bindingMode := storagev1.VolumeBindingImmediate
	reclaimPolicy := corev1.PersistentVolumeReclaimDelete
	_, err := c.StorageV1().StorageClasses().Create(context.Background(), &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: scName},
		Provisioner: kube.DriverName,
		Parameters: map[string]string{
			scParamClusterID: clusterIDForTests(f),
			scParamPool:      poolNameForTests(),
			scParamFSType:    pnfsFSType,
		},
		ReclaimPolicy:     &reclaimPolicy,
		VolumeBindingMode: &bindingMode,
	}, metav1.CreateOptions{})
	framework.ExpectNoError(err, "create StorageClass %s", scName)
}

func createObjectStore(env *s3Env, rootSecret string) error {
	store := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "storage.simplyblock.io/v1alpha2",
		"kind":       "ObjectStore",
		"metadata":   map[string]any{"name": s3StoreName, "namespace": env.ns},
		"spec": map[string]any{
			"image":       os.Getenv("GATEWAY_IMAGE"),
			"port":        int64(s3Port),
			"tlsProvider": "cert-manager",
			"envFrom": []any{
				map[string]any{"secretRef": map[string]any{"name": rootSecret}},
				map[string]any{"secretRef": map[string]any{"name": s3IAMSecret}},
			},
			"buckets": map[string]any{
				"storageClassName": env.scName,
				"capacityBytes":    int64(s3BucketCapacity),
			},
		},
	}}
	_, err := env.dyn.Resource(objectStoreGVR).Namespace(env.ns).
		Create(context.Background(), store, metav1.CreateOptions{})
	return err
}

func waitForObjectStoreReady(dyn dynamic.Interface, ns string, timeout time.Duration) error {
	phase := ""
	err := wait.PollUntilContextTimeout(context.Background(), 5*time.Second, timeout, true,
		func(ctx context.Context) (bool, error) {
			obj, err := dyn.Resource(objectStoreGVR).Namespace(ns).Get(ctx, s3StoreName, metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			phase, _, _ = unstructured.NestedString(obj.Object, "status", "phase")
			return phase == "Ready", nil
		})
	if err != nil {
		return fmt.Errorf("ObjectStore %s did not become Ready within %s (phase %q): %w",
			s3StoreName, timeout, phase, err)
	}
	return nil
}

// bucketPhase is the phase of the S3Bucket a bucket is provisioned as, or ""
// when there is none.
func bucketPhase(env *s3Env, bucket string) (string, error) {
	obj, err := env.dyn.Resource(s3BucketGVR).Namespace(env.ns).
		Get(context.Background(), bucket, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
	return phase, nil
}

// expectBucketReady checks the S3Bucket behind a bucket the gateway just
// reported created. The gateway answers CreateBucket only once the operator
// reports the S3Bucket Ready, so nothing is waited for here.
func expectBucketReady(env *s3Env, bucket string) error {
	phase, err := bucketPhase(env, bucket)
	if err != nil {
		return fmt.Errorf("read S3Bucket %s: %w", bucket, err)
	}
	if phase != "Ready" {
		return fmt.Errorf("bucket %s was created, but its S3Bucket is in phase %q, not Ready", bucket, phase)
	}
	return nil
}

// bucketGoneTimeout is how long a deleted bucket's S3Bucket may take to
// disappear.
const bucketGoneTimeout = 3 * time.Minute

// waitForBucketGone waits for a deleted bucket's S3Bucket to disappear, which
// is its volume released (issues/02 is the case where it never does).
func waitForBucketGone(env *s3Env, bucket string) error {
	timeout := bucketGoneTimeout
	phase := ""
	err := wait.PollUntilContextTimeout(context.Background(), 2*time.Second, timeout, true,
		func(context.Context) (bool, error) {
			var err error
			phase, err = bucketPhase(env, bucket)
			return err == nil && phase == "", nil
		})
	if err != nil {
		return fmt.Errorf("S3Bucket %s still exists %s after DeleteBucket (phase %q)", bucket, timeout, phase)
	}
	return nil
}

// storeBuckets is every S3Bucket of this run's ObjectStore, whoever created it:
// the specs, mint, or warp.
func storeBuckets(env *s3Env) ([]string, error) {
	list, err := env.dyn.Resource(s3BucketGVR).Namespace(env.ns).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var names []string
	for i := range list.Items {
		ref, _, _ := unstructured.NestedString(list.Items[i].Object, "spec", "objectStoreRef")
		if ref == s3StoreName {
			names = append(names, list.Items[i].GetName())
		}
	}
	return names, nil
}

// teardownObjectStore removes every bucket the run left behind, through S3
// where the gateway still answers, then the users and the ObjectStore, and
// fails the run when an S3Bucket outlives it.
func teardownObjectStore(env *s3Env) {
	if env.gw != nil {
		for access := range env.users {
			if err := env.gw.deleteUser(env.root, access); err != nil {
				framework.Logf("failed to delete user %s: %v", access, err)
			}
		}
	}

	buckets, err := storeBuckets(env)
	framework.ExpectNoError(err, "list the run's S3Buckets")
	for _, bucket := range buckets {
		if env.gw != nil {
			err := emptyAndDeleteBucket(env.gw.s3Client(env.root), bucket)
			if err == nil {
				continue
			}
			framework.Logf("failed to delete bucket %s through S3, deleting its S3Bucket: %v", bucket, err)
		}
		if err := env.dyn.Resource(s3BucketGVR).Namespace(env.ns).
			Delete(context.Background(), bucket, metav1.DeleteOptions{}); err != nil && !k8serrors.IsNotFound(err) {
			framework.Logf("failed to delete S3Bucket %s: %v", bucket, err)
		}
	}

	if env.gw != nil {
		env.gw.close()
	}

	var left []string
	for _, bucket := range buckets {
		if err := waitForBucketGone(env, bucket); err != nil {
			left = append(left, err.Error())
		}
	}

	if err := env.dyn.Resource(objectStoreGVR).Namespace(env.ns).
		Delete(context.Background(), s3StoreName, metav1.DeleteOptions{}); err != nil && !k8serrors.IsNotFound(err) {
		framework.Logf("failed to delete ObjectStore %s: %v", s3StoreName, err)
	}

	gomega.Expect(left).To(gomega.BeEmpty(), "S3Buckets left after teardown")
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// gatewayLogLines bounds what logGatewayOutput writes per pod. A warp spec
// alone logs thousands of requests, and the end of a failed spec is where its
// cause shows.
const gatewayLogLines = 400

// logGatewayOutput writes what every gateway pod logged since a failed spec
// started into the spec's output. Teardown deletes the pods, and on the daily
// run the whole cluster after it, so this is the only copy of the gateway's
// side of a failure.
func logGatewayOutput(env *s3Env, since time.Time) {
	ctx := context.Background()
	svc, err := env.c.CoreV1().Services(env.ns).Get(ctx, s3StoreName, metav1.GetOptions{})
	if err != nil {
		framework.Logf("cannot read the gateway's logs: read Service %s: %v", s3StoreName, err)
		return
	}
	pods, err := env.c.CoreV1().Pods(env.ns).List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(svc.Spec.Selector).String(),
	})
	if err != nil {
		framework.Logf("cannot read the gateway's logs: list its pods: %v", err)
		return
	}
	sinceTime := metav1.NewTime(since)
	tail := int64(gatewayLogLines)
	for i := range pods.Items {
		pod := &pods.Items[i]
		raw, err := env.c.CoreV1().Pods(env.ns).GetLogs(pod.Name, &corev1.PodLogOptions{
			SinceTime: &sinceTime, TailLines: &tail, Timestamps: true,
		}).DoRaw(ctx)
		if err != nil {
			framework.Logf("cannot read the logs of gateway pod %s: %v", pod.Name, err)
			continue
		}
		framework.Logf("gateway pod %s on %s, last %d lines since the spec started:\n%s",
			pod.Name, pod.Spec.NodeName, gatewayLogLines, raw)
	}
}
