package boshdeployment

import (
	"context"
	"strings"

	"code.cloudfoundry.org/cf-operator/pkg/kube/util/names"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

	bdm "code.cloudfoundry.org/cf-operator/pkg/bosh/manifest"
	"code.cloudfoundry.org/cf-operator/pkg/kube/util/config"
	"code.cloudfoundry.org/cf-operator/pkg/kube/util/ctxlog"
	"code.cloudfoundry.org/cf-operator/pkg/kube/util/versionedsecretstore"
)

// AddBPM creates a new BPM Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func AddBPM(ctx context.Context, config *config.Config, mgr manager.Manager) error {
	ctx = ctxlog.NewContextWithRecorder(ctx, "bpm-reconciler", mgr.GetRecorder("bpm-recorder"))
	r := NewBPMReconciler(
		ctx, config, mgr,
		bdm.NewResolver(mgr.GetClient(), func() bdm.Interpolator { return bdm.NewInterpolator() }),
		controllerutil.SetControllerReference,
		bdm.NewKubeConverter(config.Namespace),
	)

	// Create a new controller
	c, err := controller.New("bpm-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// We have to watch the versioned secret for each Instance Group
	p := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			o := e.Object.(*corev1.Secret)
			return isVersionedSecret(o) && isBPMInfoSecret(o.Name)
		},
		DeleteFunc:  func(e event.DeleteEvent) bool { return false },
		GenericFunc: func(e event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			o := e.ObjectNew.(*corev1.Secret)
			return isVersionedSecret(o) && isBPMInfoSecret(o.Name)
		},
	}

	// We have to watch the BPM secret. It gives us information about how to
	// start containers for each process.
	// The BPM secret is annotated with the name of the BOSHDeployment.

	err = c.Watch(&source.Kind{Type: &corev1.Secret{}}, &handler.EnqueueRequestForObject{}, p)
	if err != nil {
		return err
	}

	return nil
}

func isVersionedSecret(secret *corev1.Secret) bool {
	// TODO: Use annotation/label for this
	secretLabels := secret.GetLabels()
	if secretLabels == nil {
		return false
	}

	secretKind, ok := secretLabels[versionedsecretstore.LabelSecretKind]
	if !ok {
		return false
	}
	if secretKind != versionedsecretstore.VersionSecretKind {
		return false
	}

	return true
}

func isBPMInfoSecret(name string) bool {
	// TODO: Use annotation/label for this
	if strings.Contains(name, names.DeploymentSecretBpmInformation.String()) {
		return true
	}

	return false
}
