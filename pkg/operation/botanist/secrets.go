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

	gardencorev1alpha1 "github.com/gardener/gardener/pkg/apis/core/v1alpha1"
	gardencorev1alpha1helper "github.com/gardener/gardener/pkg/apis/core/v1alpha1/helper"
	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	v1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	gardencorev1beta1helper "github.com/gardener/gardener/pkg/apis/core/v1beta1/helper"
	"github.com/gardener/gardener/pkg/features"
	gardenletfeatures "github.com/gardener/gardener/pkg/gardenlet/features"
	"github.com/gardener/gardener/pkg/operation/common"
	"github.com/gardener/gardener/pkg/operation/seed"
	"github.com/gardener/gardener/pkg/operation/shootsecrets"
	kutil "github.com/gardener/gardener/pkg/utils/kubernetes"
	"github.com/gardener/gardener/pkg/utils/secrets"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// LoadExistingSecretsIntoShootState loads secrets which are already present in the Shoot's Control Plane
// extracts their metadata and saves them into the ShootState.
// TODO: This step can be removed in a future version, once secrets for all existing shoots have been synced to the ShootState.
func (b *Botanist) LoadExistingSecretsIntoShootState(ctx context.Context) error {
	gardenerResourceDataList := gardencorev1alpha1helper.GardenerResourceDataList(b.ShootState.Spec.Gardener)
	existingSecretsMap, err := b.fetchExistingSecrets(ctx)
	if err != nil {
		return err
	}

	secretsManager := shootsecrets.NewSecretsManager(
		gardenerResourceDataList,
		b.generateStaticTokenConfig(),
		wantedCertificateAuthorities,
		b.generateWantedSecretConfigs,
	)

	if gardencorev1beta1helper.ShootWantsBasicAuthentication(b.Shoot.Info) {
		secretsManager = secretsManager.WithAPIServerBasicAuthConfig(basicAuthSecretAPIServer)
	}

	if err := secretsManager.WithExistingSecrets(existingSecretsMap).Load(); err != nil {
		return err
	}

	shootState := &gardencorev1alpha1.ShootState{ObjectMeta: kutil.ObjectMeta(b.Shoot.Info.Namespace, b.Shoot.Info.Name)}
	if _, err = controllerutil.CreateOrUpdate(ctx, b.K8sGardenClient.Client(), shootState, func() error {
		shootState.Spec.Gardener = secretsManager.GardenerResourceDataList
		return nil
	}); err != nil {
		return err
	}

	b.ShootState = shootState
	return nil
}

// GenerateAndSaveSecrets creates a CA certificate for the Shoot cluster and uses it to sign the server certificate
// used by the kube-apiserver, and all client certificates used for communication. It also creates RSA key
// pairs for SSH connections to the nodes/VMs and for the VPN tunnel. Moreover, basic authentication
// credentials are computed which will be used to secure the Ingress resources and the kube-apiserver itself.
// Server certificates for the exposed monitoring endpoints (via Ingress) are generated as well.
// In the end it saves the generated secrets in the ShootState
func (b *Botanist) GenerateAndSaveSecrets(ctx context.Context) error {
	gardenerResourceDataList := gardencorev1alpha1helper.GardenerResourceDataList(b.ShootState.Spec.Gardener)

	if val, ok := common.GetShootOperationAnnotation(b.Shoot.Info.Annotations); ok && val == common.ShootOperationRotateKubeconfigCredentials {
		if err := b.rotateKubeconfigSecrets(ctx, &gardenerResourceDataList); err != nil {
			return err
		}
	}

	if b.Shoot.Info.DeletionTimestamp == nil {
		if b.Shoot.KonnectivityTunnelEnabled {
			if err := b.cleanupTunnelSecrets(ctx, &gardenerResourceDataList, "vpn-seed", "vpn-seed-tlsauth", "vpn-shoot"); err != nil {
				return err
			}
		} else {
			if err := b.cleanupTunnelSecrets(ctx, &gardenerResourceDataList, common.KonnectivityServerKubeconfig, common.KonnectivityServerCertName); err != nil {
				return err
			}
		}
	}

	shootWantsBasicAuth := gardencorev1beta1helper.ShootWantsBasicAuthentication(b.Shoot.Info)
	shootHasBasicAuth := gardenerResourceDataList.Get(common.BasicAuthSecretName) != nil
	if shootWantsBasicAuth != shootHasBasicAuth {
		if err := b.deleteBasicAuthDependantSecrets(ctx, &gardenerResourceDataList); err != nil {
			return err
		}
	}

	secretsManager := shootsecrets.NewSecretsManager(
		gardenerResourceDataList,
		b.generateStaticTokenConfig(),
		wantedCertificateAuthorities,
		b.generateWantedSecretConfigs,
	)

	if shootWantsBasicAuth {
		secretsManager = secretsManager.WithAPIServerBasicAuthConfig(basicAuthSecretAPIServer)
	}

	if err := secretsManager.Generate(); err != nil {
		return err
	}

	shootState := &gardencorev1alpha1.ShootState{ObjectMeta: kutil.ObjectMeta(b.Shoot.Info.Namespace, b.Shoot.Info.Name)}
	if _, err := controllerutil.CreateOrUpdate(ctx, b.K8sGardenClient.Client(), shootState, func() error {
		shootState.Spec.Gardener = secretsManager.GardenerResourceDataList
		return nil
	}); err != nil {
		return err
	}

	b.ShootState = shootState
	return nil
}

// DeploySecrets takes all existing secrets from the ShootState resource and deploys them in the shoot's control plane.
func (b *Botanist) DeploySecrets(ctx context.Context) error {
	gardenerResourceDataList := gardencorev1alpha1helper.GardenerResourceDataList(b.ShootState.Spec.Gardener)
	existingSecrets, err := b.fetchExistingSecrets(ctx)
	if err != nil {
		return err
	}

	secretsManager := shootsecrets.NewSecretsManager(
		gardenerResourceDataList,
		b.generateStaticTokenConfig(),
		wantedCertificateAuthorities,
		b.generateWantedSecretConfigs,
	)

	if gardencorev1beta1helper.ShootWantsBasicAuthentication(b.Shoot.Info) {
		secretsManager.WithAPIServerBasicAuthConfig(basicAuthSecretAPIServer)
	}

	if err := secretsManager.WithExistingSecrets(existingSecrets).Deploy(ctx, b.K8sSeedClient.Client(), b.Shoot.SeedNamespace); err != nil {
		return err
	}

	if err := b.storeAPIServerHealthCheckToken(secretsManager.StaticToken); err != nil {
		return err
	}

	if b.Shoot.WantsVerticalPodAutoscaler {
		if err := b.storeStaticTokenAsSecrets(ctx, secretsManager.StaticToken, secretsManager.DeployedSecrets[v1beta1constants.SecretNameCACluster].Data[secrets.DataKeyCertificateCA], vpaSecrets); err != nil {
			return err
		}
	}

	func() {
		b.mutex.Lock()
		defer b.mutex.Unlock()
		for name, secret := range secretsManager.DeployedSecrets {
			b.Secrets[name] = secret
		}
		for name, secret := range b.Secrets {
			b.CheckSums[name] = common.ComputeSecretCheckSum(secret.Data)
		}
	}()

	wildcardCert, err := seed.GetWildcardCertificate(ctx, b.K8sSeedClient.Client())
	if err != nil {
		return err
	}

	if wildcardCert != nil {
		// Copy certificate to shoot namespace
		crt := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      wildcardCert.GetName(),
				Namespace: b.Shoot.SeedNamespace,
			},
		}

		if _, err := controllerutil.CreateOrUpdate(ctx, b.K8sSeedClient.Client(), crt, func() error {
			crt.Data = wildcardCert.Data
			return nil
		}); err != nil {
			return err
		}

		b.ControlPlaneWildcardCert = crt
	}

	return nil
}

// DeployCloudProviderSecret creates or updates the cloud provider secret in the Shoot namespace
// in the Seed cluster.
func (b *Botanist) DeployCloudProviderSecret(ctx context.Context) error {
	var (
		checksum = common.ComputeSecretCheckSum(b.Shoot.Secret.Data)
		secret   = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      v1beta1constants.SecretNameCloudProvider,
				Namespace: b.Shoot.SeedNamespace,
			},
		}
	)

	if _, err := controllerutil.CreateOrUpdate(ctx, b.K8sSeedClient.Client(), secret, func() error {
		secret.Annotations = map[string]string{
			"checksum/data": checksum,
		}
		secret.Type = corev1.SecretTypeOpaque
		secret.Data = b.Shoot.Secret.Data
		return nil
	}); err != nil {
		return err
	}

	b.mutex.Lock()
	defer b.mutex.Unlock()

	b.Secrets[v1beta1constants.SecretNameCloudProvider] = b.Shoot.Secret
	b.CheckSums[v1beta1constants.SecretNameCloudProvider] = checksum

	return nil
}

func (b *Botanist) fetchExistingSecrets(ctx context.Context) (map[string]*corev1.Secret, error) {
	secretList := &corev1.SecretList{}
	if err := b.K8sSeedClient.Client().List(ctx, secretList, client.InNamespace(b.Shoot.SeedNamespace)); err != nil {
		return nil, err
	}

	existingSecretsMap := make(map[string]*corev1.Secret, len(secretList.Items))
	for _, secret := range secretList.Items {
		secretObj := secret
		existingSecretsMap[secret.Name] = &secretObj
	}

	return existingSecretsMap, nil
}

func (b *Botanist) rotateKubeconfigSecrets(ctx context.Context, gardenerResourceDataList *gardencorev1alpha1helper.GardenerResourceDataList) error {
	for _, secretName := range []string{common.StaticTokenSecretName, common.BasicAuthSecretName, common.KubecfgSecretName} {
		if err := b.K8sSeedClient.Client().Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: b.Shoot.SeedNamespace}}); client.IgnoreNotFound(err) != nil {
			return err
		}
		gardenerResourceDataList.Delete(secretName)
	}
	_, err := kutil.TryUpdateShootAnnotations(b.K8sGardenClient.GardenCore(), retry.DefaultRetry, b.Shoot.Info.ObjectMeta, func(shoot *gardencorev1beta1.Shoot) (*gardencorev1beta1.Shoot, error) {
		delete(shoot.Annotations, v1beta1constants.GardenerOperation)
		delete(shoot.Annotations, common.ShootOperationDeprecated)
		return shoot, nil
	})
	return err
}

func (b *Botanist) deleteBasicAuthDependantSecrets(ctx context.Context, gardenerResourceDataList *gardencorev1alpha1helper.GardenerResourceDataList) error {
	for _, secretName := range []string{common.BasicAuthSecretName, common.KubecfgSecretName} {
		if err := b.K8sSeedClient.Client().Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: b.Shoot.SeedNamespace}}); client.IgnoreNotFound(err) != nil {
			return err
		}
		gardenerResourceDataList.Delete(secretName)
	}
	return nil
}

func (b *Botanist) storeAPIServerHealthCheckToken(staticToken *secrets.StaticToken) error {
	kubeAPIServerHealthCheckToken, err := staticToken.GetTokenForUsername(common.KubeAPIServerHealthCheck)
	if err != nil {
		return err
	}

	b.APIServerHealthCheckToken = kubeAPIServerHealthCheckToken.Token
	return nil
}

func (b *Botanist) storeStaticTokenAsSecrets(ctx context.Context, staticToken *secrets.StaticToken, caCert []byte, secretNameToUsername map[string]string) error {
	for secretName, username := range secretNameToUsername {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretName,
				Namespace: b.Shoot.SeedNamespace,
			},
			Type: corev1.SecretTypeOpaque,
		}

		token, err := staticToken.GetTokenForUsername(username)
		if err != nil {
			return err
		}

		if _, err := controllerutil.CreateOrUpdate(ctx, b.K8sSeedClient.Client(), secret, func() error {
			secret.Data = map[string][]byte{
				secrets.DataKeyToken:         []byte(token.Token),
				secrets.DataKeyCertificateCA: caCert,
			}
			return nil
		}); err != nil {
			return err
		}

		b.CheckSums[secretName] = common.ComputeSecretCheckSum(secret.Data)
	}

	return nil
}

const (
	secretSuffixKubeConfig = "kubeconfig"
	secretSuffixSSHKeyPair = v1beta1constants.SecretNameSSHKeyPair
	secretSuffixMonitoring = "monitoring"
	secretSuffixLogging    = "logging"
)

func computeProjectSecretName(shootName, suffix string) string {
	return fmt.Sprintf("%s.%s", shootName, suffix)
}

type projectSecret struct {
	secretName  string
	suffix      string
	annotations map[string]string
}

// SyncShootCredentialsToGarden copies the kubeconfig generated for the user, the SSH keypair to
// the project namespace in the Garden cluster and the monitoring credentials for the
// user-facing monitoring stack are also copied.
func (b *Botanist) SyncShootCredentialsToGarden(ctx context.Context) error {
	kubecfgURL := common.GetAPIServerDomain(b.Shoot.InternalClusterDomain)
	if b.Shoot.ExternalClusterDomain != nil {
		kubecfgURL = common.GetAPIServerDomain(*b.Shoot.ExternalClusterDomain)
	}

	projectSecrets := []projectSecret{
		{
			secretName:  common.KubecfgSecretName,
			suffix:      secretSuffixKubeConfig,
			annotations: map[string]string{"url": "https://" + kubecfgURL},
		},
		{
			secretName: v1beta1constants.SecretNameSSHKeyPair,
			suffix:     secretSuffixSSHKeyPair,
		},
		{
			secretName:  "monitoring-ingress-credentials-users",
			suffix:      secretSuffixMonitoring,
			annotations: map[string]string{"url": "https://" + b.ComputeGrafanaUsersHost()},
		},
	}

	if gardenletfeatures.FeatureGate.Enabled(features.Logging) {
		projectSecrets = append(projectSecrets, projectSecret{
			secretName:  "logging-ingress-credentials-users",
			suffix:      secretSuffixLogging,
			annotations: map[string]string{"url": "https://" + b.ComputeKibanaHost()},
		})
	}

	for _, projectSecret := range projectSecrets {
		secretObj := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      computeProjectSecretName(b.Shoot.Info.Name, projectSecret.suffix),
				Namespace: b.Shoot.Info.Namespace,
			},
		}

		if _, err := controllerutil.CreateOrUpdate(ctx, b.K8sGardenClient.Client(), secretObj, func() error {
			secretObj.OwnerReferences = []metav1.OwnerReference{
				*metav1.NewControllerRef(b.Shoot.Info, gardencorev1beta1.SchemeGroupVersion.WithKind("Shoot")),
			}
			secretObj.Annotations = projectSecret.annotations
			secretObj.Type = corev1.SecretTypeOpaque
			secretObj.Data = b.Secrets[projectSecret.secretName].Data
			return nil
		}); err != nil {
			return err
		}
	}

	return nil
}

func (b *Botanist) cleanupTunnelSecrets(ctx context.Context, gardenerResourceDataList *gardencorev1alpha1helper.GardenerResourceDataList, secretNames ...string) error {
	// TODO: remove when all Gardener supported versions are >= 1.18
	for _, secret := range secretNames {
		if err := b.K8sSeedClient.Client().Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secret, Namespace: b.Shoot.SeedNamespace}}); client.IgnoreNotFound(err) != nil {
			return err
		}
		gardenerResourceDataList.Delete(secret)
	}
	return nil
}

func dnsNamesForService(name, namespace string) []string {
	return []string{
		name,
		fmt.Sprintf("%s.%s", name, namespace),
		fmt.Sprintf("%s.%s.svc", name, namespace),
		fmt.Sprintf("%s.%s.svc.%s", name, namespace, gardencorev1beta1.DefaultDomain),
	}
}

func dnsNamesForEtcd(namespace string) []string {
	names := []string{
		fmt.Sprintf("%s-local", v1beta1constants.ETCDMain),
		fmt.Sprintf("%s-local", v1beta1constants.ETCDEvents),
	}
	names = append(names, dnsNamesForService(fmt.Sprintf("%s-client", v1beta1constants.ETCDMain), namespace)...)
	names = append(names, dnsNamesForService(fmt.Sprintf("%s-client", v1beta1constants.ETCDEvents), namespace)...)
	return names
}
