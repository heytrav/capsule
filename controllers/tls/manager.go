// Copyright 2020-2021 Clastix Labs
// SPDX-License-Identifier: Apache-2.0

package tls

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/sync/errgroup"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/clastix/capsule/controllers/utils"
	"github.com/clastix/capsule/pkg/cert"
	"github.com/clastix/capsule/pkg/configuration"
)

const (
	certificateExpirationThreshold     = 3 * 24 * time.Hour
	certificateReconciliationThreshold = 4 * 24 * time.Hour
	certificateValidity                = 6 * 30 * 24 * time.Hour
	PodUpdateAnnotationName            = "capsule.clastix.io/updated"
)

type Reconciler struct {
	client.Client
	Log           logr.Logger
	Scheme        *runtime.Scheme
	Namespace     string
	Configuration configuration.Configuration
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	enqueueFn := handler.EnqueueRequestsFromMapFunc(func(client.Object) []reconcile.Request {
		return []reconcile.Request{
			{
				NamespacedName: types.NamespacedName{
					Namespace: r.Namespace,
					Name:      r.Configuration.TLSSecretName(),
				},
			},
		}
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Secret{}, utils.NamesMatchingPredicate(r.Configuration.TLSSecretName())).
		Watches(source.NewKindWithCache(&admissionregistrationv1.ValidatingWebhookConfiguration{}, mgr.GetCache()), enqueueFn, builder.WithPredicates(predicate.NewPredicateFuncs(func(object client.Object) bool {
			return object.GetName() == r.Configuration.ValidatingWebhookConfigurationName()
		}))).
		Watches(source.NewKindWithCache(&admissionregistrationv1.MutatingWebhookConfiguration{}, mgr.GetCache()), enqueueFn, builder.WithPredicates(predicate.NewPredicateFuncs(func(object client.Object) bool {
			return object.GetName() == r.Configuration.MutatingWebhookConfigurationName()
		}))).
		Watches(source.NewKindWithCache(&apiextensionsv1.CustomResourceDefinition{}, mgr.GetCache()), enqueueFn, builder.WithPredicates(predicate.NewPredicateFuncs(func(object client.Object) bool {
			return object.GetName() == r.Configuration.TenantCRDName()
		}))).
		Complete(r)
}

func (r Reconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	r.Log = r.Log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)

	// Fetch the CA instance
	certSecret := &corev1.Secret{}

	if err := r.Client.Get(ctx, request.NamespacedName, certSecret); err != nil {
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	if r.shouldUpdateCertificate(certSecret) {
		r.Log.Info("Generating new TLS certificate")

		ca, err := cert.GenerateCertificateAuthority()
		if err != nil {
			return reconcile.Result{}, err
		}

		opts := cert.NewCertOpts(time.Now().Add(certificateValidity), fmt.Sprintf("capsule-webhook-service.%s.svc", r.Namespace))

		crt, key, err := ca.GenerateCertificate(opts)
		if err != nil {
			r.Log.Error(err, "Cannot generate new TLS certificate")

			return reconcile.Result{}, err
		}

		caCrt, _ := ca.CACertificatePem()

		certSecret.Data = map[string][]byte{
			corev1.TLSCertKey:              crt.Bytes(),
			corev1.TLSPrivateKeyKey:        key.Bytes(),
			corev1.ServiceAccountRootCAKey: caCrt.Bytes(),
		}

		t := &corev1.Secret{ObjectMeta: certSecret.ObjectMeta}

		_, err = controllerutil.CreateOrUpdate(ctx, r.Client, t, func() error {
			t.Data = certSecret.Data

			return nil
		})
		if err != nil {
			r.Log.Error(err, "cannot update Capsule TLS")

			return reconcile.Result{}, err
		}
	}

	var caBundle []byte

	var ok bool

	if caBundle, ok = certSecret.Data[corev1.ServiceAccountRootCAKey]; !ok {
		return reconcile.Result{}, fmt.Errorf("missing %s field in %s secret", corev1.ServiceAccountRootCAKey, r.Configuration.TLSSecretName())
	}

	operatorPods, err := r.getOperatorPods(ctx)
	if err != nil {
		return reconcile.Result{}, err
	}

	r.Log.Info("Updating caBundle in webhooks and crd")

	group := new(errgroup.Group)
	group.Go(func() error {
		return r.updateMutatingWebhookConfiguration(ctx, caBundle)
	})
	group.Go(func() error {
		return r.updateValidatingWebhookConfiguration(ctx, caBundle)
	})
	group.Go(func() error {
		return r.updateCustomResourceDefinition(ctx, caBundle)
	})

	r.Log.Info("Updating capsule operator pods")

	for _, pod := range operatorPods.Items {
		p := pod

		group.Go(func() error {
			return r.updateOperatorPod(ctx, p)
		})
	}

	if err := group.Wait(); err != nil {
		return reconcile.Result{}, err
	}

	if r.Configuration.GenerateCertificates() {
		certificate, err := cert.GetCertificateFromBytes(certSecret.Data[corev1.TLSCertKey])
		if err != nil {
			return reconcile.Result{}, err
		}

		now := time.Now()

		rq := (time.Duration(certificate.NotAfter.Unix()-now.Unix()) * time.Second) - certificateReconciliationThreshold

		r.Log.Info("Reconciliation completed, processing back in " + rq.String())

		return reconcile.Result{Requeue: true, RequeueAfter: rq}, nil
	}

	return reconcile.Result{}, nil
}

func (r Reconciler) shouldUpdateCertificate(secret *corev1.Secret) bool {
	if !r.Configuration.GenerateCertificates() {
		r.Log.Info("Skipping TLS certificate generation as it is disabled in CapsuleConfiguration")

		return false
	}

	if _, ok := secret.Data[corev1.ServiceAccountRootCAKey]; !ok {
		return true
	}

	certificate, key, err := cert.GetCertificateWithPrivateKeyFromBytes(secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey])
	if err != nil {
		return true
	}

	if err := cert.ValidateCertificate(certificate, key, certificateExpirationThreshold); err != nil {
		r.Log.Error(err, "failed to validate certificate, generating new one")

		return true
	}

	r.Log.Info("Skipping TLS certificate generation as it is still valid")

	return false
}

// By default helm doesn't allow to use templates in CRD (https://helm.sh/docs/chart_best_practices/custom_resource_definitions/#method-1-let-helm-do-it-for-you).
// In order to overcome this, we are setting conversion strategy in helm chart to None, and then update it with CA and namespace information.
func (r *Reconciler) updateCustomResourceDefinition(ctx context.Context, caBundle []byte) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() (err error) {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		err = r.Get(ctx, types.NamespacedName{Name: "tenants.capsule.clastix.io"}, crd)
		if err != nil {
			r.Log.Error(err, "cannot retrieve CustomResourceDefinition")

			return err
		}

		_, err = controllerutil.CreateOrUpdate(ctx, r.Client, crd, func() error {
			crd.Spec.Conversion = &apiextensionsv1.CustomResourceConversion{
				Strategy: "Webhook",
				Webhook: &apiextensionsv1.WebhookConversion{
					ClientConfig: &apiextensionsv1.WebhookClientConfig{
						Service: &apiextensionsv1.ServiceReference{
							Namespace: r.Namespace,
							Name:      "capsule-webhook-service",
							Path:      pointer.StringPtr("/convert"),
							Port:      pointer.Int32Ptr(443),
						},
						CABundle: caBundle,
					},
					ConversionReviewVersions: []string{"v1alpha1", "v1beta1"},
				},
			}

			return nil
		})

		return err
	})
}

//nolint:dupl
func (r Reconciler) updateValidatingWebhookConfiguration(ctx context.Context, caBundle []byte) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() (err error) {
		vw := &admissionregistrationv1.ValidatingWebhookConfiguration{}
		err = r.Get(ctx, types.NamespacedName{Name: r.Configuration.ValidatingWebhookConfigurationName()}, vw)
		if err != nil {
			r.Log.Error(err, "cannot retrieve ValidatingWebhookConfiguration")

			return err
		}
		for i, w := range vw.Webhooks {
			// Updating CABundle only in case of an internal service reference
			if w.ClientConfig.Service != nil {
				vw.Webhooks[i].ClientConfig.CABundle = caBundle
			}
		}

		return r.Update(ctx, vw, &client.UpdateOptions{})
	})
}

//nolint:dupl
func (r Reconciler) updateMutatingWebhookConfiguration(ctx context.Context, caBundle []byte) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() (err error) {
		mw := &admissionregistrationv1.MutatingWebhookConfiguration{}
		err = r.Get(ctx, types.NamespacedName{Name: r.Configuration.MutatingWebhookConfigurationName()}, mw)
		if err != nil {
			r.Log.Error(err, "cannot retrieve MutatingWebhookConfiguration")

			return err
		}
		for i, w := range mw.Webhooks {
			// Updating CABundle only in case of an internal service reference
			if w.ClientConfig.Service != nil {
				mw.Webhooks[i].ClientConfig.CABundle = caBundle
			}
		}

		return r.Update(ctx, mw, &client.UpdateOptions{})
	})
}

func (r Reconciler) updateOperatorPod(ctx context.Context, pod corev1.Pod) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Need to get latest version of pod
		p := &corev1.Pod{}

		if err := r.Client.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}, p); err != nil && !apierrors.IsNotFound(err) {
			r.Log.Error(err, "cannot get pod", "name", pod.Name, "namespace", pod.Namespace)

			return err
		}

		if p.Annotations == nil {
			p.Annotations = map[string]string{}
		}

		p.Annotations[PodUpdateAnnotationName] = time.Now().Format(time.RFC3339Nano)

		if err := r.Client.Update(ctx, p, &client.UpdateOptions{}); err != nil {
			r.Log.Error(err, "cannot update pod", "name", pod.Name, "namespace", pod.Namespace)

			return err
		}

		return nil
	})
}

func (r Reconciler) getOperatorPods(ctx context.Context) (*corev1.PodList, error) {
	hostname, _ := os.Hostname()

	leaderPod := &corev1.Pod{}

	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: os.Getenv("NAMESPACE"), Name: hostname}, leaderPod); err != nil {
		r.Log.Error(err, "cannot retrieve the leader Pod, probably running in out of the cluster mode")

		return nil, err
	}

	podList := &corev1.PodList{}
	if err := r.Client.List(ctx, podList, client.MatchingLabels(leaderPod.ObjectMeta.Labels)); err != nil {
		r.Log.Error(err, "cannot retrieve list of Capsule pods")

		return nil, err
	}

	return podList, nil
}
