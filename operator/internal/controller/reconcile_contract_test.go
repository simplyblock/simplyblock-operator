package controller

import (
	"context"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type notFoundReconciler interface {
	Reconcile(context.Context, reconcile.Request) (reconcile.Result, error)
}

func expectIgnoreNotFoundNoRequeue(r notFoundReconciler, missingName string) {
	res, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      missingName,
			Namespace: "default",
		},
	})

	Expect(err).NotTo(HaveOccurred())
	Expect(res).To(Equal(reconcile.Result{}))
}
