package cloudinit

import "gopkg.in/yaml.v3"

type CloudConfig struct {
	Hostname         string      `yaml:"hostname,omitempty"`
	PreserveHostname *bool       `yaml:"preserve_hostname,omitempty"`
	RunCmd           []string    `yaml:"runcmd,omitempty"`
	WriteFiles       []WriteFile `yaml:"write_files,omitempty"`
	FSSetup          []FSSetup   `yaml:"fs_setup,omitempty"`
	Mounts           []string    `yaml:"mounts,omitempty"`
}

func (c CloudConfig) Generate() ([]byte, error) {
	return yaml.Marshal(c)
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
