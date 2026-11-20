// Tests for the client a read-only stage runs against.
//
// The property is two-sided and both sides matter. A write that got through
// would change a cluster the user was told nothing would change on, and a read
// that is refused turns a check somebody writes later into a failure whose
// message blames the stage rather than the code.

package upgrade

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// readOnlyOver wraps a cluster holding one Pod, which is an object with a
// status subresource to read.
func readOnlyOver(t *testing.T) (client.Client, *corev1.Pod) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("building the scheme: %v", err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "operator", Namespace: "simplyblock"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	inner := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(pod).WithStatusSubresource(pod).Build()
	return NewReadOnlyClient(inner), pod
}

func TestReadOnlyClient_PassesReadsThrough(t *testing.T) {
	c, pod := readOnlyOver(t)

	var read corev1.Pod
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(pod), &read); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if read.Status.Phase != corev1.PodRunning {
		t.Errorf("phase = %q", read.Status.Phase)
	}

	var pods corev1.PodList
	if err := c.List(t.Context(), &pods); err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Errorf("listed %d pods", len(pods.Items))
	}
}

func TestReadOnlyClient_PassesASubresourceReadThrough(t *testing.T) {
	// A subresource read is a read. Refusing it would fail a check that looks
	// at a status with a message saying the stage may not write, which is a
	// message about the wrong thing.
	//
	// The read is intercepted rather than served, because the fake client
	// answers a subresource Get for scale and for nothing else. What is under
	// test is that the call arrives at the client underneath, and an
	// interceptor is what observes that.
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("building the scheme: %v", err)
	}

	var reached string
	inner := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		SubResourceGet: func(
			_ context.Context, _ client.Client, subResource string,
			_ client.Object, _ client.Object, _ ...client.SubResourceGetOption,
		) error {
			reached = subResource
			return nil
		},
	}).Build()

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "operator", Namespace: "simplyblock"}}
	if err := NewReadOnlyClient(inner).SubResource("status").Get(t.Context(), pod, pod); err != nil {
		t.Fatalf("SubResource(\"status\").Get: %v", err)
	}
	if reached != "status" {
		t.Errorf("the read reached %q, want it to arrive at the client underneath", reached)
	}
}

func TestReadOnlyClient_RefusesEveryWrite(t *testing.T) {
	c, pod := readOnlyOver(t)
	other := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "simplyblock"}}

	for name, write := range map[string]func() error{
		"create": func() error { return c.Create(t.Context(), other) },
		"update": func() error { return c.Update(t.Context(), pod) },
		"delete": func() error { return c.Delete(t.Context(), pod) },
		"patch": func() error {
			return c.Patch(t.Context(), pod, client.RawPatch(types.MergePatchType, []byte(`{}`)))
		},
		"deleteAllOf":   func() error { return c.DeleteAllOf(t.Context(), other) },
		"status update": func() error { return c.Status().Update(t.Context(), pod) },
		"subresource update": func() error {
			return c.SubResource("status").Update(t.Context(), pod)
		},
		"subresource patch": func() error {
			return c.SubResource("status").Patch(t.Context(), pod, client.RawPatch(types.MergePatchType, []byte(`{}`)))
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := write()
			if err == nil {
				t.Fatal("the write was accepted")
			}
			if !errors.Is(err, ErrReadOnly) {
				t.Errorf("error is %v, want it to wrap ErrReadOnly", err)
			}
		})
	}
}

func TestReadOnlyClient_RefusesADryRunWrite(t *testing.T) {
	// §27: the plan mutates nothing, including through server-side dry-run
	// writes. A dry-run write is still a request every mutating webhook sees.
	c, pod := readOnlyOver(t)

	if err := c.Update(t.Context(), pod, client.DryRunAll); !errors.Is(err, ErrReadOnly) {
		t.Errorf("a dry-run update returned %v", err)
	}
}
