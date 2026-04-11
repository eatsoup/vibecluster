package kubeletshim

import (
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
)

func TestParseKubeletPath(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		subResource string
		wantNS      string
		wantPod     string
		wantCont    string
		wantErr     bool
	}{
		{
			name:        "containerLogs with container",
			path:        "/containerLogs/default/nginx-abc/nginx",
			subResource: "log",
			wantNS:      "default",
			wantPod:     "nginx-abc",
			wantCont:    "nginx",
		},
		{
			name:        "exec with container",
			path:        "/exec/myns/my-pod-xyz/sidecar",
			subResource: "exec",
			wantNS:      "myns",
			wantPod:     "my-pod-xyz",
			wantCont:    "sidecar",
		},
		{
			name:        "attach with container",
			path:        "/attach/default/foo/bar",
			subResource: "attach",
			wantNS:      "default",
			wantPod:     "foo",
			wantCont:    "bar",
		},
		{
			name:        "portforward has no container",
			path:        "/portForward/default/nginx-abc",
			subResource: "portforward",
			wantNS:      "default",
			wantPod:     "nginx-abc",
			wantCont:    "",
		},
		{
			name:        "logs without container is allowed (single-container pod)",
			path:        "/containerLogs/default/nginx-abc",
			subResource: "log",
			wantNS:      "default",
			wantPod:     "nginx-abc",
			wantCont:    "",
		},
		{
			name:        "too short",
			path:        "/exec/foo",
			subResource: "exec",
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns, pod, cont, err := parseKubeletPath(tt.path, tt.subResource)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got ns=%q pod=%q cont=%q", ns, pod, cont)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ns != tt.wantNS || pod != tt.wantPod || cont != tt.wantCont {
				t.Errorf("got (%q,%q,%q), want (%q,%q,%q)",
					ns, pod, cont, tt.wantNS, tt.wantPod, tt.wantCont)
			}
		})
	}
}

// TestHandleRewritesLogsRequest spins up a fake "host kube-apiserver" with
// httptest, points the shim's hostTransport at it, and verifies that an
// inbound kubelet-style /containerLogs/<vNS>/<vPod>/<container> request is
// rewritten to /api/v1/namespaces/<hostNS>/pods/<hostPod>/log with the
// container set as a query param, and that the bearer token is forwarded.
func TestHandleRewritesLogsRequest(t *testing.T) {
	var (
		gotPath  string
		gotQuery url.Values
		gotAuth  string
	)
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query()
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "log line\n")
	}))
	defer backend.Close()

	hostURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}

	// Use the test server's TLS-trusting client as the host transport so
	// the proxy doesn't have to validate the httptest cert.
	insecureRT := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}

	s := &Shim{
		cfg: Config{
			HostNamespace: "vc-test",
			TranslateName: func(name, namespace string) string {
				return "test-x-" + name + "-x-" + namespace
			},
		},
		hostURL:       hostURL,
		hostTransport: insecureRT,
		cachedTok:     "abc123",
	}

	// Sanity-check: the rest.Config code path that produces hostTransport
	// is exercised by New(), so make sure New is at least callable for a
	// well-formed config (we use a dummy config here — TransportFor only
	// needs Host).
	if _, err := New(Config{
		HostConfig:    &rest.Config{Host: backend.URL},
		HostNamespace: "vc-test",
		TranslateName: s.cfg.TranslateName,
		Port:          1, // satisfies validation; we never call Run
	}); err != nil {
		t.Fatalf("New(): %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/containerLogs/default/nginx-abc/nginx?follow=true&tailLines=5", nil)
	s.handle("log").ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	wantPath := "/api/v1/namespaces/vc-test/pods/test-x-nginx-abc-x-default/log"
	if gotPath != wantPath {
		t.Errorf("backend path = %q, want %q", gotPath, wantPath)
	}
	if gotQuery.Get("container") != "nginx" {
		t.Errorf("container query = %q, want %q", gotQuery.Get("container"), "nginx")
	}
	if gotQuery.Get("follow") != "true" {
		t.Errorf("follow query missing: %v", gotQuery)
	}
	if gotQuery.Get("tailLines") != "5" {
		t.Errorf("tailLines query missing: %v", gotQuery)
	}
	if gotAuth != "Bearer abc123" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer abc123")
	}

	body := rec.Body.String()
	if !strings.Contains(body, "log line") {
		t.Errorf("body = %q, want to contain log line", body)
	}
}

// TestHandleRewritesExecRequest verifies that the kubelet exec query
// parameters input/output/error are translated to the host kube-apiserver
// pod/exec subresource names stdin/stdout/stderr — without this rewrite the
// host apiserver rejects every kubectl exec call with "you must specify at
// least 1 of stdin, stdout, stderr". See issue #21.
func TestHandleRewritesExecRequest(t *testing.T) {
	var gotPath string
	var gotQuery url.Values
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query()
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	hostURL, _ := url.Parse(backend.URL)
	insecureRT := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}

	s := &Shim{
		cfg: Config{
			HostNamespace: "vc-test",
			TranslateName: func(name, namespace string) string {
				return "test-x-" + name + "-x-" + namespace
			},
		},
		hostURL:       hostURL,
		hostTransport: insecureRT,
	}

	rec := httptest.NewRecorder()
	// kube-apiserver translates PodExecOptions stdin/stdout/stderr to the
	// kubelet's input/output/error before proxying to the kubelet — this
	// is what we receive on the wire.
	req := httptest.NewRequest(http.MethodPost,
		"/exec/default/nginx-abc/nginx?command=sh&command=-c&command=echo+hi&input=1&output=1&error=1&tty=1", nil)
	s.handle("exec").ServeHTTP(rec, req)

	wantPath := "/api/v1/namespaces/vc-test/pods/test-x-nginx-abc-x-default/exec"
	if gotPath != wantPath {
		t.Errorf("backend path = %q, want %q", gotPath, wantPath)
	}
	if gotQuery.Get("stdin") != "1" {
		t.Errorf("stdin = %q, want 1 (input should be renamed to stdin)", gotQuery.Get("stdin"))
	}
	if gotQuery.Get("stdout") != "1" {
		t.Errorf("stdout = %q, want 1 (output should be renamed to stdout)", gotQuery.Get("stdout"))
	}
	if gotQuery.Get("stderr") != "1" {
		t.Errorf("stderr = %q, want 1 (error should be renamed to stderr)", gotQuery.Get("stderr"))
	}
	if gotQuery.Get("tty") != "1" {
		t.Errorf("tty = %q, want 1", gotQuery.Get("tty"))
	}
	if gotQuery.Has("input") || gotQuery.Has("output") || gotQuery.Has("error") {
		t.Errorf("kubelet-style params should be stripped, got %v", gotQuery)
	}
	if cmds := gotQuery["command"]; len(cmds) != 3 || cmds[0] != "sh" || cmds[1] != "-c" || cmds[2] != "echo hi" {
		t.Errorf("command = %v, want [sh -c echo hi]", cmds)
	}
	if gotQuery.Get("container") != "nginx" {
		t.Errorf("container = %q, want nginx", gotQuery.Get("container"))
	}
}

// TestHandleRewritesPortForwardRequest exercises the path rewrite without a
// container (portforward has no container component) and confirms the
// kubelet camelCase /portForward/ → host lowercase /portforward mapping.
func TestHandleRewritesPortForwardRequest(t *testing.T) {
	var gotPath string
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	hostURL, _ := url.Parse(backend.URL)
	insecureRT := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}

	s := &Shim{
		cfg: Config{
			HostNamespace: "vc-test",
			TranslateName: func(name, namespace string) string {
				return "test-x-" + name + "-x-" + namespace
			},
		},
		hostURL:       hostURL,
		hostTransport: insecureRT,
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/portForward/default/nginx-abc?ports=8080", nil)
	s.handle("portforward").ServeHTTP(rec, req)

	want := "/api/v1/namespaces/vc-test/pods/test-x-nginx-abc-x-default/portforward"
	if gotPath != want {
		t.Errorf("backend path = %q, want %q", gotPath, want)
	}
}
