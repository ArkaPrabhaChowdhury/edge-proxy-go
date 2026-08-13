# Kubernetes deployment

Create the operational secret out of band, then apply the examples:

```powershell
kubectl create secret generic edge-proxy-auth --from-literal=username=admin --from-literal=password="use-a-random-value"
kubectl apply -f k8s/configmap.yaml -f k8s/deployment.yaml -f k8s/service.yaml
kubectl rollout status deployment/edge-proxy --timeout=5m
```

Do not commit real credentials or expose the stats port through a public LoadBalancer.
