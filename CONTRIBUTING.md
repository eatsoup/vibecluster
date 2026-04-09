# Contributing to vibecluster

Thanks for your interest in contributing! This guide will help you get started.

## Development setup

### Prerequisites

- Go 1.23+
- Docker (for building the syncer image)
- Access to a Kubernetes cluster (k3s recommended for local dev)
- `kubectl` configured for your cluster

### Building

```bash
# Build the CLI
make build

# Build the syncer container image
make syncer-image

# Run tests
make test
```

### Project structure

```
cmd/
  vibecluster/    CLI entry point
  syncer/         Syncer binary (runs inside the cluster as a sidecar)
pkg/
  cli/            Cobra command definitions and flag handling
  k8s/            Kubernetes client, manifest generation, port-forwarding
  kubeconfig/     Kubeconfig retrieval, merging, and output
  syncer/         Resource sync logic (pods, services, configmaps, secrets, nodes)
```

## Making changes

1. Fork the repository and create a branch from `main`
2. Make your changes
3. Add or update tests for any changed functionality
4. Run `make test` and ensure all tests pass
5. Run `go vet ./...` to catch issues
6. Open a pull request

## Testing

Tests use the standard Go testing framework with `k8s.io/client-go/kubernetes/fake` for Kubernetes API interactions.

```bash
# Run all tests
go test ./...

# Run tests with race detector
go test -race ./...

# Run tests for a specific package
go test ./pkg/syncer/ -v

# Run a specific test
go test ./pkg/k8s/ -run TestCreateVirtualCluster -v
```

### What to test

- **pkg/k8s**: Manifest generation, resource creation/deletion, label/naming logic
- **pkg/kubeconfig**: Config writing, merging, removal, path handling
- **pkg/syncer**: Resource translation, sync logic, filtering (system namespaces/secrets)
- **pkg/cli**: Command structure, flag defaults, argument validation

### Manual testing

To test against a real cluster:

```bash
# Create a virtual cluster
./bin/vibecluster create test --connect=false

# Check it's running
./bin/vibecluster list
kubectl get pods -n vc-test

# View syncer logs
./bin/vibecluster logs test

# Connect and use it
./bin/vibecluster connect test --print 2>/dev/null > /tmp/vc-test.kubeconfig
kubectl port-forward -n vc-test svc/test 16443:443 &
kubectl --kubeconfig=/tmp/vc-test.kubeconfig --server=https://127.0.0.1:16443 get nodes

# Clean up
./bin/vibecluster delete test
```

## CI

Every push and pull request runs:

- **Lint** — golangci-lint
- **Test** — `go test -race` with coverage
- **Build** — cross-compilation for linux/darwin (amd64/arm64)
- **Build Syncer Image** — Docker build verification

All checks must pass before merging.

## Code style

- Follow standard Go conventions (`gofmt`, `go vet`)
- Keep functions focused and testable
- Use `kubernetes.Interface` instead of `*kubernetes.Clientset` so code is testable with fake clients
- Public API functions accept `context.Context` as the first parameter

## Releases

Releases are automated via GitHub Actions when a version tag is pushed:

```bash
git tag -a v0.2.0 -m "v0.2.0"
git push origin v0.2.0
```

This builds multi-arch CLI binaries, creates a GitHub release, and pushes the syncer image to `ghcr.io/eatsoup/vibecluster/syncer`.

## License

By contributing, you agree that your contributions will be licensed under the [MIT License](LICENSE).
