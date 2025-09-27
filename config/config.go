package config

import (
	"encoding/json"
	"os"

	"github.com/containernetworking/cni/pkg/types"
)

const (
	DefaultSubnetFile = "/var/lib/simple-cni/subnets.json"
	DefaultBridgeName = "simple-cni0"
)

type SubnetConf struct {
	Subnet string `json:"subnet"` // 如果 subnet = "10.244.0.0/24"，那么插件可以从 10.244.0.1 ~ 10.244.0.254 中选一个未被使用的 IP 分配给新容器。
	Bridge string `json:"bridge"` // 桥接接口名称
}

type PluginConf struct {
	types.NetConf

	RuntimeConf *struct {
		Config map[string]any `json:"config"`
	} `json:"runtime,omitempty"`

	Args *struct {
		Args map[string]any `json:"cni"`
	} `json:"args"`

	DataDir string `json:"dataDir"`
}

type CNIConf struct {
	SubnetConf
	PluginConf
}

// loadSubnetConfig 从默认路径加载子网配置文件
func loadSubnetConfig() (*SubnetConf, error) {
	data, err := os.ReadFile(DefaultSubnetFile)
	if err != nil {
		return nil, err
	}

	config := &SubnetConf{}
	if err := json.Unmarshal(data, config); err != nil {
		return nil, err
	}

	return config, nil
}

func StoreSubnetConfig(config *SubnetConf) error {
	data, err := json.Marshal(config)
	if err != nil {
		return err
	}

	return os.WriteFile(DefaultSubnetFile, data, 0644)
}

func parsePluginConfig(data []byte) (*PluginConf, error) {
	config := &PluginConf{}
	if err := json.Unmarshal(data, config); err != nil {
		return nil, err
	}
	return config, nil
}

func LoadCNIConfig(stdin []byte) (*CNIConf, error) {
	pluginConf, err := parsePluginConfig(stdin)
	if err != nil {
		return nil, err
	}

	subnetConf, err := loadSubnetConfig()
	if err != nil {
		return nil, err
	}

	return &CNIConf{
		SubnetConf: *subnetConf,
		PluginConf: *pluginConf,
	}, nil
}
