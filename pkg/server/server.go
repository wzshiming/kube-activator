package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"net"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

const (
	scaleDeploymentKey = "scale-from-zero.zsm.io/deployment"

	// TODO: remove this when service supports selector with external process managing its endpoints
	cacheTargetSelectorKey = "cache.scale-from-zero.zsm.io"
)

type Server struct {
	ip        string
	clientset kubernetes.Interface

	serviceIndexer  cache.Indexer
	endpointIndexer cache.Indexer

	manager *PortManager
}

func NewServer(ip string, clientset kubernetes.Interface) *Server {
	return &Server{
		ip:        ip,
		clientset: clientset,
	}
}

func (s *Server) Run(ctx context.Context) error {
	s.manager = NewPortManager(s.scaleUp)

	err := s.watchService(ctx)
	if err != nil {
		return err
	}

	err = s.watchEndpoints(ctx)
	if err != nil {
		return err
	}
	return nil
}

func (s *Server) watchService(ctx context.Context) error {
	indexer, controller := cache.NewIndexerInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return s.clientset.CoreV1().Services(corev1.NamespaceAll).List(ctx, options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return s.clientset.CoreV1().Services(corev1.NamespaceAll).Watch(ctx, options)
			},
		},
		&corev1.Service{},
		0,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				svc := obj.(*corev1.Service)
				s.restoreSelector(ctx, svc)
			},
		},
		cache.Indexers{},
	)

	s.serviceIndexer = indexer
	go controller.Run(ctx.Done())

	return nil
}

func (s *Server) watchEndpoints(ctx context.Context) error {
	indexer, controller := cache.NewIndexerInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return s.clientset.CoreV1().Endpoints(corev1.NamespaceAll).List(ctx, options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return s.clientset.CoreV1().Endpoints(corev1.NamespaceAll).Watch(ctx, options)
			},
		},
		&corev1.Endpoints{},
		0,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				ep := obj.(*corev1.Endpoints)
				s.inject(ctx, ep)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				ep := newObj.(*corev1.Endpoints)
				s.inject(ctx, ep)
			},
			DeleteFunc: func(obj interface{}) {
				ep := obj.(*corev1.Endpoints)
				s.eject(ctx, ep)
			},
		},
		cache.Indexers{},
	)

	s.endpointIndexer = indexer
	go controller.Run(ctx.Done())
	return nil
}

func (s *Server) inject(ctx context.Context, ep *corev1.Endpoints) {
	key := cache.MetaObjectToName(ep).String()
	svcObj, exists, err := s.serviceIndexer.GetByKey(key)
	if err != nil {
		klog.ErrorS(err, "get service failed")
		return
	}
	var svc *corev1.Service
	if !exists {
		svc, err = s.clientset.CoreV1().Services(ep.Namespace).Get(ctx, ep.Name, metav1.GetOptions{})
		if err != nil {
			klog.ErrorS(err, "get service failed")
			return
		}
	} else {
		svc = svcObj.(*corev1.Service)
	}

	ports, ok := s.needInject(svc)
	if !ok {
		return
	}

	if len(ep.Subsets) == 0 {
		// inject
		klog.InfoS("injectEndpoint", "ep", key)
		err = s.injectEndpoint(ctx, ep, svc, ports)
		if err != nil {
			klog.ErrorS(err, "injectEndpoint failed")
			return
		}
	} else if ep.Subsets[0].Addresses != nil &&
		len(ep.Subsets[0].Addresses) > 0 &&
		ep.Subsets[0].Addresses[0].IP != s.ip &&
		len(ep.Subsets[0].Ports) > 0 {
		// forward
		klog.InfoS("forwardEndpoint", "ep", key)
		err = s.forwardEndpoint(ctx, ep, ports)
		if err != nil {
			klog.ErrorS(err, "forwardEndpoint failed")
			return
		}
	} else {
		// nothing to do
	}
}

func (s *Server) needInject(svc *corev1.Service) ([]corev1.ServicePort, bool) {
	if svc == nil {
		return nil, false
	}

	if svc.Annotations == nil {
		return nil, false
	}

	if name, ok := svc.Annotations[scaleDeploymentKey]; !ok || name == "" {
		return nil, false
	}
	if len(svc.Spec.Ports) == 0 {
		return nil, false
	}

	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		return nil, false
	}

	p := make([]corev1.ServicePort, 0, len(svc.Spec.Ports))
	for _, port := range svc.Spec.Ports {
		if port.Port == 0 {
			return nil, false
		}
		if port.Protocol != corev1.ProtocolTCP {
			return nil, false
		}
		p = append(p, port)
	}
	return p, true
}

// restore the service selector
func (s *Server) restoreSelector(ctx context.Context, svc *corev1.Service) {
	selectorStr := svc.Annotations[cacheTargetSelectorKey]
	if selectorStr == "" {
		return
	}

	key := cache.ObjectName{Namespace: svc.Namespace, Name: svc.Name}.String()
	sel := map[string]string{}
	err := json.Unmarshal([]byte(selectorStr), &sel)
	if err != nil {
		klog.ErrorS(err, "scaleTargetRefSelector unmarshal failed", "ep", key)
		return
	}
	if len(sel) == 0 {
		klog.ErrorS(nil, "scaleTargetRefSelector not found", "ep", key)
		return
	}

	svc = svc.DeepCopy()

	delete(svc.Annotations, cacheTargetSelectorKey)
	svc.Spec.Selector = sel
	_, err = s.clientset.CoreV1().Services(svc.Namespace).Update(ctx, svc, metav1.UpdateOptions{})
	if err != nil {
		klog.ErrorS(err, "scaleTargetRefSelector update failed", "ep", key)
		return
	}
}

// injectEndpoint injects the ip and port to the endpoints
// TODO: maybe we can use webhook to injectEndpoint this when the endpoints is empty
func (s *Server) injectEndpoint(ctx context.Context, ep *corev1.Endpoints, svc *corev1.Service, ports []corev1.ServicePort) error {
	eps := make([]corev1.EndpointSubset, 0, len(ports))
	for _, port := range ports {
		ds, err := s.manager.AddTarget(ep.Name, ep.Namespace, int(port.Port))
		if err != nil {
			return err
		}

		klog.InfoS("injectEndpoint",
			"ep", cache.MetaObjectToName(ep).String(),
			"port", port.Port,
			"listener", ds.Listener.Port())
		eps = append(eps, corev1.EndpointSubset{
			Addresses: []corev1.EndpointAddress{
				{
					IP: s.ip,
				},
			},
			Ports: []corev1.EndpointPort{
				{
					Name: port.Name,
					Port: int32(ds.Listener.Port()),
				},
			},
		})
	}
	ep.Subsets = eps

	_, err := s.clientset.CoreV1().Endpoints(ep.Namespace).Update(ctx, ep, metav1.UpdateOptions{})
	if err != nil {
		return err
	}

	// TODO: remove this
	klog.InfoS("modify service selector")
	selectorBytes, _ := json.Marshal(svc.Spec.Selector)
	svc.Annotations[cacheTargetSelectorKey] = string(selectorBytes)
	svc.Spec.Selector = nil
	_, err = s.clientset.CoreV1().Services(ep.Namespace).Update(ctx, svc, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	return nil
}

func (s *Server) eject(ctx context.Context, ep *corev1.Endpoints) error {
	pis := s.manager.RemoveTargetForAllPorts(ep.Name, ep.Namespace)
	for _, pi := range pis {
		klog.InfoS("ejectEndpoint",
			"ep", cache.MetaObjectToName(ep).String(),
			"port", pi.Target.Port,
			"listener", pi.Listener.Port(),
			"connections", len(pi.Connections),
		)
		err := pi.Listener.Close()
		if err != nil {
			klog.ErrorS(err, "ejectEndpoint failed")
		}
		for _, c := range pi.Connections {
			err = c.Close()
			if err != nil {
				klog.ErrorS(err, "ejectEndpoint failed")
			}
		}
	}
	return nil
}

// get the address from the endpoints
func (s *Server) getEndpointAddress(ep *corev1.Endpoints, ports []corev1.ServicePort, target *Target) (string, error) {
	for _, port := range ports {
		if int(port.Port) != target.Port {
			continue
		}

		for _, subset := range ep.Subsets {
			for _, p := range subset.Ports {
				if port.TargetPort.Type != intstr.Int {
					continue
				}
				if int(p.Port) != int(port.TargetPort.IntVal) {
					continue
				}
				return fmt.Sprintf("%s:%d", subset.Addresses[0].IP, p.Port), nil
			}
		}
	}
	return "", fmt.Errorf("address not found")
}

func (s *Server) forwardEndpoint(ctx context.Context, ep *corev1.Endpoints, ports []corev1.ServicePort) error {
	for _, port := range ports {
		ds := s.manager.RemoveTarget(ep.Name, ep.Namespace, int(port.Port))
		if ds == nil {
			continue
		}

		address, err := s.getEndpointAddress(ep, ports, &ds.Target)
		if err != nil {
			klog.ErrorS(err, "forwardEndpoint failed")
			continue
		}
		for _, c := range ds.Connections {
			conn, err := net.Dial("tcp", address)
			if err != nil {
				klog.ErrorS(err, "forwardEndpoint failed")
				continue
			}
			tunnel(c, conn)
		}

		klog.InfoS("forwardEndpoint",
			"ep", cache.MetaObjectToName(ep).String(),
			"port", port.Port,
			"listener", ds.Listener.Port(),
			"connections", len(ds.Connections),
		)
		err = ds.Listener.Close()
		if err != nil {
			klog.ErrorS(err, "forwardEndpoint failed")
			return err
		}
	}

	return nil

}

func tunnel(a, b net.Conn) {
	go io.Copy(a, b)
	go io.Copy(b, a)
}

func (s *Server) scaleUp(ds *PortInformation) {
	key := cache.ObjectName{Namespace: ds.Target.Namespace, Name: ds.Target.Name}.String()
	svcObj, exists, err := s.serviceIndexer.GetByKey(key)
	if err != nil {
		klog.ErrorS(err, "get service failed")
		return
	}
	if !exists {
		klog.ErrorS(err, "service not found", "ep", key)
		return
	}
	svc := svcObj.(*corev1.Service)
	name := svc.Annotations[scaleDeploymentKey]
	if name == "" {
		klog.ErrorS(err, "scaleDeploymentKey not found", "ep", key)
		return
	}

	selectorStr := svc.Annotations[cacheTargetSelectorKey]
	if selectorStr == "" {
		klog.ErrorS(err, "scaleTargetRefSelector not found", "ep", key)
		return
	}

	_, err = s.clientset.AppsV1().Deployments(svc.Namespace).UpdateScale(context.Background(), name, &autoscalingv1.Scale{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: svc.Namespace,
		},
		Spec: autoscalingv1.ScaleSpec{
			Replicas: 1,
		},
	}, metav1.UpdateOptions{})
	if err != nil {
		klog.ErrorS(err, "scale up failed")
		return
	}
	// Wait for the deployment to be ready
	err = waitUntilDeploymentIsReady(s.clientset, name, svc.Namespace)
	if err != nil {
		klog.ErrorS(err, "wait for deployment ready failed")
		return
	}

	// TODO: remove this
	klog.InfoS("restore service selector")
	s.restoreSelector(context.Background(), svc)

}

func (s *Server) deploymentIsReady(dep *appsv1.Deployment) bool {
	if dep.Status.AvailableReplicas == 0 {
		return false
	}
	return true
}

func waitUntilDeploymentIsReady(clientset kubernetes.Interface, name, namespace string) error {
	return wait.PollUntilContextTimeout(context.Background(), time.Second/10, time.Second*30, false, func(ctx context.Context) (bool, error) {
		dep, err := clientset.AppsV1().Deployments(namespace).Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return deploymentIsReady(dep), nil
	})
}

func deploymentIsReady(dep *appsv1.Deployment) bool {
	if dep.Status.AvailableReplicas == 0 {
		return false
	}
	return true
}
