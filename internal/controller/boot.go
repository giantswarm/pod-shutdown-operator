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

	shutdownv1alpha1 "gitlab.opendefense.cloud/odc/shutdown-op/pod-shutdown-operator/api/v1alpha1"
)

// readBootID returns the boot ID of the operator node from the boot ID file.
func readBootID(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // The path is an operator setting.
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
// When the Node list fails, it also returns the error, and the map has no entry for the other nodes.
func (r *ShutdownRequestReconciler) currentBootIDs(ctx context.Context) (map[string]string, error) {
	log := ctrl.LoggerFrom(ctx)
	// The API reader keeps no Node cache in memory.
	var nodes corev1.NodeList
	listErr := r.APIReader.List(ctx, &nodes)
	if listErr != nil {
		listErr = fmt.Errorf("list the Nodes: %w", listErr)
	}
	bootIDs := map[string]string{}
	for _, node := range nodes.Items {
		if bootID := node.Status.NodeInfo.BootID; bootID != "" {
			bootIDs[node.Name] = bootID
		}
	}

	// After a reboot, the Node object can still show the old boot ID.
	delete(bootIDs, r.NodeName)
	bootID, err := readBootID(r.BootIDFile)
	// An unreadable file leaves the own entry unknown.
	if err != nil {
		log.Error(err, "Failed to read the boot ID, records it as unknown")
		return bootIDs, listErr
	}
	bootIDs[r.NodeName] = bootID
	return bootIDs, listErr
}

// checkBoot sets the action Cancel when the own boot ID does not match the record.
func (r *ShutdownRequestReconciler) checkBoot(ctx context.Context, request *shutdownv1alpha1.ShutdownRequest) error {
	// A request with no phase or in Restored has no record to restore.
	if request.Status.Phase == "" || request.Status.Phase == shutdownv1alpha1.PhaseRestored {
		return nil
	}

	log := ctrl.LoggerFrom(ctx)
	bootID, err := readBootID(r.BootIDFile)
	recorded := request.Status.BootIDs[r.NodeName]
	// An equal boot ID means an operator restart.
	if err == nil && bootID == recorded {
		return r.markBootChecked(ctx, request)
	}
	// A missing own entry also counts as a power cycle, on purpose. The node joined after the record,
	// or had an empty boot ID at record time. The operator cannot tell this from a power cycle,
	// and a wrong "no power cycle" keeps the cluster at 0 after a power-on. See #84.
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
	bootIDs, err := r.currentBootIDs(ctx)
	// A failed list must not keep the cluster at 0, so the record then holds only the own boot ID.
	if err != nil {
		log.Error(err, "Failed to list the Nodes, records only the own boot ID")
	}
	updated := request.DeepCopy()
	updated.Status.BootIDs = bootIDs
	if err := r.Client.Status().Update(ctx, updated); err != nil {
		return fmt.Errorf("record the boot IDs after a power cycle: %w", err)
	}
	*request = *updated
	return nil
}

// markBootChecked writes the identity of this process to status.bootCheckedBy.
// GET /progress then shows a Complete request as complete.
func (r *ShutdownRequestReconciler) markBootChecked(ctx context.Context, request *shutdownv1alpha1.ShutdownRequest) error {
	identity, err := r.leaderIdentity(ctx)
	if err != nil {
		return err
	}
	// A repeat check writes nothing.
	if request.Status.BootCheckedBy == identity {
		return nil
	}
	updated := request.DeepCopy()
	updated.Status.BootCheckedBy = identity
	if err := r.Client.Status().Update(ctx, updated); err != nil {
		return fmt.Errorf("mark the boot ID check: %w", err)
	}
	*request = *updated
	return nil
}

// leaderIdentity returns the leader election identity of this process. It reads the identity once.
// The identity does not change while the process runs.
func (r *ShutdownRequestReconciler) leaderIdentity(ctx context.Context) (string, error) {
	if r.identity != "" {
		return r.identity, nil
	}
	identity, err := r.LeaderIdentity(ctx)
	if err != nil {
		return "", fmt.Errorf("read the leader identity: %w", err)
	}
	// An empty identity matches no marker, so the endpoint could never show complete.
	if identity == "" {
		return "", errors.New("the leader identity is empty")
	}
	r.identity = identity
	return identity, nil
}
