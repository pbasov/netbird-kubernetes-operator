/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"text/template"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/go-logr/logr"
	netbirdiov1 "github.com/netbirdio/kubernetes-operator/api/v1"
	"github.com/netbirdio/kubernetes-operator/internal/util"
	netbird "github.com/netbirdio/netbird/management/client/rest"
	"github.com/netbirdio/netbird/management/server/http/api"
)

const nodePeerFinalizer = "netbird.io/node-peer-cleanup"

// NBNodePeerReconciler reconciles a NBNodePeer object
type NBNodePeerReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	APIKey        string
	ManagementURL string
	ClusterName   string
	DefaultLabels map[string]string
	netbird       *netbird.Client
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *NBNodePeerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, err error) {
	logger := ctrl.Log.WithName("NBNodePeer").WithValues("name", req.Name)
	logger.Info("Reconciling NBNodePeer")

	// Get NBNodePeer resource
	nodePeer := &netbirdiov1.NBNodePeer{}
	err = r.Get(ctx, req.NamespacedName, nodePeer)
	if err != nil {
		if !errors.IsNotFound(err) {
			logger.Error(errKubernetesAPI, "error getting NBNodePeer", "err", err)
		}
		return ctrl.Result{RequeueAfter: defaultRequeueAfter}, nil
	}

	originalNodePeer := nodePeer.DeepCopy()
	defer func() {
		if originalNodePeer.DeletionTimestamp != nil && len(nodePeer.Finalizers) == 0 {
			return
		}
		if !originalNodePeer.Status.Equal(nodePeer.Status) {
			updateErr := r.Client.Status().Update(ctx, nodePeer)
			if updateErr != nil {
				logger.Error(errKubernetesAPI, "error updating NBNodePeer Status", "err", updateErr)
				err = updateErr
			}
		}
		if err != nil {
			res = ctrl.Result{}
			return
		}
		if !res.Requeue && res.RequeueAfter == 0 {
			res.RequeueAfter = defaultRequeueAfter
		}
	}()

	// Handle deletion
	if nodePeer.DeletionTimestamp != nil {
		if len(nodePeer.Finalizers) == 0 {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, r.handleDelete(ctx, nodePeer, logger)
	}

	// Ensure finalizer
	if !util.Contains(nodePeer.Finalizers, nodePeerFinalizer) {
		nodePeer.Finalizers = append(nodePeer.Finalizers, nodePeerFinalizer)
		if err := r.Update(ctx, nodePeer); err != nil {
			logger.Error(errKubernetesAPI, "error adding finalizer", "err", err)
			return ctrl.Result{}, err
		}
	}

	// Parse node selector
	selector, err := metav1.LabelSelectorAsSelector(&nodePeer.Spec.NodeSelector)
	if err != nil {
		logger.Error(errInvalidValue, "invalid node selector", "err", err)
		nodePeer.Status.Conditions = netbirdiov1.NBConditionFalse("InvalidSelector", err.Error())
		return ctrl.Result{}, nil
	}

	// List nodes matching selector
	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList, client.MatchingLabelsSelector{Selector: selector}); err != nil {
		logger.Error(errKubernetesAPI, "error listing nodes", "err", err)
		nodePeer.Status.Conditions = netbirdiov1.NBConditionFalse("APIError", err.Error())
		return ctrl.Result{}, err
	}

	logger.Info("Found matching nodes", "count", len(nodeList.Items))

	// Initialize status.Nodes if nil
	if nodePeer.Status.Nodes == nil {
		nodePeer.Status.Nodes = make(map[string]netbirdiov1.NBNodePeerNodeStatus)
	}

	// Reconcile each node
	currentNodes := make(map[string]bool)
	for _, node := range nodeList.Items {
		currentNodes[node.Name] = true
		if err := r.reconcileNode(ctx, nodePeer, &node, logger); err != nil {
			logger.Error(err, "failed to reconcile node", "node", node.Name)
			// Continue with other nodes
		}
	}

	// Clean up nodes that no longer match
	for nodeName := range nodePeer.Status.Nodes {
		if !currentNodes[nodeName] {
			logger.Info("Cleaning up removed node", "node", nodeName)
			if err := r.cleanupNode(ctx, nodePeer, nodeName, logger); err != nil {
				logger.Error(err, "failed to cleanup node", "node", nodeName)
			}
		}
	}

	nodePeer.Status.Conditions = netbirdiov1.NBConditionTrue()
	return ctrl.Result{}, nil
}

// reconcileNode ensures a setup key and secret exist for a node
func (r *NBNodePeerReconciler) reconcileNode(ctx context.Context, nodePeer *netbirdiov1.NBNodePeer, node *corev1.Node, logger logr.Logger) error {
	nodeStatus, exists := nodePeer.Status.Nodes[node.Name]
	secretName := r.generateSecretName(nodePeer, node.Name)
	secretNamespace := nodePeer.Spec.SecretNamespace
	if secretNamespace == "" {
		secretNamespace = "netbird-system"
	}

	if !exists {
		// Create new setup key
		logger.Info("Creating setup key for node", "node", node.Name)

		setupKeyType := nodePeer.Spec.SetupKeyType
		if setupKeyType == "" {
			setupKeyType = "one-off"
		}

		ephemeral := true
		if nodePeer.Spec.Ephemeral != nil {
			ephemeral = *nodePeer.Spec.Ephemeral
		}

		createReq := api.CreateSetupKeyRequest{
			AutoGroups: nodePeer.Spec.AutoGroups,
			Ephemeral:  util.Ptr(ephemeral),
			Name:       fmt.Sprintf("%s-%s", r.ClusterName, node.Name),
			Type:       setupKeyType,
		}

		if nodePeer.Spec.ExpiresIn != nil {
			createReq.ExpiresIn = *nodePeer.Spec.ExpiresIn
		}

		if nodePeer.Spec.UsageLimit != nil {
			createReq.UsageLimit = *nodePeer.Spec.UsageLimit
		}

		setupKey, err := r.netbird.SetupKeys.Create(ctx, createReq)
		if err != nil {
			logger.Error(errNetBirdAPI, "error creating setup key", "err", err, "node", node.Name)
			return fmt.Errorf("failed to create setup key: %w", err)
		}

		// Create secret
		secretLabels := make(map[string]string)
		for k, v := range r.DefaultLabels {
			secretLabels[k] = v
		}
		secretLabels["netbird.io/node-peer"] = nodePeer.Name
		secretLabels["netbird.io/node-name"] = node.Name

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretName,
				Namespace: secretNamespace,
				Labels:    secretLabels,
			},
			StringData: map[string]string{
				"setupKey":      setupKey.Key,
				"managementURL": r.ManagementURL,
				"nodeName":      node.Name,
			},
		}

		if err := r.Create(ctx, secret); err != nil {
			if errors.IsAlreadyExists(err) {
				// Update existing secret
				existingSecret := &corev1.Secret{}
				if getErr := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: secretNamespace}, existingSecret); getErr != nil {
					return fmt.Errorf("failed to get existing secret: %w", getErr)
				}
				existingSecret.Data = map[string][]byte{
					"setupKey":      []byte(setupKey.Key),
					"managementURL": []byte(r.ManagementURL),
					"nodeName":      []byte(node.Name),
				}
				if updateErr := r.Update(ctx, existingSecret); updateErr != nil {
					return fmt.Errorf("failed to update existing secret: %w", updateErr)
				}
			} else {
				return fmt.Errorf("failed to create secret: %w", err)
			}
		}

		// Update status
		nodePeer.Status.Nodes[node.Name] = netbirdiov1.NBNodePeerNodeStatus{
			SetupKeyID:   setupKey.Id,
			SecretName:   secretName,
			LastSyncTime: metav1.Now(),
		}

		logger.Info("Created setup key and secret for node", "node", node.Name, "secret", secretName)
	} else {
		// Verify setup key still exists and is valid
		setupKey, err := r.netbird.SetupKeys.Get(ctx, nodeStatus.SetupKeyID)
		if err != nil {
			if strings.Contains(err.Error(), "not found") {
				// Setup key was deleted, recreate
				logger.Info("Setup key not found, will recreate", "node", node.Name)
				delete(nodePeer.Status.Nodes, node.Name)
				return nil
			}
			return fmt.Errorf("failed to get setup key: %w", err)
		}

		if setupKey.Revoked {
			logger.Info("Setup key revoked, will recreate", "node", node.Name)
			// Delete the revoked key
			if err := r.netbird.SetupKeys.Delete(ctx, nodeStatus.SetupKeyID); err != nil {
				if !strings.Contains(err.Error(), "not found") {
					logger.Error(errNetBirdAPI, "error deleting revoked setup key", "err", err)
				}
			}
			delete(nodePeer.Status.Nodes, node.Name)
			return nil
		}

		// Verify secret exists
		secret := &corev1.Secret{}
		secretKey := types.NamespacedName{
			Name:      nodeStatus.SecretName,
			Namespace: secretNamespace,
		}
		if err := r.Get(ctx, secretKey, secret); errors.IsNotFound(err) {
			// Secret was deleted, recreate by removing from status
			logger.Info("Secret not found, will recreate", "node", node.Name)
			delete(nodePeer.Status.Nodes, node.Name)
		}
	}

	return nil
}

// cleanupNode removes the setup key and secret for a node
func (r *NBNodePeerReconciler) cleanupNode(ctx context.Context, nodePeer *netbirdiov1.NBNodePeer, nodeName string, logger logr.Logger) error {
	nodeStatus, exists := nodePeer.Status.Nodes[nodeName]
	if !exists {
		return nil
	}

	secretNamespace := nodePeer.Spec.SecretNamespace
	if secretNamespace == "" {
		secretNamespace = "netbird-system"
	}

	// Delete secret
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nodeStatus.SecretName,
			Namespace: secretNamespace,
		},
	}
	if err := r.Delete(ctx, secret); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("failed to delete secret: %w", err)
	}

	// Revoke setup key if configured
	revokeOnDelete := true
	if nodePeer.Spec.RevokeOnDelete != nil {
		revokeOnDelete = *nodePeer.Spec.RevokeOnDelete
	}

	if revokeOnDelete {
		if err := r.netbird.SetupKeys.Delete(ctx, nodeStatus.SetupKeyID); err != nil {
			if !strings.Contains(err.Error(), "not found") {
				return fmt.Errorf("failed to delete setup key: %w", err)
			}
		}
		logger.Info("Revoked setup key for node", "node", nodeName)
	}

	delete(nodePeer.Status.Nodes, nodeName)
	logger.Info("Cleaned up node", "node", nodeName)
	return nil
}

// handleDelete handles the deletion of NBNodePeer
func (r *NBNodePeerReconciler) handleDelete(ctx context.Context, nodePeer *netbirdiov1.NBNodePeer, logger logr.Logger) error {
	logger.Info("Handling NBNodePeer deletion")

	// Clean up all nodes
	for nodeName := range nodePeer.Status.Nodes {
		if err := r.cleanupNode(ctx, nodePeer, nodeName, logger); err != nil {
			logger.Error(err, "failed to cleanup node during deletion", "node", nodeName)
			return err
		}
	}

	// Remove finalizer
	nodePeer.Finalizers = util.Without(nodePeer.Finalizers, nodePeerFinalizer)
	if err := r.Update(ctx, nodePeer); err != nil {
		logger.Error(errKubernetesAPI, "error removing finalizer", "err", err)
		return err
	}

	logger.Info("NBNodePeer deletion complete")
	return nil
}

// generateSecretName generates the secret name from template
func (r *NBNodePeerReconciler) generateSecretName(nodePeer *netbirdiov1.NBNodePeer, nodeName string) string {
	templateStr := nodePeer.Spec.SecretNameTemplate
	if templateStr == "" {
		templateStr = "nb-node-{{.NodeName}}"
	}

	tmpl, err := template.New("secretName").Parse(templateStr)
	if err != nil {
		return fmt.Sprintf("nb-node-%s", nodeName)
	}

	var buf bytes.Buffer
	data := struct{ NodeName string }{NodeName: nodeName}
	if err := tmpl.Execute(&buf, data); err != nil {
		return fmt.Sprintf("nb-node-%s", nodeName)
	}
	return buf.String()
}

// mapNodeToNBNodePeer maps Node events to NBNodePeer reconcile requests
func (r *NBNodePeerReconciler) mapNodeToNBNodePeer(ctx context.Context, obj client.Object) []reconcile.Request {
	node := obj.(*corev1.Node)

	// List all NBNodePeer resources
	nodePeerList := &netbirdiov1.NBNodePeerList{}
	if err := r.List(ctx, nodePeerList); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for _, np := range nodePeerList.Items {
		selector, err := metav1.LabelSelectorAsSelector(&np.Spec.NodeSelector)
		if err != nil {
			continue
		}
		if selector.Matches(labels.Set(node.Labels)) {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: np.Name},
			})
		}
	}
	return requests
}

// mapSecretToNBNodePeer maps Secret events to NBNodePeer reconcile requests
func (r *NBNodePeerReconciler) mapSecretToNBNodePeer(ctx context.Context, obj client.Object) []reconcile.Request {
	secret := obj.(*corev1.Secret)

	// Check if this secret is managed by us
	nodePeerName, ok := secret.Labels["netbird.io/node-peer"]
	if !ok {
		return nil
	}

	return []reconcile.Request{
		{NamespacedName: types.NamespacedName{Name: nodePeerName}},
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *NBNodePeerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.netbird = netbird.New(r.ManagementURL, r.APIKey)

	return ctrl.NewControllerManagedBy(mgr).
		For(&netbirdiov1.NBNodePeer{}).
		Named("nbnodepeer").
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.mapNodeToNBNodePeer)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.mapSecretToNBNodePeer)).
		Complete(r)
}
