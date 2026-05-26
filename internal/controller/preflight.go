package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	migrationv1alpha1 "github.com/openshift-splat-team/vcf-ocp-migration/api/v1alpha1"
	"github.com/openshift-splat-team/vcf-ocp-migration/internal/openshift"
	"github.com/openshift-splat-team/vcf-ocp-migration/internal/vsphere"
)

const vsphereCSIDriverName = "csi.vsphere.vmware.com"

var (
	machineHealthCheckGVR = schema.GroupVersionResource{Group: "machine.openshift.io", Version: "v1beta1", Resource: "machinehealthchecks"}
	clusterAutoscalerGVR  = schema.GroupVersionResource{Group: "autoscaling.openshift.io", Version: "v1", Resource: "clusterautoscalers"}
	machineAutoscalerGVR  = schema.GroupVersionResource{Group: "autoscaling.openshift.io", Version: "v1beta1", Resource: "machineautoscalers"}
)

var (
	rootTagPrivileges = []string{
		"InventoryService.Tagging.AttachTag",
		"InventoryService.Tagging.CreateCategory",
		"InventoryService.Tagging.CreateTag",
	}
	objectAttachPrivileges = []string{
		"InventoryService.Tagging.ObjectAttachable",
	}
	vmFolderPrivileges = []string{
		"Folder.Create",
	}
)

func (r *VmwareCloudFoundationMigrationReconciler) runPreflightChecks(ctx context.Context, migration *migrationv1alpha1.VmwareCloudFoundationMigration) (migrationv1alpha1.MigrationPath, string, error) {
	log := klog.FromContext(ctx)
	condType := migrationv1alpha1.ConditionInfrastructurePrepared

	if len(migration.Spec.FailureDomains) == 0 {
		return "", "", fmt.Errorf("spec.failureDomains must not be empty")
	}

	secretRef := migration.Spec.TargetVCenterCredentialsSecret
	if secretRef.Name == "" {
		return "", "", fmt.Errorf("spec.targetVCenterCredentialsSecret.name must not be empty")
	}
	ns := secretRef.Namespace
	if ns == "" {
		ns = migration.Namespace
	}
	if _, err := r.KubeClient.CoreV1().Secrets(ns).Get(ctx, secretRef.Name, metav1.GetOptions{}); err != nil {
		return "", "", fmt.Errorf("target credentials secret %s/%s not found: %w", ns, secretRef.Name, err)
	}

	support, err := openshift.GetVSphereMultiVCenterSupport(ctx, r.ConfigClient)
	if err != nil {
		return "", "", fmt.Errorf("determining migration path: %w", err)
	}

	path := migrationv1alpha1.MigrationPathLegacy
	pathMessage := fmt.Sprintf("Using legacy compatibility path for OpenShift %s", support.ClusterVersion)
	if support.SupportsNativePath() {
		path = migrationv1alpha1.MigrationPathNative
		pathMessage = fmt.Sprintf("Using native multi-vCenter path for OpenShift %s", support.ClusterVersion)
	}
	migration.Status.MigrationPath = path
	r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, pathMessage)

	if err := checkNoVSphereCSIPersistentVolumes(ctx, r.KubeClient); err != nil {
		return path, "", err
	}
	if err := checkInterferingRolloutResources(ctx, r.DynamicClient); err != nil {
		return path, "", err
	}

	infraMgr := openshift.NewInfrastructureManager(r.ConfigClient, r.APIExtensionsClient)
	sourceVC, err := infraMgr.GetSourceVCenter(ctx)
	if err != nil {
		return path, "", fmt.Errorf("getting source vCenter: %w", err)
	}

	sm := openshift.NewSecretManager(r.KubeClient)
	srcUser, srcPass, err := sm.GetCredentials(ctx, sourceVC.Server)
	if err != nil {
		return path, "", fmt.Errorf("getting source vCenter credentials: %w", err)
	}

	if len(sourceVC.Datacenters) == 0 {
		return path, "", fmt.Errorf("source vCenter has no datacenters configured")
	}
	srcDC := sourceVC.Datacenters[0]
	srcSession, err := getVSphereSession(ctx, sourceVC.Server, srcDC, srcUser, srcPass)
	if err != nil {
		return path, "", fmt.Errorf("connecting to source vCenter %s: %w", sourceVC.Server, err)
	}
	if _, err := srcSession.Finder.Datacenter(ctx, srcDC); err != nil {
		return path, "", fmt.Errorf("source datacenter %q not accessible: %w", srcDC, err)
	}
	log.V(1).Info("source vCenter connectivity validated", "server", sourceVC.Server)

	for i := range migration.Spec.FailureDomains {
		fd := &migration.Spec.FailureDomains[i]
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, fmt.Sprintf("Validating target failure domain %q", fd.Name))

		username, password, err := getTargetCredentials(ctx, r.KubeClient, migration, fd.Server)
		if err != nil {
			return path, "", fmt.Errorf("getting credentials for target %s: %w", fd.Server, err)
		}

		session, err := getVSphereSession(ctx, fd.Server, fd.Topology.Datacenter, username, password)
		if err != nil {
			return path, "", fmt.Errorf("connecting to target vCenter %s: %w", fd.Server, err)
		}

		datacenter, err := session.Finder.Datacenter(ctx, fd.Topology.Datacenter)
		if err != nil {
			return path, "", fmt.Errorf("target datacenter %q on %s not found: %w", fd.Topology.Datacenter, fd.Server, err)
		}
		cluster, err := session.Finder.ClusterComputeResource(ctx, fd.Topology.ComputeCluster)
		if err != nil {
			return path, "", fmt.Errorf("target cluster %q on %s not found: %w", fd.Topology.ComputeCluster, fd.Server, err)
		}
		if _, err := session.Finder.Datastore(ctx, fd.Topology.Datastore); err != nil {
			return path, "", fmt.Errorf("target datastore %q on %s not found: %w", fd.Topology.Datastore, fd.Server, err)
		}
		for _, networkName := range fd.Topology.Networks {
			if _, err := session.Finder.Network(ctx, networkName); err != nil {
				return path, "", fmt.Errorf("target network %q on %s not found: %w", networkName, fd.Server, err)
			}
		}
		if fd.Topology.ResourcePool != "" {
			if _, err := session.Finder.ResourcePool(ctx, fd.Topology.ResourcePool); err != nil {
				return path, "", fmt.Errorf("target resource pool %q on %s not found: %w", fd.Topology.ResourcePool, fd.Server, err)
			}
		}
		if fd.Topology.Template != "" {
			if _, err := session.Finder.VirtualMachine(ctx, fd.Topology.Template); err != nil {
				return path, "", fmt.Errorf("target template %q on %s not found: %w", fd.Topology.Template, fd.Server, err)
			}
		}
		if err := validateTargetPrivileges(ctx, session, datacenter, cluster); err != nil {
			return path, "", fmt.Errorf("validating target privileges for failure domain %q: %w", fd.Name, err)
		}
		log.V(1).Info("target failure domain validated", "name", fd.Name, "server", fd.Server)
	}

	return path, fmt.Sprintf("Preflight validation passed using %s path", strings.ToLower(string(path))), nil
}

func getMigrationPath(migration *migrationv1alpha1.VmwareCloudFoundationMigration) (migrationv1alpha1.MigrationPath, error) {
	if migration == nil {
		return "", fmt.Errorf("migration must not be nil")
	}
	if migration.Status.MigrationPath == "" {
		return "", fmt.Errorf("status.migrationPath is empty; preflight must complete before continuing")
	}
	return migration.Status.MigrationPath, nil
}

type infrastructurePathMutator interface {
	AddTargetVCenter(context.Context, *configv1.Infrastructure, []configv1.VSpherePlatformFailureDomainSpec) (*configv1.Infrastructure, error)
	AddTargetVCenterWithCRDModification(context.Context, *configv1.Infrastructure, []configv1.VSpherePlatformFailureDomainSpec) (*configv1.Infrastructure, error)
	RemoveSourceVCenter(context.Context, *configv1.Infrastructure, string) (*configv1.Infrastructure, error)
	RemoveSourceVCenterWithCRDModification(context.Context, *configv1.Infrastructure, string) (*configv1.Infrastructure, error)
}

func addTargetVCenterForPath(ctx context.Context, path migrationv1alpha1.MigrationPath, mutator infrastructurePathMutator, infra *configv1.Infrastructure, failureDomains []configv1.VSpherePlatformFailureDomainSpec) (*configv1.Infrastructure, error) {
	if path == migrationv1alpha1.MigrationPathLegacy {
		return mutator.AddTargetVCenterWithCRDModification(ctx, infra, failureDomains)
	}
	return mutator.AddTargetVCenter(ctx, infra, failureDomains)
}

func removeSourceVCenterForPath(ctx context.Context, path migrationv1alpha1.MigrationPath, mutator infrastructurePathMutator, infra *configv1.Infrastructure, sourceServer string) (*configv1.Infrastructure, error) {
	if path == migrationv1alpha1.MigrationPathLegacy {
		return mutator.RemoveSourceVCenterWithCRDModification(ctx, infra, sourceServer)
	}
	return mutator.RemoveSourceVCenter(ctx, infra, sourceServer)
}

func (r *VmwareCloudFoundationMigrationReconciler) hasTargetVCenterConfiguration(ctx context.Context, migration *migrationv1alpha1.VmwareCloudFoundationMigration) (bool, error) {
	if len(migration.Spec.FailureDomains) == 0 {
		return false, nil
	}

	infraMgr := openshift.NewInfrastructureManager(r.ConfigClient, r.APIExtensionsClient)
	infra, err := infraMgr.Get(ctx)
	if err != nil {
		return false, fmt.Errorf("getting infrastructure for target vCenter check: %w", err)
	}
	if infra.Spec.PlatformSpec.VSphere == nil {
		return false, nil
	}

	targetServers := make(map[string]bool, len(migration.Spec.FailureDomains))
	for _, vc := range infra.Spec.PlatformSpec.VSphere.VCenters {
		targetServers[vc.Server] = true
	}

	targetFailureDomains := make(map[string]bool, len(infra.Spec.PlatformSpec.VSphere.FailureDomains))
	for i := range infra.Spec.PlatformSpec.VSphere.FailureDomains {
		fd := &infra.Spec.PlatformSpec.VSphere.FailureDomains[i]
		targetFailureDomains[fd.Name+"|"+fd.Server] = true
	}

	for i := range migration.Spec.FailureDomains {
		fd := &migration.Spec.FailureDomains[i]
		if !targetServers[fd.Server] {
			return false, nil
		}
		if !targetFailureDomains[fd.Name+"|"+fd.Server] {
			return false, nil
		}
	}

	return true, nil
}

func checkNoVSphereCSIPersistentVolumes(ctx context.Context, kubeClient kubernetes.Interface) error {
	pvs, err := kubeClient.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing persistentvolumes: %w", err)
	}

	var blocked []string
	for i := range pvs.Items {
		pv := &pvs.Items[i]
		if pv.Spec.CSI != nil && pv.Spec.CSI.Driver == vsphereCSIDriverName {
			blocked = append(blocked, pv.Name)
		}
	}

	if len(blocked) == 0 {
		return nil
	}

	sort.Strings(blocked)
	return fmt.Errorf("vSphere CSI-backed persistent volumes are not supported for migration; remove PersistentVolumes using driver %q: %s", vsphereCSIDriverName, strings.Join(blocked, ", "))
}

func checkInterferingRolloutResources(ctx context.Context, dynamicClient dynamic.Interface) error {
	var blockers []string

	mhcs, err := listDynamicResourceNames(ctx, dynamicClient, machineHealthCheckGVR)
	if err != nil {
		return fmt.Errorf("listing machinehealthchecks: %w", err)
	}
	if len(mhcs) > 0 {
		blockers = append(blockers, fmt.Sprintf("MachineHealthCheck resources: %s", strings.Join(mhcs, ", ")))
	}

	clusterAutoscalers, err := listDynamicResourceNames(ctx, dynamicClient, clusterAutoscalerGVR)
	if err != nil {
		return fmt.Errorf("listing clusterautoscalers: %w", err)
	}
	if len(clusterAutoscalers) > 0 {
		blockers = append(blockers, fmt.Sprintf("ClusterAutoscaler resources: %s", strings.Join(clusterAutoscalers, ", ")))
	}

	machineAutoscalers, err := listDynamicResourceNames(ctx, dynamicClient, machineAutoscalerGVR)
	if err != nil {
		return fmt.Errorf("listing machineautoscalers: %w", err)
	}
	if len(machineAutoscalers) > 0 {
		blockers = append(blockers, fmt.Sprintf("MachineAutoscaler resources: %s", strings.Join(machineAutoscalers, ", ")))
	}

	if len(blockers) == 0 {
		return nil
	}

	return fmt.Errorf("remove interfering rollout resources before migration: %s", strings.Join(blockers, "; "))
}

func validateTargetPrivileges(ctx context.Context, session *vsphere.Session, datacenter *object.Datacenter, cluster *object.ClusterComputeResource) error {
	if session == nil || session.Client == nil || session.Client.Client == nil {
		return fmt.Errorf("session client must not be nil")
	}

	userSession, err := session.Client.SessionManager.UserSession(ctx)
	if err != nil {
		return fmt.Errorf("getting current vSphere user session: %w", err)
	}
	if userSession == nil {
		return fmt.Errorf("current vSphere user session not found")
	}

	authMgr := object.NewAuthorizationManager(session.Client.Client)
	folders, err := datacenter.Folders(ctx)
	if err != nil {
		return fmt.Errorf("getting datacenter folders: %w", err)
	}

	checks := []struct {
		entity     types.ManagedObjectReference
		privileges []string
		label      string
	}{
		{
			entity:     session.Client.Client.ServiceContent.RootFolder,
			privileges: rootTagPrivileges,
			label:      "root folder",
		},
		{
			entity:     folders.VmFolder.Reference(),
			privileges: vmFolderPrivileges,
			label:      fmt.Sprintf("VM folder %q", folders.VmFolder.InventoryPath),
		},
		{
			entity:     datacenter.Reference(),
			privileges: objectAttachPrivileges,
			label:      fmt.Sprintf("datacenter %q", datacenter.InventoryPath),
		},
		{
			entity:     cluster.Reference(),
			privileges: objectAttachPrivileges,
			label:      fmt.Sprintf("cluster %q", cluster.InventoryPath),
		},
	}

	for _, check := range checks {
		results, err := authMgr.HasUserPrivilegeOnEntities(ctx, []types.ManagedObjectReference{check.entity}, userSession.UserName, check.privileges)
		if err != nil {
			return fmt.Errorf("checking privileges on %s: %w", check.label, err)
		}
		if len(results) == 0 {
			return fmt.Errorf("no privilege results returned for %s", check.label)
		}
		missing := missingPrivileges(results[0], check.privileges)
		if len(missing) > 0 {
			return fmt.Errorf("user %q is missing %s on %s", userSession.UserName, strings.Join(missing, ", "), check.label)
		}
	}

	return nil
}

func missingPrivileges(entityPrivilege types.EntityPrivilege, requested []string) []string {
	available := make(map[string]bool, len(entityPrivilege.PrivAvailability))
	for _, privilege := range entityPrivilege.PrivAvailability {
		available[privilege.PrivId] = privilege.IsGranted
	}

	var missing []string
	for _, privilege := range requested {
		if !available[privilege] {
			missing = append(missing, privilege)
		}
	}
	sort.Strings(missing)
	return missing
}

func listDynamicResourceNames(ctx context.Context, dynamicClient dynamic.Interface, gvr schema.GroupVersionResource) ([]string, error) {
	resourceList, err := dynamicClient.Resource(gvr).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(resourceList.Items))
	for i := range resourceList.Items {
		item := &resourceList.Items[i]
		name := item.GetName()
		if namespace := item.GetNamespace(); namespace != "" {
			name = namespace + "/" + name
		}
		names = append(names, name)
	}

	sort.Strings(names)
	return names, nil
}
