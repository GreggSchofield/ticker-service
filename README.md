# ticker-service

Exercise in Go Web Services, Containerisation and Kubernetes Deployments

## System Requirements

- Go 1.25
- Docker
- Kubectl
- Helm
- Minikube

## Running Locally

Build Docker Image and tag with semantic-versioning:

```sh
docker build -t stock-service:0.1.1 .
```

Start a basic Kind cluster (single-node cluster running within a container):

```sh
kind create cluster
```

Load the previously build image from the host system to the kind container:

```sh
kind load docker-image stock-service:0.1.1
```

Create Kubernetes namespace for this project:

```sh
kubectl create namespace ticker
```

Create Kubernetes secret to store the Aplha Vantage API Key:

```sh
kubectl create secret generic alpha-vantage-api-key \
  --from-literal=api-key=<alpha-advantage-api-key> \
  -n ticker
```

Create core Kubernetes resources for this project (Deployment, ConfigMap, Service etc):

```sh
kubectl create -f deployment.yaml
```

Download Traefik Helm chart to local registry:

```sh
helm repo add traefik https://traefik.github.io/charts
helm repo update
```

Install Traefik ingress controller via Helm in dedicated namespace:

```sh
helm install traefik traefik/traefik \
  --create-namespace \
  --namespace traefik \
  --set ingressClass.enabled=true \
  --set ingressClass.isDefaultClass=true
```

Allow port forwarding to test via localhost:

```sh
kubectl port-forward -n traefik deployment/traefik 8080:8000 9000:9000
```

Test ticker-service by senting a GET /stock request to localhost:

```sh
curl http://localhost:8080/stock
```

Cleanup by deleting Kind cluster:

```sh
kind delete cluster
```

