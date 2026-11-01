package app

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

// These tests run the real manager against envtest's kube-apiserver. They need
// the binaries scripts/go-coverage.sh and `make test` fetch with setup-envtest
// (KUBEBUILDER_ASSETS).

var testCfg *rest.Config

func TestMain(m *testing.M) {
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot start envtest:", err)
		os.Exit(1)
	}
	testCfg = cfg
	code := m.Run()
	if err := env.Stop(); err != nil {
		fmt.Fprintln(os.Stderr, "cannot stop envtest:", err)
		code = 1
	}
	os.Exit(code)
}

// skipNameValidation lets several managers register the same controller name
// in one test process (controller-runtime otherwise rejects the second).
func skipNameValidation(o *ctrl.Options) {
	skip := true
	o.Controller = config.Controller{SkipNameValidation: &skip}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

func getConfig() (*rest.Config, error) { return testCfg, nil }

// start runs the operator in the background and returns a stop function that
// cancels it and returns Run's result.
func start(t *testing.T, args []string, opts ...Option) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, args, getConfig, io.Discard, opts...) }()
	var once bool
	return func() error {
		if once {
			return nil
		}
		once = true
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(30 * time.Second):
			return errors.New("Run did not return after cancel")
		}
	}
}

func waitForOK(t *testing.T, url string) {
	t.Helper()
	g := NewWithT(t)
	g.Eventually(func() (int, error) {
		resp, err := http.Get(url) //nolint:gosec,noctx // a test probing its own server
		if err != nil {
			return 0, err
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode, nil
	}, 30*time.Second, 100*time.Millisecond).Should(Equal(http.StatusOK))
}

func writeCert(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func(name, kind string, b []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: b}), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("tls.crt", "CERTIFICATE", der)
	write("tls.key", "EC PRIVATE KEY", keyDER)
	return dir
}

func TestRunRejectsUnknownFlags(t *testing.T) {
	g := NewWithT(t)
	err := Run(context.Background(), []string{"--no-such-flag"}, getConfig, io.Discard)
	g.Expect(err).To(HaveOccurred())
}

func TestProbeOnlyServesWithoutACluster(t *testing.T) {
	g := NewWithT(t)
	addr := freeAddr(t)
	failConfig := func() (*rest.Config, error) { return nil, errors.New("must not be called") }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, []string{"--probe-only", "--health-probe-bind-address=" + addr}, failConfig, io.Discard)
	}()
	waitForOK(t, "http://"+addr+"/healthz")
	waitForOK(t, "http://"+addr+"/readyz")
	cancel()
	g.Eventually(done, 10*time.Second).Should(Receive(BeNil()))
}

func TestProbeOnlyReportsAListenError(t *testing.T) {
	g := NewWithT(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	g.Expect(err).NotTo(HaveOccurred())
	defer func() { _ = l.Close() }()

	err = Run(context.Background(), []string{"--probe-only", "--health-probe-bind-address=" + l.Addr().String()}, getConfig, io.Discard)
	g.Expect(err).To(HaveOccurred())
}

func TestProbeOnlyDefaultsFromTheEnvironment(t *testing.T) {
	g := NewWithT(t)
	t.Setenv("MANAGER_PROBE_ONLY", "true")
	s, err := parseFlags(nil, io.Discard)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(s.probeOnly).To(BeTrue())
}

func TestRunReportsAConfigError(t *testing.T) {
	g := NewWithT(t)
	boom := errors.New("no kubeconfig")
	err := Run(context.Background(), nil, func() (*rest.Config, error) { return nil, boom }, io.Discard)
	g.Expect(err).To(MatchError(boom))
}

func TestRunReportsAManagerError(t *testing.T) {
	g := NewWithT(t)
	// Leader election outside a cluster needs --leader-election-namespace.
	err := Run(context.Background(), []string{"--leader-elect", "--health-probe-bind-address=0", "--metrics-bind-address=0"}, getConfig, io.Discard)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("create the manager"))
}

func TestRunReportsAStartError(t *testing.T) {
	g := NewWithT(t)
	// The webhook server cannot start without its certificate.
	args := []string{
		"--health-probe-bind-address=0", "--metrics-bind-address=0",
		"--webhook-port=" + portOf(freeAddr(t)), "--webhook-cert-path=" + t.TempDir(),
	}
	err := Run(context.Background(), args, getConfig, io.Discard, skipNameValidation)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("run the manager"))
}

func portOf(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		panic(err)
	}
	return port
}

func TestRunServesSecuredMetricsAndWebhooks(t *testing.T) {
	g := NewWithT(t)
	probe, metrics, hook := freeAddr(t), freeAddr(t), freeAddr(t)
	certs := writeCert(t)
	stop := start(t, []string{
		"--health-probe-bind-address=" + probe,
		"--metrics-bind-address=" + metrics, "--metrics-cert-path=" + certs,
		"--webhook-port=" + portOf(hook), "--webhook-cert-path=" + certs,
		"--leader-elect", "--leader-election-namespace=default",
		"--enable-http2", "--zap-devel",
	}, skipNameValidation)
	defer func() { _ = stop() }()

	waitForOK(t, "http://"+probe+"/healthz")
	waitForOK(t, "http://"+probe+"/readyz")

	// /metrics demands a bearer token: the filter is the authentication layer.
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // self-signed test certificate
	g.Eventually(func() (int, error) {
		resp, err := client.Get("https://" + metrics + "/metrics") //nolint:noctx // a test probing its own server
		if err != nil {
			return 0, err
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode, nil
	}, 30*time.Second, 100*time.Millisecond).Should(Equal(http.StatusUnauthorized))

	g.Expect(stop()).To(Succeed())
}

func TestRunWithPlainMetricsAndNoWebhooks(t *testing.T) {
	g := NewWithT(t)
	probe, metrics := freeAddr(t), freeAddr(t)
	stop := start(t, []string{
		"--health-probe-bind-address=" + probe,
		"--metrics-bind-address=" + metrics, "--metrics-secure=false",
		"--webhook-port=-1",
	}, skipNameValidation)
	defer func() { _ = stop() }()

	waitForOK(t, "http://"+probe+"/healthz")
	waitForOK(t, "http://"+metrics+"/metrics")
	g.Expect(stop()).To(Succeed())
}

func TestRunRejectsASecondControllerWithTheSameName(t *testing.T) {
	g := NewWithT(t)
	args := []string{"--health-probe-bind-address=0", "--metrics-bind-address=0", "--webhook-port=-1"}

	// Names are checked process-wide, so the first run registers "greeting"
	// and the second must fail while setting up the controller.
	stop := start(t, args)
	defer func() { _ = stop() }()
	time.Sleep(2 * time.Second)
	g.Expect(stop()).To(Succeed())

	err := Run(context.Background(), args, getConfig, io.Discard)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("set up the greeting controller"))
}

type fakeChecks struct{ healthzErr, readyzErr error }

func (f fakeChecks) AddHealthzCheck(string, healthz.Checker) error { return f.healthzErr }
func (f fakeChecks) AddReadyzCheck(string, healthz.Checker) error  { return f.readyzErr }

func TestAddChecks(t *testing.T) {
	g := NewWithT(t)
	boom := errors.New("boom")
	g.Expect(addChecks(fakeChecks{})).To(Succeed())
	g.Expect(addChecks(fakeChecks{healthzErr: boom})).To(MatchError(boom))
	g.Expect(addChecks(fakeChecks{readyzErr: boom})).To(MatchError(boom))
}
