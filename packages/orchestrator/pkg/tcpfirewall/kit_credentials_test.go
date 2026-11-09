//go:build linux

package tcpfirewall

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	e2b "github.com/docker/kits-e2b"
	"github.com/inetaf/tcpproxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/sandboxtypes"
)

type testSecrets struct {
	mu    sync.Mutex
	value string
}

func (s *testSecrets) Resolve(_ context.Context, project, name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if project != "project-a" || name != "key" || s.value == "" {
		return "", errors.New("missing")
	}

	return s.value, nil
}
func (s *testSecrets) set(value string) { s.mu.Lock(); defer s.mu.Unlock(); s.value = value }

func testInjector(t *testing.T) (*KitInjector, *testSecrets) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	require.NoError(t, err)
	priv, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	secrets := &testSecrets{value: "host-secret"}
	i, err := NewKitInjector(secrets, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: priv}))
	require.NoError(t, err)

	return i, secrets
}

func credentialPolicy() *orchestrator.SandboxNetworkEgressConfig {
	return &orchestrator.SandboxNetworkEgressConfig{KitPolicy: true, AllowedDomains: []string{"api.example.com"}, DeniedCidrs: []string{"0.0.0.0/0", "::/0"}, Rules: map[string]*orchestrator.SandboxNetworkDomainRules{
		"api.example.com": {Rules: []*orchestrator.SandboxNetworkRule{{Transform: &orchestrator.SandboxNetworkTransform{Headers: map[string]string{"Authorization": "Bearer ${e2b.secrets.key}"}}}}},
	}}
}

func credentialConn() egressConn {
	return egressConn{sbx: &sandbox.Sandbox{Metadata: &sandbox.Metadata{Config: sandbox.NewConfig(sandbox.Config{Network: &orchestrator.SandboxNetworkConfig{Egress: credentialPolicy()}}), Runtime: sandboxtypes.RuntimeMetadata{TeamID: "project-a"}}}, logger: logger.NewNopLogger(), metrics: NewMetrics(noop.NewMeterProvider())}
}

// Exercise real upstream TLS with certificate validation, a redirect, and an
// attempted Connection-header removal of the injected credential.
func TestNativeCredentialTLSUpstream(t *testing.T) {
	t.Parallel()
	i, secrets := testInjector(t)
	var calls atomic.Int64
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(t, "api.example.com", r.Host)
		expected, resolveErr := secrets.Resolve(r.Context(), "project-a", "key")
		assert.NoError(t, resolveErr)
		assert.Equal(t, "Bearer "+expected, r.Header.Get("Authorization"))
		w.Header().Set("Location", "https://evil.example.com/")
		w.WriteHeader(http.StatusFound)
	}))
	cert, err := i.certificate("api.example.com")
	require.NoError(t, err)
	upstream.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	upstream.StartTLS()
	t.Cleanup(upstream.Close)
	roots := x509.NewCertPool()
	roots.AddCert(i.ca.Leaf)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}}
	t.Cleanup(transport.CloseIdleConnections)
	c := credentialConn()
	handler := i.handler(c, "api.example.com", transport)
	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/messages", nil)
		r.Host = "api.example.com:443"
		r.Header.Set("Authorization", "guest-value")
		r.Header.Set("Connection", "Authorization")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		require.Equal(t, "guest-value", r.Header.Get("Authorization"), "incoming request must never contain the host secret")

		return w
	}
	w := request()
	require.Equal(t, http.StatusFound, w.Code)
	require.Equal(t, "https://evil.example.com/", w.Header().Get("Location"))
	require.EqualValues(t, 1, calls.Load(), "proxy must not follow redirects with injected headers")
	secrets.set("rotated")
	require.Equal(t, http.StatusFound, request().Code)
	secrets.set("")
	w = request()
	require.Equal(t, http.StatusBadGateway, w.Code)
	require.NotContains(t, w.Body.String(), "host-secret")
	require.EqualValues(t, 2, calls.Load())
	secrets.set("restored")
	c.sbx.Config.SetNetworkEgress(&orchestrator.SandboxNetworkEgressConfig{KitPolicy: true})
	require.Equal(t, http.StatusForbidden, request().Code)
	require.EqualValues(t, 2, calls.Load())
	// Upstream verification failure must never send a secret on an untrusted TLS link.
	c.sbx.Config.SetNetworkEgress(credentialPolicy())
	badTransport := transport.Clone()
	badTransport.TLSClientConfig = &tls.Config{RootCAs: x509.NewCertPool(), MinVersion: tls.VersionTLS12}
	t.Cleanup(badTransport.CloseIdleConnections)
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	r.Host = "api.example.com"
	w = httptest.NewRecorder()
	i.handler(c, "api.example.com", badTransport).ServeHTTP(w, r)
	require.Equal(t, http.StatusBadGateway, w.Code)
	require.EqualValues(t, 2, calls.Load())
}

type forbiddenTransport struct{ t *testing.T }

func (f forbiddenTransport) RoundTrip(*http.Request) (*http.Response, error) {
	f.t.Error("forbidden request reached upstream")

	return nil, errors.New("forbidden")
}

func TestNativeCredentialRequestAuthority(t *testing.T) {
	t.Parallel()
	i, _ := testInjector(t)
	for _, tc := range []struct{ name, host, url, method, upgrade, project string }{
		{"fronting", "evil.example.com", "/", "GET", "", "project-a"},
		{"port", "api.example.com:8443", "/", "GET", "", "project-a"},
		{"absolute target", "api.example.com", "https://evil.example.com/", "GET", "", "project-a"},
		{"tunnel", "api.example.com", "/", "CONNECT", "", "project-a"},
		{"upgrade", "api.example.com", "/", "GET", "websocket", "project-a"},
		{"other project", "api.example.com", "/", "GET", "", "project-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := credentialConn()
			c.sbx.Runtime.TeamID = tc.project
			r := httptest.NewRequestWithContext(t.Context(), tc.method, tc.url, nil)
			r.Host = tc.host
			r.Header.Set("Upgrade", tc.upgrade)
			w := httptest.NewRecorder()
			i.handler(c, "api.example.com", forbiddenTransport{t: t}).ServeHTTP(w, r)
			require.GreaterOrEqual(t, w.Code, 400)
		})
	}
}

func TestNativeCredentialGuestTLSAndPrivateUpstream(t *testing.T) {
	t.Parallel()
	i, _ := testInjector(t)
	server, client := net.Pipe()
	c := credentialConn()
	c.conn = server
	// localhost is used only to prove the production dialer refuses private IPs.
	policy := credentialPolicy()
	policy.AllowedDomains = []string{"localhost"}
	policy.Rules["localhost"] = policy.GetRules()["api.example.com"]
	c.sbx.Config.SetNetworkEgress(policy)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); i.serve(ctx, c, "localhost") }()
	roots := x509.NewCertPool()
	roots.AddCert(i.ca.Leaf)
	guest := tls.Client(client, &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}})
	defer guest.Close()
	require.NoError(t, guest.HandshakeContext(ctx))
	_, err := io.WriteString(guest, "GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")
	require.NoError(t, err)
	response, err := http.ReadResponse(bufio.NewReader(guest), nil)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusBadGateway, response.StatusCode)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NotContains(t, string(body), "host-secret")
	guest.Close()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("proxy connection leaked")
	}
}

func TestNativeCredentialPreflight(t *testing.T) {
	t.Parallel()
	i, _ := testInjector(t)
	p := &Proxy{kitInjector: i}
	plan := &e2b.ExecutionPlan{Network: e2b.NetworkCandidate{Runtime: e2b.EgressCandidate{Rules: map[string][]e2b.NetworkRule{"api.example.com": {{Transform: e2b.NetworkTransform{Headers: map[string]string{"Authorization": "Bearer ${e2b.secrets.key}"}}}}}}}}
	require.NoError(t, p.ValidateNativeKitCredentials(t.Context(), "project-a", plan))
	require.Error(t, p.ValidateNativeKitCredentials(t.Context(), "project-b", plan))
	require.Error(t, (&Proxy{}).ValidateNativeKitCredentials(t.Context(), "project-a", plan))
	for name := range strings.FieldsSeq("Host Connection Transfer-Encoding Te Trailer Upgrade Proxy-Authorization") {
		require.False(t, credentialHeader(name))
	}
}

func TestNativeCredentialPlaintextAndMissingBackendFailClosed(t *testing.T) {
	t.Parallel()
	for _, protocol := range []Protocol{ProtocolHTTP, ProtocolTLS} {
		t.Run(string(protocol), func(t *testing.T) {
			t.Parallel()
			server, client := net.Pipe()
			defer client.Close()
			c := credentialConn()
			c.protocol, c.dstPort, c.dstIP = protocol, 443, net.ParseIP("1.1.1.1")
			if protocol == ProtocolHTTP {
				c.dstPort = 80
				c.kitInjector, _ = testInjector(t)
			}
			c.conn = &tcpproxy.Conn{Conn: server, HostName: "api.example.com"}
			domainHandler(t.Context(), c)
			_ = client.SetReadDeadline(time.Now().Add(time.Second))
			_, err := client.Read(make([]byte, 1))
			require.ErrorIs(t, err, io.EOF)
		})
	}
}
