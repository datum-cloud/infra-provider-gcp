package config

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"go.datum.net/infra-provider-gcp/internal/providers"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:defaulter-gen=true

type GCPProvider struct {
	metav1.TypeMeta

	MetricsServer MetricsServerConfig `json:"metricsServer"`

	WebhookServer WebhookServerConfig `json:"webhookServer"`

	Discovery DiscoveryConfig `json:"discovery"`

	DownstreamResourceManagement DownstreamResourceManagementConfig `json:"downstreamResourceManagement"`

	// LocationClassName configures the operator to only consider resources
	// attached to locations with the specified location class.
	//
	// TODO(jreese) move to an approach similar to GatewayClass, where a controller
	// manager is associated with each class, and this will define the controller.
	// +default="self-managed"
	LocationClassName string `json:"locationClassName"`

	// ImageMap is a map of image names to GCP image project/image names.
	ImageMap map[string]string `json:"imageMap,omitempty"`

	// MachineTypeMap is a map of machine type names to GCP machine type names.
	MachineTypeMap map[string]string `json:"machineTypeMap,omitempty"`
}

func SetDefaults_GCPProvider(obj *GCPProvider) {
	if obj.ImageMap == nil {
		obj.ImageMap = map[string]string{
			"datumcloud/ubuntu-2204-lts":           "projects/ubuntu-os-cloud/global/images/ubuntu-2204-jammy-v20240927",
			"datumcloud/cos-stable-117-18613-0-79": "projects/cos-cloud/global/images/cos-stable-117-18613-0-79",
		}
	}

	if obj.MachineTypeMap == nil {
		obj.MachineTypeMap = map[string]string{
			"datumcloud/d1-standard-2": "n2-standard-2",
		}
	}
}

// +k8s:deepcopy-gen=true

type WebhookServerConfig struct {
	// Host is the address that the server will listen on.
	// Defaults to "" - all addresses.
	Host string `json:"host"`

	// Port is the port number that the server will serve.
	// +default=9443
	Port int `json:"port"`

	// TLS is the TLS configuration for the webhook server, allowing configuration
	// of what path to find a certificate and key in, and what file names to use.
	//
	// The CertDir field will be defaulted to <temp-dir>/k8s-webhook-server/serving-certs.
	TLS TLSConfig `json:"tls"`

	// ClientCAName is the CA certificate name which server used to verify remote(client)'s certificate.
	// Defaults to "", which means server does not verify client's certificate.
	ClientCAName string `json:"clientCAName"`
}

func SetDefaults_WebhookServerConfig(obj *WebhookServerConfig) {
	if obj.TLS.CertDir == "" {
		obj.TLS.CertDir = filepath.Join(os.TempDir(), "k8s-webhook-server", "serving-certs")
	}
}

func (c *WebhookServerConfig) Options(ctx context.Context, secretsClient client.Client) webhook.Options {
	opts := webhook.Options{
		Host:     c.Host,
		Port:     c.Port,
		CertDir:  c.TLS.CertDir,
		CertName: c.TLS.CertName,
		KeyName:  c.TLS.KeyName,
	}

	if secretRef := c.TLS.SecretRef; secretRef != nil {
		opts.TLSOpts = c.TLS.Options(ctx, secretsClient)
	}

	return opts
}

// +k8s:deepcopy-gen=true

type MetricsServerConfig struct {
	// SecureServing enables serving metrics via https.
	// +default=true
	SecureServing *bool `json:"secureServing,omitempty"`

	// BindAddress is the bind address for the metrics server.
	// Use :8443 for HTTPS or :8080 for HTTP
	//
	// Set this to "0" to disable the metrics server.
	// +default="0"
	BindAddress string `json:"bindAddress"`

	// TLS is the TLS configuration for the metrics server, allowing configuration
	// of what path to find a certificate and key in, and what file names to use.
	//
	// The CertDir field will be defaulted to <temp-dir>/k8s-metrics-server/serving-certs.
	TLS TLSConfig `json:"tls"`
}

func SetDefaults_MetricsServerConfig(obj *MetricsServerConfig) {
	if len(obj.TLS.CertDir) == 0 {
		obj.TLS.CertDir = filepath.Join(os.TempDir(), "k8s-metrics-server", "serving-certs")
	}
}

func (c *MetricsServerConfig) Options(ctx context.Context, secretsClient client.Client) metricsserver.Options {
	opts := metricsserver.Options{
		SecureServing: *c.SecureServing,
		BindAddress:   c.BindAddress,
		CertDir:       c.TLS.CertDir,
		CertName:      c.TLS.CertName,
		KeyName:       c.TLS.KeyName,
	}

	if *c.SecureServing {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.19.1/pkg/metrics/filters#WithAuthenticationAndAuthorization
		opts.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	if secretRef := c.TLS.SecretRef; secretRef != nil {
		opts.TLSOpts = c.TLS.Options(ctx, secretsClient)
	}

	return opts
}

// +k8s:deepcopy-gen=true

type TLSConfig struct {
	// SecretRef is a reference to a secret that contains the server key and
	// certificate. If provided, CertDir will be ignored, and CertName and KeyName
	// will be used as key names in the secret data.
	//
	// Note: This option is not currently recommended for production, as the secret
	// will be read from the API on every request.
	SecretRef *corev1.ObjectReference `json:"secretRef,omitempty"`

	// CertDir is the directory that contains the server key and certificate.
	// Default depends on the parent config these settings are contained in.
	CertDir string `json:"certDir"`

	// CertName is the server certificate name.
	//
	// Note: This option is only used when TLSOpts does not set GetCertificate.
	// +default="tls.crt"
	CertName string `json:"certName"`

	// KeyName is the server key name.
	//
	// Note: This option is only used when TLSOpts does not set GetCertificate.
	// +default="tls.key"
	KeyName string `json:"keyName"`
}

func (c *TLSConfig) Options(ctx context.Context, secretsClient client.Client) []func(*tls.Config) {
	var tlsOpts []func(*tls.Config)

	if secretRef := c.SecretRef; secretRef != nil {
		tlsOpts = append(tlsOpts, func(c *tls.Config) {
			logger := ctrl.Log.WithName("webhook-tls-client")
			c.GetCertificate = func(clientHello *tls.ClientHelloInfo) (*tls.Certificate, error) {
				logger.Info("getting certificate")

				// Look at https://github.com/cert-manager/cert-manager/blob/master/pkg/server/tls/dynamic_source.go

				// TODO(jreese) caching & background refresh

				var secret corev1.Secret
				secretObjectKey := types.NamespacedName{
					Name:      secretRef.Name,
					Namespace: secretRef.Namespace,
				}
				if err := secretsClient.Get(ctx, secretObjectKey, &secret); err != nil {
					return nil, fmt.Errorf("failed to get secret: %w", err)
				}

				cert, err := tls.X509KeyPair(secret.Data["tls.crt"], secret.Data["tls.key"])
				if err != nil {
					return nil, fmt.Errorf("failed to parse certificate: %w", err)
				}

				return &cert, nil
			}
		})
	}

	return tlsOpts
}

// +k8s:deepcopy-gen=true

type DownstreamResourceManagementConfig struct {
	// KubeconfigPath is the path to the kubeconfig file to use when managing
	// downstream resources. When not provided, the operator will use the
	// in-cluster config.
	KubeconfigPath string `json:"kubeconfigPath"`

	// ProviderConfigStrategy is the strategy to use to identify the ProviderConfig
	// to use when managing downstream resources.
	ProviderConfigStrategy ProviderConfigStrategy `json:"providerConfigStrategy"`

	// ManagedResourceLabels is a set of labels to apply to all resources managed
	// by the operator. These labels will be used to filter downstream resources
	// such as Crossplane types and anchor entities.
	ManagedResourceLabels map[string]string `json:"managedResourceLabels,omitempty"`
}

// +k8s:deepcopy-gen=true

type ProviderConfigStrategy struct {

	// Name can be used to program resources with a single provider config.
	Single SingleProviderConfigStrategy `json:"single,omitempty"`

	// DiscoveryClusterName will result in the operator to use the discovery cluster
	// name as the provider config name.
	Discovery DiscoveryProviderConfigStrategy `json:"discovery,omitempty"`
}

func (c *GCPProvider) GetProviderConfigName(clusterName string) string {
	switch c.Discovery.Mode {
	case providers.ProviderSingle:
		if c.DownstreamResourceManagement.ProviderConfigStrategy.Single.Name == "" {
			panic("single provider config strategy name is required")
		}
		return c.DownstreamResourceManagement.ProviderConfigStrategy.Single.Name
	default:
		if c.DownstreamResourceManagement.ProviderConfigStrategy.Discovery.Prefix == "" {
			return clusterName
		}
		return c.DownstreamResourceManagement.ProviderConfigStrategy.Discovery.Prefix + strings.TrimPrefix(clusterName, "/")
	}
}

// +k8s:deepcopy-gen=true

type SingleProviderConfigStrategy struct {
	// Name is the name of the provider config to use when managing downstream resources.
	Name string `json:"name,omitempty"`
}

// +k8s:deepcopy-gen=true

type DiscoveryProviderConfigStrategy struct {
	// Prefix will be added to the cluster name when locating the provider config.
	Prefix string `json:"prefix,omitempty"`
}

func (c *DownstreamResourceManagementConfig) RestConfig() (*rest.Config, error) {
	if c.KubeconfigPath == "" {
		return ctrl.GetConfig()
	}

	return clientcmd.BuildConfigFromFlags("", c.KubeconfigPath)
}

// +k8s:deepcopy-gen=true

type DiscoveryConfig struct {
	// Mode is the mode that the operator should use to discover clusters.
	//
	// +default="single"
	Mode providers.Provider `json:"mode"`

	// InternalServiceDiscovery will result in the operator to connect to internal
	// service addresses for projects.
	InternalServiceDiscovery bool `json:"internalServiceDiscovery"`

	// DiscoveryKubeconfigPath is the path to the kubeconfig file to use for
	// project discovery. When not provided, the operator will use the in-cluster
	// config.
	DiscoveryKubeconfigPath string `json:"discoveryKubeconfigPath"`

	// DiscoveryContext is the context to use for discovery. When not provided,
	// the operator will use the current-context in the kubeconfig file..
	DiscoveryContext string `json:"discoveryContext"`

	// ProjectKubeconfigPath is the path to the kubeconfig file to use as a
	// template when connecting to project control planes. When not provided,
	// the operator will use the in-cluster config.
	ProjectKubeconfigPath string `json:"projectKubeconfigPath"`

	// LabelSelector is an optional selector to filter projects based on labels.
	// When provided, only projects matching this selector will be reconciled.
	LabelSelector *metav1.LabelSelector `json:"labelSelector,omitempty"`
}

func (c *DiscoveryConfig) DiscoveryRestConfig() (*rest.Config, error) {
	if c.DiscoveryKubeconfigPath == "" {
		return ctrl.GetConfig()
	}

	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: c.DiscoveryKubeconfigPath},
		&clientcmd.ConfigOverrides{ClusterInfo: clientcmdapi.Cluster{}, CurrentContext: c.DiscoveryContext}).ClientConfig()
}

func (c *DiscoveryConfig) ProjectRestConfig() (*rest.Config, error) {
	if c.ProjectKubeconfigPath == "" {
		return ctrl.GetConfig()
	}

	return clientcmd.BuildConfigFromFlags("", c.ProjectKubeconfigPath)
}

func init() {
	SchemeBuilder.Register(&GCPProvider{})
}
