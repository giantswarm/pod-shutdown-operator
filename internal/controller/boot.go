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

// readBootID returns the boot ID of the operator node from the boot ID file.
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
	// The list reads the API server once, so that the operator keeps no cache of all Nodes.
	var nodes corev1.NodeList
	// With no Node list, only the own entry is known, and the shutdown goes on.
	if err := r.APIReader.List(ctx, &nodes); err != nil {
		log.Error(err, "Failed to list the Nodes, records only the own boot ID")
	}
	bootIDs := map[string]string{}
	for _, node := range nodes.Items {
		if bootID := node.Status.NodeInfo.BootID; bootID != "" {
			bootIDs[node.Name] = bootID
		}
	}

	// The kubelet can post the boot ID of a new boot after the operator starts, so the own entry comes from the file.
	delete(bootIDs, r.NodeName)
	bootID, err := r.readBootID()
	// With no readable file, the own entry is unknown and the shutdown goes on.
	if err != nil {
		log.Error(err, "Failed to read the boot ID, records it as unknown")
		return bootIDs
	}
	bootIDs[r.NodeName] = bootID
	return bootIDs
}

// checkBoot compares the own boot ID with the record. After a power cycle, it sets the action Cancel and records the new boot IDs.
func (r *ShutdownRequestReconciler) checkBoot(ctx context.Context, request *shutdownv1alpha1.ShutdownRequest) error {
	// With no phase, the request has no record. In Restored, the record is already restored.
	if request.Status.Phase == "" || request.Status.Phase == shutdownv1alpha1.PhaseRestored {
		return nil
	}

	log := ctrl.LoggerFrom(ctx)
	bootID, err := r.readBootID()
	recorded := request.Status.BootIDs[r.NodeName]
	// An equal boot ID means an operator restart, and the current phase continues.
	if err == nil && bootID == recorded {
		return nil
	}
	// An unreadable file or a missing entry also counts as a power cycle, so that the cluster does not stay at 0.
	if err != nil {
		log.Error(err, "Failed to read the boot ID, counts it as a power cycle")
	}
	log.Info("Found a power cycle, scales up from the record", "node", r.NodeName, "bootID", bootID, "recordedBootID", recorded)

	// A Shutdown from before the power cycle must not stop the cluster again. A later POST /shutdown can.
	// With the action Cancel, the reconcile already starts or continues the scale-up.
	if request.Spec.Action != shutdownv1alpha1.ActionCancel {
		patch := client.MergeFromWithOptions(request.DeepCopy(), client.MergeFromWithOptimisticLock{})
		request.Spec.Action = shutdownv1alpha1.ActionCancel
		if err := r.Client.Patch(ctx, request, patch); err != nil {
			return fmt.Errorf("set the action Cancel after a power cycle: %w", err)
		}
	}
	// The new boot IDs come after the Cancel, so that a stop between the two writes finds the power cycle again.
	// A later restart in this boot then keeps a POST /shutdown that comes during the scale-up.
	updated := request.DeepCopy()
	updated.Status.BootIDs = r.currentBootIDs(ctx)
	if err := r.Client.Status().Update(ctx, updated); err != nil {
		return fmt.Errorf("record the boot IDs after a power cycle: %w", err)
	}
	*request = *updated
	return nil
}
