// Copyright (c) 2018 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package botanist

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	v1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	"github.com/gardener/gardener/pkg/chartrenderer"
	"github.com/gardener/gardener/pkg/client/kubernetes"
	"github.com/gardener/gardener/pkg/operation/botanist/component"
	botanistconstants "github.com/gardener/gardener/pkg/operation/botanist/constants"
	"github.com/gardener/gardener/pkg/operation/botanist/extensions/dns"
	"github.com/gardener/gardener/pkg/operation/common"
	"github.com/gardener/gardener/pkg/utils"
	"github.com/gardener/gardener/pkg/utils/flow"
	kutil "github.com/gardener/gardener/pkg/utils/kubernetes"
	"github.com/gardener/gardener/pkg/utils/secrets"
	versionutils "github.com/gardener/gardener/pkg/utils/version"

	"github.com/gardener/gardener-resource-manager/pkg/manager"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// DNSIngressName is a constant for a DNS resources used for the ingress domain name.
	DNSIngressName = "ingress"
	// SecretLabelKeyManagedResource is a key for a label on a secret with the value 'managed-resource'.
	SecretLabelKeyManagedResource = "managed-resource"
)

// GenerateKubernetesDashboardConfig generates the values which are required to render the chart of
// the kubernetes-dashboard properly.
func (b *Botanist) GenerateKubernetesDashboardConfig() (map[string]interface{}, error) {
	var (
		enabled = b.Shoot.KubernetesDashboardEnabled()
		values  map[string]interface{}
	)

	if enabled && b.Shoot.Info.Spec.Addons.KubernetesDashboard.AuthenticationMode != nil {
		values = map[string]interface{}{
			"authenticationMode": *b.Shoot.Info.Spec.Addons.KubernetesDashboard.AuthenticationMode,
		}
	}

	return common.GenerateAddonConfig(values, enabled), nil
}

// EnsureIngressDNSRecord ensures the nginx DNSEntry and waits for completion.
func (b *Botanist) EnsureIngressDNSRecord(ctx context.Context) error {
	return component.OpWaiter(b.Shoot.Components.Extensions.DNS.NginxEntry).Deploy(ctx)
}

// DefaultNginxIngressDNSEntry returns a Deployer which removes existing nginx ingress DNSEntry.
func (b *Botanist) DefaultNginxIngressDNSEntry(seedClient client.Client) component.DeployWaiter {
	return component.OpDestroy(dns.NewDNSEntry(
		&dns.EntryValues{
			Name: DNSIngressName,
		},
		b.Shoot.SeedNamespace,
		b.K8sSeedClient.ChartApplier(),
		b.ChartsRootPath,
		b.Logger,
		seedClient,
		nil,
	))
}

// SetNginxIngressAddress sets the IP address of the API server's LoadBalancer.
func (b *Botanist) SetNginxIngressAddress(address string, seedClient client.Client) {
	if b.NeedsExternalDNS() && !b.Shoot.HibernationEnabled && b.Shoot.NginxIngressEnabled() {
		b.Shoot.Components.Extensions.DNS.NginxEntry = dns.NewDNSEntry(
			&dns.EntryValues{
				Name:    DNSIngressName,
				DNSName: b.Shoot.GetIngressFQDN("*"),
				Targets: []string{address},
			},
			b.Shoot.SeedNamespace,
			b.K8sSeedClient.ChartApplier(),
			b.ChartsRootPath,
			b.Logger,
			seedClient,
			nil,
		)
	}
}

// GenerateNginxIngressConfig generates the values which are required to render the chart of
// the nginx-ingress properly.
func (b *Botanist) GenerateNginxIngressConfig() (map[string]interface{}, error) {
	var (
		enabled = b.Shoot.NginxIngressEnabled()
		values  map[string]interface{}
	)

	if enabled {
		values = map[string]interface{}{
			"controller": map[string]interface{}{
				"customConfig": b.Shoot.Info.Spec.Addons.NginxIngress.Config,
				"service": map[string]interface{}{
					"loadBalancerSourceRanges": b.Shoot.Info.Spec.Addons.NginxIngress.LoadBalancerSourceRanges,
					"externalTrafficPolicy":    *b.Shoot.Info.Spec.Addons.NginxIngress.ExternalTrafficPolicy,
				},
			},
		}
	}

	return common.GenerateAddonConfig(values, enabled), nil
}

// DeployManagedResources deploys all the ManagedResource CRDs for the gardener-resource-manager.
func (b *Botanist) DeployManagedResources(ctx context.Context) error {
	type managedResourceOptions struct {
		keepObjects     bool
		chartRenderFunc func() (*chartrenderer.RenderedChart, error)
	}

	var (
		injectedLabels = map[string]string{
			common.ShootNoCleanup: "true",
		}
		labels = map[string]string{
			botanistconstants.ManagedResourceLabelKeyOrigin: botanistconstants.ManagedResourceLabelValueGardener,
		}
		charts = map[string]managedResourceOptions{
			common.ManagedResourceShootCoreName:     {false, b.generateCoreAddonsChart},
			common.ManagedResourceCoreNamespaceName: {true, b.generateCoreNamespacesChart},
			common.ManagedResourceAddonsName:        {false, b.generateOptionalAddonsChart},
			// TODO: Just a temporary solution. Remove this in a future version once Kyma is moved out again.
			common.ManagedResourceKymaName: {false, b.generateTemporaryKymaAddonsChart},
		}
	)

	for name, options := range charts {
		renderedChart, err := options.chartRenderFunc()
		if err != nil {
			return fmt.Errorf("error rendering %q chart: %+v", name, err)
		}

		data := renderedChart.AsSecretData()
		secretName := "managedresource-" + name

		if err := manager.
			NewSecret(b.K8sSeedClient.Client()).
			WithNamespacedName(b.Shoot.SeedNamespace, secretName).
			WithKeyValues(data).
			Reconcile(ctx); err != nil {
			return err
		}

		if err := manager.
			NewManagedResource(b.K8sSeedClient.Client()).
			WithNamespacedName(b.Shoot.SeedNamespace, name).
			WithLabels(labels).
			WithSecretRef(secretName).
			WithInjectedLabels(injectedLabels).
			KeepObjects(options.keepObjects).
			Reconcile(ctx); err != nil {
			return err
		}
	}

	return b.deployCloudConfigExecutionManagedResource(ctx)
}

// deployCloudConfigExecutionManagedResource creates the cloud config managed resource that contains:
// 1. A secret containing the dedicated cloud config execution script for each worker group
// 2. A secret containing some shared RBAC policies for downloading the cloud config execution script
func (b *Botanist) deployCloudConfigExecutionManagedResource(ctx context.Context) error {
	var (
		injectedLabels = map[string]string{
			common.ShootNoCleanup: "true",
		}
		labels = map[string]string{
			botanistconstants.ManagedResourceLabelKeyOrigin: botanistconstants.ManagedResourceLabelValueGardener,
		}
		managedResourceName = "shoot-cloud-config-execution"
		secretLabels        = map[string]string{
			SecretLabelKeyManagedResource: managedResourceName,
		}
		wantedSecretNames = sets.NewString()
	)

	cloudConfigCharts := map[string]func() (*chartrenderer.RenderedChart, error){
		"shoot-cloud-config-rbac": b.generateCloudConfigRBACChart,
	}
	bootstrapTokenSecret, err := kutil.ComputeBootstrapToken(ctx, b.K8sShootClient.Client(), utils.ComputeSHA256Hex([]byte(time.Now().Format("2006-01-02")))[:6], "A bootstrap token generated by Gardener.", 48*time.Hour)
	if err != nil {
		return fmt.Errorf("error computing bootstrap token for shoot cloud config: %+v", err)
	}
	//  for each worker pool add a secret containing the cloud config execution script
	for _, worker := range b.Shoot.Info.Spec.Provider.Workers {
		name := fmt.Sprintf("shoot-cloud-config-execution-%s", worker.Name)
		cloudConfigCharts[name] = b.getGenerateCloudConfigExecutionChartFunc(name, worker, bootstrapTokenSecret)
	}
	cloudConfigManagedResource := manager.
		NewManagedResource(b.K8sSeedClient.Client()).
		WithNamespacedName(b.Shoot.SeedNamespace, managedResourceName).
		WithLabels(labels).
		WithInjectedLabels(injectedLabels).
		KeepObjects(false)

	// reconcile secrets and reference them to the ManagedResource
	fns := make([]flow.TaskFn, 0, len(cloudConfigCharts))
	for name, renderChartFunc := range cloudConfigCharts {
		renderedChart, err := renderChartFunc()
		if err != nil {
			return fmt.Errorf("error rendering %q chart: %+v", name, err)
		}
		data := renderedChart.AsSecretData()
		secretName := "managedresource-" + name
		fns = append(fns, func(ctx context.Context) error {
			return manager.
				NewSecret(b.K8sSeedClient.Client()).
				WithNamespacedName(b.Shoot.SeedNamespace, secretName).
				WithKeyValues(data).
				WithLabels(secretLabels).
				Reconcile(ctx)
		})
		cloudConfigManagedResource.WithSecretRef(secretName)
		wantedSecretNames.Insert(secretName)
	}
	if err := flow.Parallel(fns...)(ctx); err != nil {
		return err
	}
	if err := cloudConfigManagedResource.Reconcile(ctx); err != nil {
		return err
	}
	if err := b.deleteStaleSecretsMatchLabel(ctx, secretLabels, wantedSecretNames); err != nil {
		return err
	}

	// TODO: remove this in the future
	return b.deleteLegacyCloudConfigSecret(ctx)
}

// deleteStaleSecretsMatchLabel deletes the stale secrets that match the labels but are not used anymore
func (b *Botanist) deleteStaleSecretsMatchLabel(ctx context.Context, labels map[string]string, wantedSecretNames sets.String) error {
	c := b.K8sSeedClient.Client()

	secretList := &corev1.SecretList{}
	if err := c.List(ctx, secretList, client.InNamespace(b.Shoot.SeedNamespace), client.MatchingLabels(labels)); err != nil {
		return err
	}

	fns := make([]flow.TaskFn, 0, meta.LenList(secretList))
	for _, secret := range secretList.Items {
		if !wantedSecretNames.Has(secret.Name) {
			toDelete := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      secret.Name,
					Namespace: secret.Namespace,
				},
			}
			fns = append(fns, func(ctx context.Context) error {
				return client.IgnoreNotFound(c.Delete(ctx, toDelete, kubernetes.DefaultDeleteOptions...))
			})
		}
	}
	return flow.Parallel(fns...)(ctx)
}

// deleteLegacyCloudConfigSecret deletes the legacy secret created prior to v1.3.0, which is not labeled with the referenced ManagedResource
func (b *Botanist) deleteLegacyCloudConfigSecret(ctx context.Context) error {
	c := b.K8sSeedClient.Client()

	legacySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "managedresource-shoot-cloud-config-execution",
			Namespace: b.Shoot.SeedNamespace,
		},
	}
	return client.IgnoreNotFound(c.Delete(ctx, legacySecret, kubernetes.DefaultDeleteOptions...))
}

// generateCoreAddonsChart renders the gardener-resource-manager configuration for the core addons. After that it
// creates a ManagedResource CRD that references the rendered manifests and creates it.
func (b *Botanist) generateCoreAddonsChart() (*chartrenderer.RenderedChart, error) {
	var (
		kubeProxySecret = b.Secrets["kube-proxy"]
		global          = map[string]interface{}{
			"kubernetesVersion": b.Shoot.Info.Spec.Kubernetes.Version,
			"podNetwork":        b.Shoot.Networks.Pods.String(),
			"vpaEnabled":        b.Shoot.WantsVerticalPodAutoscaler,
		}
		coreDNSConfig = map[string]interface{}{
			"service": map[string]interface{}{
				"clusterDNS": b.Shoot.Networks.CoreDNS.String(),
				// TODO: resolve conformance test issue before changing:
				// https://github.com/kubernetes/kubernetes/blob/master/test/e2e/network/dns.go#L44
				"domain": map[string]interface{}{
					"clusterDomain": gardencorev1beta1.DefaultDomain,
				},
			},
		}
		podSecurityPolicies = map[string]interface{}{
			"allowPrivilegedContainers": *b.Shoot.Info.Spec.Kubernetes.AllowPrivilegedContainers,
		}
		kubeProxyConfig = map[string]interface{}{
			"kubeconfig":        kubeProxySecret.Data["kubeconfig"],
			"kubernetesVersion": b.Shoot.Info.Spec.Kubernetes.Version,
			"podAnnotations": map[string]interface{}{
				"checksum/secret-kube-proxy": b.CheckSums["kube-proxy"],
			},
			"enableIPVS": b.Shoot.IPVSEnabled(),
		}
		metricsServerConfig = map[string]interface{}{
			"tls": map[string]interface{}{
				"caBundle": b.Secrets[v1beta1constants.SecretNameCAMetricsServer].Data[secrets.DataKeyCertificateCA],
			},
			"secret": map[string]interface{}{
				"data": b.Secrets["metrics-server"].Data,
			},
		}
		verticalPodAutoscaler = map[string]interface{}{
			"admissionController": map[string]interface{}{"enableServiceAccount": false},
			"exporter":            map[string]interface{}{"enableServiceAccount": false},
			"recommender":         map[string]interface{}{"enableServiceAccount": false},
			"updater":             map[string]interface{}{"enableServiceAccount": false},
		}

		shootInfo = map[string]interface{}{
			"projectName":       b.Garden.Project.Name,
			"shootName":         b.Shoot.Info.Name,
			"provider":          b.Shoot.Info.Spec.Provider.Type,
			"region":            b.Shoot.Info.Spec.Region,
			"kubernetesVersion": b.Shoot.Info.Spec.Kubernetes.Version,
			"podNetwork":        b.Shoot.Networks.Pods.String(),
			"serviceNetwork":    b.Shoot.Networks.Services.String(),
			"maintenanceBegin":  b.Shoot.Info.Spec.Maintenance.TimeWindow.Begin,
			"maintenanceEnd":    b.Shoot.Info.Spec.Maintenance.TimeWindow.End,
		}
		nodeExporterConfig        = map[string]interface{}{}
		blackboxExporterConfig    = map[string]interface{}{}
		nodeProblemDetectorConfig = map[string]interface{}{}
		networkPolicyConfig       = map[string]interface{}{}

		nodeNetwork = b.Shoot.GetNodeNetwork()
	)

	proxyConfig := b.Shoot.Info.Spec.Kubernetes.KubeProxy
	if proxyConfig != nil {
		kubeProxyConfig["featureGates"] = proxyConfig.FeatureGates
	}

	if domain := b.Shoot.ExternalClusterDomain; domain != nil {
		shootInfo["domain"] = *domain
	}
	var extensions []string
	for extensionType := range b.Shoot.Extensions {
		extensions = append(extensions, extensionType)
	}
	shootInfo["extensions"] = strings.Join(extensions, ",")

	coreDNS, err := b.InjectShootShootImages(coreDNSConfig, common.CoreDNSImageName)
	if err != nil {
		return nil, err
	}

	nodeProblemDetector, err := b.InjectShootShootImages(nodeProblemDetectorConfig, common.NodeProblemDetectorImageName)
	if err != nil {
		return nil, err
	}

	kubeProxy, err := b.InjectShootShootImages(kubeProxyConfig, common.KubeProxyImageName, common.AlpineImageName)
	if err != nil {
		return nil, err
	}

	metricsServer, err := b.InjectShootShootImages(metricsServerConfig, common.MetricsServerImageName)
	if err != nil {
		return nil, err
	}

	nodeExporter, err := b.InjectShootShootImages(nodeExporterConfig, common.NodeExporterImageName)
	if err != nil {
		return nil, err
	}
	blackboxExporter, err := b.InjectShootShootImages(blackboxExporterConfig, common.BlackboxExporterImageName)
	if err != nil {
		return nil, err
	}

	apiserverProxyConfig := map[string]interface{}{
		"advertiseIPAddress": b.APIServerClusterIP,
		"proxySeedServer":    fmt.Sprintf("%s:8443", b.Shoot.ComputeOutOfClusterAPIServerAddress(b.APIServerAddress, true)),
	}

	apiserverProxy, err := b.InjectShootShootImages(apiserverProxyConfig, common.APIServerPorxySidecarImageName, common.APIServerProxyImageName)
	if err != nil {
		return nil, err
	}

	if nodeNetwork != nil {
		shootInfo["nodeNetwork"] = *nodeNetwork
	}

	values := map[string]interface{}{
		"global":                  global,
		"cluster-autoscaler":      common.GenerateAddonConfig(nil, b.Shoot.WantsClusterAutoscaler),
		"coredns":                 coreDNS,
		"kube-apiserver-kubelet":  common.GenerateAddonConfig(nil, true),
		"apiserver-proxy":         common.GenerateAddonConfig(apiserverProxy, b.APIServerSNIEnabled()),
		"kube-controller-manager": common.GenerateAddonConfig(nil, true),
		"kube-proxy":              common.GenerateAddonConfig(kubeProxy, true),
		"kube-scheduler":          common.GenerateAddonConfig(nil, true),
		"metrics-server":          common.GenerateAddonConfig(metricsServer, true),
		"monitoring": common.GenerateAddonConfig(map[string]interface{}{
			"node-exporter":     nodeExporter,
			"blackbox-exporter": blackboxExporter,
		}, b.Shoot.GetPurpose() != gardencorev1beta1.ShootPurposeTesting),
		"network-policies":        common.GenerateAddonConfig(networkPolicyConfig, true),
		"node-problem-detector":   common.GenerateAddonConfig(nodeProblemDetector, true),
		"podsecuritypolicies":     common.GenerateAddonConfig(podSecurityPolicies, true),
		"shoot-info":              common.GenerateAddonConfig(shootInfo, true),
		"vertical-pod-autoscaler": common.GenerateAddonConfig(verticalPodAutoscaler, b.Shoot.WantsVerticalPodAutoscaler),
		"cluster-identity":        map[string]interface{}{"clusterIdentity": b.Shoot.Info.Status.ClusterIdentity},
	}

	var shootClient = b.K8sShootClient.Client()

	if b.Shoot.KonnectivityTunnelEnabled {
		konnectivityAgentConfig := map[string]interface{}{
			"proxyHost": common.GetAPIServerDomain(b.Shoot.InternalClusterDomain),
			"podAnnotations": map[string]interface{}{
				"checksum/secret-konnectivity-agent": b.CheckSums["konnectivity-agent"],
			},
		}

		// Konnectivity agent related values
		konnectivityAgent, err := b.InjectShootShootImages(konnectivityAgentConfig, common.KonnectivityAgentImageName)
		if err != nil {
			return nil, err
		}

		values["konnectivity-agent"] = common.GenerateAddonConfig(konnectivityAgent, true)

		// TODO: remove when konnectivity tunnel is the default tunneling method for all shoots.
		secret, err := common.GetSecretFromSecretRef(context.TODO(), shootClient, &corev1.SecretReference{Namespace: metav1.NamespaceSystem, Name: "vpn-shoot"})
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, err
		}

		if secret != nil {
			if err := b.K8sShootClient.Client().Delete(context.TODO(), secret); err != nil {
				return nil, err
			}
		}
	} else {
		var (
			vpnTLSAuthSecret = b.Secrets["vpn-seed-tlsauth"]
			vpnShootSecret   = b.Secrets["vpn-shoot"]
			vpnShootConfig   = map[string]interface{}{
				"podNetwork":     b.Shoot.Networks.Pods.String(),
				"serviceNetwork": b.Shoot.Networks.Services.String(),
				"tlsAuth":        vpnTLSAuthSecret.Data["vpn.tlsauth"],
				"vpnShootSecretData": map[string]interface{}{
					"ca":     vpnShootSecret.Data["ca.crt"],
					"tlsCrt": vpnShootSecret.Data["tls.crt"],
					"tlsKey": vpnShootSecret.Data["tls.key"],
				},
				"podAnnotations": map[string]interface{}{
					"checksum/secret-vpn-shoot": b.CheckSums["vpn-shoot"],
				},
			}
		)

		// OpenVPN related values
		if openvpnDiffieHellmanSecret, ok := b.Secrets[common.GardenRoleOpenVPNDiffieHellman]; ok {
			vpnShootConfig["diffieHellmanKey"] = openvpnDiffieHellmanSecret.Data["dh2048.pem"]
		}

		if nodeNetwork != nil {
			vpnShootConfig["nodeNetwork"] = *nodeNetwork
		}

		vpnShoot, err := b.InjectShootShootImages(vpnShootConfig, common.VPNShootImageName)
		if err != nil {
			return nil, err
		}

		values["vpn-shoot"] = common.GenerateAddonConfig(vpnShoot, true)
	}

	return b.K8sShootClient.ChartRenderer().Render(filepath.Join(common.ChartPath, "shoot-core", "components"), "shoot-core", metav1.NamespaceSystem, values)
}

// generateCoreNamespacesChart renders the gardener-resource-manager configuration for the core namespaces. After that it
// creates a ManagedResource CRD that references the rendered manifests and creates it.
func (b *Botanist) generateCoreNamespacesChart() (*chartrenderer.RenderedChart, error) {
	return b.K8sShootClient.ChartRenderer().Render(filepath.Join(common.ChartPath, "shoot-core", "namespaces"), "shoot-core-namespaces", metav1.NamespaceSystem, map[string]interface{}{
		"labels": map[string]string{
			v1beta1constants.GardenerPurpose: metav1.NamespaceSystem,
		},
	})
}

// generateOptionalAddonsChart renders the gardener-resource-manager chart for the optional addons. After that it
// creates a ManagedResource CRD that references the rendered manifests and creates it.
func (b *Botanist) generateOptionalAddonsChart() (*chartrenderer.RenderedChart, error) {
	global := map[string]interface{}{
		"vpaEnabled": b.Shoot.WantsVerticalPodAutoscaler,
	}

	kubernetesDashboardConfig, err := b.GenerateKubernetesDashboardConfig()
	if err != nil {
		return nil, err
	}
	kubernetesDashboardImagesToInject := []string{common.KubernetesDashboardImageName}

	k8sVersionLessThan116, err := versionutils.CompareVersions(b.Shoot.Info.Spec.Kubernetes.Version, "<", "1.16")
	if err != nil {
		return nil, err
	}
	if !k8sVersionLessThan116 {
		kubernetesDashboardImagesToInject = append(kubernetesDashboardImagesToInject, common.KubernetesDashboardMetricsScraperImageName)
	}

	kubernetesDashboard, err := b.InjectShootShootImages(kubernetesDashboardConfig, kubernetesDashboardImagesToInject...)
	if err != nil {
		return nil, err
	}

	nginxIngressConfig, err := b.GenerateNginxIngressConfig()
	if err != nil {
		return nil, err
	}
	nginxIngress, err := b.InjectShootShootImages(nginxIngressConfig, common.NginxIngressControllerImageName, common.IngressDefaultBackendImageName)
	if err != nil {
		return nil, err
	}

	return b.K8sShootClient.ChartRenderer().Render(filepath.Join(common.ChartPath, "shoot-addons"), "addons", metav1.NamespaceSystem, map[string]interface{}{
		"global":               global,
		"kubernetes-dashboard": kubernetesDashboard,
		"nginx-ingress":        nginxIngress,
	})
}

// generateTemporaryKymaAddonsChart renders the gardener-resource-manager chart for the kyma addon. After that it
// creates a ManagedResource CRD that references the rendered manifests and creates it.
// TODO: Just a temporary solution. Remove this in a future version once Kyma is moved out again.
func (b *Botanist) generateTemporaryKymaAddonsChart() (*chartrenderer.RenderedChart, error) {
	return b.K8sShootClient.ChartRenderer().Render(filepath.Join(common.ChartPath, "shoot-addons-kyma"), "kyma", "kyma-installer", map[string]interface{}{
		"kyma": common.GenerateAddonConfig(nil, metav1.HasAnnotation(b.Shoot.Info.ObjectMeta, common.ShootExperimentalAddonKyma)),
	})
}
