// The Kubernetes observer: reads the StatefulSet, its pods, and their nodes into
// an Observation (design-pnfs-mds-standby.md §7.2).
//
// Every read is retried 3 times, 2 seconds apart, and a read that still fails
// fails the whole observation. A pass that cannot read what its decision
// depends on learns nothing, and is never counted as having seen a loss.

package mdsfailover

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// ClusterLabel is the pod and StatefulSet label carrying the storage
	// cluster's ID.
	ClusterLabel = "storage.simplyblock.io/cluster-id"
	// ContainerName is the runner container whose ID names a boot.
	ContainerName = "mds-runner"

	zoneLabel     = "topology.kubernetes.io/zone"
	revisionLabel = "controller-revision-hash"
)

// Sessions says which pods have a csi-link session now.
type Sessions interface {
	HasSession(pod string) bool
}

// KubeObserver observes one StatefulSet of the metadata server.
type KubeObserver struct {
	Client      client.Client
	Namespace   string
	StatefulSet string
	Sessions    Sessions
	Clock       Clock
}

// Observe reads the StatefulSet, its pods, and each pod's node.
func (o *KubeObserver) Observe(ctx context.Context) (Observation, error) {
	sts, err := readWithRetry(ctx, o.Clock, func() (*appsv1.StatefulSet, error) {
		var s appsv1.StatefulSet
		err := o.Client.Get(ctx, client.ObjectKey{Namespace: o.Namespace, Name: o.StatefulSet}, &s)
		return &s, err
	})
	if err != nil {
		return Observation{}, fmt.Errorf("reading StatefulSet %s: %w", o.StatefulSet, err)
	}
	rec, err := ParseRecord(sts.Annotations)
	if err != nil {
		return Observation{}, err
	}
	obs := Observation{
		Cluster:        sts.Labels[ClusterLabel],
		StatefulSet:    sts.Name,
		StatefulSetUID: string(sts.UID),
		Version:        sts.ResourceVersion,
		Record:         rec,
		StateVolume:    sts.Annotations[AnnotationStateVolume],
		UpdateRevision: sts.Status.UpdateRevision,
	}
	if sts.Spec.Replicas != nil {
		obs.Replicas = int(*sts.Spec.Replicas)
	}

	pods, err := readWithRetry(ctx, o.Clock, func() ([]corev1.Pod, error) {
		var list corev1.PodList
		selector := labels.SelectorFromSet(sts.Spec.Selector.MatchLabels)
		err := o.Client.List(ctx, &list, client.InNamespace(o.Namespace), client.MatchingLabelsSelector{Selector: selector})
		return list.Items, err
	})
	if err != nil {
		return Observation{}, fmt.Errorf("listing the pods of %s: %w", o.StatefulSet, err)
	}
	for i := range pods {
		g, err := o.guest(ctx, &pods[i])
		if err != nil {
			return Observation{}, err
		}
		obs.Guests = append(obs.Guests, g)
	}
	sortGuests(obs.Guests)
	return obs, nil
}

func (o *KubeObserver) guest(ctx context.Context, pod *corev1.Pod) (Guest, error) {
	g := Guest{
		Pod:        pod.Name,
		Ordinal:    Ordinal(pod.Name),
		Node:       pod.Spec.NodeName,
		UID:        string(pod.UID),
		Instance:   Instance(pod),
		PodIP:      pod.Status.PodIP,
		Revision:   pod.Labels[revisionLabel],
		Ready:      podReady(pod),
		Deleting:   !pod.DeletionTimestamp.IsZero(),
		HasSession: o.Sessions != nil && o.Sessions.HasSession(pod.Name),
	}
	if g.Node == "" {
		return g, nil
	}
	node, err := readWithRetry(ctx, o.Clock, func() (*corev1.Node, error) {
		var n corev1.Node
		err := o.Client.Get(ctx, client.ObjectKey{Name: g.Node}, &n)
		return &n, err
	})
	switch {
	case apierrors.IsNotFound(err):
		// A node that is gone is not Ready.
		return g, nil
	case err != nil:
		return Guest{}, fmt.Errorf("reading node %s: %w", g.Node, err)
	}
	g.NodeReady = nodeReady(node)
	g.Zone = node.Labels[zoneLabel]
	return g, nil
}

// Instance names one boot of a metadata server's guest: the pod's UID and its
// runner container's ID, the assembledBy format. A pod with no running runner
// yet is named by its UID alone, which matches no recorded instance.
func Instance(pod *corev1.Pod) string {
	for _, c := range pod.Status.ContainerStatuses {
		if c.Name == ContainerName && c.ContainerID != "" {
			return string(pod.UID) + "/" + c.ContainerID
		}
	}
	return string(pod.UID)
}

// Ordinal is the number a StatefulSet's pod name ends in, or -1.
func Ordinal(pod string) int {
	i := strings.LastIndex(pod, "-")
	if i < 0 {
		return -1
	}
	n, err := strconv.Atoi(pod[i+1:])
	if err != nil {
		return -1
	}
	return n
}

// StatefulSetOf is the name of the StatefulSet a pod belongs to, which is its
// name without the ordinal.
func StatefulSetOf(pod string) string {
	if Ordinal(pod) < 0 {
		return pod
	}
	return pod[:strings.LastIndex(pod, "-")]
}

func sortGuests(gs []Guest) {
	for i := 1; i < len(gs); i++ {
		for j := i; j > 0 && gs[j].Ordinal < gs[j-1].Ordinal; j-- {
			gs[j], gs[j-1] = gs[j-1], gs[j]
		}
	}
}

func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func nodeReady(node *corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
