# kube-activator

This is demo of activator only for scale from 0 to 1

## Usage

Build image
``` bash
docker build -t activator:latest .
```

Create cluster using kind
``` bash
kind create cluster
```

Load image to cluster
``` bash
kind load docker-image activator:latest
```

Deploy activator
``` bash
kubectl apply -k ./manifests
```

Deploy webserver
``` bash
docker pull docker.io/library/nginx:latest
kind load docker-image docker.io/library/nginx:latest
kubectl create deployment webserver --image=docker.io/library/nginx:latest
kubectl create service clusterip webserver --tcp=8080:80
```

Test webserver
``` bash
kubectl exec -it -n kube-system deploy/activator -- wget -O- webserver.default.svc:8080
```

Mark webserver as activator target
``` bash
kubectl annotate service webserver scale-from-zero.zsm.io/deployment=webserver
```

Scale webserver to 0
``` bash
kubectl scale deployment webserver --replicas=0
```

Test activator that will scale webserver to 1 and forward to it
``` bash
kubectl exec -it -n kube-system deploy/activator -- wget -O- webserver.default.svc:8080
```

## Working with HPA scale-to-zero

Kubernetes HPA supports scaling to/from zero for object/external metrics
(`HPAScaleToZero` feature gate: targeted to be enabled by default (beta) in 1.37).

HPA and the activator are complementary:

- HPA with `minReplicas: 0` scales `1 -> 0` when the metric reports idle, and `0 -> N` when metrics recover.
- The activator wakes the workload `0 -> 1` on the first incoming connection, which metrics alone cannot see once no pods are running.

Enable the feature gate when creating the cluster:

``` yaml
# kind-config.yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
featureGates:
  HPAScaleToZero: true
```

``` bash
kind create cluster --config kind-config.yaml
```

Then let an HPA scale the deployment to zero instead of scaling manually.
Scale-to-zero only works with object/external metrics (not CPU/memory), so a
custom/external metrics provider is required.

### Runnable example: scale to zero on recent requests

[examples/hpa-connections](./examples/hpa-connections) keeps the webserver at
zero replicas while no requests arrive and lets CPU usage drive `1 -> N`
while traffic keeps flowing:

- [webserver.yaml](./examples/hpa-connections/webserver.yaml): nginx with `stub_status` and the official [nginx-prometheus-exporter](https://github.com/nginx/nginx-prometheus-exporter) sidecar, annotated with the standard `prometheus.io/scrape` convention, plus the annotated Service.
- [prometheus.yaml](./examples/hpa-connections/prometheus.yaml): a minimal Prometheus using Kubernetes pod service discovery: any pod annotated `prometheus.io/scrape: "true"` is scraped on its pod IP (never through a ClusterIP, so scraping cannot wake a sleeping workload), plus pod CPU from the kubelet cAdvisor endpoint.
- [adapter-values.yaml](./examples/hpa-connections/adapter-values.yaml): workload-agnostic prometheus-adapter rules exposing two external metrics: `nginx_has_recent_requests` (0/1: did any request arrive within the last minute? A raw counter delta of `nginx_http_requests_total`, minus the exporter's own `stub_status` requests which are exactly one per stored `nginx_up` sample) and `nginx_gated_cpu` (CPU of the pods behind the selected metrics, multiplied by that presence).
- [hpa.yaml](./examples/hpa-connections/hpa.yaml): `minReplicas: 0` HPA consuming both; its metric selector (`app: webserver`) is the only place the workload is named. Desired replicas is the max of the two proposals: a quiet minute -> `max(0, 0)` scales to zero, requests but quiet CPU -> `max(1, 1)` holds one replica, busy -> the CPU metric proposes `N`.

Deploy the metrics pipeline:

``` bash
docker pull docker.io/prom/prometheus:latest
kind load docker-image docker.io/prom/prometheus:latest
kubectl apply -f examples/hpa-connections/prometheus.yaml
helm install prometheus-adapter oci://ghcr.io/prometheus-community/charts/prometheus-adapter \
  -f examples/hpa-connections/adapter-values.yaml
```

Deploy the activator (see Usage above), then the example:

``` bash
docker pull docker.io/library/nginx:latest
docker pull docker.io/nginx/nginx-prometheus-exporter:latest
kind load docker-image docker.io/library/nginx:latest docker.io/nginx/nginx-prometheus-exporter:latest
kubectl apply -f examples/hpa-connections/webserver.yaml -f examples/hpa-connections/hpa.yaml
```

With no connections the HPA scales the webserver to zero within a couple of
minutes, and the activator injects itself into the empty endpoints:

``` bash
kubectl get deploy webserver -w
```

Any connection wakes it back up:

``` bash
kubectl exec -it -n kube-system deploy/activator -- wget -O- webserver.default.svc:8080
```

While requests keep arriving the HPA keeps it running, and load that pushes
nginx CPU usage above the 500m per-replica target scales it out `1 -> N`;
once no request has arrived for a minute plus the stabilization window it
scales to zero again.

Notes:

- Instantaneous gauges like `nginx_connections_active` cannot express "is
  anybody using this?" for short-lived requests: Prometheus samples every few
  seconds and a millisecond-long request is practically never in flight at
  sampling time, so a steadily used service would still read 0 and be scaled
  away. The request counter delta counts every request regardless of how
  short it was.
- CPU cannot come from metrics-server as a plain `Resource` metric in a
  scale-to-zero HPA: desired replicas is the max over all metrics, and an
  idle pod's CPU proposal never reaches 0 (`ceil` of a positive usage ratio),
  which would pin the workload at one replica forever. That is why the
  example delivers CPU as an external metric gated by the request presence;
  the cAdvisor series it uses carries the same kubelet CPU data
  metrics-server serves.
- At zero replicas the exporter sidecar is gone too, so the metric series
  disappears and the HPA cannot compute anything until pods are back. That is
  fine: waking up on traffic is exactly the activator's job.
- The activator only sets the scale subresource to 1 when the current replicas
  is 0, so it does not fight the HPA over the desired replica count.

