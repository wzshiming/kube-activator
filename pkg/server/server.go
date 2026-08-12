package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

const (
	scaleDeploymentKey = "scale-from-zero.zsm.io/deployment"

	// managedBy marks the EndpointSlices created by the activator.
	managedBy = "activator.zsm.io"

	serviceIndex = "service"
)

type Server struct {
	ip        string
	clientset kubernetes.Interface

	serviceIndexer cache.Indexer
	sliceIndexer   cache.Indexer

	manager *PortManager

	queue workqueue.TypedRateLimitingInterface[string]
}

func NewServer(ip string, clientset kubernetes.Interface) *Server {
	return &Server{
		ip:        ip,
		clientset: clientset,
	}
}

func (s *Server) Run(ctx context.Context) error {
	s.manager = NewPortManager(func(pi *PortInformation) error {
		return s.scaleUp(ctx, pi)
	})
	s.queue = workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())

	serviceController := s.newServiceInformer(ctx)
	sliceController := s.newEndpointSliceInformer(ctx)

	go serviceController.Run(ctx.Done())
	go sliceController.Run(ctx.Done())

	if !cache.WaitForCacheSync(ctx.Done(), serviceController.HasSynced, sliceController.HasSynced) {
		return fmt.Errorf("failed to sync caches")
	}

	go func() {
		<-ctx.Done()
		s.queue.ShutDown()
	}()
	// a single worker serializes reconciles per service, the handlers only
	// enqueue keys (the initial list is covered by the informer add events)
	go s.runWorker(ctx)
	return nil
}

func (s *Server) newServiceInformer(ctx context.Context) cache.Controller {
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
			AddFunc:    s.enqueueService,
			UpdateFunc: func(_, newObj any) { s.enqueueService(newObj) },
			DeleteFunc: s.enqueueService,
		},
		cache.Indexers{},
	)

	s.serviceIndexer = indexer
	return controller
}

func (s *Server) enqueueService(obj any) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	if svc, ok := obj.(*corev1.Service); ok {
		s.queue.Add(svc.Namespace + "/" + svc.Name)
	}
}

func (s *Server) newEndpointSliceInformer(ctx context.Context) cache.Controller {
	handle := func(obj any) {
		if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
			obj = tombstone.Obj
		}
		es, ok := obj.(*discoveryv1.EndpointSlice)
		if !ok {
			return
		}
		name := es.Labels[discoveryv1.LabelServiceName]
		if name == "" {
			return
		}
		s.queue.Add(es.Namespace + "/" + name)
	}

	indexer, controller := cache.NewIndexerInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return s.clientset.DiscoveryV1().EndpointSlices(corev1.NamespaceAll).List(ctx, options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return s.clientset.DiscoveryV1().EndpointSlices(corev1.NamespaceAll).Watch(ctx, options)
			},
		},
		&discoveryv1.EndpointSlice{},
		0,
		cache.ResourceEventHandlerFuncs{
			AddFunc:    handle,
			UpdateFunc: func(_, newObj any) { handle(newObj) },
			DeleteFunc: handle,
		},
		cache.Indexers{
			serviceIndex: func(obj any) ([]string, error) {
				es, ok := obj.(*discoveryv1.EndpointSlice)
				if !ok {
					return nil, nil
				}
				name := es.Labels[discoveryv1.LabelServiceName]
				if name == "" {
					return nil, nil
				}
				return []string{es.Namespace + "/" + name}, nil
			},
		},
	)

	s.sliceIndexer = indexer
	return controller
}

func (s *Server) runWorker(ctx context.Context) {
	for {
		key, shutdown := s.queue.Get()
		if shutdown {
			return
		}
		if err := s.reconcileKey(ctx, key); err != nil {
			klog.ErrorS(err, "reconcile failed", "svc", key)
			s.queue.AddRateLimited(key)
		} else {
			s.queue.Forget(key)
		}
		s.queue.Done(key)
	}
}

func (s *Server) reconcileKey(ctx context.Context, key string) error {
	obj, exists, err := s.serviceIndexer.GetByKey(key)
	if err != nil {
		return err
	}
	if !exists {
		namespace, name, err := cache.SplitMetaNamespaceKey(key)
		if err != nil {
			return err
		}
		return s.eject(ctx, namespace, name)
	}
	return s.reconcileService(ctx, obj.(*corev1.Service))
}

func (s *Server) reconcileService(ctx context.Context, svc *corev1.Service) error {
	ports, ok := s.needInject(svc)
	if !ok {
		return s.eject(ctx, svc.Namespace, svc.Name)
	}

	slices, err := s.slicesForService(svc.Namespace, svc.Name)
	if err != nil {
		return err
	}

	backends := readyBackends(slices)
	if len(backends) == 0 {
		return s.inject(ctx, svc, ports)
	}
	// ports still without a ready backend keep their pending targets until
	// a later reconcile finds them one
	s.forward(svc, ports, backends)
	if s.hasActivatorSlice(svc.Namespace, svc.Name) {
		return s.deleteSlice(ctx, svc.Namespace, svc.Name)
	}
	return nil
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

	if svc.Spec.Type != corev1.ServiceTypeClusterIP || svc.Spec.ClusterIP == corev1.ClusterIPNone {
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

func (s *Server) slicesForService(namespace, name string) ([]*discoveryv1.EndpointSlice, error) {
	objs, err := s.sliceIndexer.ByIndex(serviceIndex, namespace+"/"+name)
	if err != nil {
		return nil, err
	}
	slices := make([]*discoveryv1.EndpointSlice, 0, len(objs))
	for _, obj := range objs {
		if es, ok := obj.(*discoveryv1.EndpointSlice); ok {
			slices = append(slices, es)
		}
	}
	return slices, nil
}

// inject points the service at the activator by publishing an EndpointSlice
// with the activator address, the service selector is left untouched
func (s *Server) inject(ctx context.Context, svc *corev1.Service, ports []corev1.ServicePort) error {
	key := cache.MetaObjectToName(svc).String()

	slicePorts := make([]discoveryv1.EndpointPort, 0, len(ports))
	for _, port := range ports {
		pi, err := s.manager.AddTarget(svc.Name, svc.Namespace, int(port.Port))
		if err != nil {
			return fmt.Errorf("add target for port %d: %w", port.Port, err)
		}
		slicePorts = append(slicePorts, discoveryv1.EndpointPort{
			Name:     new(port.Name),
			Port:     new(int32(pi.Listener.Port())),
			Protocol: ptr.To(corev1.ProtocolTCP),
		})
	}

	desired := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      activatorSliceName(svc.Name),
			Namespace: svc.Namespace,
			Labels: map[string]string{
				discoveryv1.LabelServiceName: svc.Name,
				discoveryv1.LabelManagedBy:   managedBy,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(svc, corev1.SchemeGroupVersion.WithKind("Service")),
			},
		},
		AddressType: addressType(s.ip),
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{s.ip},
			Conditions: discoveryv1.EndpointConditions{Ready: new(true)},
		}},
		Ports: slicePorts,
	}

	if err := s.applySlice(ctx, desired); err != nil {
		return err
	}
	klog.InfoS("inject", "svc", key)
	return nil
}

func (s *Server) applySlice(ctx context.Context, desired *discoveryv1.EndpointSlice) error {
	if obj, exists, _ := s.sliceIndexer.GetByKey(desired.Namespace + "/" + desired.Name); exists {
		if es, ok := obj.(*discoveryv1.EndpointSlice); ok && sliceMatches(es, desired) {
			return nil
		}
	}

	_, err := s.clientset.DiscoveryV1().EndpointSlices(desired.Namespace).Create(ctx, desired, metav1.CreateOptions{})
	if err == nil || !apierrors.IsAlreadyExists(err) {
		return err
	}

	existing, err := s.clientset.DiscoveryV1().EndpointSlices(desired.Namespace).Get(ctx, desired.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if existing.Labels[discoveryv1.LabelManagedBy] != managedBy {
		return fmt.Errorf("endpointslice %s/%s exists but is not managed by %s", desired.Namespace, desired.Name, managedBy)
	}
	if sliceMatches(existing, desired) {
		return nil
	}
	desired = desired.DeepCopy()
	desired.ResourceVersion = existing.ResourceVersion
	_, err = s.clientset.DiscoveryV1().EndpointSlices(desired.Namespace).Update(ctx, desired, metav1.UpdateOptions{})
	return err
}

func sliceMatches(existing, desired *discoveryv1.EndpointSlice) bool {
	return existing.AddressType == desired.AddressType &&
		apiequality.Semantic.DeepEqual(existing.Endpoints, desired.Endpoints) &&
		apiequality.Semantic.DeepEqual(existing.Ports, desired.Ports)
}

func activatorSliceName(name string) string {
	return name + "-activator"
}

func addressType(ip string) discoveryv1.AddressType {
	if strings.Contains(ip, ":") {
		return discoveryv1.AddressTypeIPv6
	}
	return discoveryv1.AddressTypeIPv4
}

func (s *Server) hasActivatorSlice(namespace, name string) bool {
	obj, exists, err := s.sliceIndexer.GetByKey(namespace + "/" + activatorSliceName(name))
	if err != nil || !exists {
		return false
	}
	es, ok := obj.(*discoveryv1.EndpointSlice)
	return ok && es.Labels[discoveryv1.LabelManagedBy] == managedBy
}

func (s *Server) deleteSlice(ctx context.Context, namespace, name string) error {
	err := s.clientset.DiscoveryV1().EndpointSlices(namespace).Delete(ctx, activatorSliceName(name), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (s *Server) eject(ctx context.Context, namespace, name string) error {
	// the listeners were already closed by RemoveTargetForAllPorts
	pis := s.manager.RemoveTargetForAllPorts(name, namespace)
	for _, pi := range pis {
		klog.InfoS("eject",
			"svc", cache.ObjectName{Namespace: namespace, Name: name}.String(),
			"port", pi.Target.Port,
			"listener", pi.Listener.Port(),
			"connections", len(pi.Connections),
		)
		for _, c := range pi.Connections {
			if err := c.Close(); err != nil {
				klog.ErrorS(err, "close connection failed")
			}
		}
	}

	if s.hasActivatorSlice(namespace, name) {
		return s.deleteSlice(ctx, namespace, name)
	}
	return nil
}

// readyBackends maps each service port name to a dialable "ip:port" backend,
// ignoring the EndpointSlices published by the activator itself
func readyBackends(slices []*discoveryv1.EndpointSlice) map[string]string {
	backends := map[string]string{}
	for _, es := range slices {
		if es.Labels[discoveryv1.LabelManagedBy] == managedBy {
			continue
		}
		for _, port := range es.Ports {
			if port.Port == nil {
				continue
			}
			if port.Protocol != nil && *port.Protocol != corev1.ProtocolTCP {
				continue
			}
			name := ptr.Deref(port.Name, "")
			if _, ok := backends[name]; ok {
				continue
			}
			for _, ep := range es.Endpoints {
				if len(ep.Addresses) == 0 || !endpointReady(ep) {
					continue
				}
				backends[name] = net.JoinHostPort(ep.Addresses[0], strconv.Itoa(int(*port.Port)))
				break
			}
		}
	}
	return backends
}

func endpointReady(ep discoveryv1.Endpoint) bool {
	return ep.Conditions.Ready == nil || *ep.Conditions.Ready
}

// forward hands pending connections over to the ready backends, the
// listener was already closed by RemoveTarget so Connections is stable
func (s *Server) forward(svc *corev1.Service, ports []corev1.ServicePort, backends map[string]string) {
	key := cache.MetaObjectToName(svc).String()
	for _, port := range ports {
		address, ok := backends[port.Name]
		if !ok {
			continue
		}
		pi := s.manager.RemoveTarget(svc.Name, svc.Namespace, int(port.Port))
		if pi == nil {
			continue
		}

		klog.InfoS("forward",
			"svc", key,
			"port", port.Port,
			"backend", address,
			"listener", pi.Listener.Port(),
			"connections", len(pi.Connections),
		)
		for _, c := range pi.Connections {
			go func(c net.Conn) {
				backend, err := dialBackend(address)
				if err != nil {
					klog.ErrorS(err, "dial backend failed", "svc", key, "backend", address)
					_ = c.Close()
					return
				}
				tunnel(c, backend)
			}(c)
		}
	}
}

// dialBackend retries for a while because a pod can be reported ready
// before its process has actually bound the port
func dialBackend(address string) (net.Conn, error) {
	var lastErr error
	delay := 100 * time.Millisecond
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		backend, err := net.DialTimeout("tcp", address, 5*time.Second)
		if err == nil {
			return backend, nil
		}
		lastErr = err
		time.Sleep(delay)
		if delay < 2*time.Second {
			delay *= 2
		}
	}
	return nil, lastErr
}

// tunnel copies both directions and tears down both connections once either
// side closes, so a client hangup does not leave the backend connection open
func tunnel(a, b net.Conn) {
	closeBoth := func() {
		_ = a.Close()
		_ = b.Close()
	}
	go func() {
		defer closeBoth()
		_, _ = io.Copy(a, b)
	}()
	go func() {
		defer closeBoth()
		_, _ = io.Copy(b, a)
	}()
}

func (s *Server) scaleUp(ctx context.Context, pi *PortInformation) error {
	key := cache.ObjectName{Namespace: pi.Target.Namespace, Name: pi.Target.Name}.String()
	obj, exists, err := s.serviceIndexer.GetByKey(key)
	if err != nil {
		return fmt.Errorf("get service %s: %w", key, err)
	}
	if !exists {
		return fmt.Errorf("service %s not found", key)
	}
	svc := obj.(*corev1.Service)
	name := svc.Annotations[scaleDeploymentKey]
	if name == "" {
		return fmt.Errorf("service %s has no %s annotation", key, scaleDeploymentKey)
	}

	// read-modify-write with conflict retry so the activator never fights the
	// HPA over the scale subresource, and only wakes a workload that is at zero
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		scale, err := s.clientset.AppsV1().Deployments(svc.Namespace).GetScale(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if scale.Spec.Replicas > 0 {
			return nil
		}
		scale.Spec.Replicas = 1
		_, err = s.clientset.AppsV1().Deployments(svc.Namespace).UpdateScale(ctx, name, scale, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("scale deployment %s/%s: %w", svc.Namespace, name, err)
	}
	klog.InfoS("scale up", "svc", key, "deployment", name)
	// no need to wait for readiness here: once pods become ready the
	// EndpointSlice watch hands the pending connections over to the backends
	return nil
}
