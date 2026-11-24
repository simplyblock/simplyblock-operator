// How the S3 specs reach the gateway: a NodePort Service in front of the
// gateway pods, an S3 client per account through it, and versitygw's admin API
// signed as the root account.
//
// The specs call the gateway from the test process, not from client pods,
// because the randomized spec's model lives in the test process and checks
// every answer. Running clients in pods would mean an exec per request.
//
// TLS is verified against the CA in the gateway's certificate Secret, with the
// Service name the certificate is issued for. mint and warp run as pods and
// use the gateway's own Service.

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/kubernetes/test/e2e/framework"
)

const (
	// s3Region is what every request is signed for. The gateway serves one
	// region and does not check the name, but SigV4 needs one.
	s3Region = "us-east-1"

	// s3OpTimeout bounds every S3 and admin request. A gateway that never
	// answers, which is what an async NFS COPY without its callback looks like
	// (issues/01), has to fail the spec rather than stall it until the suite's
	// own timeout kills everything with no word about why.
	s3OpTimeout = 2 * time.Minute
)

// s3Account is one identity the specs sign as: the gateway's root account or a
// user created through the admin API.
type s3Account struct {
	access string
	secret string
}

// gatewayEndpoint is the gateway as the test process reaches it, and the HTTP
// client that verifies the gateway's certificate there.
type gatewayEndpoint struct {
	endpoint string
	client   *http.Client
	c        kubernetes.Interface
	ns       string
	svc      string
}

// exposeGateway makes the named ObjectStore's gateway reachable from the test
// process and returns once it answers.
//
// The gateway's own Service is a ClusterIP, which nothing outside the cluster
// can route to, and the test process runs outside it: on a CI runner, or on a
// developer's machine. A port-forward reaches a pod, but it drops connections
// under the randomized spec's concurrency and then times out every TLS
// handshake. A NodePort Service with the same selector is reached the way a
// user outside the cluster would reach the gateway, through the node the
// kubeconfig already names. It is owned by the gateway's Service, so removing
// the ObjectStore removes it too.
func exposeGateway(
	cfg *restclient.Config, c kubernetes.Interface, ns, store string, caPEM []byte,
) (*gatewayEndpoint, error) {
	ctx := context.Background()
	gw, err := c.CoreV1().Services(ns).Get(ctx, store, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("read the gateway's Service %s: %w", store, err)
	}
	if len(gw.Spec.Ports) == 0 {
		return nil, fmt.Errorf("the gateway's Service %s exposes no port", store)
	}
	apiURL, err := url.Parse(cfg.Host)
	if err != nil || apiURL.Hostname() == "" {
		return nil, fmt.Errorf("read a node address from the kubeconfig's server %q: %w", cfg.Host, err)
	}

	controller := true
	svc, err := c.CoreV1().Services(ns).Create(ctx, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: store + "-e2e-nodeport",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1", Kind: "Service", Name: gw.Name, UID: gw.UID, Controller: &controller,
			}},
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeNodePort,
			Selector: gw.Spec.Selector,
			Ports: []corev1.ServicePort{{
				Name: "s3", Protocol: corev1.ProtocolTCP,
				Port: gw.Spec.Ports[0].Port, TargetPort: gw.Spec.Ports[0].TargetPort,
			}},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create a NodePort Service for the gateway: %w", err)
	}
	g := &gatewayEndpoint{c: c, ns: ns, svc: svc.Name}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		g.close()
		return nil, errors.New("the gateway's ca.crt holds no PEM certificate")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{
		RootCAs: roots,
		// The certificate names the Service, and the client dials a node, so
		// the name checked is the Service's.
		ServerName: store + "." + ns + ".svc",
		MinVersion: tls.VersionTLS12,
	}
	g.client = &http.Client{Transport: transport}
	g.endpoint = fmt.Sprintf("https://%s:%d", apiURL.Hostname(), svc.Spec.Ports[0].NodePort)

	// The node port starts answering once kube-proxy on that node has
	// programmed it, a moment after the Service exists.
	deadline := time.Now().Add(time.Minute)
	for {
		resp, err := g.client.Get(g.endpoint + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			return g, nil
		}
		if time.Now().After(deadline) {
			g.close()
			return nil, fmt.Errorf("the gateway did not answer at %s within a minute: %w", g.endpoint, err)
		}
		time.Sleep(2 * time.Second)
	}
}

func (g *gatewayEndpoint) close() {
	if err := g.c.CoreV1().Services(g.ns).Delete(context.Background(), g.svc, metav1.DeleteOptions{}); err != nil {
		framework.Logf("failed to delete Service %s: %v", g.svc, err)
	}
}

// s3Client is an S3 client signing as the account, path-style because bucket
// names are not resolvable hosts at a node's address.
//
// The SDK's checksum defaults are left as they are: every current AWS SDK sends
// a CRC32 trailer on uploads by default, and a gateway that refused one would
// refuse every user of such an SDK.
func (g *gatewayEndpoint) s3Client(account s3Account) *s3.Client {
	return s3.New(s3.Options{
		Region:       s3Region,
		BaseEndpoint: aws.String(g.endpoint),
		UsePathStyle: true,
		HTTPClient:   g.client,
		Credentials:  credentials.NewStaticCredentialsProvider(account.access, account.secret, ""),
	})
}

// s3ErrorCode is the S3 error code of err, or "" when err is not an S3 error
// answer at all: a timeout or a dropped connection says nothing about what the
// gateway decided, and a negative check must not pass on one (issues/09).
func s3ErrorCode(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return ""
}

// expectS3Error reports whether err is the S3 error answer code, and says what
// it was instead when it is not.
func expectS3Error(err error, code string) error {
	if err == nil {
		return fmt.Errorf("expected %s, but the request succeeded", code)
	}
	if got := s3ErrorCode(err); got != code {
		return fmt.Errorf("expected %s, got %q: %w", code, got, err)
	}
	return nil
}

// adminRequest is one call to versitygw's admin API: a PATCH with an XML body,
// SigV4-signed for the S3 service as the root account, which is what
// tests/manual/admin.sh does with curl.
func (g *gatewayEndpoint) adminRequest(root s3Account, path, body string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), s3OpTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, g.endpoint+"/"+path, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(body))
	payloadHash := hex.EncodeToString(sum[:])
	req.Header.Set("x-amz-content-sha256", payloadHash)
	creds := aws.Credentials{AccessKeyID: root.access, SecretAccessKey: root.secret}
	if err := v4.NewSigner().SignHTTP(ctx, creds, req, payloadHash, "s3", s3Region, time.Now()); err != nil {
		return nil, fmt.Errorf("sign admin %s: %w", path, err)
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("admin %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("admin %s: read the answer: %w", path, err)
	}
	if resp.StatusCode/100 != 2 {
		return out, fmt.Errorf("admin %s: HTTP %d: %s", path, resp.StatusCode, bytes.TrimSpace(out))
	}
	return out, nil
}

// The roles versitygw's Vault IAM backend knows. It has no identity policies,
// so the role alone decides what an account may do.
const (
	roleUser     = "user"
	roleUserPlus = "userplus"
)

func (g *gatewayEndpoint) createUser(root s3Account, account s3Account, role string) error {
	_, err := g.adminRequest(root, "create-user", fmt.Sprintf(
		"<Account><Access>%s</Access><Secret>%s</Secret><Role>%s</Role></Account>",
		account.access, account.secret, role))
	return err
}

func (g *gatewayEndpoint) updateUserRole(root s3Account, access, role string) error {
	_, err := g.adminRequest(root, "update-user?access="+url.QueryEscape(access),
		"<MutableProps><Role>"+role+"</Role></MutableProps>")
	return err
}

func (g *gatewayEndpoint) deleteUser(root s3Account, access string) error {
	_, err := g.adminRequest(root, "delete-user?access="+url.QueryEscape(access), "")
	return err
}

func (g *gatewayEndpoint) changeBucketOwner(root s3Account, bucket, owner string) error {
	_, err := g.adminRequest(root, fmt.Sprintf("change-bucket-owner?bucket=%s&owner=%s",
		url.QueryEscape(bucket), url.QueryEscape(owner)), "")
	return err
}

// bucketOwners is every bucket the gateway serves, with its owner's access key.
func (g *gatewayEndpoint) bucketOwners(root s3Account) (map[string]string, error) {
	out, err := g.adminRequest(root, "list-buckets", "")
	if err != nil {
		return nil, err
	}
	var answer struct {
		Buckets []struct {
			Name  string `xml:"Name"`
			Owner string `xml:"Owner"`
		} `xml:"Buckets"`
	}
	if err := xml.Unmarshal(out, &answer); err != nil {
		return nil, fmt.Errorf("parse list-buckets: %w: %s", err, out)
	}
	owners := make(map[string]string, len(answer.Buckets))
	for _, b := range answer.Buckets {
		owners[b.Name] = b.Owner
	}
	return owners, nil
}
