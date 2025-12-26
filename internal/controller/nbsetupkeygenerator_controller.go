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
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

const setupKeyGeneratorFinalizer = "netbird.io/setup-key-generator-cleanup"

// NBSetupKeyGeneratorReconciler reconciles a NBSetupKeyGenerator object
type NBSetupKeyGeneratorReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	APIKey        string
	ManagementURL string
	DefaultLabels map[string]string
	netbird       *netbird.Client
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *NBSetupKeyGeneratorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, err error) {
	logger := ctrl.Log.WithName("NBSetupKeyGenerator").WithValues("namespace", req.Namespace, "name", req.Name)
	logger.Info("Reconciling NBSetupKeyGenerator")

	// Get NBSetupKeyGenerator resource
	generator := &netbirdiov1.NBSetupKeyGenerator{}
	err = r.Get(ctx, req.NamespacedName, generator)
	if err != nil {
		if !errors.IsNotFound(err) {
			logger.Error(errKubernetesAPI, "error getting NBSetupKeyGenerator", "err", err)
		}
		return ctrl.Result{RequeueAfter: defaultRequeueAfter}, nil
	}

	originalGenerator := generator.DeepCopy()
	defer func() {
		if originalGenerator.DeletionTimestamp != nil && len(generator.Finalizers) == 0 {
			return
		}
		if !originalGenerator.Status.Equal(generator.Status) {
			updateErr := r.Client.Status().Update(ctx, generator)
			if updateErr != nil {
				logger.Error(errKubernetesAPI, "error updating NBSetupKeyGenerator Status", "err", updateErr)
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
	if generator.DeletionTimestamp != nil {
		if len(generator.Finalizers) == 0 {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, r.handleDelete(ctx, generator, logger)
	}

	// Ensure finalizer
	if !util.Contains(generator.Finalizers, setupKeyGeneratorFinalizer) {
		generator.Finalizers = append(generator.Finalizers, setupKeyGeneratorFinalizer)
		if err := r.Update(ctx, generator); err != nil {
			logger.Error(errKubernetesAPI, "error adding finalizer", "err", err)
			return ctrl.Result{}, err
		}
	}

	// Check if existing setup key needs recreation
	needsCreation := r.checkExistingSetupKey(ctx, generator, logger)

	// Create new setup key if needed
	if needsCreation {
		if err := r.createSetupKeyAndSecret(ctx, generator, logger); err != nil {
			return ctrl.Result{}, err
		}
	}

	generator.Status.Conditions = netbirdiov1.NBConditionTrue()
	return ctrl.Result{}, nil
}

// checkExistingSetupKey verifies existing setup key and secret, returns true if creation needed
func (r *NBSetupKeyGeneratorReconciler) checkExistingSetupKey(ctx context.Context, generator *netbirdiov1.NBSetupKeyGenerator, logger logr.Logger) bool {
	if generator.Status.SetupKeyID == "" {
		return true
	}

	// Verify setup key still exists and is valid
	setupKey, err := r.netbird.SetupKeys.Get(ctx, generator.Status.SetupKeyID)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			logger.Info("Setup key not found, will recreate")
			generator.Status.SetupKeyID = ""
			generator.Status.SecretName = ""
			return true
		}
		logger.Error(errNetBirdAPI, "error getting setup key", "err", err)
		generator.Status.Conditions = netbirdiov1.NBConditionFalse("APIError", err.Error())
		return false
	}

	if setupKey.Revoked {
		logger.Info("Setup key revoked, will recreate")
		r.deleteRevokedKey(ctx, generator.Status.SetupKeyID, logger)
		generator.Status.SetupKeyID = ""
		generator.Status.SecretName = ""
		return true
	}

	// Verify secret exists
	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{
		Name:      generator.Status.SecretName,
		Namespace: generator.Namespace,
	}
	if err := r.Get(ctx, secretKey, secret); errors.IsNotFound(err) {
		logger.Info("Secret not found, will recreate")
		generator.Status.SetupKeyID = ""
		generator.Status.SecretName = ""
		return true
	}

	return false
}

// deleteRevokedKey attempts to delete a revoked setup key
func (r *NBSetupKeyGeneratorReconciler) deleteRevokedKey(ctx context.Context, keyID string, logger logr.Logger) {
	if err := r.netbird.SetupKeys.Delete(ctx, keyID); err != nil {
		if !strings.Contains(err.Error(), "not found") {
			logger.Error(errNetBirdAPI, "error deleting revoked setup key", "err", err)
		}
	}
}

// createSetupKeyAndSecret creates a new setup key in NetBird and stores it in a secret
func (r *NBSetupKeyGeneratorReconciler) createSetupKeyAndSecret(ctx context.Context, generator *netbirdiov1.NBSetupKeyGenerator, logger logr.Logger) error {
	logger.Info("Creating setup key")

	secretName := generator.Spec.SecretName
	if secretName == "" {
		secretName = generator.Name
	}

	setupKey, err := r.createNetBirdSetupKey(ctx, generator)
	if err != nil {
		logger.Error(errNetBirdAPI, "error creating setup key", "err", err)
		generator.Status.Conditions = netbirdiov1.NBConditionFalse("APIError", err.Error())
		return fmt.Errorf("failed to create setup key: %w", err)
	}

	if err := r.createOrUpdateSecret(ctx, generator, secretName, setupKey.Key); err != nil {
		return err
	}

	generator.Status.SetupKeyID = setupKey.Id
	generator.Status.SecretName = secretName
	logger.Info("Created setup key and secret", "setupKeyID", setupKey.Id, "secret", secretName)
	return nil
}

// createNetBirdSetupKey creates a setup key in NetBird
func (r *NBSetupKeyGeneratorReconciler) createNetBirdSetupKey(ctx context.Context, generator *netbirdiov1.NBSetupKeyGenerator) (*api.SetupKeyClear, error) {
	setupKeyType := generator.Spec.SetupKeyType
	if setupKeyType == "" {
		setupKeyType = "one-off"
	}

	ephemeral := true
	if generator.Spec.Ephemeral != nil {
		ephemeral = *generator.Spec.Ephemeral
	}

	createReq := api.CreateSetupKeyRequest{
		AutoGroups: generator.Spec.AutoGroups,
		Ephemeral:  util.Ptr(ephemeral),
		Name:       generator.Spec.Name,
		Type:       setupKeyType,
	}

	if generator.Spec.ExpiresIn != nil {
		createReq.ExpiresIn = *generator.Spec.ExpiresIn
	}

	if generator.Spec.UsageLimit != nil {
		createReq.UsageLimit = *generator.Spec.UsageLimit
	}

	return r.netbird.SetupKeys.Create(ctx, createReq)
}

// createOrUpdateSecret creates or updates the secret containing the setup key
func (r *NBSetupKeyGeneratorReconciler) createOrUpdateSecret(ctx context.Context, generator *netbirdiov1.NBSetupKeyGenerator, secretName, setupKey string) error {
	secretLabels := make(map[string]string)
	for k, v := range r.DefaultLabels {
		secretLabels[k] = v
	}
	secretLabels["netbird.io/setup-key-generator"] = generator.Name

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: generator.Namespace,
			Labels:    secretLabels,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: generator.APIVersion,
					Kind:       generator.Kind,
					Name:       generator.Name,
					UID:        generator.UID,
				},
			},
		},
		StringData: map[string]string{
			"setupKey":      setupKey,
			"managementURL": r.ManagementURL,
		},
	}

	if err := r.Create(ctx, secret); err != nil {
		if errors.IsAlreadyExists(err) {
			return r.updateExistingSecret(ctx, generator.Namespace, secretName, setupKey)
		}
		return fmt.Errorf("failed to create secret: %w", err)
	}
	return nil
}

// updateExistingSecret updates an existing secret with new setup key
func (r *NBSetupKeyGeneratorReconciler) updateExistingSecret(ctx context.Context, namespace, secretName, setupKey string) error {
	existingSecret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: namespace}, existingSecret); err != nil {
		return fmt.Errorf("failed to get existing secret: %w", err)
	}
	existingSecret.Data = map[string][]byte{
		"setupKey":      []byte(setupKey),
		"managementURL": []byte(r.ManagementURL),
	}
	if err := r.Update(ctx, existingSecret); err != nil {
		return fmt.Errorf("failed to update existing secret: %w", err)
	}
	return nil
}

// handleDelete handles the deletion of NBSetupKeyGenerator
func (r *NBSetupKeyGeneratorReconciler) handleDelete(ctx context.Context, generator *netbirdiov1.NBSetupKeyGenerator, logger logr.Logger) error {
	logger.Info("Handling NBSetupKeyGenerator deletion")

	// Revoke setup key if configured
	revokeOnDelete := true
	if generator.Spec.RevokeOnDelete != nil {
		revokeOnDelete = *generator.Spec.RevokeOnDelete
	}

	if revokeOnDelete && generator.Status.SetupKeyID != "" {
		if err := r.netbird.SetupKeys.Delete(ctx, generator.Status.SetupKeyID); err != nil {
			if !strings.Contains(err.Error(), "not found") {
				return fmt.Errorf("failed to delete setup key: %w", err)
			}
		}
		logger.Info("Revoked setup key", "setupKeyID", generator.Status.SetupKeyID)
	}

	// Secret will be garbage collected via OwnerReference

	// Remove finalizer
	generator.Finalizers = util.Without(generator.Finalizers, setupKeyGeneratorFinalizer)
	if err := r.Update(ctx, generator); err != nil {
		logger.Error(errKubernetesAPI, "error removing finalizer", "err", err)
		return err
	}

	logger.Info("NBSetupKeyGenerator deletion complete")
	return nil
}

// mapSecretToNBSetupKeyGenerator maps Secret events to NBSetupKeyGenerator reconcile requests
func (r *NBSetupKeyGeneratorReconciler) mapSecretToNBSetupKeyGenerator(ctx context.Context, obj client.Object) []reconcile.Request {
	secret := obj.(*corev1.Secret)

	// Check if this secret is managed by us
	generatorName, ok := secret.Labels["netbird.io/setup-key-generator"]
	if !ok {
		return nil
	}

	return []reconcile.Request{
		{NamespacedName: types.NamespacedName{Name: generatorName, Namespace: secret.Namespace}},
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *NBSetupKeyGeneratorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.netbird = netbird.New(r.ManagementURL, r.APIKey)

	return ctrl.NewControllerManagedBy(mgr).
		For(&netbirdiov1.NBSetupKeyGenerator{}).
		Named("nbsetupkeygenerator").
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.mapSecretToNBSetupKeyGenerator)).
		Complete(r)
}
