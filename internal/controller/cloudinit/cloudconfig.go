package cloudinit

import (
	"fmt"
	"strconv"

	"github.com/coreos/butane/base/v0_5"
	"github.com/coreos/butane/config/common"
	"github.com/coreos/butane/config/flatcar/v1_1"
	"gopkg.in/yaml.v3"
	"k8s.io/utils/ptr"
)

type CloudConfig struct {
	Hostname             string      `yaml:"hostname,omitempty"`
	PreserveHostname     *bool       `yaml:"preserve_hostname,omitempty"`
	RunCmd               []string    `yaml:"runcmd,omitempty"`
	WriteFiles           []WriteFile `yaml:"write_files,omitempty"`
	FSSetup              []FSSetup   `yaml:"fs_setup,omitempty"`
	Mounts               []string    `yaml:"mounts,omitempty"`
	SecretsParameterName string      `yaml:"secretsParameterName,omitempty"`
	AWSRegion            string      `yaml:"awsRegion,omitempty"`
}

func (c CloudConfig) Generate() ([]byte, error) {
	return yaml.Marshal(c)
}

// ToButane converts CloudConfig to ButaneConfig using Flatcar
func (c CloudConfig) ToButane() (*ButaneConfig, error) {
	config := v1_1.Config{
		Config: v0_5.Config{
			Variant: "flatcar",
			Version: "1.1.0",
		},
	}

	// Handle hostname
	if c.Hostname != "" {
		hostnameContent := c.Hostname
		mode := 0644
		file := v0_5.File{
			Path: "/etc/hostname",
			Mode: &mode,
			Contents: v0_5.Resource{
				Inline: &hostnameContent,
			},
			Overwrite: ptr.To(true),
		}
		config.Storage.Files = append(config.Storage.Files, file)
	}

	// Convert WriteFiles to Butane storage files
	for _, wf := range c.WriteFiles {
		file, err := convertWriteFileToButaneFile(wf)
		if err != nil {
			return nil, fmt.Errorf("failed to convert write file %s: %w", wf.Path, err)
		}
		config.Storage.Files = append(config.Storage.Files, file)
	}

	// Add static files
	mode := 0644
	staticFiles := []v0_5.File{
		{
			Path: "/etc/tmpfiles.d/10-kubelet-prep.conf",
			Mode: &mode,
			Contents: v0_5.Resource{
				Inline: func() *string {
					s := `# Remove kubeadm init concerns
r /etc/systemd/system/kubelet.service.d/10-kubeadm.conf
r /etc/sysconfig/kubelet`
					return &s
				}(),
			},
		},
		{
			Path: "/etc/systemd/system/kubelet.service.d/10-kubelet-args.conf",
			Mode: &mode,
			Contents: v0_5.Resource{
				Inline: func() *string {
					s := `[Service]
ExecStart=
ExecStart=/opt/bin/kubelet --config /var/lib/kubelet/config.yaml --cloud-provider=""`
					return &s
				}(),
			},
		},
		{
			Path: "/var/lib/kubelet/config.yaml",
			Mode: &mode,
			Contents: v0_5.Resource{
				Inline: func() *string {
					s := `apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
containerRuntimeEndpoint: unix:///run/containerd/containerd.sock
authentication:
  webhook:
    enabled: false
authorization:
  mode: AlwaysAllow
enableServer: false
podCIDR: 10.42.0.0/24
staticPodPath: /etc/kubernetes/manifests
cgroupDriver: systemd`
					return &s
				}(),
			},
		},
	}
	config.Storage.Files = append(config.Storage.Files, staticFiles...)

	// Conditionally add systemd unit to run script with a parameter
	if c.SecretsParameterName != "" && c.AWSRegion != "" {
		execStartCmd := fmt.Sprintf("/etc/secrets/populate_secrets_from_ssm.sh %s %s", c.SecretsParameterName, c.AWSRegion)

		unitContents := fmt.Sprintf(`[Unit]
Description=Run secrets population script
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=%s

[Install]
WantedBy=multi-user.target`, execStartCmd)

		newUnit := v0_5.Unit{
			Name:     "populate-secrets.service",
			Enabled:  ptr.To(true),
			Contents: &unitContents,
		}
		config.Systemd.Units = append(config.Systemd.Units, newUnit)
	}

	return &ButaneConfig{Config: config}, nil
}

// convertWriteFileToButaneFile converts a CloudConfig WriteFile to a Butane File
func convertWriteFileToButaneFile(wf WriteFile) (v0_5.File, error) {
	file := v0_5.File{
		Path: wf.Path,
	}

	// Handle file mode/permissions
	if wf.Permissions != "" {
		mode, err := strconv.ParseInt(wf.Permissions, 8, 32)
		if err != nil {
			return file, fmt.Errorf("invalid permissions format: %w", err)
		}
		intMode := int(mode)
		file.Mode = &intMode
	}

	// Handle content encoding
	switch wf.Encoding {
	case "b64", "base64":
		// For Butane, we pass the base64 content as-is with data URI
		source := fmt.Sprintf("data:;base64,%s", wf.Content)
		file.Contents = v0_5.Resource{
			Source: &source,
		}
	case "":
		// Plain text content - Butane will handle encoding
		file.Contents = v0_5.Resource{
			Inline: &wf.Content,
		}
	default:
		return file, fmt.Errorf("unsupported encoding: %s", wf.Encoding)
	}

	return file, nil
}

type WriteFile struct {
	Encoding    string `yaml:"encoding"`
	Content     string `yaml:"content"`
	Owner       string `yaml:"owner"`
	Path        string `yaml:"path"`
	Permissions string `yaml:"permissions"`
}

type FSSetup struct {
	Label      string `yaml:"label"`
	Filesystem string `yaml:"filesystem"`
	Device     string `yaml:"device"`
}

// ButaneConfig wraps the Butane v1_1.Config
type ButaneConfig struct {
	Config v1_1.Config
}

// Generate returns the Butane YAML representation
func (b ButaneConfig) Generate() ([]byte, error) {
	return yaml.Marshal(b.Config)
}

// ToIgnitionJSON converts the Butane config to Ignition JSON
func (b ButaneConfig) ToIgnitionJSON() ([]byte, error) {
	// Convert Butane config to Ignition config using the v1_1 specific method
	butaneYAML, err := b.Generate()
	if err != nil {
		return nil, fmt.Errorf("failed to generate Butane YAML: %w", err)
	}

	ignitionConfig, rpt, err := v1_1.ToIgn3_4Bytes(butaneYAML, common.TranslateBytesOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to translate Butane to Ignition: %w. Report: %v", err, rpt)
	}
	if rpt.IsFatal() {
		return nil, fmt.Errorf("failed to translate Butane to Ignition: %v", rpt)
	}

	return ignitionConfig, nil
}
