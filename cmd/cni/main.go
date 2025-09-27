package main

import (
	"net"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	type100 "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/ns"
	bv "github.com/containernetworking/plugins/pkg/utils/buildversion"

	"github.com/kerolt/simple-cni/bridge"
	"github.com/kerolt/simple-cni/config"
	"github.com/kerolt/simple-cni/ipam"
	"github.com/kerolt/simple-cni/store"
)

const (
	pluginName = "simple-cni"
)

func main() {
	skel.PluginMainFuncs(skel.CNIFuncs{Add: cmdAdd, Del: cmdDel, Check: cmdCheck}, version.All, bv.BuildString(pluginName))
}

func setupIPAM(args *skel.CmdArgs) (*ipam.IPAM, *config.CNIConf, error) {
	// 加载 CNI 配置
	conf, err := config.LoadCNIConfig(args.StdinData)
	if err != nil {
		return nil, nil, err
	}

	// 加载持久化存储
	s, err := store.NewStore(conf.DataDir, conf.Name)
	if err != nil {
		return nil, nil, err
	}
	defer s.Close()

	// 创建 IPAM
	im, err := ipam.NewIPAM(conf, s)
	if err != nil {
		return nil, nil, err
	}

	return im, conf, nil
}

func cmdAdd(args *skel.CmdArgs) error {
	im, conf, err := setupIPAM(args)
	if err != nil {
		return err
	}

	// 获取网关并分配 IP 地址
	gateway := im.Gateway()
	podIP, err := im.AllocateIP(args.ContainerID, args.IfName)
	if err != nil {
		return err
	}

	// 创建并配置桥接设备，如果之前已经创建了，就使用创建好了的
	mtu := 1500
	br, err := bridge.CreateBridge(conf.Bridge, mtu, im.IPNet(gateway))
	if err != nil {
		return err
	}

	// 获取容器的网络命名空间
	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		return err
	}
	defer netns.Close()

	// 创建并配置 veth
	if err := bridge.SetupVeth(netns, br, mtu, args.IfName, im.IPNet(podIP), gateway); err != nil {
		return err
	}

	result := &type100.Result{
		IPs: []*type100.IPConfig{
			{
				Address: net.IPNet{IP: podIP, Mask: im.Mask()},
				Gateway: gateway,
			},
		},
	}

	return types.PrintResult(result, conf.CNIVersion)
}

func cmdDel(args *skel.CmdArgs) error {
	im, _, err := setupIPAM(args)
	if err != nil {
		return err
	}

	// 释放 IP 地址
	if err := im.ReleaseIP(args.ContainerID); err != nil {
		return err
	}

	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		return err
	}
	defer netns.Close()

	// 删除 veth
	return bridge.DelVeth(netns, args.IfName)
}

func cmdCheck(args *skel.CmdArgs) error {
	im, _, err := setupIPAM(args)
	if err != nil {
		return err
	}

	// 检查 IP 地址是否被分配
	podIP, err := im.CheckIP(args.ContainerID)
	if err != nil {
		return err
	}

	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		return err
	}
	defer netns.Close()

	return bridge.CheckVeth(netns, args.IfName, podIP)
}
