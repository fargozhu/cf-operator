package instancegroupmanifest

import (
	"context"

	"github.com/pkg/errors"

	"sigs.k8s.io/controller-runtime/pkg/client"

	bdm "code.cloudfoundry.org/cf-operator/pkg/bosh/manifest"
	"code.cloudfoundry.org/quarks-utils/pkg/names"
	"code.cloudfoundry.org/quarks-utils/pkg/versionedsecretstore"
)

// the key of instance group properties inside the secret
var igPropertiesKey = "properties.yaml"

// InstanceGroupManifest resolves references from desired manifest to a single instance group manifest
type InstanceGroupManifest struct {
	client               client.Client
	versionedSecretStore versionedsecretstore.VersionedSecretStore
}

// NewInstanceGroupManifest constructs a resolver
func NewInstanceGroupManifest(client client.Client) *InstanceGroupManifest {
	return &InstanceGroupManifest{
		client:               client,
		versionedSecretStore: versionedsecretstore.NewVersionedSecretStore(client),
	}
}

// InstanceGroupManifest reads the versioned secret created by the variable interpolation job
// and unmarshals it into a Manifest object
func (r *InstanceGroupManifest) InstanceGroupManifest(ctx context.Context, boshDeploymentName, instanceGroupName, namespace string) (*bdm.Manifest, error) {
	// unversioned instance group manifest name
	secretName := names.InstanceGroupSecretName(
		names.DeploymentSecretTypeInstanceGroupResolvedProperties,
		boshDeploymentName, instanceGroupName, "")

	secret, err := r.versionedSecretStore.Latest(ctx, namespace, secretName)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to read latest versioned secret %s for instance group manifest %s.%s", secretName, boshDeploymentName, instanceGroupName)
	}

	manifestData := secret.Data[igPropertiesKey]

	manifest, err := bdm.LoadYAML(manifestData)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to unmarshal manifest from secret %s for instance group manifest %s.%s", secretName, boshDeploymentName, instanceGroupName)
	}

	return manifest, nil
}
