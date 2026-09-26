package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/influence/influence/internal/config"
)

// writeTestCertKey generates a self-signed certificate and matching private key,
// writes them to PEM files in a temp dir, and returns their paths.
func writeTestCertKey(t *testing.T) (certPath, keyPath string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}

	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}

	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")

	certOut, err := os.Create(certPath)
	if err != nil {
		t.Fatalf("creating cert file: %v", err)
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("encoding cert: %v", err)
	}
	certOut.Close()

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshaling key: %v", err)
	}
	keyOut, err := os.Create(keyPath)
	if err != nil {
		t.Fatalf("creating key file: %v", err)
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		t.Fatalf("encoding key: %v", err)
	}
	keyOut.Close()

	return certPath, keyPath
}

// TestTLSConfigMinVersion asserts the built tls.Config pins TLS 1.2 as the
// minimum negotiated version (Requirement 19.1).
func TestTLSConfigMinVersion(t *testing.T) {
	cert, key := writeTestCertKey(t)
	cfg := config.Config{TLS: true, TLSCert: cert, TLSKey: key}

	tlsCfg, err := TLSConfig(cfg)
	if err != nil {
		t.Fatalf("TLSConfig returned error: %v", err)
	}
	if tlsCfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %#x, want TLS 1.2 (%#x)", tlsCfg.MinVersion, tls.VersionTLS12)
	}
	if len(tlsCfg.Certificates) != 1 {
		t.Fatalf("Certificates length = %d, want 1", len(tlsCfg.Certificates))
	}
}

// TestTLSConfigDisabled asserts TLSConfig refuses to build when TLS is off.
func TestTLSConfigDisabled(t *testing.T) {
	if _, err := TLSConfig(config.Config{TLS: false}); err == nil {
		t.Fatal("TLSConfig with TLS disabled returned nil error, want error")
	}
}

// TestTLSConfigMissingKey asserts an unreadable/missing cert or key surfaces as
// an error (feeds Requirement 19.4 fail-to-start in task 2.2).
func TestTLSConfigMissingKey(t *testing.T) {
	cfg := config.Config{TLS: true, TLSCert: "/nonexistent/cert.pem", TLSKey: "/nonexistent/key.pem"}
	if _, err := TLSConfig(cfg); err == nil {
		t.Fatal("TLSConfig with missing cert/key returned nil error, want error")
	}
}

// TestTLSConfigEmptyPaths asserts empty cert/key paths are rejected.
func TestTLSConfigEmptyPaths(t *testing.T) {
	if _, err := TLSConfig(config.Config{TLS: true}); err == nil {
		t.Fatal("TLSConfig with empty paths returned nil error, want error")
	}
}

// TestBuildListenerPlaintext asserts a plaintext listener binds when TLS is off,
// and that it is a plain TCP listener (not TLS).
func TestBuildListenerPlaintext(t *testing.T) {
	cfg := config.Config{TLS: false, Port: 0}
	ln, err := BuildListener(cfg)
	if err != nil {
		t.Fatalf("BuildListener returned error: %v", err)
	}
	defer ln.Close()
	if _, ok := ln.Addr().(*net.TCPAddr); !ok {
		t.Errorf("listener addr = %T, want *net.TCPAddr", ln.Addr())
	}
}

// TestBuildListenerTLSServesOnlyTLS binds a TLS listener and asserts a TLS
// client can complete a handshake while a plaintext client cannot be served,
// which is the transport-level enforcement of Requirement 19.2.
func TestBuildListenerTLSServesOnlyTLS(t *testing.T) {
	cert, key := writeTestCertKey(t)
	cfg := config.Config{TLS: true, TLSCert: cert, TLSKey: key, Port: 0}

	ln, err := BuildListener(cfg)
	if err != nil {
		t.Fatalf("BuildListener returned error: %v", err)
	}
	defer ln.Close()

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}
	go srv.Serve(ln)
	defer srv.Close()

	addr := ln.Addr().String()

	// A TLS client (skipping verification of the self-signed cert) succeeds.
	tlsClient := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	resp, err := tlsClient.Get("https://" + addr + "/")
	if err != nil {
		t.Fatalf("TLS request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("TLS status = %d, want 200", resp.StatusCode)
	}

	// A plaintext HTTP client against the TLS listener must not reach the
	// handler. crypto/tls answers a plaintext request on a TLS port with a
	// 400-class response instead of the handler's 200, so the request is
	// refused rather than served (Requirement 19.2).
	plainClient := &http.Client{Timeout: 2 * time.Second}
	presp, perr := plainClient.Get("http://" + addr + "/")
	if perr == nil {
		defer presp.Body.Close()
		if presp.StatusCode == http.StatusOK {
			t.Errorf("plaintext request to TLS listener was served (status %d), want refusal", presp.StatusCode)
		}
	}
}

// TestPlaintextGuardRefusesNonTLS asserts the guard rejects a request without a
// TLS handshake when SSL is configured (Requirement 19.2).
func TestPlaintextGuardRefusesNonTLS(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	guard := PlaintextGuard(true, next)

	req := httptest.NewRequest(http.MethodGet, "http://example/", nil)
	req.TLS = nil // plaintext
	rec := httptest.NewRecorder()
	guard.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Errorf("plaintext request was served (status %d), want refusal", rec.Code)
	}
}

// TestPlaintextGuardAllowsTLS asserts the guard passes through a request that
// arrived over TLS when SSL is configured.
func TestPlaintextGuardAllowsTLS(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	guard := PlaintextGuard(true, next)

	req := httptest.NewRequest(http.MethodGet, "https://example/", nil)
	req.TLS = &tls.ConnectionState{} // simulates a completed handshake
	rec := httptest.NewRecorder()
	guard.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("TLS request status = %d, want 200", rec.Code)
	}
}

// TestPlaintextGuardPassThroughWhenDisabled asserts that when SSL is not
// configured the guard is a pass-through so plaintext is served normally.
func TestPlaintextGuardPassThroughWhenDisabled(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	guard := PlaintextGuard(false, next)

	req := httptest.NewRequest(http.MethodGet, "http://example/", nil)
	req.TLS = nil
	rec := httptest.NewRecorder()
	guard.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("plaintext status with TLS disabled = %d, want 200", rec.Code)
	}
}
