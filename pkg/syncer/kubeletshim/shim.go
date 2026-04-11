// Package kubeletshim implements a small HTTPS server that speaks the
// kubelet API (logs/exec/attach/portforward) and proxies each request to the
// host kube-apiserver's matching pod subresource, after translating the
// virtual pod name and namespace to the host pod name and namespace.
//
// Why this exists
//
// The virtual k3s API server forwards `kubectl logs`, `kubectl exec` and
// `kubectl port-forward` requests to whatever address is in
// node.status.addresses[InternalIP]:daemonEndpoints.kubeletEndpoint.port —
// which, since vibecluster syncs nodes from the host, points at the *host*
// kubelet. The host kubelet doesn't know about virtual pod names: virtual
// pods are translated to host pod names of the form
// `<vcluster>-x-<pod>-x-<namespace>`. So a request for
// `/containerLogs/default/nginx-abc/nginx` 404s on the host kubelet, which
// the apiserver proxy chain converts into a 502.
//
// The fix is to insert a per-vcluster shim between the virtual apiserver and
// the kubelet:
//
//   1. The syncer overrides synced virtual nodes' .status.addresses
//      [InternalIP] to point at this shim's pod IP, and overrides
//      .status.daemonEndpoints.kubeletEndpoint.port to KubeletShimPort.
//   2. The shim accepts the kubelet-format URL, translates virtual ->
//      host, builds the equivalent host-API URL
//      (`/api/v1/namespaces/<hostNS>/pods/<hostPod>/log` and friends),
//      and forwards the request via apimachinery's UpgradeAwareHandler so
//      log streams and SPDY upgrades for exec/portforward pass through
//      transparently.
//
// Going through the host *API server* (not the host kubelet) means we reuse
// the syncer's existing service-account RBAC instead of having to mint
// kubelet client certs. The host API server in turn proxies to the host
// kubelet, doing all the SPDY upgrade plumbing for us.
//
// Upstream vcluster solves the same problem differently — it implements its
// own kube-apiserver-like proxy and rewrites the kubelet path inside its
// request handler chain (see pkg/server/filters/kubelet.go and nodename.go).
// We can't do that here because we run upstream k3s unmodified, so we need
// an out-of-process shim that the apiserver dials over TLS.
package kubeletshim

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/proxy"
	"k8s.io/client-go/rest"
)

// HostNameTranslator returns the host pod name for a (virtual name, virtual
// namespace) pair. It mirrors syncer.Syncer.HostName but is passed in as a
// callback to avoid an import cycle.
type HostNameTranslator func(virtualName, virtualNamespace string) string

// Config configures a Shim.
type Config struct {
	// HostConfig is the rest.Config used to talk to the host kube-apiserver
	// (typically the in-cluster config of the syncer pod).
	HostConfig *rest.Config
	// HostNamespace is the namespace on the host where translated pods live.
	HostNamespace string
	// TranslateName converts virtual pod (name, namespace) into the host
	// pod name.
	TranslateName HostNameTranslator
	// PodIP is the IP the shim should bind to and put in the serving cert
	// SAN. If empty, the shim binds to all interfaces but the cert only
	// covers 127.0.0.1.
	PodIP string
	// Port is the TCP port the shim listens on.
	Port int
	// CACertPath / CAKeyPath point at the k3s server CA used to sign the
	// shim's TLS serving cert. The kube-apiserver started by k3s verifies
	// kubelet certs against this CA (--kubelet-certificate-authority).
	CACertPath string
	// CAKeyPath is the matching CA private key.
	CAKeyPath string
}

// Shim is the running kubelet API → host kube-API proxy.
type Shim struct {
	cfg          Config
	hostURL      *url.URL
	hostTransport http.RoundTripper

	tokenMu   sync.RWMutex
	cachedTok string
	tokenFile string
}

// New constructs a Shim. It does not start any listeners or read any files
// from disk; call Run for that.
func New(cfg Config) (*Shim, error) {
	if cfg.HostConfig == nil {
		return nil, errors.New("kubeletshim: HostConfig is required")
	}
	if cfg.HostNamespace == "" {
		return nil, errors.New("kubeletshim: HostNamespace is required")
	}
	if cfg.TranslateName == nil {
		return nil, errors.New("kubeletshim: TranslateName is required")
	}
	if cfg.Port == 0 {
		return nil, errors.New("kubeletshim: Port is required")
	}

	hostURL, err := url.Parse(cfg.HostConfig.Host)
	if err != nil {
		return nil, fmt.Errorf("parsing host URL: %w", err)
	}

	rt, err := rest.TransportFor(cfg.HostConfig)
	if err != nil {
		return nil, fmt.Errorf("building host transport: %w", err)
	}

	return &Shim{
		cfg:           cfg,
		hostURL:       hostURL,
		hostTransport: rt,
		cachedTok:     cfg.HostConfig.BearerToken,
		tokenFile:     cfg.HostConfig.BearerTokenFile,
	}, nil
}

// Run starts the HTTPS server and blocks until ctx is cancelled or the
// listener fails.
func (s *Shim) Run(ctx context.Context) error {
	caCert, caKey, err := loadCA(s.cfg.CACertPath, s.cfg.CAKeyPath)
	if err != nil {
		return fmt.Errorf("loading k3s CA: %w", err)
	}
	servingCert, err := generateServingCert(s.cfg.PodIP, caCert, caKey)
	if err != nil {
		return fmt.Errorf("generating shim serving cert: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/containerLogs/", s.handle("log"))
	mux.Handle("/exec/", s.handle("exec"))
	mux.Handle("/attach/", s.handle("attach"))
	mux.Handle("/portForward/", s.handle("portforward"))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", s.cfg.Port),
		Handler: mux,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{servingCert},
			MinVersion:   tls.VersionTLS12,
			ClientAuth:   tls.NoClientCert,
		},
		ReadHeaderTimeout: 30 * time.Second,
	}

	listener, err := tls.Listen("tcp", srv.Addr, srv.TLSConfig)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", srv.Addr, err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(listener)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// handle returns the http.Handler that translates a kubelet-style request
// into the matching host kube-apiserver pod subresource and proxies it.
func (s *Shim) handle(subResource string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		vNS, vPod, container, err := parseKubeletPath(req.URL.Path, subResource)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		hostPod := s.cfg.TranslateName(vPod, vNS)

		target := *s.hostURL
		target.Path = path.Join("/api/v1/namespaces", s.cfg.HostNamespace, "pods", hostPod, subResource)

		// Preserve the original query (command, follow, tailLines, ports, …)
		// and pin the container if the kubelet path included one.
		// kube-apiserver translates PodExecOptions stdin/stdout/stderr
		// into kubelet-style input/output/error when proxying to a
		// kubelet (see pkg/registry/core/pod/strategy.go streamLocation
		// in upstream k8s). We're going the other way — back to the
		// host pods/exec subresource — so we have to translate them
		// back, otherwise the apiserver rejects the request with
		// "you must specify at least 1 of stdin, stdout, stderr".
		query := req.URL.Query()
		if container != "" && subResource != "portforward" {
			query.Set("container", container)
		}
		if subResource == "exec" || subResource == "attach" {
			renameParam(query, "input", "stdin")
			renameParam(query, "output", "stdout")
			renameParam(query, "error", "stderr")
		}
		// We have to set the rewritten query on BOTH target.RawQuery and
		// req.URL.RawQuery because UpgradeAwareHandler takes different
		// paths for upgrade vs. plain HTTP requests:
		//
		//   - tryUpgrade  (exec/attach/portforward, SPDY or WebSocket):
		//     does `location := *h.Location` and never copies from
		//     req.URL.RawQuery, so the dialed URL's query comes from
		//     target.RawQuery. Without this set, the rename above is
		//     silently dropped on upgrade requests.
		//   - serveHTTP   (logs / non-upgrade): does
		//     `loc.RawQuery = req.URL.RawQuery`, so the dialed URL's
		//     query comes from req.URL.RawQuery instead.
		//
		// Setting both keeps both code paths consistent.
		target.RawQuery = query.Encode()
		req.URL.RawQuery = target.RawQuery

		// Inject the host kube-apiserver bearer token. We must set this on
		// the inbound request *before* invoking UpgradeAwareHandler because
		// for SPDY upgrades the proxy bypasses transport.RoundTrip — it
		// dials the URL directly and writes req.Write(conn), so the bearer
		// token wrapper inside hostTransport never runs. For non-upgrade
		// requests setting Authorization here is also fine: the wrapper
		// won't overwrite an Authorization header that already exists.
		if token := s.token(); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		// Drop hop-by-hop headers we don't want to forward.
		req.Header.Del("Impersonate-User")
		req.Header.Del("Impersonate-Group")
		req.Header.Del("Impersonate-Uid")

		handler := proxy.NewUpgradeAwareHandler(&target, s.hostTransport, false, false, errResponder{})
		handler.UseLocationHost = true
		handler.ServeHTTP(w, req)
	})
}

// token returns the current host kube-apiserver bearer token. If a
// BearerTokenFile is configured (the in-cluster case) it is re-read on every
// call so that token rotations from kubelet's projected ServiceAccountToken
// volume don't break the shim.
func (s *Shim) token() string {
	if s.tokenFile == "" {
		s.tokenMu.RLock()
		defer s.tokenMu.RUnlock()
		return s.cachedTok
	}
	data, err := os.ReadFile(s.tokenFile)
	if err != nil {
		// On read errors, fall back to the cached token rather than
		// returning empty: a stale token is more likely to work than no
		// token, and the next refresh will recover.
		s.tokenMu.RLock()
		defer s.tokenMu.RUnlock()
		return s.cachedTok
	}
	tok := strings.TrimSpace(string(data))
	s.tokenMu.Lock()
	s.cachedTok = tok
	s.tokenMu.Unlock()
	return tok
}

// parseKubeletPath splits a kubelet-style URL path into (namespace, pod,
// container). The container component is empty for portforward.
//
//   /containerLogs/<ns>/<pod>/<container>
//   /exec/<ns>/<pod>/<container>
//   /attach/<ns>/<pod>/<container>
//   /portForward/<ns>/<pod>
func parseKubeletPath(p, subResource string) (ns, pod, container string, err error) {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) < 3 {
		return "", "", "", fmt.Errorf("kubeletshim: malformed path %q", p)
	}
	ns = parts[1]
	pod = parts[2]
	if subResource == "portforward" {
		// /portForward/<ns>/<pod>
		return ns, pod, "", nil
	}
	if len(parts) < 4 {
		// Some kubelet clients omit the container for single-container
		// pods; the host API will pick the only container in that case.
		return ns, pod, "", nil
	}
	container = parts[3]
	return ns, pod, container, nil
}

// renameParam moves all values for `from` over to `to` (overwriting any
// existing values under `to`) and deletes `from`. If `from` is not present
// it leaves the query untouched. The kubelet exec/attach API and the host
// kube-apiserver pod exec/attach subresource use different names for the
// same booleans (input/output/error vs. stdin/stdout/stderr); the shim has
// to translate so apiserver doesn't reject the proxied request.
func renameParam(q url.Values, from, to string) {
	if vs, ok := q[from]; ok {
		q[to] = vs
		delete(q, from)
	}
}

// errResponder converts proxy errors into 502 responses with the upstream
// error in the body. proxy.NewUpgradeAwareHandler requires a responder so
// that connection errors don't panic the handler.
type errResponder struct{}

func (errResponder) Error(w http.ResponseWriter, _ *http.Request, err error) {
	http.Error(w, "kubeletshim proxy error: "+err.Error(), http.StatusBadGateway)
}
