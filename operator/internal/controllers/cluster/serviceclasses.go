// The cluster's I/O service classes: spec.serviceClasses as the control plane's
// QoS endpoint holds them, and the steady-state step that keeps the two equal.
//
// It lives in its own file because it is a reconcile step of its own with a wire
// format of its own; sync calls it and writes what it returns.

package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"

	corev1 "k8s.io/api/core/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	simplyblockv1alpha2 "github.com/simplyblock/simplyblock-operator/api/v1alpha2"
)

// The values the CRD defaults to, repeated for an object whose defaults the API
// server never applied: one written before the field existed and then edited
// through the v1alpha1 version.
const (
	defaultServiceClassWeight = int32(100)
	defaultReplicationShare   = int32(10)
)

// ServiceClassConfig is a cluster's service-class configuration as the control
// plane reads and writes it.
type ServiceClassConfig struct {
	DefaultWeight     int32              `json:"default_weight"`
	Classes           []ServiceClassWire `json:"classes"`
	ReplicationWeight int32              `json:"replication_weight"`
}

// ServiceClassWire is one class besides Default. ID is the control plane's and
// is never sent: a class keeps its id, and a new one is given the lowest free.
type ServiceClassWire struct {
	ID     int32  `json:"id,omitempty"`
	Name   string `json:"name"`
	Weight int32  `json:"weight"`
}

func (c *httpControlPlane) ServiceClasses(ctx context.Context, clusterID string) (ServiceClassConfig, error) {
	var config ServiceClassConfig
	body, err := c.call(ctx, http.MethodGet, "/api/v2/clusters/"+clusterID+"/qos/", nil)
	if err != nil {
		return config, err
	}
	if err := json.Unmarshal(body, &config); err != nil {
		return config, fmt.Errorf("read the service classes: %w", err)
	}
	return config, nil
}

func (c *httpControlPlane) SetServiceClasses(
	ctx context.Context, clusterID string, config ServiceClassConfig,
) error {
	_, err := c.call(ctx, http.MethodPut, "/api/v2/clusters/"+clusterID+"/qos/", config)
	return err
}

// desiredServiceClasses is spec.serviceClasses on the wire, with the defaults
// filled in and the classes in a fixed order.
func desiredServiceClasses(spec *simplyblockv1alpha2.ServiceClassesSpec) ServiceClassConfig {
	config := ServiceClassConfig{
		DefaultWeight:     defaultServiceClassWeight,
		ReplicationWeight: defaultReplicationShare,
		// Never null: the control plane reads the list as the whole set.
		Classes: []ServiceClassWire{},
	}
	if spec.DefaultWeight != nil {
		config.DefaultWeight = *spec.DefaultWeight
	}
	if spec.ReplicationShare != nil {
		config.ReplicationWeight = *spec.ReplicationShare
	}
	for _, class := range spec.Classes {
		config.Classes = append(config.Classes, ServiceClassWire{Name: class.Name, Weight: class.Weight})
	}
	sortByName(config.Classes)
	return config
}

// serviceClassesMatch compares what the control plane holds with what is wanted,
// ignoring the ids and the order the control plane lists the classes in.
func serviceClassesMatch(current, want ServiceClassConfig) bool {
	if current.DefaultWeight != want.DefaultWeight ||
		current.ReplicationWeight != want.ReplicationWeight ||
		len(current.Classes) != len(want.Classes) {
		return false
	}
	weights := make(map[string]int32, len(current.Classes))
	for _, class := range current.Classes {
		weights[class.Name] = class.Weight
	}
	for _, class := range want.Classes {
		if weight, ok := weights[class.Name]; !ok || weight != class.Weight {
			return false
		}
	}
	return true
}

func serviceClassStatus(config ServiceClassConfig) []simplyblockv1alpha2.ServiceClassStatus {
	classes := append([]ServiceClassWire(nil), config.Classes...)
	sortByName(classes)
	status := make([]simplyblockv1alpha2.ServiceClassStatus, 0, len(classes))
	for _, class := range classes {
		status = append(status, simplyblockv1alpha2.ServiceClassStatus{
			Name: class.Name, ID: class.ID, Weight: class.Weight,
		})
	}
	return status
}

func sortByName(classes []ServiceClassWire) {
	sort.Slice(classes, func(i, j int) bool { return classes[i].Name < classes[j].Name })
}

// applyServiceClasses makes the control plane's service classes equal to
// spec.serviceClasses, and returns them for status with true when they were
// read. Nothing it meets fails the sync: a refusal and an old control plane are
// events, and a failed read leaves status.serviceClasses as it was.
//
// It asks before it acts: the configuration is sent only when the reading
// differs, so a steady cluster costs one GET per sync and no events.
func (r *StorageClusterReconciler) applyServiceClasses(
	ctx context.Context, cluster *simplyblockv1alpha2.StorageCluster,
) ([]simplyblockv1alpha2.ServiceClassStatus, bool) {
	spec := cluster.Spec.ServiceClasses
	if spec == nil {
		// Unmanaged: the classes are whatever the control plane's own tools
		// made them, and status does not claim to know them.
		return nil, true
	}
	log := logf.FromContext(ctx)

	current, err := r.API.ServiceClasses(ctx, cluster.Status.UUID)
	if isNotFound(err) {
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning,
			ServiceClassesUnsupported, ServiceClassesUnsupported,
			"The control plane cannot configure service classes; spec.serviceClasses is not applied")
		return nil, false
	}
	if err != nil {
		log.Error(err, "the service classes could not be read", "cluster", cluster.Name)
		return nil, false
	}

	want := desiredServiceClasses(spec)
	if serviceClassesMatch(current, want) {
		return serviceClassStatus(current), true
	}
	if err := r.API.SetServiceClasses(ctx, cluster.Status.UUID, want); err != nil {
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning,
			ServiceClassesRefused, ServiceClassesRefused,
			"The service classes were not applied: %v", err)
		return serviceClassStatus(current), true
	}
	r.Recorder.Eventf(cluster, nil, corev1.EventTypeNormal,
		ServiceClassesApplied, ServiceClassesApplied,
		"Applied %d service classes besides Default, default weight %d, replication share %d%%",
		len(want.Classes), want.DefaultWeight, want.ReplicationWeight)

	// Read back for the ids the control plane gave the new classes.
	applied, err := r.API.ServiceClasses(ctx, cluster.Status.UUID)
	if err != nil {
		log.Error(err, "the applied service classes could not be read back", "cluster", cluster.Name)
		return nil, false
	}
	return serviceClassStatus(applied), true
}

// isNotFound reports a control plane that answered 404, which for the QoS
// endpoint means one older than it.
func isNotFound(err error) bool {
	var refused *ControlPlaneError
	return errors.As(err, &refused) && refused.Status == http.StatusNotFound
}
