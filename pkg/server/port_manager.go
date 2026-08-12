package server

import (
	"net"
	"sync"

	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

type Target struct {
	Name      string
	Namespace string
	Port      int
}

type PortInformation struct {
	Target      Target
	Listener    Listener
	Connections []net.Conn
}

type PortManager struct {
	portMap        map[int]*PortInformation
	reversePortMap map[Target]int
	mut            sync.Mutex

	cb func(*PortInformation)
}

func NewPortManager(cb func(*PortInformation)) *PortManager {
	return &PortManager{
		portMap:        map[int]*PortInformation{},
		reversePortMap: map[Target]int{},
		cb:             cb,
	}
}

func (pm *PortManager) AddTarget(name string, namespace string, port int) (*PortInformation, error) {
	pm.mut.Lock()
	defer pm.mut.Unlock()

	target := Target{
		Name:      name,
		Namespace: namespace,
		Port:      port,
	}

	port, ok := pm.reversePortMap[target]
	if ok {
		return pm.portMap[port], nil
	}

	listener, err := NewListener()
	if err != nil {
		return nil, err
	}
	port = listener.Port()
	downstream := &PortInformation{
		Target:   target,
		Listener: listener,
	}
	pm.portMap[port] = downstream
	pm.reversePortMap[target] = port

	go pm.startListener(downstream)
	return downstream, nil
}

func (pm *PortManager) RemoveTarget(name string, namespace string, port int) *PortInformation {
	pm.mut.Lock()
	defer pm.mut.Unlock()

	target := Target{
		Name:      name,
		Namespace: namespace,
		Port:      port,
	}

	port, ok := pm.reversePortMap[target]
	if !ok {
		return nil
	}
	downstream := pm.portMap[port]
	delete(pm.portMap, port)
	delete(pm.reversePortMap, target)
	// closing under the lock stops new connections from being tracked,
	// so Connections is stable once this returns
	_ = downstream.Listener.Close()
	return downstream
}

func (pm *PortManager) RemoveTargetForAllPorts(name string, namespace string) []*PortInformation {
	pm.mut.Lock()
	defer pm.mut.Unlock()

	var downstreams []*PortInformation
	for port, downstream := range pm.portMap {
		if downstream.Target.Name == name && downstream.Target.Namespace == namespace {
			delete(pm.portMap, port)
			delete(pm.reversePortMap, downstream.Target)
			_ = downstream.Listener.Close()
			downstreams = append(downstreams, downstream)
		}
	}
	return downstreams
}

func (pm *PortManager) startListener(downstream *PortInformation) {
	start := false
	for {
		conn, err := downstream.Listener.Accept()
		if err != nil {
			return
		}

		if !pm.track(downstream, conn) {
			// the target was removed after this conn was accepted, drop it
			// and let the client retry against the real endpoints
			_ = conn.Close()
			continue
		}

		key := cache.ObjectName{Namespace: downstream.Target.Namespace, Name: downstream.Target.Name}.String()
		port := downstream.Target.Port
		klog.InfoS("accept connection", "ep", key, "port", port)
		if !start {
			klog.InfoS("start forwarding", "ep", key, "port", port)
			go pm.cb(downstream)
			start = true
		}
	}
}

// track records the connection while the target is still registered
func (pm *PortManager) track(downstream *PortInformation, conn net.Conn) bool {
	pm.mut.Lock()
	defer pm.mut.Unlock()
	if pm.portMap[downstream.Listener.Port()] != downstream {
		return false
	}
	downstream.Connections = append(downstream.Connections, conn)
	return true
}
