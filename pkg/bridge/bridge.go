package bridge

import (
	"fmt"
	"net"
	"syscall"

	types "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/plugins/pkg/ip"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
)

// CreateBridge 创建网桥设备
func CreateBridge(bridgeName string, mtu int, gateway *net.IPNet) (netlink.Link, error) {
	// 如果名称为 bridgeName 的设备已经存在，直接返回它
	if link, _ := netlink.LinkByName(bridgeName); link != nil {
		return link, nil
	}

	bridge := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name:   bridgeName,
			MTU:    mtu,
			TxQLen: -1,
		},
	}

	// 尝试添加设备
	if err := netlink.LinkAdd(bridge); err != nil && err != syscall.EEXIST {
		return nil, err
	}

	// 将设备作为网关添加 IP 地址
	link, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return nil, err
	}
	if err := netlink.AddrAdd(link, &netlink.Addr{IPNet: gateway}); err != nil {
		return nil, err
	}

	// 启动设备，等价于 ip link set br0 up
	if err := netlink.LinkSetUp(link); err != nil {
		return nil, err
	}

	return link, nil
}

// SetupVeth 创建并配置容器的 veth
//  1. 在容器网络命名空间中创建一个 veth pair（一端在容器内，一端在宿主机）
//  2. 为容器端 veth 配置 IP 地址（podIP）和默认路由（指向 gateway）
//  3. 将宿主机端 veth 插入到指定的桥接设备 bridge 中（如 cni0）
//  4. 实现容器 ↔ 宿主机 ↔ 外部网络的连通性
func SetupVeth(netns ns.NetNS, bridge netlink.Link, mtu int, ifName string, podIP *net.IPNet, gateway net.IP) error {
	hostIf := &types.Interface{}
	err := netns.Do(func(hostNS ns.NetNS) error {
		// 创建 veth pair，一根虚拟网线，一头在容器，一头在宿主机
		hostVeth, containerVeth, err := ip.SetupVeth(ifName, mtu, "", hostNS)
		if err != nil {
			return err
		}

		hostIf.Name = hostVeth.Name

		// 为 container veth 设置 IP
		containerLink, err := netlink.LinkByName(containerVeth.Name)
		if err != nil {
			return err
		}
		if err := netlink.AddrAdd(containerLink, &netlink.Addr{IPNet: podIP}); err != nil {
			return err
		}

		// 设置容器的 veth 为 up 状态
		if err := netlink.LinkSetUp(containerLink); err != nil {
			return err
		}

		// 设置路由
		if err := ip.AddDefaultRoute(gateway, containerLink); err != nil {
			return err
		}

		return nil
	})

	if err != nil {
		return err
	}

	// 宿主机侧的 veth = 接入点，通常会接到一个 bridge 上
	hostVeth, err := netlink.LinkByName(hostIf.Name)
	if err != nil {
		return fmt.Errorf("failed to lookup %q: %v", hostIf.Name, err)
	}
	if hostVeth == nil {
		return fmt.Errorf("host veth is null")
	}

	// 将主机 veth 与网桥连到一起
	if err := netlink.LinkSetMaster(hostVeth, bridge); err != nil {
		return fmt.Errorf("failed to connect %q to bridge %v: %v", hostVeth.Attrs().Name, bridge.Attrs().Name, err)
	}

	return nil
}

// DelVeth 删除指定的 veth。对于 veth pair，删除其中一端时，内核会自动清理另一端。
func DelVeth(netns ns.NetNS, ifName string) error {
	return netns.Do(func(ns.NetNS) error {
		link, err := netlink.LinkByName(ifName)
		if err != nil {
			return err
		}
		return netlink.LinkDel(link)
	})
}

// CheckVeth 检查容器内的 veth 是否存在且配置了指定的 IP
func CheckVeth(netns ns.NetNS, ifName string, ip net.IP) error {
	return netns.Do(func(ns.NetNS) error {
		link, err := netlink.LinkByName(ifName)
		if err != nil {
			return err
		}

		addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
		if err != nil {
			return err
		}

		for _, addr := range addrs {
			if addr.IP.Equal(ip) {
				return nil
			}
		}

		return fmt.Errorf("failed to find ip %s for %s", ip, ifName)
	})
}
