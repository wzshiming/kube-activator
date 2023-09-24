# kube-activator

This is demo of activator only for scala to 1 from 0

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
