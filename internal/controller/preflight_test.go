package controller

import (
	"context"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	configfake "github.com/openshift/client-go/config/clientset/versioned/fake"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	fakekube "k8s.io/client-go/kubernetes/fake"

	migrationv1alpha1 "github.com/openshift-splat-team/vcf-ocp-migration/api/v1alpha1"
	"github.com/openshift-splat-team/vcf-ocp-migration/internal/openshift"
)

func TestCheckNoVSphereCSIPersistentVolumes(t *testing.T) {
	tests := []struct {
		name    string
		pvs     []runtime.Object
		wantErr bool
	}{
		{
			name: "passes without vsphere csi pvs",
			pvs: []runtime.Object{
				&corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{Name: "nfs-pv"},
				},
			},
			wantErr: false,
		},
		{
			name: "fails when vsphere csi pv exists",
			pvs: []runtime.Object{
				&corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{Name: "vsphere-csi-pv"},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{Driver: vsphereCSIDriverName},
						},
					},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fakekube.NewClientset(tt.pvs...)
			err := checkNoVSphereCSIPersistentVolumes(context.Background(), client)
			if (err != nil) != tt.wantErr {
				t.Fatalf("checkNoVSphereCSIPersistentVolumes error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCheckInterferingRolloutResources(t *testing.T) {
	tests := []struct {
		name    string
		objects []runtime.Object
		wantErr bool
	}{
		{
			name:    "passes when no interfering resources exist",
			objects: nil,
			wantErr: false,
		},
		{
			name: "fails when machine health check exists",
			objects: []runtime.Object{
				newUnstructuredResource("machine.openshift.io/v1beta1", "MachineHealthCheck", "openshift-machine-api", "worker-mhc"),
			},
			wantErr: true,
		},
		{
			name: "fails when cluster autoscaler exists",
			objects: []runtime.Object{
				newUnstructuredResource("autoscaling.openshift.io/v1", "ClusterAutoscaler", "", "default"),
			},
			wantErr: true,
		},
		{
			name: "fails when machine autoscaler exists",
			objects: []runtime.Object{
				newUnstructuredResource("autoscaling.openshift.io/v1beta1", "MachineAutoscaler", "openshift-machine-api", "worker-a"),
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, preflightListKinds(), tt.objects...)
			err := checkInterferingRolloutResources(context.Background(), client)
			if (err != nil) != tt.wantErr {
				t.Fatalf("checkInterferingRolloutResources error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func newUnstructuredResource(apiVersion, kind, namespace, name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion(apiVersion)
	obj.SetKind(kind)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	return obj
}

func preflightListKinds() map[schema.GroupVersionResource]string {
	return map[schema.GroupVersionResource]string{
		machineHealthCheckGVR: "MachineHealthCheckList",
		clusterAutoscalerGVR:  "ClusterAutoscalerList",
		machineAutoscalerGVR:  "MachineAutoscalerList",
	}
}

func TestAddTargetVCenterForPath(t *testing.T) {
	tests := []struct {
		name            string
		path            migrationv1alpha1.MigrationPath
		wantDirectCalls int
		wantLegacyCalls int
	}{
		{
			name:            "native path uses direct infrastructure update",
			path:            migrationv1alpha1.MigrationPathNative,
			wantDirectCalls: 1,
			wantLegacyCalls: 0,
		},
		{
			name:            "legacy path uses crd modification update",
			path:            migrationv1alpha1.MigrationPathLegacy,
			wantDirectCalls: 0,
			wantLegacyCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mutator := &fakeInfrastructurePathMutator{}
			_, err := addTargetVCenterForPath(context.Background(), tt.path, mutator, &configv1.Infrastructure{}, nil)
			if err != nil {
				t.Fatalf("addTargetVCenterForPath: %v", err)
			}
			if mutator.addDirectCalls != tt.wantDirectCalls {
				t.Fatalf("addDirectCalls = %d, want %d", mutator.addDirectCalls, tt.wantDirectCalls)
			}
			if mutator.addLegacyCalls != tt.wantLegacyCalls {
				t.Fatalf("addLegacyCalls = %d, want %d", mutator.addLegacyCalls, tt.wantLegacyCalls)
			}
		})
	}
}

func TestRemoveSourceVCenterForPath(t *testing.T) {
	tests := []struct {
		name            string
		path            migrationv1alpha1.MigrationPath
		wantDirectCalls int
		wantLegacyCalls int
	}{
		{
			name:            "native path uses direct source removal",
			path:            migrationv1alpha1.MigrationPathNative,
			wantDirectCalls: 1,
			wantLegacyCalls: 0,
		},
		{
			name:            "legacy path uses crd modification source removal",
			path:            migrationv1alpha1.MigrationPathLegacy,
			wantDirectCalls: 0,
			wantLegacyCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mutator := &fakeInfrastructurePathMutator{}
			_, err := removeSourceVCenterForPath(context.Background(), tt.path, mutator, &configv1.Infrastructure{}, "source.example.com")
			if err != nil {
				t.Fatalf("removeSourceVCenterForPath: %v", err)
			}
			if mutator.removeDirectCalls != tt.wantDirectCalls {
				t.Fatalf("removeDirectCalls = %d, want %d", mutator.removeDirectCalls, tt.wantDirectCalls)
			}
			if mutator.removeLegacyCalls != tt.wantLegacyCalls {
				t.Fatalf("removeLegacyCalls = %d, want %d", mutator.removeLegacyCalls, tt.wantLegacyCalls)
			}
		})
	}
}

func TestHasTargetVCenterConfiguration(t *testing.T) {
	migration := &migrationv1alpha1.VmwareCloudFoundationMigration{
		Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
			FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{
				{Name: "fd-a", Server: "target-a.example.com"},
				{Name: "fd-b", Server: "target-b.example.com"},
			},
		},
	}

	tests := []struct {
		name  string
		spec  migrationv1alpha1.VmwareCloudFoundationMigrationSpec
		infra *configv1.Infrastructure
		want  bool
	}{
		{
			name: "returns false when migration has no failure domains",
			spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{},
			infra: &configv1.Infrastructure{
				ObjectMeta: metav1.ObjectMeta{Name: openshift.InfrastructureName},
				Spec: configv1.InfrastructureSpec{
					PlatformSpec: configv1.PlatformSpec{
						Type: configv1.VSpherePlatformType,
						VSphere: &configv1.VSpherePlatformSpec{
							VCenters: []configv1.VSpherePlatformVCenterSpec{
								{Server: "target-a.example.com"},
							},
						},
					},
				},
			},
			want: false,
		},
		{
			name: "returns false when target vcenters missing",
			spec: migration.Spec,
			infra: &configv1.Infrastructure{
				ObjectMeta: metav1.ObjectMeta{Name: openshift.InfrastructureName},
				Spec: configv1.InfrastructureSpec{
					PlatformSpec: configv1.PlatformSpec{
						Type: configv1.VSpherePlatformType,
						VSphere: &configv1.VSpherePlatformSpec{
							VCenters: []configv1.VSpherePlatformVCenterSpec{
								{Server: "source.example.com"},
							},
						},
					},
				},
			},
			want: false,
		},
		{
			name: "returns false when target failure domains missing",
			spec: migration.Spec,
			infra: &configv1.Infrastructure{
				ObjectMeta: metav1.ObjectMeta{Name: openshift.InfrastructureName},
				Spec: configv1.InfrastructureSpec{
					PlatformSpec: configv1.PlatformSpec{
						Type: configv1.VSpherePlatformType,
						VSphere: &configv1.VSpherePlatformSpec{
							VCenters: []configv1.VSpherePlatformVCenterSpec{
								{Server: "target-a.example.com"},
								{Server: "target-b.example.com"},
							},
							FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{
								{Name: "fd-a", Server: "target-a.example.com"},
							},
						},
					},
				},
			},
			want: false,
		},
		{
			name: "returns true when all target vcenters and failure domains present",
			spec: migration.Spec,
			infra: &configv1.Infrastructure{
				ObjectMeta: metav1.ObjectMeta{Name: openshift.InfrastructureName},
				Spec: configv1.InfrastructureSpec{
					PlatformSpec: configv1.PlatformSpec{
						Type: configv1.VSpherePlatformType,
						VSphere: &configv1.VSpherePlatformSpec{
							VCenters: []configv1.VSpherePlatformVCenterSpec{
								{Server: "source.example.com"},
								{Server: "target-a.example.com"},
								{Server: "target-b.example.com"},
							},
							FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{
								{Name: "fd-a", Server: "target-a.example.com"},
								{Name: "fd-b", Server: "target-b.example.com"},
							},
						},
					},
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reconciler := &VmwareCloudFoundationMigrationReconciler{
				ConfigClient: configfake.NewClientset(tt.infra),
			}
			got, err := reconciler.hasTargetVCenterConfiguration(context.Background(), &migrationv1alpha1.VmwareCloudFoundationMigration{
				Spec: tt.spec,
			})
			if err != nil {
				t.Fatalf("hasTargetVCenterConfiguration: %v", err)
			}
			if got != tt.want {
				t.Fatalf("hasTargetVCenterConfiguration = %v, want %v", got, tt.want)
			}
		})
	}
}

type fakeInfrastructurePathMutator struct {
	addDirectCalls    int
	addLegacyCalls    int
	removeDirectCalls int
	removeLegacyCalls int
}

func (f *fakeInfrastructurePathMutator) AddTargetVCenter(_ context.Context, infra *configv1.Infrastructure, _ []configv1.VSpherePlatformFailureDomainSpec) (*configv1.Infrastructure, error) {
	f.addDirectCalls++
	return infra, nil
}

func (f *fakeInfrastructurePathMutator) AddTargetVCenterWithCRDModification(_ context.Context, infra *configv1.Infrastructure, _ []configv1.VSpherePlatformFailureDomainSpec) (*configv1.Infrastructure, error) {
	f.addLegacyCalls++
	return infra, nil
}

func (f *fakeInfrastructurePathMutator) RemoveSourceVCenter(_ context.Context, infra *configv1.Infrastructure, _ string) (*configv1.Infrastructure, error) {
	f.removeDirectCalls++
	return infra, nil
}

func (f *fakeInfrastructurePathMutator) RemoveSourceVCenterWithCRDModification(_ context.Context, infra *configv1.Infrastructure, _ string) (*configv1.Infrastructure, error) {
	f.removeLegacyCalls++
	return infra, nil
}
