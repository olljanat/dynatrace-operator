package kubemon

import (
	"context"
	"fmt"
	"os"

	dynatracev1alpha1 "github.com/Dynatrace/dynatrace-operator/api/v1alpha1"
	"github.com/Dynatrace/dynatrace-operator/controllers/customproperties"
	"github.com/Dynatrace/dynatrace-operator/controllers/dtpullsecret"
	"github.com/Dynatrace/dynatrace-operator/controllers/dtversion"
	"github.com/Dynatrace/dynatrace-operator/controllers/kubesystem"
	"github.com/Dynatrace/dynatrace-operator/dtclient"
	"github.com/go-logr/logr"
	v1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	Name                   = "kubernetes-monitoring"
	annotationTemplateHash = "internal.operator.dynatrace.com/template-hash"
	annotationImageHash    = "internal.operator.dynatrace.com/image-hash"
	annotationImageVersion = "internal.operator.dynatrace.com/image-version"
	envVarDisableUpdates   = "OPERATOR_DEBUG_DISABLE_UPDATES"
)

type Reconciler struct {
	client.Client
	scheme    *runtime.Scheme
	dtc       dtclient.Client
	log       logr.Logger
	token     *corev1.Secret
	instance  *dynatracev1alpha1.DynaKube
	apiReader client.Reader

	imageVersionProvider dtversion.ImageVersionProvider
}

func NewReconciler(clt client.Client, apiReader client.Reader, scheme *runtime.Scheme, dtc dtclient.Client, log logr.Logger, token *corev1.Secret,
	instance *dynatracev1alpha1.DynaKube, imgVerProvider dtversion.ImageVersionProvider) *Reconciler {
	return &Reconciler{
		Client:    clt,
		apiReader: apiReader,
		scheme:    scheme,
		dtc:       dtc,
		log:       log,
		token:     token,
		instance:  instance,

		imageVersionProvider: imgVerProvider,
	}
}

func (r *Reconciler) Reconcile(_ reconcile.Request) (reconcile.Result, error) {
	err := dtpullsecret.
		NewReconciler(r, r.apiReader, r.scheme, r.instance, r.dtc, r.log, r.token, r.instance.Spec.ActiveGate.Image).
		Reconcile()
	if err != nil {
		r.log.Error(err, "could not reconcile Dynatrace pull secret")
		return reconcile.Result{}, err
	}

	if r.instance.Spec.KubernetesMonitoringSpec.CustomProperties != nil {
		err = customproperties.
			NewReconciler(r, r.instance, r.log, Name, *r.instance.Spec.KubernetesMonitoringSpec.CustomProperties, r.scheme).
			Reconcile()
		if err != nil {
			r.log.Error(err, "could not reconcile custom properties")
			return reconcile.Result{}, err
		}
	}

	if err = r.manageStatefulSet(r.instance); err != nil {
		r.log.Error(err, "could not reconcile stateful set")
		return reconcile.Result{}, err
	}

	if r.instance.Spec.KubernetesMonitoringSpec.KubernetesAPIEndpoint != "" {
		id, err := r.addToDashboard()
		r.handleAddToDashboardResult(id, err, r.log)
	}

	return reconcile.Result{}, nil
}

func (r *Reconciler) manageStatefulSet(instance *dynatracev1alpha1.DynaKube) error {
	if os.Getenv(envVarDisableUpdates) != "true" {
		img := buildActiveGateImage(instance)
		if err := r.updateImageVersion(instance, img); err != nil {
			r.log.Error(err, "Failed to fetch image version", "image", img)
		}
	}

	desiredStatefulSet, err := r.buildDesiredStatefulSet(instance)
	if err != nil {
		return err
	}

	if err := controllerutil.SetControllerReference(instance, desiredStatefulSet, r.scheme); err != nil {
		return err
	}

	currentStatefulSet, err := r.createStatefulSetIfNotExists(desiredStatefulSet)
	if err != nil {
		return err
	}

	if err = r.updateStatefulSetIfOutdated(currentStatefulSet, desiredStatefulSet); err != nil {
		return err
	}

	return r.updateInstanceStatus(instance)
}

func (r *Reconciler) updateImageVersion(instance *dynatracev1alpha1.DynaKube, img string) error {
	pullSecret, err := dtpullsecret.GetImagePullSecret(r, r.instance)
	if err != nil {
		return fmt.Errorf("failed to get image pull secret: %w", err)
	}

	dockerCfg, err := dtversion.NewDockerConfig(pullSecret)
	if err != nil {
		return fmt.Errorf("failed to get Dockerconfig for pull secret: %w", err)
	}

	verProvider := dtversion.GetImageVersion
	if r.imageVersionProvider != nil {
		verProvider = r.imageVersionProvider
	}

	ver, err := verProvider(img, dockerCfg)
	if err != nil {
		return fmt.Errorf("failed to get image version: %w", err)
	}

	if instance.Status.ActiveGateImageHash != ver.Hash {
		r.log.Info("Update found",
			"oldVersion", instance.Status.ActiveGateImageVersion,
			"newVersion", ver.Version,
			"oldHash", instance.Status.ActiveGateImageHash,
			"newHash", ver.Hash)
	}

	instance.Status.ActiveGateImageVersion = ver.Version
	instance.Status.ActiveGateImageHash = ver.Hash
	return nil
}

func (r *Reconciler) buildDesiredStatefulSet(instance *dynatracev1alpha1.DynaKube) (*v1.StatefulSet, error) {
	kubeUID, err := kubesystem.GetUID(r.apiReader)
	if err != nil {
		return nil, err
	}

	return newStatefulSet(instance, kubeUID)
}

func (r *Reconciler) createStatefulSetIfNotExists(desired *v1.StatefulSet) (*v1.StatefulSet, error) {
	currentStatefulSet, err := r.getCurrentStatefulSet(desired)
	if err != nil && k8serrors.IsNotFound(err) {
		r.log.Info("creating new stateful set for kubernetes monitoring")
		return desired, r.createStatefulSet(desired)
	}
	return currentStatefulSet, err
}

func (r *Reconciler) updateStatefulSetIfOutdated(current *v1.StatefulSet, desired *v1.StatefulSet) error {
	if hasStatefulSetChanged(current, desired) {
		r.log.Info("updating existing stateful set")
		err := r.Update(context.TODO(), desired)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *Reconciler) updateInstanceStatus(instance *dynatracev1alpha1.DynaKube) error {
	instance.Status.UpdatedTimestamp = metav1.Now()
	return r.Status().Update(context.TODO(), instance)
}

func (r *Reconciler) getCurrentStatefulSet(desired *v1.StatefulSet) (*v1.StatefulSet, error) {
	var currentStatefulSet v1.StatefulSet
	err := r.Get(context.TODO(), client.ObjectKey{Name: desired.Name, Namespace: desired.Namespace}, &currentStatefulSet)
	if err != nil {
		return nil, err
	}
	return &currentStatefulSet, nil
}

func (r *Reconciler) createStatefulSet(desired *v1.StatefulSet) error {
	return r.Create(context.TODO(), desired)
}

func hasStatefulSetChanged(a *v1.StatefulSet, b *v1.StatefulSet) bool {
	return getTemplateHash(a) != getTemplateHash(b)
}

func getTemplateHash(a metav1.Object) string {
	if annotations := a.GetAnnotations(); annotations != nil {
		return annotations[annotationTemplateHash]
	}
	return ""
}
