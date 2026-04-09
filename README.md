# vibecluster

Lightweight virtual Kubernetes clusters running inside a host cluster.

vibecluster creates isolated virtual clusters by deploying [k3s](https://k3s.io) as a StatefulSet in a dedicated namespace. Each virtual cluster gets its own API server, control plane, and resource isolation — while sharing the underlying host cluster's compute.

## How it works

```
┌─────────────────────────────────────────────────┐
│ Host Cluster                                    │
│                                                 │
│  ┌───────────────── vc-mycluster ─────────────┐ │
│  │ Namespace                                  │ │
│  │                                            │ │
│  │  ┌──────────────────────────────────────┐  │ │
│  │  │ StatefulSet: mycluster               │  │ │
│  │  │                                      │  │ │
│  │  │  ┌────────────┐  ┌───────────────┐   │  │ │
│  │  │  │ k3s server │  │ syncer        │   │  │ │
│  │  │  │ (API, etcd,│  │ (pods, svc,   │   │  │ │
│  │  │  │  ctrl-mgr) │  │  cm, secrets) │   │  │ │
│  │  │  └────────────┘  └───────────────┘   │  │ │
│  │  └──────────────────────────────────────┘  │ │
│  │                                            │ │
│  │  Service: mycluster (ClusterIP :443)       │ │
│  │  RBAC: ServiceAccount, ClusterRole         │ │
│  │  Synced resources: pods, svc, cm, secrets  │ │
│  └────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────┘
```

**Syncer** runs as a sidecar alongside k3s. It watches the virtual cluster and syncs resources bidirectionally:
- **Virtual -> Host:** Pods, Services, ConfigMaps, Secrets (name-translated into the host namespace)
- **Host -> Virtual:** Nodes (so workloads can be scheduled)

## Install

### From release

```bash
# Linux amd64
curl -L -o vibecluster https://github.com/eatsoup/vibecluster/releases/latest/download/vibecluster-linux-amd64
chmod +x vibecluster
sudo mv vibecluster /usr/local/bin/

# macOS arm64 (Apple Silicon)
curl -L -o vibecluster https://github.com/eatsoup/vibecluster/releases/latest/download/vibecluster-darwin-arm64
chmod +x vibecluster
sudo mv vibecluster /usr/local/bin/
```

### From source

```bash
git clone https://github.com/eatsoup/vibecluster.git
cd vibecluster
make build
# Binary at ./bin/vibecluster
```

## Usage

### Create a virtual cluster

```bash
vibecluster create mycluster
```

This will:
1. Create namespace `vc-mycluster`
2. Deploy k3s + syncer as a StatefulSet
3. Set up RBAC and services
4. Wait for the cluster to be ready
5. Set up port-forward and write kubeconfig

### Connect to an existing virtual cluster

```bash
# Write kubeconfig and keep port-forward running
vibecluster connect mycluster

# Print kubeconfig to stdout
vibecluster connect mycluster --print

# Write to a specific file
vibecluster connect mycluster --kubeconfig ./my-kubeconfig.yaml
```

### Use the virtual cluster

```bash
# With the context set by connect/create:
kubectl get nodes
kubectl create deployment nginx --image=nginx
kubectl get pods
```

### List virtual clusters

```bash
vibecluster list
```

```
NAME        NAMESPACE      STATUS    CREATED
mycluster   vc-mycluster   Running   2026-04-09T19:27:30Z
dev         vc-dev         Running   2026-04-09T20:15:00Z
```

### View syncer/k3s logs

```bash
# Syncer logs (default)
vibecluster logs mycluster

# k3s logs
vibecluster logs mycluster -c k3s

# Follow
vibecluster logs mycluster -f
```

### Delete a virtual cluster

```bash
vibecluster delete mycluster
```

## Resource syncing

Resources created in the virtual cluster are synced to the host namespace with translated names:

| Virtual Cluster | Host Cluster |
|---|---|
| `default/my-configmap` | `vc-mycluster/mycluster-x-my-configmap-x-default` |
| `default/my-pod` | `vc-mycluster/mycluster-x-my-pod-x-default` |

Synced resources on the host are labeled with:
- `vibecluster.dev/synced-from` — source virtual cluster name
- `vibecluster.dev/virtual-name` — original resource name
- `vibecluster.dev/virtual-namespace` — original namespace

## Configuration

### Global flags

| Flag | Description |
|---|---|
| `--context` | Kubernetes context to use for the host cluster |

### Create flags

| Flag | Default | Description |
|---|---|---|
| `--connect` | `true` | Auto-connect after creation |
| `--timeout` | `5m` | Timeout waiting for readiness |
| `--print` | `false` | Print kubeconfig to stdout |

### Connect flags

| Flag | Default | Description |
|---|---|---|
| `--server` | (auto) | Override API server address |
| `--print` | `false` | Print kubeconfig to stdout |
| `--kubeconfig` | `~/.kube/config` | Kubeconfig output file |

## Operator / GitOps

vibecluster can be deployed as a **Kubernetes operator**, enabling GitOps workflows where virtual clusters are managed declaratively via `VirtualCluster` custom resources. This integrates seamlessly with tools like [ArgoCD](https://argo-cd.readthedocs.io/) and [Flux](https://fluxcd.io/).

### Install the CRD

```bash
kubectl apply -f https://raw.githubusercontent.com/eatsoup/vibecluster/main/config/crd/vibecluster.dev_virtualclusters.yaml
```

### Deploy the operator

```bash
# Using kustomize (includes CRD + RBAC + Deployment)
kubectl apply -k https://github.com/eatsoup/vibecluster/config/operator

# Or manually
kubectl apply -f config/crd/vibecluster.dev_virtualclusters.yaml
kubectl apply -f config/operator/rbac.yaml
kubectl apply -f config/operator/deployment.yaml
```

### Create a VirtualCluster

```yaml
apiVersion: vibecluster.dev/v1alpha1
kind: VirtualCluster
metadata:
  name: dev-cluster
  namespace: default
spec:
  # All optional — sensible defaults applied
  k3sImage: "rancher/k3s:v1.28.5-k3s1"
  syncerImage: "ghcr.io/eatsoup/vibecluster/syncer:latest"
  storage: "5Gi"
```

```bash
kubectl apply -f my-cluster.yaml
```

### Check status

```bash
kubectl get virtualclusters
```

```
NAME          PHASE     READY   NAMESPACE         AGE
dev-cluster   Running   true    vc-dev-cluster    5m
```

### VirtualCluster spec fields

| Field | Default | Description |
|---|---|---|
| `k3sImage` | `rancher/k3s:v1.28.5-k3s1` | k3s container image |
| `syncerImage` | `ghcr.io/eatsoup/vibecluster/syncer:latest` | Syncer sidecar image |
| `storage` | `5Gi` | Persistent volume size for k3s data |

### VirtualCluster status fields

| Field | Description |
|---|---|
| `phase` | `Pending`, `Running`, `Failed`, or `Deleting` |
| `ready` | `true` when StatefulSet has ready replicas |
| `message` | Human-readable status message |
| `namespace` | Host namespace (e.g., `vc-dev-cluster`) |
| `observedGeneration` | Last reconciled generation |

### Delete a VirtualCluster

```bash
kubectl delete virtualcluster dev-cluster
```

The operator will clean up the namespace, RBAC, and all associated resources.

### ArgoCD / Flux integration

Store your `VirtualCluster` manifests in a Git repository and point your GitOps tool at them:

```yaml
# argocd-app.yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: virtual-clusters
spec:
  source:
    repoURL: https://github.com/your-org/cluster-configs
    path: virtual-clusters
  destination:
    server: https://kubernetes.default.svc
    namespace: default
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
```

## Development

```bash
# Build CLI
make build

# Build syncer image
make syncer-image

# Push syncer image
make syncer-push

# Build operator
make build-operator

# Build operator image
make operator-image

# Push operator image
make operator-push

# Install CRD
make install-crd

# Deploy operator (CRD + RBAC + Deployment)
make deploy-operator

# Undeploy operator
make undeploy-operator

# Run tests
make test
```

## License

[MIT](LICENSE)

