package syncer

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/proxy"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	// KubeletShimPort is the port the kubelet shim listens on.
	// This must match what the virtual k3s API expects (standard kubelet port).
	KubeletShimPort = 10250
)

// KubeletShim implements a minimal kubelet-compatible HTTPS server that
// translates virtual pod names to host pod names and proxies log/exec/attach/
// portForward requests through the host Kubernetes API server.
type KubeletShim struct {
	name       string
	hostNS     string
	hostConfig *rest.Config
	hostClient kubernetes.Interface
	server     *http.Server
}

// NewKubeletShim creates a new kubelet shim.
func NewKubeletShim(name, hostNS string, hostConfig *rest.Config, hostClient kubernetes.Interface) *KubeletShim {
	return &KubeletShim{
		name:       name,
		hostNS:     hostNS,
		hostConfig: hostConfig,
		hostClient: hostClient,
	}
}

// hostPodName translates a virtual pod name+namespace to the host pod name.
func (k *KubeletShim) hostPodName(virtualName, virtualNamespace string) string {
	return fmt.Sprintf("%s-x-%s-x-%s", k.name, virtualName, virtualNamespace)
}

// Start starts the kubelet shim HTTPS server. Blocks until the context is
// cancelled or an error occurs.
func (k *KubeletShim) Start(ctx context.Context) error {
	mux := http.NewServeMux()

	// Register kubelet-compatible endpoints.
	// The k3s API server uses these paths when proxying to "the kubelet".
	mux.HandleFunc("/containerLogs/", k.handleLogs)
	mux.HandleFunc("/exec/", k.handleSubresourceProxy("exec"))
	mux.HandleFunc("/attach/", k.handleSubresourceProxy("attach"))
	mux.HandleFunc("/portForward/", k.handleSubresourceProxy("portforward"))
	mux.HandleFunc("/run/", k.handleSubresourceProxy("exec"))

	// Health endpoint
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	tlsConfig, err := selfSignedTLSConfig()
	if err != nil {
		return fmt.Errorf("generating TLS config: %w", err)
	}

	addr := fmt.Sprintf("0.0.0.0:%d", KubeletShimPort)
	k.server = &http.Server{
		Addr:      addr,
		Handler:   mux,
		TLSConfig: tlsConfig,
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}
	tlsLn := tls.NewListener(ln, tlsConfig)

	fmt.Printf("  [kubelet-shim] listening on %s\n", addr)

	// Shut down gracefully when context is cancelled.
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = k.server.Shutdown(shutdownCtx)
	}()

	if err := k.server.Serve(tlsLn); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("kubelet shim server: %w", err)
	}
	return nil
}

// parseKubeletPath extracts namespace, pod name, and container from a kubelet-
// style URL path like /containerLogs/{namespace}/{pod}/{container}.
func parseKubeletPath(path, prefix string) (namespace, pod, container string, err error) {
	// Strip the prefix (e.g. "/containerLogs/")
	trimmed := strings.TrimPrefix(path, prefix)
	trimmed = strings.TrimPrefix(trimmed, "/")
	trimmed = strings.TrimSuffix(trimmed, "/")

	parts := strings.SplitN(trimmed, "/", 3)
	if len(parts) < 2 {
		return "", "", "", fmt.Errorf("invalid path: expected /<prefix>/<namespace>/<pod>[/<container>], got %s", path)
	}
	namespace = parts[0]
	pod = parts[1]
	if len(parts) == 3 {
		container = parts[2]
	}
	return namespace, pod, container, nil
}

// handleLogs handles /containerLogs/{namespace}/{pod}/{container} requests
// by translating the virtual pod name to the host pod name and streaming
// logs from the host Kubernetes API.
func (k *KubeletShim) handleLogs(w http.ResponseWriter, r *http.Request) {
	namespace, podName, container, err := parseKubeletPath(r.URL.Path, "/containerLogs")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	hostName := k.hostPodName(podName, namespace)
	fmt.Printf("  [kubelet-shim] logs %s/%s/%s -> %s/%s/%s\n",
		namespace, podName, container, k.hostNS, hostName, container)

	opts := &corev1.PodLogOptions{}
	if container != "" {
		opts.Container = container
	}
	if r.URL.Query().Get("follow") == "true" {
		opts.Follow = true
	}
	if r.URL.Query().Get("previous") == "true" {
		opts.Previous = true
	}
	if r.URL.Query().Get("timestamps") == "true" {
		opts.Timestamps = true
	}
	if tailLines := r.URL.Query().Get("tailLines"); tailLines != "" {
		var tl int64
		if _, err := fmt.Sscanf(tailLines, "%d", &tl); err == nil {
			opts.TailLines = &tl
		}
	}

	stream, err := k.hostClient.CoreV1().Pods(k.hostNS).GetLogs(hostName, opts).Stream(r.Context())
	if err != nil {
		errMsg := fmt.Sprintf("error getting logs for %s/%s: %v", k.hostNS, hostName, err)
		fmt.Printf("  [kubelet-shim] %s\n", errMsg)
		http.Error(w, errMsg, http.StatusInternalServerError)
		return
	}
	defer stream.Close()

	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)

	if f, ok := w.(http.Flusher); ok {
		// Stream the logs, flushing periodically
		buf := make([]byte, 4096)
		for {
			n, readErr := stream.Read(buf)
			if n > 0 {
				if _, writeErr := w.Write(buf[:n]); writeErr != nil {
					return
				}
				f.Flush()
			}
			if readErr != nil {
				return
			}
		}
	} else {
		_, _ = io.Copy(w, stream)
	}
}

// handleSubresourceProxy returns an HTTP handler that proxies exec/attach/
// portforward requests by rewriting the URL to target the translated host
// pod via the host Kubernetes API server, then reverse-proxying the full
// request (including SPDY/WebSocket upgrade) to the host API.
func (k *KubeletShim) handleSubresourceProxy(subresource string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		prefix := "/" + strings.TrimSuffix(subresource, "/")
		if subresource == "exec" && strings.HasPrefix(r.URL.Path, "/run/") {
			prefix = "/run"
		}
		namespace, podName, container, err := parseKubeletPath(r.URL.Path, prefix)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		hostName := k.hostPodName(podName, namespace)
		fmt.Printf("  [kubelet-shim] %s %s/%s/%s -> %s/%s/%s\n",
			subresource, namespace, podName, container, k.hostNS, hostName, container)

		// Build the target URL on the host API server.
		// e.g. /api/v1/namespaces/{hostNS}/pods/{hostName}/{subresource}
		hostPath := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/%s",
			k.hostNS, hostName, subresource)

		// Preserve query parameters (command, stdin, stdout, stderr, tty, etc.)
		targetQuery := r.URL.RawQuery
		if container != "" && r.URL.Query().Get("container") == "" {
			if targetQuery != "" {
				targetQuery += "&"
			}
			targetQuery += "container=" + container
		}

		// Build the host API URL
		hostURLStr := strings.TrimSuffix(k.hostConfig.Host, "/") + hostPath
		hostURL, err := url.Parse(hostURLStr)
		if err != nil {
			http.Error(w, fmt.Sprintf("invalid host URL: %v", err), http.StatusInternalServerError)
			return
		}
		if targetQuery != "" {
			hostURL.RawQuery = targetQuery
		}

		// Use the host config's transport (includes service account auth)
		transport, err := rest.TransportFor(k.hostConfig)
		if err != nil {
			http.Error(w, fmt.Sprintf("error creating transport: %v", err), http.StatusInternalServerError)
			return
		}

		proxyHandler := proxy.NewUpgradeAwareHandler(hostURL, transport, true, false, &responder{})
		proxyHandler.UseRequestLocation = false
		proxyHandler.ServeHTTP(w, r)
	}
}

type responder struct{}

func (r *responder) Error(w http.ResponseWriter, req *http.Request, err error) {
	fmt.Printf("  [kubelet-shim] proxy error: %v\n", err)
	http.Error(w, fmt.Sprintf("proxy error: %v", err), http.StatusBadGateway)
}

// selfSignedTLSConfig generates a self-signed TLS certificate for the kubelet
// shim. The k3s API server connects to kubelet endpoints and typically does not
// verify the kubelet's certificate (InsecureSkipVerify or uses its own CA), so
// a self-signed cert is sufficient.
func selfSignedTLSConfig() (*tls.Config, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating key: %w", err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "vibecluster-kubelet-shim",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("0.0.0.0")},
		DNSNames:              []string{"localhost"},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("creating certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshalling key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("loading key pair: %w", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}
