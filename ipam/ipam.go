package ipam

import (
	"errors"
	"fmt"
	"net"

	"github.com/kerolt/simple-cni/config"
	"github.com/kerolt/simple-cni/store"

	cip "github.com/containernetworking/plugins/pkg/ip"
)

var (
	ErrIPOverflow = errors.New("IP address overflow")
)

type IPAM struct {
	subnet  *net.IPNet   // IPAM 管理的网段
	gateway net.IP       // 默认网关 IP，一般分配给容器网络的第一个 IP
	store   *store.Store // 记录已经分配的 IP 信息
}

func NewIPAM(conf *config.CNIConf, store *store.Store) (*IPAM, error) {
	_, ipnet, err := net.ParseCIDR(conf.Subnet)
	if err != nil {
		return nil, err
	}

	ipam := &IPAM{
		subnet: ipnet,
		store:  store,
	}

	ipam.gateway, err = ipam.NextIP(ipnet.IP)
	if err != nil {
		return nil, err
	}

	return ipam, nil
}

// NextIP 计算给定 IP 的下一个 IP 地址，并确保它在子网范围内
func (ipam *IPAM) NextIP(ip net.IP) (net.IP, error) {
	next := cip.NextIP(ip)
	if !ipam.subnet.Contains(next) {
		return nil, ErrIPOverflow
	}
	return next, nil
}

func (ipam *IPAM) Mask() net.IPMask {
	return ipam.subnet.Mask
}

func (ipam *IPAM) Gateway() net.IP {
	return ipam.gateway
}

func (ipam *IPAM) GenIPNet(ip net.IP) *net.IPNet {
	return &net.IPNet{
		IP:   ip,
		Mask: ipam.Mask(),
	}
}

// AllocateIP 为指定容器分配一个尚未被使用的 IP 地址
//
//	ip 容器唯一标识符
//	ifName 接口名称
func (ipam *IPAM) AllocateIP(id, ifName string) (net.IP, error) {
	ipam.store.Lock()
	defer ipam.store.Unlock()

	if err := ipam.store.LoadData(); err != nil {
		return nil, err
	}

	// 检查该容器是否已经分配了 IP
	ip, ok := ipam.store.GetIPById(id)
	if ok {
		return ip, nil
	}

	// 如果之前还没分配，则从网关ip开始
	// 通常网关是 .1，比如 192.168.1.1，所以第一个可用 IP 可能是 .2
	lastIP := ipam.store.Last()
	if len(lastIP) == 0 {
		lastIP = ipam.gateway
	}

	currIP := make(net.IP, len(lastIP))
	copy(currIP, lastIP)
	for {
		nextIP, err := ipam.NextIP(currIP)

		// 如果 ip 溢出了并且上次不是从网关开始的，从头再来避免漏掉前面未分配的 ip
		if err == ErrIPOverflow && !lastIP.Equal(ipam.gateway) {
			currIP = ipam.gateway
			continue
		} else if err != nil {
			return nil, err
		}

		// 如果 nextIP 未分配过，那么就分配这个，并将其与 id、ifName 绑定
		if !ipam.store.Contain(nextIP) {
			err := ipam.store.Add(nextIP, id, ifName)
			return nextIP, err
		}

		// 如果分配过了，下一个
		currIP = nextIP

		// 如果又回到了和 lastIP 一样，说明可用 IP 已经分配完了
		if currIP.Equal(lastIP) {
			break
		}
	}

	return nil, fmt.Errorf("no available IP")
}

// ReleaseIP 收回容器 id 的 IP
func (ipam *IPAM) ReleaseIP(id string) error {
	ipam.store.Lock()
	defer ipam.store.Unlock()

	if err := ipam.store.LoadData(); err != nil {
		return err
	}

	return ipam.store.Del(id)
}

// 根据容器 ID，查询并返回它当前被分配的 IP 地址，查不到就返回 err
func (ipam *IPAM) CheckIP(id string) (net.IP, error) {
	ipam.store.Lock()
	defer ipam.store.Unlock()

	if err := ipam.store.LoadData(); err != nil {
		return nil, err
	}

	ip, ok := ipam.store.GetIPById(id)
	if !ok {
		return nil, fmt.Errorf("failed to find container %s 's ip", id)
	}

	return ip, nil
}
