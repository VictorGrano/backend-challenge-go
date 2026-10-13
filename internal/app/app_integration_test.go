//go:build integration

package app

import (
	"context"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.uber.org/fx"
)

func setTestEnv(t *testing.T, addr string) {
	t.Helper()

	defaults := map[string]string{
		"DATABASE_URL":          "postgres://jungle:devpassword@localhost:5432/jungle_challenge?sslmode=disable",
		"OIDC_ISSUER":           "http://localhost:8081/realms/wagering",
		"OIDC_JWKS_URL":         "http://localhost:8081/realms/wagering/protocol/openid-connect/certs",
		"OIDC_AUDIENCE":         "wagering-api",
		"AWS_REGION":            "us-east-1",
		"AWS_ACCESS_KEY_ID":     "test",
		"AWS_SECRET_ACCESS_KEY": "test",
		"AWS_ENDPOINT_URL":      "http://localhost:4566",
	}

	for key, value := range defaults {
		if os.Getenv(key) == "" {
			t.Setenv(key, value)
		}
	}

	t.Setenv("HTTP_ADDR", addr)
}

func freeAddr(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	return listener.Addr().String()
}

func appGoroutines() int {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]

	count := 0
	for _, stack := range strings.Split(string(buf), "\n\n") {
		for _, marker := range []string{
			"backend-challenge-go/internal/",
			"jackc/pgx/v5/pgxpool",
			"net/http.(*Server).Serve",
		} {
			if strings.Contains(stack, marker) && !strings.Contains(stack, "app.appGoroutines") &&
				!strings.Contains(stack, "testing.tRunner") {
				count++
				break
			}
		}
	}

	return count
}

func TestApplicationStartsAndStopsCleanly(t *testing.T) {
	addr := freeAddr(t)
	setTestEnv(t, addr)

	before := appGoroutines()

	application := New(fx.StartTimeout(30*time.Second), fx.StopTimeout(20*time.Second))
	if err := application.Err(); err != nil {
		t.Fatalf("build: %v", err)
	}

	startCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := application.Start(startCtx); err != nil {
		t.Fatalf("start: %v", err)
	}

	stopped := false
	defer func() {
		if !stopped {
			_ = application.Stop(context.Background())
		}
	}()

	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()

	for _, path := range []string{"/health/live", "/health/ready", "/metrics"} {
		res, err := client.Get("http://" + addr + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		res.Body.Close()

		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, res.StatusCode)
		}
	}

	res, err := client.Get("http://" + addr + "/wallets/00000000-0000-0000-0000-000000000000")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated request = %d, want 401", res.StatusCode)
	}

	if running := appGoroutines(); running <= before {
		t.Errorf("no application goroutines detected while running (%d)", running)
	}

	stopCtx, cancelStop := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelStop()

	started := time.Now()
	if err := application.Stop(stopCtx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	stopped = true
	t.Logf("stopped in %s", time.Since(started).Round(time.Millisecond))

	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		conn.Close()
		t.Error("port still accepting connections after stop")
	}

	client.CloseIdleConnections()

	deadline := time.Now().Add(5 * time.Second)
	for appGoroutines() > before && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}

	if leaked := appGoroutines() - before; leaked > 0 {
		buf := make([]byte, 1<<20)
		t.Errorf("%d application goroutines still running after stop:\n%s", leaked, buf[:runtime.Stack(buf, true)])
	}
}

func TestApplicationFailsFastWithoutDatabase(t *testing.T) {
	setTestEnv(t, freeAddr(t))
	t.Setenv("DATABASE_URL", "postgres://jungle:devpassword@127.0.0.1:1/none?sslmode=disable&connect_timeout=2")

	application := New(fx.NopLogger)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := application.Start(ctx); err == nil {
		_ = application.Stop(context.Background())
		t.Fatal("application started without a database")
	}
}

func TestApplicationRejectsInvalidConfiguration(t *testing.T) {
	setTestEnv(t, freeAddr(t))
	t.Setenv("OIDC_ISSUER", "")
	t.Setenv("SQS_CONCURRENCY", "zero")

	err := New(fx.NopLogger).Err()
	if err == nil {
		t.Fatal("application built with invalid configuration")
	}

	for _, want := range []string{"OIDC_ISSUER is required", "SQS_CONCURRENCY must be an integer"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}
