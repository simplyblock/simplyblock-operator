package controller

import (
	"os"
	"strings"
	"testing"
)

func TestTaskControllerHasNoDuplicateClusterAuthErrorBranch(t *testing.T) {
	content, err := os.ReadFile("simplyblocktask_controller.go")
	if err != nil {
		t.Fatalf("failed to read task controller source: %v", err)
	}

	const branch = "if err != nil {\n\t\tlog.Error(err, \"Failed to get cluster auth\")\n\t\treturn ctrl.Result{RequeueAfter: 10 * time.Second}, nil\n\t}"
	if got := strings.Count(string(content), branch); got != 1 {
		t.Fatalf("expected exactly one cluster-auth error branch, found %d", got)
	}
}
