package openshift

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	configv1 "github.com/openshift/api/config/v1"
	apifeatures "github.com/openshift/api/features"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	clusterVersionName = "version"
	featureGateName    = "cluster"
)

// VSphereMultiVCenterSupport captures the cluster version and feature-gate state
// relevant to selecting the migration path.
type VSphereMultiVCenterSupport struct {
	ClusterVersion       string
	FeatureGateEnabled   bool
	NativeVersionCapable bool
}

// SupportsNativePath returns true when the cluster version is 5.x or newer and
// the VSphereMultiVCenterDay2 feature gate is enabled for that payload version.
func (s *VSphereMultiVCenterSupport) SupportsNativePath() bool {
	return s != nil && s.NativeVersionCapable && s.FeatureGateEnabled
}

// GetVSphereMultiVCenterSupport returns the cluster version and feature-gate
// status needed to choose between the native and legacy migration paths.
func GetVSphereMultiVCenterSupport(ctx context.Context, client configclient.Interface) (*VSphereMultiVCenterSupport, error) {
	clusterVersion, err := client.ConfigV1().ClusterVersions().Get(ctx, clusterVersionName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting clusterversion %q: %w", clusterVersionName, err)
	}

	version := strings.TrimSpace(clusterVersion.Status.Desired.Version)
	if version == "" {
		return nil, fmt.Errorf("clusterversion %q has empty status.desired.version", clusterVersionName)
	}

	nativeVersionCapable, err := isVersionAtLeastFive(version)
	if err != nil {
		return nil, fmt.Errorf("parsing clusterversion %q desired version %q: %w", clusterVersionName, version, err)
	}

	featureGate, err := client.ConfigV1().FeatureGates().Get(ctx, featureGateName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting featuregate %q: %w", featureGateName, err)
	}

	enabled, err := isFeatureGateEnabledForVersion(featureGate, version, apifeatures.FeatureGateVSphereMultiVCenterDay2)
	if err != nil {
		return nil, fmt.Errorf("checking feature gate %q for version %q: %w", apifeatures.FeatureGateVSphereMultiVCenterDay2, version, err)
	}

	return &VSphereMultiVCenterSupport{
		ClusterVersion:       version,
		FeatureGateEnabled:   enabled,
		NativeVersionCapable: nativeVersionCapable,
	}, nil
}

func isFeatureGateEnabledForVersion(featureGate *configv1.FeatureGate, version string, gateName configv1.FeatureGateName) (bool, error) {
	if featureGate == nil {
		return false, fmt.Errorf("featuregate must not be nil")
	}

	for i := range featureGate.Status.FeatureGates {
		gateVersion := &featureGate.Status.FeatureGates[i]
		if gateVersion.Version != version {
			continue
		}
		for _, enabled := range gateVersion.Enabled {
			if enabled.Name == gateName {
				return true, nil
			}
		}
		return false, nil
	}

	return false, fmt.Errorf("no feature gate status found for version %q", version)
}

func isVersionAtLeastFive(version string) (bool, error) {
	baseVersion := strings.TrimSpace(version)
	if baseVersion == "" {
		return false, fmt.Errorf("version is empty")
	}
	if idx := strings.Index(baseVersion, "-"); idx >= 0 {
		baseVersion = baseVersion[:idx]
	}

	parts := strings.Split(baseVersion, ".")
	if len(parts) < 2 {
		return false, fmt.Errorf("version %q must contain at least major.minor", version)
	}

	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return false, fmt.Errorf("parsing major version %q: %w", parts[0], err)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return false, fmt.Errorf("parsing minor version %q: %w", parts[1], err)
	}

	return major > 5 || (major == 5 && minor >= 0), nil
}
