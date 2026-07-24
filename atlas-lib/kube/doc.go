// Package kube maps a simplyblock logical volume to the Kubernetes
// storage objects that represent it, and back.
//
// A single logical volume shows up across several Kubernetes resources:
//
//	lvol.VolumeHandle  ==  PV.Spec.CSI.VolumeHandle  (identity)
//	PV             <-  PVC  (PVC.Spec.VolumeName / PV.Spec.ClaimRef)
//	PV + Node      <-  VolumeAttachment              (where it's attached)
//	StorageClass.Parameters -> pool / qos / ...      (how it was created)
//	PV.Spec.CSI.VolumeAttributes -> VolumeContext    (node-stage inputs)
//
// This package centralizes those correlations and the string keys
// (parameters, volume context, publish context, labels, annotations,
// finalizers) so the operator and CSI driver agree on them. It depends on
// k8s.io/api directly; the live lookups are expressed as the Resolver interface.
// The package ships two implementations:
// LiveResolver (direct, uncached reads via a client-go clientset) and
// InformerResolver (cached reads via client-go shared informers, e.g. a
// controller-runtime manager cache). A consumer may also implement the
// interfaces with its own client — e.g. a controller-runtime client.Client.
package kube
