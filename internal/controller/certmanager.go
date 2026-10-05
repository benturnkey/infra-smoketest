package controller

import (
	"context"

	api "github.com/tkhq/infra-smoketest/api/v1alpha1"
	"github.com/tkhq/infra-smoketest/internal/definition"
	"github.com/tkhq/infra-smoketest/internal/probe"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Called only after all Run Pods are gone, so a probe cannot race cleanup by
// creating another fixture. Names are deterministic and ownership includes the
// recorded Pod UID; a matching name alone never authorizes deletion.
func (r *Reconciler) cleanupCertificates(ctx context.Context, run *api.SmokeTestRun) (bool, error) {
	if run.Status.Definition == nil {
		return false, nil
	}
	for _, stage := range run.Status.Definition.Stages {
		if stage.CreatePod == nil || stage.CreatePod.Template.Spec.Containers[0].Args[1] != "cert-manager" {
			continue
		}
		rec := record(run, stage.Name)
		if rec == nil || rec.UID == "" {
			continue
		}
		// Remove the request before its signing key and issuer. There are no
		// Certificate-owned temporary Secrets or renewal jobs in this test.
		existingIssuer := false
		for _, env := range stage.CreatePod.Template.Spec.Containers[0].Env {
			if env.Name == definition.CertManagerIssuerNameEnv && env.Value != "" {
				existingIssuer = true
			}
		}
		objects := []client.Object{probe.CertManagerObject("CertificateRequest", run.Namespace, rec.Name)}
		if !existingIssuer {
			objects = append(objects, probe.CertManagerObject("Issuer", run.Namespace, rec.Name))
		}
		objects = append(objects, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: rec.Name, Namespace: run.Namespace}})
		for _, obj := range objects {
			if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
				if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
					continue
				}
				return false, err
			}
			for _, owner := range obj.GetOwnerReferences() {
				if owner.APIVersion == "v1" && owner.Kind == "Pod" && owner.Name == rec.Name && string(owner.UID) == rec.UID {
					if obj.GetDeletionTimestamp().IsZero() {
						if err := r.deleteResource(ctx, obj, "cert-manager fixture"); err != nil {
							return false, err
						}
					}
					return true, nil
				}
			}
		}
	}
	return false, nil
}
