// The `kubectl get lvm` columns.
//
// This is the one thing an aggregated API can do that a PersistentVolume
// annotation cannot: the columns of a built-in type are compiled into kubectl,
// so no amount of data attached to a PersistentVolume changes what `kubectl get
// pv` prints. Here the server decides, through rest.TableConvertor, and the
// choice of columns is therefore part of the API rather than a client's problem.
//
// What they answer, in order: whose volume, how big it was asked to be, how much
// of that it is really using, and where it lives. Used against Provisioned is
// the pair the type exists for, so they are adjacent and the percentage is
// beside them rather than left to the reader's arithmetic.

package metricsapi

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/duration"

	metricsv1alpha1 "github.com/simplyblock/simplyblock-operator/api/metrics/v1alpha1"
)

// columns are the table headers, in print order. Priority 1 hides a column
// unless the client asked for wide output, which is where the handle goes: it is
// the join key an operator occasionally needs and never the thing being read.
var columns = []metav1.TableColumnDefinition{
	{Name: "Name", Type: "string", Format: "name", Description: "The PersistentVolumeClaim this volume backs"},
	{Name: "Provisioned", Type: "string", Description: "The volume's nominal size"},
	{Name: "Used", Type: "string", Description: "The space the volume actually occupies"},
	{Name: "Used%", Type: "string", Description: "The control plane's utilization figure"},
	{Name: "Pool", Type: "string", Description: "The backend storage pool"},
	{Name: "Volume Handle", Type: "string", Priority: 1, Description: "The CSI volume handle"},
	{Name: "Age", Type: "date", Description: "The age of the claim, not of the reading"},
}

// ConvertToTable implements rest.TableConvertor for both a single reading and a
// list of them.
func (s *Storage) ConvertToTable(_ context.Context, object runtime.Object, _ runtime.Object) (*metav1.Table, error) {
	table := &metav1.Table{ColumnDefinitions: columns}

	switch typed := object.(type) {
	case *metricsv1alpha1.LogicalVolumeMetrics:
		table.Rows = append(table.Rows, row(typed))
	case *metricsv1alpha1.LogicalVolumeMetricsList:
		table.ResourceVersion = typed.ResourceVersion
		for i := range typed.Items {
			table.Rows = append(table.Rows, row(&typed.Items[i]))
		}
	default:
		return nil, fmt.Errorf("metricsapi: cannot render %T as a table", object)
	}

	// The list's own metadata is what paging and resource-version echoing read,
	// and kubectl treats a missing one as a malformed response.
	if m, err := meta.ListAccessor(object); err == nil {
		table.ResourceVersion = m.GetResourceVersion()
		table.Continue = m.GetContinue()
	}
	return table, nil
}

func row(reading *metricsv1alpha1.LogicalVolumeMetrics) metav1.TableRow {
	return metav1.TableRow{
		Cells: []any{
			reading.Name,
			reading.Capacity.Provisioned.String(),
			reading.Capacity.Used.String(),
			fmt.Sprintf("%d%%", reading.Capacity.UtilizationPercent),
			reading.PoolName,
			reading.VolumeHandle,
			translateTimestampSince(reading.CreationTimestamp),
		},
		Object: runtime.RawExtension{Object: reading},
	}
}

// translateTimestampSince renders an age the way every built-in printer does.
// An unset timestamp prints "<unknown>" rather than an age counted from the
// epoch, which is what a claim deleted between the list and the read produces.
func translateTimestampSince(timestamp metav1.Time) string {
	if timestamp.IsZero() {
		return "<unknown>"
	}
	return duration.HumanDuration(time.Since(timestamp.Time))
}
