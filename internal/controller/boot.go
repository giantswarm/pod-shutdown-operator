package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	shutdownv1alpha1 "github.com/giantswarm/pod-shutdown-operator/api/v1alpha1"
)

// readBootID returns the boot ID of the operator node.
func (r *ShutdownRequestReconciler) readBootID() (string, error) {
	data, err := os.ReadFile(r.BootIDFile)
	if err != nil {
		return "", fmt.Errorf("read the boot ID file: %w", err)
	}
	bootID := strings.TrimSpace(string(data))
	if bootID == "" {
		return "", errors.New("the boot ID file is empty")
	}
	return bootID, nil
}

// currentBootIDs returns the boot ID of each node by node name.
func (r *ShutdownRequestReconciler) currentBootIDs(ctx context.Context) map[string]string {
	log := ctrl.LoggerFrom(ctx)
	// The API reader keeps no Node cache in memory.
	var nodes corev1.NodeList
	// A failed list must not block the shutdown.
	if err := r.APIReader.List(ctx, &nodes); err != nil {
		log.Error(err, "Failed to list the Nodes, records only the own boot ID")
	}
	bootIDs := map[string]string{}
	for _, node := range nodes.Items {
		if bootID := node.Status.NodeInfo.BootID; bootID != "" {
			bootIDs[node.Name] = bootID
		}
	}

	// After a reboot, the Node object can still show the old boot ID.
	delete(bootIDs, r.NodeName)
	bootID, err := r.readBootID()
	// An unreadable file leaves the own entry unknown.
	if err != nil {
		log.Error(err, "Failed to read the boot ID, records it as unknown")
		return bootIDs
	}
	bootIDs[r.NodeName] = bootID
	return bootIDs
}

// checkBoot sets the action Cancel when the own boot ID does not match the record.
func (r *ShutdownRequestReconciler) checkBoot(ctx context.Context, request *shutdownv1alpha1.ShutdownRequest) error {
	// A request with no phase or in Restored has no record to restore.
	if request.Status.Phase == "" || request.Status.Phase == shutdownv1alpha1.PhaseRestored {
		return nil
	}

	log := ctrl.LoggerFrom(ctx)
	bootID, err := r.readBootID()
	recorded := request.Status.BootIDs[r.NodeName]
	// An equal boot ID means an operator restart.
	if err == nil && bootID == recorded {
		return nil
	}
	// An unknown boot ID counts as a power cycle, so that the cluster does not stay at 0.
	if err != nil {
		log.Error(err, "Failed to read the boot ID, counts it as a power cycle")
	}
	log.Info("Found a power cycle, scales up from the record", "node", r.NodeName, "bootID", bootID, "recordedBootID", recorded)

	// A Shutdown from before the power cycle must not stop the cluster again.
	if request.Spec.Action != shutdownv1alpha1.ActionCancel {
		patch := client.MergeFromWithOptions(request.DeepCopy(), client.MergeFromWithOptimisticLock{})
		request.Spec.Action = shutdownv1alpha1.ActionCancel
		if err := r.Client.Patch(ctx, request, patch); err != nil {
			return fmt.Errorf("set the action Cancel after a power cycle: %w", err)
		}
	}
	// Write the boot IDs after the Cancel, so that a crash between the writes repeats the check.
	// A later restart in this boot then does not revert a new Shutdown.
	updated := request.DeepCopy()
	updated.Status.BootIDs = r.currentBootIDs(ctx)
	if err := r.Client.Status().Update(ctx, updated); err != nil {
		return fmt.Errorf("record the boot IDs after a power cycle: %w", err)
	}
	*request = *updated
	return nil
}
