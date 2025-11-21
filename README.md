# alert-dedupe-proxy


## Build binary & image:

docker build -t <your-registry>/alert-dedupe-proxy:1.0.0 .
docker push <your-registry>/alert-dedupe-proxy:1.0.0


## Apply manifests (namespace monitoring must exist):

kubectl create ns monitoring || true
kubectl apply -f secret.yaml
kubectl apply -f configmap.yaml
kubectl apply -f deployment.yaml
kubectl apply -f service.yaml


## Point Alertmanager(s) to http://dedupe-proxy.monitoring.svc:80/webhook.

Verify:

readiness /ready

health /healthz

metrics /metrics