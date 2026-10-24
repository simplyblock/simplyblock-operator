// The control plane's tasks: the asynchronous work a call queues, such as a
// replication cutover, read by the caller that waits for it.

package controlplane

import (
	"context"
	"fmt"

	"github.com/simplyblock/atlas/internal/cpapi"
)

// Task is one control-plane task.
type Task struct {
	ID       string
	Function string
	Status   string

	// Result is what the task reported when it finished, or the reason it is
	// waiting.
	Result string

	Canceled bool
	Retry    int
	MaxRetry int
}

// Done is whether the task has finished, successfully or not.
func (t Task) Done() bool { return t.Status == string(cpapi.TaskDTOStatusDone) }

// Task reads a task of the cluster.
func (c *Client) Task(ctx context.Context, clusterID, taskID string) (Task, error) {
	cluster, err := parseUUID("cluster id", clusterID)
	if err != nil {
		return Task{}, err
	}
	id, err := parseUUID("task id", taskID)
	if err != nil {
		return Task{}, err
	}
	resp, err := c.api.ClustersTasksDetailApiV2ClustersClusterIdTasksTaskIdGetWithResponse(ctx, cluster, id, nil)
	if err != nil {
		return Task{}, fmt.Errorf("task %s of cluster %s: %w", taskID, clusterID, err)
	}
	d, err := payload("task "+taskID+" of cluster "+clusterID, resp.JSON200, resp.StatusCode(), resp.Body)
	if err != nil {
		return Task{}, err
	}
	return Task{
		ID:       d.Id.String(),
		Function: string(d.FunctionName),
		Status:   string(d.Status),
		Result:   d.FunctionResult,
		Canceled: d.Canceled,
		Retry:    d.Retry,
		MaxRetry: intFrom(d.MaxRetry),
	}, nil
}
