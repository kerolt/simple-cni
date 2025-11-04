// 作为 CNI 插件的守护进程，运行在节点上的控制器（Controller），通过监听 Kubernetes Node 对象的变化，自动维护本地路由表（以及可选的 iptables 规则），为基于 PodCIDR 的简单三层网络模型 提供支持。
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"net"
	"os"

	"github.com/kerolt/simple-cni/pkg/bridge"
	myconf "github.com/kerolt/simple-cni/pkg/config"

	"github.com/containernetworking/plugins/pkg/ip"
	"github.com/coreos/go-iptables/iptables"
	"github.com/vishvananda/netlink"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/event"
	crlog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var (
	log = crlog.Log.WithName("daemon")
)

// 保存守护进程（daemon）的配置信息
type daemonConf struct {
	clusterCIDR    string // 集群 CIDR
	nodeName       string // 节点名称
	enableIptables bool   // 是否启用 iptables 规则
}

func (d *daemonConf) addFlags() {
	flag.StringVar(&d.clusterCIDR, "cluster-cidr", "", "Cluster CIDR")
	flag.StringVar(&d.nodeName, "node-name", "", "Node Name")
	flag.BoolVar(&d.enableIptables, "enable-iptables", false, "Enable iptables")
}

// 解析并验证配置参数
func (d *daemonConf) validConfig() error {
	if _, _, err := net.ParseCIDR(d.clusterCIDR); err != nil {
		return err
	}

	if len(d.nodeName) == 0 {
		d.nodeName = os.Getenv("NODE_NAME")
		if len(d.nodeName) == 0 {
			return fmt.Errorf("node name is empty")
		}
	}

	return nil
}

// 负责网络资源的创建与管理
type reconciler struct {
	client       client.Client
	conf         *daemonConf
	clusterCIDR  *net.IPNet
	hostLink     netlink.Link
	routes       map[string]netlink.Route
	subnetConfig *myconf.SubnetConf
}

func (r *reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	result := reconcile.Result{}
	nodes := &corev1.NodeList{}
	if err := r.client.List(ctx, nodes); err != nil {
		return result, err
	}

	// 当前集群里（除本节点外）其它每个节点的 Pod 网段应该对应的一条路由
	routes := make(map[string]netlink.Route)

	for _, node := range nodes.Items {
		// 跳过自身节点
		if node.Name == r.conf.nodeName {
			continue
		}

		// 跳过还未分配 PodCIDR 的节点
		if len(node.Spec.PodCIDR) == 0 {
			continue
		}

		_, podCIDR, err := net.ParseCIDR(node.Spec.PodCIDR)
		if err != nil {
			return result, err
		}

		nodeIP, err := getNodeInternalIP(&node)
		if err != nil {
			log.Error(err, "failed to get %s's host", node.Name)
			continue
		}

		// Dst 目标网段为改节点的 Pod 子网
		// Gw 下一跳为该节点的 InternalIP
		route := netlink.Route{
			Dst:       podCIDR,
			Gw:        nodeIP,
			LinkIndex: r.hostLink.Attrs().Index,
		}

		routes[podCIDR.String()] = route

		// 更新路由表
		if curRoute, ok := r.routes[podCIDR.String()]; ok {
			if isRouteEqual(curRoute, route) {
				continue
			}
			if err := r.replaceRoute(route); err != nil {
				return result, err
			}
		} else {
			if err := r.addRoute(route); err != nil {
				return result, err
			}
		}
	}

	// 删去过时的数据
	for cidr, route := range r.routes {
		if _, ok := routes[cidr]; !ok {
			if err := r.delRoute(route); err != nil {
				return result, err
			}
		}
	}

	return result, nil
}

func (r *reconciler) replaceRoute(route netlink.Route) error {
	if err := netlink.RouteReplace(&route); err != nil {
		log.Error(err, "replace route failed. %s: %v", route.String(), err)
		return fmt.Errorf("replace route failed: %v", err)
	}

	r.routes[route.Dst.String()] = route
	log.Info("replace route: %s", route.String())
	return nil
}

func (r *reconciler) addRoute(route netlink.Route) error {
	if err := netlink.RouteAdd(&route); err != nil {
		log.Error(err, "add route failed. dst: %s, gw: %s, index: %d", route.Dst, route.Gw, route.LinkIndex)
		return fmt.Errorf("add route %s: %v", route.String(), err)
	}

	r.routes[route.Dst.String()] = route
	log.Info("add route. dst: %s, gw: %s, index: %d", route.Dst, route.Gw, route.LinkIndex)
	return nil
}

func (r *reconciler) delRoute(route netlink.Route) error {
	if err := netlink.RouteDel(&route); err != nil {
		log.Error(err, "delete route failed. dst: %s, gw: %s, index: %d", route.Dst, route.Gw, route.LinkIndex)
		return fmt.Errorf("delete route %s: %v", route.String(), err)
	}
	delete(r.routes, route.Dst.String())
	log.Info("delete route. dst: %s, gw: %s, index: %d", route.Dst, route.Gw, route.LinkIndex)
	return nil
}

// 在程序启动时完成本节点网络基础设施的一次性配置，并为后续的路由同步（Reconcile）准备好上下文状态
func newReconciler(conf *daemonConf, mgr manager.Manager) (*reconciler, error) {
	_, clusterCIDR, err := net.ParseCIDR(conf.clusterCIDR)
	if err != nil {
		return nil, err
	}

	// 获取本节点node对象
	node := &corev1.Node{}
	if err := mgr.GetAPIReader().Get(context.TODO(), types.NamespacedName{Name: conf.nodeName}, node); err != nil {
		return nil, err
	}

	hostIP, err := getNodeInternalIP(node)
	if err != nil {
		return nil, fmt.Errorf("failed to get host ip for node %s", conf.nodeName)
	}

	// 解析本节点的pod CIDR
	_, nodeCIDR, err := net.ParseCIDR(node.Spec.PodCIDR)
	if err != nil {
		return nil, err
	}

	// 生成并持久化 subnet.json
	subnetConf := &myconf.SubnetConf{
		Subnet: nodeCIDR.String(),
		Bridge: myconf.DefaultBridgeName,
	}
	if err := myconf.StoreSubnetConfig(subnetConf); err != nil {
		return nil, err
	}

	linkList, err := netlink.LinkList()
	if err != nil {
		return nil, err
	}

	// 定位本机主网卡
	var hostLink netlink.Link
	for _, link := range linkList {
		flag := false
		if link.Attrs() != nil {
			addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
			if err != nil {
				return nil, err
			}
			for _, addr := range addrs {
				if addr.IP.Equal(hostIP) {
					hostLink = link
					flag = true
				}
			}
		}
		if flag {
			break
		}
	}
	if hostLink == nil {
		return nil, fmt.Errorf("failed to get host link device")
	}

	log.Info("get host link successful, name: %s, index: %s", hostLink.Attrs().Name, hostLink.Attrs().Index)

	// 创建网桥设备，网桥的 IP 通常是 PodCIDR 的第一个可用 IP
	if _, err := bridge.CreateBridge(subnetConf.Bridge, 1500, &net.IPNet{IP: ip.NextIP(nodeCIDR.IP), Mask: nodeCIDR.Mask}); err != nil {
		return nil, err
	}

	// 如果启用了 iptables
	if conf.enableIptables {
		if err := addIPTables(subnetConf.Bridge, hostLink.Attrs().Name, subnetConf.Subnet); err != nil {
			return nil, err
		}
		log.Info("set iptables successful")
	}

	routes := make(map[string]netlink.Route)
	routeList, err := netlink.RouteList(hostLink, netlink.FAMILY_V4)
	if err != nil {
		return nil, err
	}

	// 把当前宿主上存在、且其目的网段落在 clusterCIDR（集群网段）内的路由收集到 routes map
	for _, route := range routeList {
		if route.Dst != nil && route.Dst.String() != nodeCIDR.String() && clusterCIDR.Contains(route.Dst.IP) {
			routes[route.Dst.String()] = route
		}
	}

	return &reconciler{
		client:       mgr.GetClient(),
		clusterCIDR:  clusterCIDR,
		hostLink:     hostLink,
		routes:       routes,
		conf:         conf,
		subnetConfig: subnetConf,
	}, nil
}

func addIPTables(bridgeName, hostDeviceName, nodeCIDR string) error {
	ipt, err := iptables.NewWithProtocol(iptables.ProtocolIPv4)
	if err != nil {
		return err
	}

	// 凡是进入本机、入口网卡是创建的 CNI 网桥的转发流量允许被继续转发（不被默认策略 DROP）
	if err := ipt.AppendUnique("filter", "FORWARD", "-i", bridgeName, "-j", "ACCEPT"); err != nil {
		return err
	}

	// 允许来自主机物理接口（例如 eth0）的转发包被继续处理
	if err := ipt.AppendUnique("filter", "FORWARD", "-i", hostDeviceName, "-j", "ACCEPT"); err != nil {
		return err
	}

	if err := ipt.AppendUnique("nat", "POSTROUTING", "-s", nodeCIDR, "-j", "MASQUERADE"); err != nil {
		return err
	}

	return nil
}

// 从 Kubernetes Node 对象中提取节点的内部 IP 地址
func getNodeInternalIP(node *corev1.Node) (net.IP, error) {
	if node == nil {
		return nil, fmt.Errorf("empty node")
	}

	var ip net.IP
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			ip = net.ParseIP(addr.Address)
			break
		}
	}

	if len(ip) == 0 {
		return nil, fmt.Errorf("node %s ip is nil", node.Name)
	}

	return ip, nil
}

func isRouteEqual(a, b netlink.Route) bool {
	return a.Dst.IP.Equal(b.Dst.IP) && a.Gw.Equal(b.Gw) && bytes.Equal(a.Dst.Mask, b.Dst.Mask) && a.LinkIndex == b.LinkIndex
}

func main() {
	crlog.SetLogger(zap.New())

	conf := &daemonConf{}
	conf.addFlags()
	flag.Parse()
	if err := conf.validConfig(); err != nil {
		log.Error(err, "faild to parse config")
		os.Exit(1)
	}

	if err := runController(conf); err != nil {
		log.Error(err, "faild to run controller")
	}
}

func runController(conf *daemonConf) error {
	mgr, err := manager.New(config.GetConfigOrDie(), manager.Options{})
	if err != nil {
		log.Error(err, "couldn't create manager")
		return err
	}

	reconciler, err := newReconciler(conf, mgr)
	if err != nil {
		return err
	}
	log.Info("create reconciler successful")

	err = builder.ControllerManagedBy(mgr).For(&corev1.Node{}).WithEventFilter(predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			old, ok := e.ObjectOld.(*corev1.Node)
			if !ok {
				return true
			}

			new, ok := e.ObjectNew.(*corev1.Node)
			if !ok {
				return true
			}

			return old.Spec.PodCIDR != new.Spec.PodCIDR
		},
	}).Complete(reconciler)

	if err != nil {
		log.Error(err, "failed to create controller")
		return err
	}

	return mgr.Start(signals.SetupSignalHandler())
}
