//go:build linux

package tcpfirewall

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"log"
	"maps"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	e2b "github.com/docker/kits-e2b"
	"golang.org/x/net/http/httpguts"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/kitcredentials"
)

// KitInjector terminates only declared credential destinations. Its CA key and
// secret values stay on the host; only the public CA is installed by envd.
type KitInjector struct {
	resolver kitcredentials.Resolver
	ca       tls.Certificate
	bundle   string
}

func NewKitInjector(resolver kitcredentials.Resolver, certPEM, keyPEM []byte) (*KitInjector, error) {
	ca, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, errors.New("invalid native Kit proxy CA")
	}
	leaf := ca.Leaf
	if resolver == nil || leaf == nil || !leaf.IsCA || leaf.KeyUsage&x509.KeyUsageCertSign == 0 || time.Now().Before(leaf.NotBefore) || time.Now().After(leaf.NotAfter) {
		return nil, errors.New("invalid native Kit proxy CA or resolver")
	}

	return &KitInjector{resolver: resolver, ca: ca, bundle: string(certPEM)}, nil
}

// ConfigureNativeKitCredentials is explicit and all-or-nothing. Default CE
// deployments retain their existing TCP passthrough behavior.
func (p *Proxy) ConfigureNativeKitCredentials() error {
	storePath, certPath, keyPath := os.Getenv("NATIVE_KIT_SECRETS_FILE"), os.Getenv("NATIVE_KIT_CA_CERT_FILE"), os.Getenv("NATIVE_KIT_CA_KEY_FILE")
	if storePath == "" && certPath == "" && keyPath == "" {
		return nil
	}
	store, err := kitcredentials.NewFileStore(storePath)
	if err != nil {
		return errors.New("native Kit secrets file must be valid, private and readable")
	}
	cert, err := os.ReadFile(certPath)
	if err != nil {
		return errors.New("native Kit CA certificate unavailable")
	}
	key, err := kitcredentials.ReadPrivateFile(keyPath)
	if err != nil {
		return errors.New("native Kit CA private key unavailable")
	}
	p.kitInjector, err = NewKitInjector(store, cert, key)

	return err
}

func (p *Proxy) SupportsNativeKitCredentials() bool { return p.kitInjector != nil }

// ValidateNativeKitCredentials runs before guest boot (including resume). Never
// resolve a secret in the template builder: templates may be shared with another
// project, whose own identically named binding must be used when it creates a VM.
func (p *Proxy) ValidateNativeKitCredentials(ctx context.Context, project string, plan *e2b.ExecutionPlan) error {
	for _, phase := range []e2b.EgressCandidate{plan.Network.Install, plan.Network.Runtime} {
		for _, rules := range phase.Rules {
			if p.kitInjector == nil {
				return errors.New("native Kit secret-injection backend unavailable")
			}
			for _, rule := range rules {
				for name, format := range rule.Transform.Headers {
					if !credentialHeader(name) {
						return errors.New("unsupported native Kit credential header")
					}
					if _, err := kitcredentials.Expand(ctx, p.kitInjector.resolver, project, format); err != nil {
						return err
					}
				}
			}
		}
	}

	return nil
}

func credentialHeader(name string) bool {
	if !httpguts.ValidHeaderFieldName(name) {
		return false
	}
	switch strings.ToLower(name) {
	case "host", "content-length", "transfer-encoding", "connection", "proxy-connection", "trailer", "te", "upgrade", "keep-alive", "proxy-authorization", "proxy-authenticate":
		return false
	}

	return true
}

func (i *KitInjector) certificate(host string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	until := now.Add(time.Hour)
	if until.After(i.ca.Leaf.NotAfter) {
		until = i.ca.Leaf.NotAfter
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: host}, DNSNames: []string{host}, NotBefore: now.Add(-time.Minute), NotAfter: until, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, i.ca.Leaf, &key.PublicKey, i.ca.PrivateKey)
	if err != nil {
		return tls.Certificate{}, err
	}

	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// handler rechecks host authority and the current phase for every request on a
// persistent TLS connection. ReverseProxy removes hop headers before Rewrite;
// injection therefore cannot be suppressed through a Connection header.
func (i *KitInjector) handler(c egressConn, host string, transport http.RoundTripper) http.Handler {
	proxy := &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme, r.Out.URL.Host, r.Out.Host = "https", net.JoinHostPort(host, "443"), host
			r.Out.Header.Del("Forwarded")
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "native Kit upstream unavailable", http.StatusBadGateway)
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authority := strings.ToLower(r.Host)
		policy := c.sbx.Config.GetNetworkEgress()
		if (authority != host && authority != host+":443") || r.URL.IsAbs() || r.Method == http.MethodConnect || r.Header.Get("Upgrade") != "" || !policy.GetKitPolicy() || !slices.Contains(policy.GetAllowedDomains(), host) {
			http.Error(w, "native Kit destination denied", http.StatusForbidden)

			return
		}
		headers := make(http.Header)
		for _, rule := range policy.GetRules()[host].GetRules() {
			for name, format := range rule.GetTransform().GetHeaders() {
				if !credentialHeader(name) {
					http.Error(w, "native Kit credential unavailable", http.StatusBadGateway)

					return
				}
				value, err := kitcredentials.Expand(r.Context(), i.resolver, c.sbx.Runtime.TeamID, format)
				if err != nil {
					http.Error(w, "native Kit credential unavailable", http.StatusBadGateway)

					return
				}
				headers.Set(name, value)
			}
		}
		// Clone the proxy for this request so secrets never live in shared state,
		// request contexts, logs, or the incoming request visible to middleware.
		requestProxy := *proxy
		requestProxy.Rewrite = func(p *httputil.ProxyRequest) {
			proxy.Rewrite(p)
			maps.Copy(p.Out.Header, headers)
		}
		requestProxy.ServeHTTP(w, r)
	})
}

func (i *KitInjector) serve(ctx context.Context, c egressConn, host string) {
	defer c.conn.Close()
	certificate, err := i.certificate(host)
	if err != nil {
		return
	}
	conn := tls.Server(c.conn, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}})
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(upstreamDialTimeout))
	if conn.HandshakeContext(ctx) != nil {
		return
	}
	if !strings.EqualFold(conn.ConnectionState().ServerName, host) {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	transport := &http.Transport{
		Proxy: nil, DialContext: upstreamDialer(ctx, c, net.JoinHostPort(host, "443"), true).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host},
		TLSHandshakeTimeout: upstreamDialTimeout, ResponseHeaderTimeout: upstreamDialTimeout, DisableKeepAlives: true,
	}
	defer transport.CloseIdleConnections()
	listener := &singleConnectionListener{conn: conn, done: make(chan struct{})}
	server := &http.Server{
		Handler: i.handler(c, host, transport), ReadHeaderTimeout: upstreamDialTimeout, IdleTimeout: upstreamDialTimeout, MaxHeaderBytes: 64 << 10,
		BaseContext: func(net.Listener) context.Context { return ctx }, ErrorLog: log.New(io.Discard, "", 0),
	}
	_ = server.Serve(listener)
}

// Serve must stay alive until http.Server closes its one accepted connection.
type singleConnectionListener struct {
	conn     net.Conn
	accepted bool
	done     chan struct{}
	once     sync.Once
}

func (l *singleConnectionListener) Accept() (net.Conn, error) {
	if l.accepted {
		<-l.done

		return nil, net.ErrClosed
	}
	l.accepted = true

	return &closingConnection{Conn: l.conn, close: l.Close}, nil
}

func (l *singleConnectionListener) Close() error {
	l.once.Do(func() { close(l.done); _ = l.conn.Close() })

	return nil
}
func (l *singleConnectionListener) Addr() net.Addr { return l.conn.LocalAddr() }

type closingConnection struct {
	net.Conn

	close func() error
}

func (c *closingConnection) Close() error { return c.close() }
