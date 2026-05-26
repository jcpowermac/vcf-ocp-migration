package openshift

import (
	"context"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	apifeatures "github.com/openshift/api/features"
	fakeconfigclient "github.com/openshift/client-go/config/clientset/versioned/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestGetVSphereMultiVCenterSupport(t *testing.T) {
	tests := []struct {
		name        string
		version     string
		enabled     bool
		wantNative  bool
		wantVersion string
	}{
		{
			name:        "uses native path when version is 5 and gate enabled",
			version:     "5.0.0",
			enabled:     true,
			wantNative:  true,
			wantVersion: "5.0.0",
		},
		{
			name:        "uses legacy path when version is 5 and gate disabled",
			version:     "5.0.1",
			enabled:     false,
			wantNative:  false,
			wantVersion: "5.0.1",
		},
		{
			name:        "uses legacy path when version is 4 and gate enabled",
			version:     "4.19.0",
			enabled:     true,
			wantNative:  false,
			wantVersion: "4.19.0",
		},
		{
			name:        "treats 5 prerelease as native-capable",
			version:     "5.0.0-0.nightly-2026-05-26",
			enabled:     true,
			wantNative:  true,
			wantVersion: "5.0.0-0.nightly-2026-05-26",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clusterVersion := &configv1.ClusterVersion{
				ObjectMeta: metav1.ObjectMeta{Name: clusterVersionName},
				Status: configv1.ClusterVersionStatus{
					Desired: configv1.Release{Version: tt.version},
				},
			}
			featureGate := newFeatureGateForVersion(tt.version, tt.enabled)
			client := fakeconfigclient.NewClientset(clusterVersion, featureGate)

			got, err := GetVSphereMultiVCenterSupport(context.Background(), client)
			if err != nil {
				t.Fatalf("GetVSphereMultiVCenterSupport: %v", err)
			}
			if got.ClusterVersion != tt.wantVersion {
				t.Fatalf("ClusterVersion = %q, want %q", got.ClusterVersion, tt.wantVersion)
			}
			if got.SupportsNativePath() != tt.wantNative {
				t.Fatalf("SupportsNativePath = %v, want %v", got.SupportsNativePath(), tt.wantNative)
			}
		})
	}
}

func TestGetVSphereMultiVCenterSupportErrors(t *testing.T) {
	tests := []struct {
		name    string
		objects []runtime.Object
	}{
		{
			name: "fails when cluster version missing",
			objects: []runtime.Object{
				newFeatureGateForVersion("5.0.0", true),
			},
		},
		{
			name: "fails when feature gate missing",
			objects: []runtime.Object{
				&configv1.ClusterVersion{
					ObjectMeta: metav1.ObjectMeta{Name: clusterVersionName},
					Status: configv1.ClusterVersionStatus{
						Desired: configv1.Release{Version: "5.0.0"},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fakeconfigclient.NewClientset(tt.objects...)
			if _, err := GetVSphereMultiVCenterSupport(context.Background(), client); err == nil {
				t.Fatal("GetVSphereMultiVCenterSupport succeeded, want error")
			}
		})
	}
}

func TestIsVersionAtLeastFive(t *testing.T) {
	tests := []struct {
		version string
		want    bool
	}{
		{version: "5.0.0", want: true},
		{version: "5.0.0-rc.1", want: true},
		{version: "5.1.2", want: true},
		{version: "4.19.0", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			got, err := isVersionAtLeastFive(tt.version)
			if err != nil {
				t.Fatalf("isVersionAtLeastFive: %v", err)
			}
			if got != tt.want {
				t.Fatalf("isVersionAtLeastFive(%q) = %v, want %v", tt.version, got, tt.want)
			}
		})
	}
}

func newFeatureGateForVersion(version string, enabled bool) *configv1.FeatureGate {
	details := configv1.FeatureGateDetails{Version: version}
	if enabled {
		details.Enabled = []configv1.FeatureGateAttributes{
			{Name: apifeatures.FeatureGateVSphereMultiVCenterDay2},
		}
	}

	return &configv1.FeatureGate{
		ObjectMeta: metav1.ObjectMeta{Name: featureGateName},
		Status: configv1.FeatureGateStatus{
			FeatureGates: []configv1.FeatureGateDetails{details},
		},
	}
}
