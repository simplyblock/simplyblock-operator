// Tests for the StorageDevice delete guard: who may remove a record of
// hardware, and the one caller that must never be refused.

package webhook

import (
	"context"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	validatorOperatorNamespace = "simplyblock-system"
	// deviceNamespace is where the devices under test live, which is a storage
	// cluster's namespace and deliberately not the operator's: the identity test
	// is about who the caller is, not about where the object sits.
	deviceNamespace = "sb"
)

func deviceValidator(t *testing.T, objs ...client.Object) *StorageDeviceValidator {
	t.Helper()
	scheme := newScheme(t)
	return &StorageDeviceValidator{
		Client:            fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(),
		OperatorNamespace: validatorOperatorNamespace,
	}
}

func deleteRequest(username string) admission.Request {
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Delete,
		Namespace: deviceNamespace,
		Name:      "production-7f3a9c-5e0000a1",
		UserInfo:  authenticationv1.UserInfo{Username: username},
	}}
}

func liveNamespace() *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: deviceNamespace}}
}

func terminatingNamespace() *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: deviceNamespace},
		Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceTerminating},
	}
}

// A device object is discovered rather than declared, so it is not a request
// anybody made and not theirs to withdraw.
func TestAUserMayNotDeleteAStorageDevice(t *testing.T) {
	v := deviceValidator(t, liveNamespace())

	resp := v.Handle(context.Background(), deleteRequest("kubernetes-admin"))
	if resp.Allowed {
		t.Fatal("a user's delete was admitted")
	}
	if resp.Result == nil || resp.Result.Message == "" {
		t.Error("a refusal has to say why")
	}
}

// The operator's own deletions are the only ones §5.3 permits: a device that
// stopped being reported, and a node whose garbage collection takes its devices
// with it.
func TestTheOperatorMayDeleteAStorageDevice(t *testing.T) {
	v := deviceValidator(t, liveNamespace())

	resp := v.Handle(context.Background(),
		deleteRequest("system:serviceaccount:"+validatorOperatorNamespace+":simplyblock-operator"))
	if !resp.Allowed {
		t.Errorf("the operator's own delete was refused: %v", resp.Result)
	}
}

// A service account of the same name in another namespace is a different
// identity, and a tenant who can create one must not inherit the operator's
// permission.
func TestAServiceAccountElsewhereMayNotDelete(t *testing.T) {
	v := deviceValidator(t, liveNamespace())

	resp := v.Handle(context.Background(),
		deleteRequest("system:serviceaccount:tenant-a:simplyblock-operator"))
	if resp.Allowed {
		t.Error("a service account outside the operator's namespace was admitted")
	}
}

// Deleting the namespace makes Kubernetes delete the objects in it, and those
// deletes do not come from the operator. A webhook that refuses them leaves the
// namespace in Terminating forever.
func TestTheNamespaceControllersTeardownIsAdmitted(t *testing.T) {
	v := deviceValidator(t, terminatingNamespace())

	resp := v.Handle(context.Background(),
		deleteRequest("system:serviceaccount:kube-system:namespace-controller"))
	if !resp.Allowed {
		t.Errorf("a terminating namespace's teardown was refused: %v", resp.Result)
	}
}

// The exemption is the namespace's state and not the caller's name: anybody
// deleting a device out of a namespace that is going away is deleting something
// that is going away regardless.
func TestAUsersDeleteInATerminatingNamespaceIsAdmitted(t *testing.T) {
	v := deviceValidator(t, terminatingNamespace())

	if resp := v.Handle(context.Background(), deleteRequest("kubernetes-admin")); !resp.Allowed {
		t.Errorf("delete refused in a terminating namespace: %v", resp.Result)
	}
}

// A namespace that cannot be read is not evidence that it is terminating. The
// guard fails closed, because the alternative admits every delete for as long as
// the read fails.
func TestAnUnreadableNamespaceDoesNotOpenTheGuard(t *testing.T) {
	v := deviceValidator(t) // no Namespace object at all

	if resp := v.Handle(context.Background(), deleteRequest("kubernetes-admin")); resp.Allowed {
		t.Error("delete admitted while the namespace's state was unknown")
	}
}

// Only DELETE is this webhook's business. The mirror writes these objects
// constantly and a guard that inspected updates would have to admit them all.
func TestOtherOperationsAreNotThisWebhooksBusiness(t *testing.T) {
	v := deviceValidator(t, liveNamespace())

	for _, op := range []admissionv1.Operation{
		admissionv1.Create, admissionv1.Update, admissionv1.Connect,
	} {
		req := deleteRequest("kubernetes-admin")
		req.Operation = op
		if resp := v.Handle(context.Background(), req); !resp.Allowed {
			t.Errorf("%s was refused: %v", op, resp.Result)
		}
	}
}

// Regression: 2026-09-11-device-guard-refuses-the-collector. The guard's own
// contract says a device goes away with the StorageNode that owns it, and the
// mirror sets that owner reference. Kubernetes acts on it through the garbage
// collector, whose deletes carry its own identity rather than the operator's, so
// the guard refused them and every device outlived the node it belonged to. The
// records left behind are unreclaimable: their owner is gone, so nothing
// recreates the link, and no caller the guard admits ever asks again.
func TestTheGarbageCollectorMayDeleteAStorageDevice(t *testing.T) {
	v := deviceValidator(t, liveNamespace())

	resp := v.Handle(context.Background(), deleteRequest(
		"system:serviceaccount:kube-system:generic-garbage-collector"))
	if !resp.Allowed {
		t.Fatalf("the collector's cascade was refused: %s", resp.Result.Message)
	}
}

// The exemption is the collector's identity and not kube-system's: a service
// account that happens to live there is still a caller with no business
// withdrawing a device record.
func TestAnotherKubeSystemAccountMayNotDeleteAStorageDevice(t *testing.T) {
	v := deviceValidator(t, liveNamespace())

	resp := v.Handle(context.Background(), deleteRequest(
		"system:serviceaccount:kube-system:some-other-controller"))
	if resp.Allowed {
		t.Fatal("a delete from an unrelated kube-system account was admitted")
	}
}
