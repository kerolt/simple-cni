// 持久化并协调管理 CNI 插件分配的网络信息（主要是 IP 和对应的容器/接口信息）
//
// 为什么需要 store
//  1. 防止 IP 冲突与丢失状态：CNI 插件在给容器分配 IP 时需要记录哪些 IP 已被分配、分配给哪个容器。如果只保存在内存，进程重启或机器重启后会丢失分配状态，可能导致重复分配同一 IP。store 把这些信息写到磁盘（/var/lib/cni/<network>.json）以便恢复。
//  2. 多进程/并发协调：在同一主机上可能有多个 CNI 操作同时进行，文件锁（go-filemutex）用于在修改这个数据文件时做同步，避免并发写入造成的数据损坏或竞争。
//  3. 实现基本的 IPAM 操作：store 提供读取（LoadData）、查询（Contain、GetIPByID、Last）、修改（Add、Del）和持久化（Store）等 API，便于上层插件逻辑实现分配、释放和恢复流程。
package store

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path"

	"github.com/alexflint/go-filemutex"
)

const (
	DefaultStoreDir = "/var/lib/simple-cni"
)

// CNI 插件通常会被多个进程（不同容器的配置/解绑操作）并发调用，多个进程可能同时访问同一个 network 的 data 文件（network.json）。如果没有进程间锁，两个进程同时写入会产生竞态（race）或写入不完整/损坏的 JSON 文件。
func newFileLock(lockPath string) (*filemutex.FileMutex, error) {
	fileInfo, err := os.Stat(lockPath)
	if err != nil {
		return nil, err
	}

	if fileInfo.IsDir() {
		lockPath = path.Join(lockPath, "lock")
	}

	fm, err := filemutex.New(lockPath)
	if err != nil {
		return nil, err
	}

	return fm, nil
}

// IfName 是容器内的网络接口名称（network interface name），例如 "eth0"、"eth1" 或自定义名。
//
// CNI 插件用它来在容器的网络命名空间中定位并配置/解绑对应的接口（创建 veth pair 时作为容器端的接口名，或解绑时用来查找接口）。
//
// 与 ContainerID 不同，ContainerID 标识容器本身，IfName 标识容器里的某个网络接口
type containerNetInfo struct {
	ContainerID string `json:"container_id"`
	IfName      string `json:"if_name"`
}

type data struct {
	IPs  map[string]containerNetInfo `json:"ips"`  // key 是 IP 地址，value 是对应的容器信息
	Last string                      `json:"last"` // 最近分配的 IP 地址
}

type Store struct {
	*filemutex.FileMutex
	dir      string
	data     *data
	dataFile string
}

func NewStore(storeDir, networkName string) (*Store, error) {
	if storeDir == "" {
		storeDir = DefaultStoreDir
	}

	dir := path.Join(storeDir, networkName)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err := os.Mkdir(dir, 0755); err != nil {
			return nil, err
		}
	}

	fl, err := newFileLock(dir)
	if err != nil {
		return nil, err
	}

	dataFile := path.Join(dir, networkName+".json")
	data := &data{
		IPs: make(map[string]containerNetInfo),
	}

	return &Store{fl, dir, data, dataFile}, nil
}

// LoadData 从 json 文件中读取数据到 s.data
func (s *Store) LoadData() error {
	data := &data{}
	raw, err := os.ReadFile(s.dataFile)
	if err != nil {
		if os.IsNotExist(err) {
			// 文件不存在则创建
			file, err := os.Create(s.dataFile)
			if err != nil {
				return err
			}
			defer file.Close()

			// 写入空的 JSON 对象
			_, err = file.Write([]byte("{}"))
			if err != nil {
				return err
			}
		} else {
			return err
		}
	} else {
		if err := json.Unmarshal(raw, &data); err != nil {
			return err
		}
	}

	if data.IPs == nil {
		data.IPs = make(map[string]containerNetInfo)
	}

	s.data = data
	return nil
}

// GetIPById 根据容器 ID 查找对应的 IP 地址
func (s *Store) GetIPById(id string) (net.IP, bool) {
	for ip, info := range s.data.IPs {
		if info.ContainerID == id {
			return net.ParseIP(ip), true
		}
	}
	return nil, false
}

// Last 返回最近分配的 IP 地址
func (s *Store) Last() net.IP {
	return net.ParseIP(s.data.Last)
}

// Save 将 s.data 保存到 json 文件中
func (s *Store) Save() error {
	raw, err := json.Marshal(s.data)
	if err != nil {
		return err
	}

	return os.WriteFile(s.dataFile, raw, 0644)
}

// Add 添加一个新的 IP 分配记录
func (s *Store) Add(ip net.IP, id, ifName string) error {
	if len(ip) == 0 {
		return fmt.Errorf("invalid IP")
	}

	s.data.IPs[ip.String()] = containerNetInfo{
		ContainerID: id,
		IfName:      ifName,
	}
	s.data.Last = ip.String()
	return s.Save()
}

// Del 根据容器 ID 删除一个 IP 分配记录
func (s *Store) Del(id string) error {
	for ip, info := range s.data.IPs {
		if info.ContainerID == id {
			delete(s.data.IPs, ip)
			return s.Save()
		}
	}
	return nil
}

// Contain 检查某个 IP 是否已经被分配
func (s *Store) Contain(ip net.IP) bool {
	_, ok := s.data.IPs[ip.String()]
	return ok
}
