package boshdeployment

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pkg/errors"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crc "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"code.cloudfoundry.org/cf-operator/pkg/bosh/converter"
	bdm "code.cloudfoundry.org/cf-operator/pkg/bosh/manifest"
	bdv1 "code.cloudfoundry.org/cf-operator/pkg/kube/apis/boshdeployment/v1alpha1"
	qsv1a1 "code.cloudfoundry.org/cf-operator/pkg/kube/apis/quarkssecret/v1alpha1"
	"code.cloudfoundry.org/cf-operator/pkg/kube/util/boshdns"
	"code.cloudfoundry.org/cf-operator/pkg/kube/util/mutate"
	qjv1a1 "code.cloudfoundry.org/quarks-job/pkg/kube/apis/quarksjob/v1alpha1"
	"code.cloudfoundry.org/quarks-utils/pkg/config"
	log "code.cloudfoundry.org/quarks-utils/pkg/ctxlog"
	"code.cloudfoundry.org/quarks-utils/pkg/meltdown"
	"code.cloudfoundry.org/quarks-utils/pkg/names"
)

// JobFactory creates Jobs for a given manifest
type JobFactory interface {
	VariableInterpolationJob(deploymentName string, manifest bdm.Manifest) (*qjv1a1.QuarksJob, error)
	InstanceGroupManifestJob(deploymentName string, manifest bdm.Manifest, linkInfos converter.LinkInfos, initialRollout bool) (*qjv1a1.QuarksJob, error)
}

// VariablesConverter converts BOSH variables into QuarksSecrets
type VariablesConverter interface {
	Variables(manifestName string, variables []bdm.Variable) ([]qsv1a1.QuarksSecret, error)
}

// WithOps interpolates BOSH manifests and operations files to create the WithOps manifest
type WithOps interface {
	Manifest(instance *bdv1.BOSHDeployment, namespace string) (*bdm.Manifest, []string, error)
}

// Check that ReconcileBOSHDeployment implements the reconcile.Reconciler interface
var _ reconcile.Reconciler = &ReconcileBOSHDeployment{}

type setReferenceFunc func(owner, object metav1.Object, scheme *runtime.Scheme) error

// NewDeploymentReconciler returns a new reconcile.Reconciler
func NewDeploymentReconciler(ctx context.Context, config *config.Config, mgr manager.Manager, withops WithOps, jobFactory JobFactory, converter VariablesConverter, srf setReferenceFunc) reconcile.Reconciler {

	return &ReconcileBOSHDeployment{
		ctx:          ctx,
		config:       config,
		client:       mgr.GetClient(),
		scheme:       mgr.GetScheme(),
		withops:      withops,
		setReference: srf,
		jobFactory:   jobFactory,
		converter:    converter,
	}
}

// ReconcileBOSHDeployment reconciles a BOSHDeployment object
type ReconcileBOSHDeployment struct {
	ctx          context.Context
	config       *config.Config
	client       client.Client
	scheme       *runtime.Scheme
	withops      WithOps
	setReference setReferenceFunc
	jobFactory   JobFactory
	converter    VariablesConverter
}

// Reconcile starts the deployment process for a BOSHDeployment and deploys QuarksJobs to generate required properties for instance groups and rendered BPM
func (r *ReconcileBOSHDeployment) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	// Fetch the BOSHDeployment instance
	instance := &bdv1.BOSHDeployment{}

	// Set the ctx to be Background, as the top-level context for incoming requests.
	ctx, cancel := context.WithTimeout(r.ctx, r.config.CtxTimeOut)
	defer cancel()

	log.Infof(ctx, "Reconciling BOSHDeployment %s", request.NamespacedName)
	err := r.client.Get(ctx, request.NamespacedName, instance)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			log.Debug(ctx, "Skip reconcile: BOSHDeployment not found")
			return reconcile.Result{}, nil
		}

		return reconcile.Result{},
			log.WithEvent(instance, "GetBOSHDeploymentError").Errorf(ctx, "failed to get BOSHDeployment '%s': %v", request.NamespacedName, err)
	}

	if meltdown.NewWindow(r.config.MeltdownDuration, instance.Status.LastReconcile).Contains(time.Now()) {
		log.WithEvent(instance, "Meltdown").Debugf(ctx, "Resource '%s' is in meltdown, requeue reconcile after %s", instance.Name, r.config.MeltdownRequeueAfter)
		return reconcile.Result{RequeueAfter: r.config.MeltdownRequeueAfter}, nil
	}

	// Resolve the manifest with ops
	manifest, err := r.resolveManifest(ctx, instance)
	if err != nil {
		return reconcile.Result{},
			log.WithEvent(instance, "WithOpsManifestError").Errorf(ctx, "failed to get with-ops manifest for BOSHDeployment '%s': %v", request.NamespacedName, err)
	}

	// Get link infos containing provider name and its secret name
	linkInfos, err := r.listLinkInfos(instance, manifest)
	if err != nil {
		return reconcile.Result{},
			log.WithEvent(instance, "InstanceGroupManifestError").Errorf(ctx, "failed to list quarks-link secrets for BOSHDeployment '%s': %v", request.NamespacedName, err)
	}

	// Apply the "with-ops" manifest secret
	log.Debug(ctx, "Creating with-ops manifest secret")
	manifestSecret, err := r.createManifestWithOps(ctx, instance, *manifest)
	if err != nil {
		return reconcile.Result{},
			log.WithEvent(instance, "WithOpsManifestError").Errorf(ctx, "failed to create with-ops manifest secret for BOSHDeployment '%s': %v", request.NamespacedName, err)
	}

	// Create all QuarksSecret variables
	log.Debug(ctx, "Converting BOSH manifest variables to QuarksSecret resources")
	secrets, err := r.converter.Variables(instance.Name, manifest.Variables)
	if err != nil {
		return reconcile.Result{},
			log.WithEvent(instance, "BadManifestError").Error(ctx, errors.Wrap(err, "failed to generate quarks secrets from manifest"))

	}

	// Create/update all explicit BOSH Variables
	if len(secrets) > 0 {
		err = r.createQuarksSecrets(ctx, manifestSecret, secrets)
		if err != nil {
			return reconcile.Result{},
				log.WithEvent(instance, "VariableGenerationError").Errorf(ctx, "failed to create quarks secrets for BOSH manifest '%s': %v", instance.Name, err)
		}
	}

	// Apply the "Variable Interpolation" QuarksJob, which creates the desired manifest secret
	qJob, err := r.jobFactory.VariableInterpolationJob(instance.Name, *manifest)
	if err != nil {
		return reconcile.Result{}, log.WithEvent(instance, "DesiredManifestError").Errorf(ctx, "failed to build the desired manifest qJob: %v", err)
	}

	log.Debug(ctx, "Creating desired manifest QuarksJob")
	err = r.createQuarksJob(ctx, instance, qJob)
	if err != nil {
		return reconcile.Result{},
			log.WithEvent(instance, "DesiredManifestError").Errorf(ctx, "failed to create desired manifest qJob for BOSHDeployment '%s': %v", request.NamespacedName, err)
	}

	// Apply the "Instance group manifest" QuarksJob, which creates instance group manifests (ig-resolved) secrets and BPM config secrets
	// once the "Variable Interpolation" job created the desired manifest.
	qJob, err = r.jobFactory.InstanceGroupManifestJob(instance.Name, *manifest, linkInfos, instance.ObjectMeta.Generation == 1)
	if err != nil {
		return reconcile.Result{},
			log.WithEvent(instance, "InstanceGroupManifestError").Errorf(ctx, "failed to build instance group manifest qJob: %v", err)
	}

	log.Debug(ctx, "Creating instance group manifest QuarksJob")
	err = r.createQuarksJob(ctx, instance, qJob)
	if err != nil {
		return reconcile.Result{},
			log.WithEvent(instance, "InstanceGroupManifestError").Errorf(ctx, "failed to create instance group manifest qJob for BOSHDeployment '%s': %v", request.NamespacedName, err)
	}

	// Update status of bdpl with the timestamp of the last reconcile
	now := metav1.Now()
	instance.Status.LastReconcile = &now

	err = r.client.Status().Update(ctx, instance)
	if err != nil {
		log.WithEvent(instance, "UpdateError").Errorf(ctx, "failed to update reconcile timestamp on bdpl '%s' (%v): %s", instance.Name, instance.ResourceVersion, err)
		return reconcile.Result{Requeue: false}, nil
	}

	return reconcile.Result{}, nil
}

// resolveManifest resolves manifest with ops manifest
func (r *ReconcileBOSHDeployment) resolveManifest(ctx context.Context, instance *bdv1.BOSHDeployment) (*bdm.Manifest, error) {
	log.Debug(ctx, "Resolving manifest")
	manifest, _, err := r.withops.Manifest(instance, instance.GetNamespace())
	if err != nil {
		return nil, log.WithEvent(instance, "WithOpsManifestError").Errorf(ctx, "Error resolving the manifest %s: %s", instance.GetName(), err)
	}

	return manifest, nil
}

// createManifestWithOps creates a secret containing the deployment manifest with ops files applied
func (r *ReconcileBOSHDeployment) createManifestWithOps(ctx context.Context, instance *bdv1.BOSHDeployment, manifest bdm.Manifest) (*corev1.Secret, error) {
	log.Debug(ctx, "Creating manifest secret with ops")

	// Create manifest with ops, which will be used as a base for variable interpolation in desired manifest job input.
	manifestBytes, err := manifest.Marshal()
	if err != nil {
		return nil, log.WithEvent(instance, "ManifestWithOpsMarshalError").Errorf(ctx, "Error marshaling the manifest %s: %s", instance.GetName(), err)
	}

	manifestSecretName := names.DeploymentSecretName(names.DeploymentSecretTypeManifestWithOps, instance.Name, "")

	// Create a secret object for the manifest
	manifestSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      manifestSecretName,
			Namespace: instance.GetNamespace(),
			Labels: map[string]string{
				bdv1.LabelDeploymentName:       instance.Name,
				bdv1.LabelDeploymentSecretType: names.DeploymentSecretTypeManifestWithOps.String(),
			},
		},
		StringData: map[string]string{
			"manifest.yaml": string(manifestBytes),
		},
	}

	// Set ownership reference
	if err := r.setReference(instance, manifestSecret, r.scheme); err != nil {
		return nil, log.WithEvent(instance, "ManifestWithOpsRefError").Errorf(ctx, "failed to set ownerReference for Secret '%s': %v", manifestSecretName, err)
	}

	// Apply the secret
	op, err := controllerutil.CreateOrUpdate(ctx, r.client, manifestSecret, mutate.SecretMutateFn(manifestSecret))
	if err != nil {
		return nil, log.WithEvent(instance, "ManifestWithOpsApplyError").Errorf(ctx, "failed to apply Secret '%s': %v", manifestSecretName, err)
	}

	log.Debugf(ctx, "ResourceReference secret '%s' has been %s", manifestSecret.Name, op)

	return manifestSecret, nil
}

// createQuarksJob creates a QuarksJob and sets its ownership
func (r *ReconcileBOSHDeployment) createQuarksJob(ctx context.Context, instance *bdv1.BOSHDeployment, qJob *qjv1a1.QuarksJob) error {
	if err := r.setReference(instance, qJob, r.scheme); err != nil {
		return errors.Errorf("failed to set ownerReference for QuarksJob '%s': %v", qJob.GetName(), err)
	}

	op, err := controllerutil.CreateOrUpdate(ctx, r.client, qJob, mutate.QuarksJobMutateFn(qJob))
	if err != nil {
		return errors.Wrapf(err, "creating or updating QuarksJob '%s'", qJob.Name)
	}

	log.Debugf(ctx, "QuarksJob '%s' has been %s", qJob.Name, op)

	return err
}

// listLinkInfos returns a LinkInfos containing link providers if needed
// and updates `quarks_links` properties
func (r *ReconcileBOSHDeployment) listLinkInfos(instance *bdv1.BOSHDeployment, manifest *bdm.Manifest) (converter.LinkInfos, error) {
	linkInfos := converter.LinkInfos{}

	// find all missing providers in the manifest, so we can look for secrets
	missingProviders := manifest.ListMissingProviders()

	// quarksLinks store for missing provider names with types read from secrets
	quarksLinks := map[string]bdm.QuarksLink{}
	if len(missingProviders) != 0 {
		// list secrets and services from target deployment
		secrets := &corev1.SecretList{}
		err := r.client.List(r.ctx, secrets,
			crc.InNamespace(instance.Namespace),
		)
		if err != nil {
			return linkInfos, errors.Wrapf(err, "listing secrets for link in deployment '%s':", instance.Name)
		}

		services := &corev1.ServiceList{}
		err = r.client.List(r.ctx, services,
			crc.InNamespace(instance.Namespace),
		)
		if err != nil {
			return linkInfos, errors.Wrapf(err, "listing services for link in deployment '%s':", instance.Name)
		}

		for _, s := range secrets.Items {
			if deploymentName, ok := s.GetAnnotations()[bdv1.LabelDeploymentName]; ok && deploymentName == instance.Name {
				linkProvider, err := newLinkProvider(s.GetAnnotations())
				if err != nil {
					return linkInfos, errors.Wrapf(err, "failed to parse link JSON for  '%s'", instance.Name)
				}
				if dup, ok := missingProviders[linkProvider.Name]; ok {
					if dup {
						return linkInfos, errors.New(fmt.Sprintf("duplicated secrets of provider: %s", linkProvider.Name))
					}

					linkInfos = append(linkInfos, converter.LinkInfo{
						SecretName:   s.Name,
						ProviderName: linkProvider.Name,
						ProviderType: linkProvider.ProviderType,
					})

					if linkProvider.ProviderType != "" {
						quarksLinks[s.Name] = bdm.QuarksLink{
							Type: linkProvider.ProviderType,
						}
					}
					missingProviders[linkProvider.Name] = true
				}
			}
		}

		serviceRecords, err := r.getServiceRecords(instance.Namespace, instance.Name, services.Items)
		if err != nil {
			return linkInfos, errors.Wrapf(err, "failed to get link services for '%s'", instance.Name)
		}

		for qName := range quarksLinks {
			if svcRecord, ok := serviceRecords[qName]; ok {
				pods, err := r.listPodsFromSelector(instance.Namespace, svcRecord.selector)
				if err != nil {
					return linkInfos, errors.Wrapf(err, "Failed to get link pods for '%s'", instance.Name)
				}

				var jobsInstances []bdm.JobInstance
				for i, p := range pods {
					if len(p.Status.PodIP) == 0 {
						return linkInfos, fmt.Errorf("empty ip of kube native component: '%s/%s'", p.Namespace, p.Name)
					}
					jobsInstances = append(jobsInstances, bdm.JobInstance{
						Name:      qName,
						ID:        string(p.GetUID()),
						Index:     i,
						Address:   p.Status.PodIP,
						Bootstrap: i == 0,
					})
				}

				quarksLinks[qName] = bdm.QuarksLink{
					Type:      quarksLinks[qName].Type,
					Address:   svcRecord.dnsRecord,
					Instances: jobsInstances,
				}
			}

		}
	}

	missingPs := make([]string, 0, len(missingProviders))
	for key, found := range missingProviders {
		if !found {
			missingPs = append(missingPs, key)
		}
	}

	if len(missingPs) != 0 {
		return linkInfos, errors.New(fmt.Sprintf("missing link secrets for providers: %s", strings.Join(missingPs, ", ")))
	}

	if len(quarksLinks) != 0 {
		if manifest.Properties == nil {
			manifest.Properties = map[string]interface{}{}
		}
		manifest.Properties["quarks_links"] = quarksLinks
	}

	return linkInfos, nil
}

// getServiceRecords gets service records from Kube Services
func (r *ReconcileBOSHDeployment) getServiceRecords(namespace string, name string, svcs []corev1.Service) (map[string]serviceRecord, error) {
	svcRecords := map[string]serviceRecord{}
	for _, svc := range svcs {
		if deploymentName, ok := svc.GetAnnotations()[bdv1.LabelDeploymentName]; ok && deploymentName == name {
			providerName, ok := svc.GetAnnotations()[bdv1.AnnotationLinkProviderService]
			if ok {
				if _, ok := svcRecords[providerName]; ok {
					return svcRecords, errors.New(fmt.Sprintf("duplicated services of provider: %s", providerName))
				}

				svcRecords[providerName] = serviceRecord{
					selector:  svc.Spec.Selector,
					dnsRecord: fmt.Sprintf("%s.%s.svc.%s", svc.Name, namespace, boshdns.GetClusterDomain()),
				}
			}
		}
	}

	return svcRecords, nil
}

// listPodsFromSelector lists pods from the selector
func (r *ReconcileBOSHDeployment) listPodsFromSelector(namespace string, selector map[string]string) ([]corev1.Pod, error) {
	podList := &corev1.PodList{}
	err := r.client.List(r.ctx, podList,
		crc.InNamespace(namespace),
		crc.MatchingLabels(selector),
	)
	if err != nil {
		return podList.Items, errors.Wrapf(err, "listing pods from selector '%+v':", selector)
	}

	if len(podList.Items) == 0 {
		return podList.Items, fmt.Errorf("got an empty list of pods")
	}

	return podList.Items, nil
}

// createQuarksSecrets create variables quarksSecrets
func (r *ReconcileBOSHDeployment) createQuarksSecrets(ctx context.Context, manifestSecret *corev1.Secret, variables []qsv1a1.QuarksSecret) error {
	for _, variable := range variables {
		log.Debugf(ctx, "CreateOrUpdate QuarksSecrets for explicit variable '%s'", variable.Name)

		// Set the "manifest with ops" secret as the owner for the QuarksSecrets
		// The "manifest with ops" secret is owned by the actual BOSHDeployment, so everything
		// should be garbage collected properly.
		if err := r.setReference(manifestSecret, &variable, r.scheme); err != nil {
			err = log.WithEvent(manifestSecret, "OwnershipError").Errorf(ctx, "failed to set ownership for %s: %v", variable.Name, err)
			return err
		}

		op, err := controllerutil.CreateOrUpdate(ctx, r.client, &variable, mutate.QuarksSecretMutateFn(&variable))
		if err != nil {
			return errors.Wrapf(err, "creating or updating QuarksSecret '%s'", variable.Name)
		}

		// Update does not update status. We only trigger quarks secret
		// reconciler again if variable was updated by previous CreateOrUpdate
		if op == controllerutil.OperationResultUpdated {
			variable.Status.Generated = false
			if err := r.client.Status().Update(ctx, &variable); err != nil {
				log.WithEvent(&variable, "UpdateError").Errorf(ctx, "failed to update generated status on quarks secret '%s' (%v): %s", variable.Name, variable.ResourceVersion, err)
				return err
			}
		}

		log.Debugf(ctx, "QuarksSecret '%s' has been %s", variable.Name, op)
	}

	return nil
}

type serviceRecord struct {
	selector  map[string]string
	dnsRecord string
}
