// S3 and S4: MinIO's mint suites and a warp load run, each as a pod in the
// spec's namespace that reaches the gateway's Service directly.
//
// Both pods trust the gateway's CA from a ConfigMap the spec writes, because
// the certificate Secret lives in the system namespace and a pod can mount
// only its own namespace's objects.

package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
)

const (
	// mintImage is archived upstream and no longer updated, so the latest tag
	// does not move.
	defaultMintImage = "docker.io/minio/mint:latest"
	defaultWarpImage = "docker.io/minio/warp:latest"

	mintLogMarker = "=== mint log.json ==="
	mintFail      = "FAIL"
	mintPass      = "PASS"
	caConfigMap   = "s3-gateway-ca"
)

// mintKnownFailure is a mint test that fails for a recorded reason. match is
// a substring of the record's name, function, and error fields joined by
// spaces, and an empty one matches every test of the suite.
type mintKnownFailure struct {
	suite string
	match string
	issue string
}

// mintKnownFailures are the failures recorded under issues/ in the monorepo:
// open bugs, and behavior unsupported by design (issues/12). A test listed
// here that passes is logged, so the entry can be removed.
var mintKnownFailures = []mintKnownFailure{
	{suite: "aws-sdk-java-v2", match: "putBucketVersioning", issue: "04: no versioning"},
	{suite: "minio-go", match: "testFunctional RemoveBucket", issue: "05: NoSuchBucket message text"},
	{suite: "minio-go", match: "Versioning has not been configured", issue: "04: no versioning"},
	// Every V2 test signs with signature version 2, under its own test name.
	{suite: "minio-go", match: "valid Date or x-amz-date header",
		issue: "12: signature version 2 is unsupported by design"},
	{suite: "minio-go", match: "testStorageClass", issue: "12: storage classes are accepted and ignored by design"},
	{suite: "awscli", match: "StorageClass was applied incorrectly",
		issue: "12: storage classes are accepted and ignored by design"},
	{suite: "mc", match: "test_put_object_with_storage_class_error",
		issue: "12: storage classes are accepted and ignored by design"},
	{suite: "minio-go", match: "region 'eu-central-1' is wrong", issue: "13: one region, fixed to us-east-1"},
	{suite: "minio-go", match: "testTrailingChecksums", issue: "14: trailing checksums are not returned"},
	{suite: "minio-go", match: "Unexpected object version ID", issue: "15: version ID without versioning"},
	{suite: "minio-go", match: "testPresignedPostPolicy", issue: "16: presigned POST is rejected"},
}

// mintSuites are the mint suites run, out of the image's /mint/run/core: the
// clients users most often point at an S3 endpoint. The rest are left out to
// keep the block inside the suite's time budget. healthcheck could not pass in
// any case, since it requests MinIO's own health endpoint (issues/08).
//
// aws-sdk-go is left out as well. Every failure in it ends the program, and its
// first test is the presigned PUT of issues/06, so a run tests nothing past it.
// The SDK is the end-of-life v1, and the randomized spec covers its successor.
//
// mc runs before minio-go. Its test_rb removes every bucket the account
// sees, and minio-go's full mode leaves many behind.
var mintSuites = []string{"awscli", "aws-sdk-java-v2", "mc", "minio-go"}

// mintFullSuites run in mint's full mode. minio-go's core mode is two tests,
// each stopping at its first error, where full mode runs a hundred separate
// tests that RUN_ON_FAIL keeps going past a failure.
var mintFullSuites = []string{"minio-go"}

// mintRecord is one line of mint's log.json.
type mintRecord struct {
	Name     string `json:"name"`
	Function string `json:"function"`
	Status   string `json:"status"`
	Error    string `json:"error"`
	Message  string `json:"message"`
}

// parseMintLog reads mint's log.json lines from a pod log, after the marker
// the pod prints before them.
func parseMintLog(log string) ([]mintRecord, error) {
	_, after, found := strings.Cut(log, mintLogMarker)
	if !found {
		return nil, fmt.Errorf("the mint pod's log has no %q marker", mintLogMarker)
	}
	var records []mintRecord
	scanner := bufio.NewScanner(strings.NewReader(after))
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var r mintRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("parse mint record %q: %w", line, err)
		}
		records = append(records, r)
	}
	return records, scanner.Err()
}

// suite is the mint suite a record came from. Most suites name records after
// themselves, but minio-go names each after its test, as minio-go: <test>.
func (r mintRecord) suite() string {
	suite, _, _ := strings.Cut(r.Name, ":")
	return suite
}

func knownMintFailure(r mintRecord) (string, bool) {
	for _, k := range mintKnownFailures {
		if k.suite == r.suite() && strings.Contains(r.Name+" "+r.Function+" "+r.Error+" "+r.Message, k.match) {
			return k.issue, true
		}
	}
	return "", false
}

// judgeMint returns the failures not explained by mintKnownFailures, and logs
// the known ones and the known ones that passed.
func judgeMint(records []mintRecord) []string {
	var unexpected []string
	for _, r := range records {
		issue, known := knownMintFailure(r)
		switch {
		case r.Status == mintFail && known:
			framework.Logf("mint %s %s: known failure, issues/%s", r.Name, r.Function, issue)
		case r.Status == mintFail:
			unexpected = append(unexpected, fmt.Sprintf("%s %s: %s %s", r.Name, r.Function, r.Error, r.Message))
		case r.Status == mintPass && known:
			framework.Logf("mint %s %s passes now; remove it from mintKnownFailures (issues/%s)",
				r.Name, r.Function, issue)
		}
	}
	return unexpected
}

// mintScript runs mintSuites, each under its own
// timeout because one hanging suite would otherwise block the rest
// (issues/09), and prints their combined log.json after the marker. A suite in
// full mode runs many times the tests and gets fullTimeout instead.
func mintScript(suiteTimeout, fullTimeout time.Duration) string {
	return fmt.Sprintf(`set -u
cat /ca/ca.crt >> /etc/ssl/certs/ca-certificates.crt
# The Java suites read the JVM's own truststore, not the system bundle (issues/09).
keytool -importcert -noprompt -cacerts -storepass changeit -alias s3-gateway -file /ca/ca.crt >/dev/null 2>&1 || true
: > /tmp/all.json
for suite in %s; do
  if [ ! -d "/mint/run/core/$suite" ]; then
    printf '{"name":"%%s","function":"(suite)","status":"FAIL","error":"not in this mint image"}\n' \
      "$suite" >> /tmp/all.json
    continue
  fi
  mode=core
  limit=%d
  case " %s " in *" $suite "*) mode=full limit=%d ;; esac
  rm -f /mint/log/log.json
  MINT_MODE=$mode timeout "$limit" /mint/entrypoint.sh "$suite" >/dev/null 2>&1
  rc=$?
  [ -f /mint/log/log.json ] && cat /mint/log/log.json >> /tmp/all.json
  if [ "$rc" = 124 ]; then
    printf '{"name":"%%s","function":"(suite)","status":"FAIL","error":"timed out after %%ss"}\n' \
      "$suite" "$limit" >> /tmp/all.json
  fi
done
echo '%s'
cat /tmp/all.json
`, strings.Join(mintSuites, " "), int(suiteTimeout.Seconds()),
		strings.Join(mintFullSuites, " "), int(fullTimeout.Seconds()), mintLogMarker)
}

func runMint(f *framework.Framework, env *s3Env) {
	ns := f.Namespace.Name
	suiteTimeout := envDuration("S3_E2E_MINT_SUITE_TIMEOUT", 6*time.Minute)
	// minio-go's full mode did not finish in 6 minutes on cluster91.
	fullTimeout := envDuration("S3_E2E_MINT_FULL_SUITE_TIMEOUT", 15*time.Minute)
	writeCAConfigMap(f, env)

	pod := toolPod("mint", envOr("S3_E2E_MINT_IMAGE", defaultMintImage),
		[]string{"sh", "-c", mintScript(suiteTimeout, fullTimeout)},
		map[string]string{
			"SERVER_ENDPOINT": fmt.Sprintf("%s.%s.svc:%d", s3StoreName, env.ns, s3Port),
			"ACCESS_KEY":      env.root.access,
			"SECRET_KEY":      env.root.secret,
			"ENABLE_HTTPS":    "1",
			"SERVER_REGION":   s3Region,
			// minio-go runs its KMS tests unless told otherwise, and there is
			// no KMS behind the gateway.
			"ENABLE_KMS": "0",
			// Without it each suite stops at its first failure and hides the
			// rest (issues/09).
			"RUN_ON_FAIL": "1",
		})
	log := runToolPod(f, ns, pod, envDuration("S3_E2E_MINT_TIMEOUT", 45*time.Minute))

	records, err := parseMintLog(log)
	framework.ExpectNoError(err)
	gomega.Expect(records).NotTo(gomega.BeEmpty(), "mint ran no test at all")
	summarizeMint(records)
	unexpected := judgeMint(records)
	// One line each and cut short: gomega truncates a long list in its failure
	// message, which hid the cause of the last entries.
	for _, u := range unexpected {
		framework.Logf("mint failure not recorded under issues/: %s", failureLines(u, mintFailureChars))
	}
	gomega.Expect(unexpected).To(gomega.BeEmpty(),
		"%d mint failures not recorded under issues/, each logged above", len(unexpected))
}

func summarizeMint(records []mintRecord) {
	counts := map[string]map[string]int{}
	for _, r := range records {
		if counts[r.suite()] == nil {
			counts[r.suite()] = map[string]int{}
		}
		counts[r.suite()][r.Status]++
	}
	for suite, byStatus := range counts {
		framework.Logf("mint %-16s PASS %3d  FAIL %3d  NA %3d", suite, byStatus["PASS"], byStatus["FAIL"], byStatus["NA"])
	}
}

// warpErrors matches warp's per-operation and total error counts.
var warpErrors = regexp.MustCompile(`Errors:\s*([1-9][0-9]*)`)

// warpArgs is a mixed GET, PUT, STAT, and DELETE load over objects of random
// size. The object count and size keep the data under a quarter of a bucket.
// S3_E2E_WARP_CHECKSUM names a trailing checksum for the gateway to validate
// every upload against.
func warpArgs(env *s3Env) []string {
	args := []string{
		"mixed",
		"--no-color",
		"--host", fmt.Sprintf("%s.%s.svc:%d", s3StoreName, env.ns, s3Port),
		"--tls",
		"--region", s3Region,
		"--bucket", env.prefix + "-warp",
		"--duration", envOr("S3_E2E_WARP_DURATION", "3m"),
		"--concurrent", envOr("S3_E2E_WARP_CONCURRENT", "16"),
		"--objects", "100",
		"--obj.size", "4MiB",
		"--obj.randsize",
		// warp's default mix puts more objects than it deletes, so the data
		// grows for the whole run and fills the bucket, after which every
		// error is the full volume's. Equal shares hold it near --objects.
		"--put-distrib", "15",
		"--delete-distrib", "15",
	}
	// Off by default: warp's --checksum races on a header map its parallel
	// uploads share, which crashes warp and can send one upload's checksum
	// with another's body.
	if checksum := envOr("S3_E2E_WARP_CHECKSUM", "none"); checksum != "none" {
		args = append(args, "--checksum", checksum)
	}
	if s3Keep {
		// warp otherwise deletes its objects when it finishes.
		args = append(args, "--noclear")
	}
	return args
}

func runWarp(f *framework.Framework, env *s3Env) {
	writeCAConfigMap(f, env)
	pod := toolPod("warp", envOr("S3_E2E_WARP_IMAGE", defaultWarpImage), nil,
		map[string]string{
			"WARP_ACCESS_KEY": env.root.access,
			"WARP_SECRET_KEY": env.root.secret,
			// warp is a Go binary and reads its roots from here.
			"SSL_CERT_FILE": "/ca/ca.crt",
		})
	pod.Spec.Containers[0].Args = warpArgs(env)
	log := runToolPod(f, f.Namespace.Name, pod, 15*time.Minute)

	if m := warpErrors.FindAllString(log, -1); len(m) > 0 {
		framework.Failf("warp reported errors (%s):\n%s", strings.Join(m, ", "), log)
	}
	gomega.Expect(log).To(gomega.ContainSubstring("Report:"), "warp printed no report:\n%s", log)
}

// writeCAConfigMap copies the gateway's CA into the spec's namespace.
func writeCAConfigMap(f *framework.Framework, env *s3Env) {
	_, err := f.ClientSet.CoreV1().ConfigMaps(f.Namespace.Name).Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: caConfigMap},
		Data:       map[string]string{"ca.crt": string(env.caPEM)},
	}, metav1.CreateOptions{})
	framework.ExpectNoError(err, "create ConfigMap %s", caConfigMap)
}

func toolPod(name, image string, command []string, vars map[string]string) *corev1.Pod {
	envVars := make([]corev1.EnvVar, 0, len(vars))
	for k, v := range vars {
		envVars = append(envVars, corev1.EnvVar{Name: k, Value: v})
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"app": name}},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:         name,
				Image:        image,
				Command:      command,
				Env:          envVars,
				VolumeMounts: []corev1.VolumeMount{{Name: "ca", MountPath: "/ca", ReadOnly: true}},
			}},
			Volumes: []corev1.Volume{{
				Name: "ca",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: caConfigMap},
					},
				},
			}},
		},
	}
}

// runToolPod runs the pod to completion and returns its log, failing the spec
// when it does not succeed within timeout.
func runToolPod(f *framework.Framework, ns string, pod *corev1.Pod, timeout time.Duration) string {
	_, err := f.ClientSet.CoreV1().Pods(ns).Create(context.Background(), pod, metav1.CreateOptions{})
	framework.ExpectNoError(err, "create pod %s", pod.Name)

	waitErr := e2epod.WaitForPodSuccessInNamespaceTimeout(context.Background(), f.ClientSet, pod.Name, ns, timeout)
	raw, err := f.ClientSet.CoreV1().Pods(ns).GetLogs(pod.Name, &corev1.PodLogOptions{}).DoRaw(context.Background())
	if err != nil {
		framework.Logf("failed to read the log of pod %s: %v", pod.Name, err)
	}
	framework.ExpectNoError(waitErr, "pod %s did not succeed within %s; its log:\n%s", pod.Name, timeout, raw)
	return string(raw)
}

func envDuration(name string, fallback time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	framework.ExpectNoError(err, "%s=%q", name, v)
	return d
}

// mintFailureChars bounds one logged mint failure. Some suites put a whole
// command transcript in the error field.
const mintFailureChars = 600

// errorLine matches a transcript line that reports what failed.
var errorLine = regexp.MustCompile(`(?i)error|unable|fail`)

// failureLines bounds s to about n bytes. A long transcript is reduced to its
// lines that report an error, because mc's ends with whatever it did after
// the failure. Without such a line, the end is kept.
func failureLines(s string, n int) string {
	if len(s) <= n {
		return s
	}
	var kept []string
	for _, line := range strings.Split(s, "\n") {
		if errorLine.MatchString(line) {
			kept = append(kept, strings.TrimSpace(line))
		}
	}
	if len(kept) > 0 {
		s = strings.Join(kept, " | ")
	}
	return truncate(s, n)
}

// truncate cuts s to at most n bytes, keeping its end: a transcript ends with
// the error that failed it.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
