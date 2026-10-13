//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	tokenURL   = "http://localhost:8081/realms/wagering/protocol/openid-connect/token"
	inputQueue = "wager-transactions.fifo"
)

var baseEnv = map[string]string{
	"DATABASE_URL":            "postgres://jungle:devpassword@localhost:5432/jungle_challenge?sslmode=disable",
	"OIDC_ISSUER":             "http://localhost:8081/realms/wagering",
	"OIDC_JWKS_URL":           "http://localhost:8081/realms/wagering/protocol/openid-connect/certs",
	"OIDC_AUDIENCE":           "wagering-api",
	"AWS_REGION":              "us-east-1",
	"AWS_ACCESS_KEY_ID":       "test",
	"AWS_SECRET_ACCESS_KEY":   "test",
	"AWS_ENDPOINT_URL":        "http://localhost:4566",
	"SQS_VISIBILITY_TIMEOUT":  "4s",
	"OUTBOX_LEASE":            "3s",
	"REFERENCE_BASE_BACKOFF":  "1s",
	"REFERENCE_MAX_BACKOFF":   "2s",
	"REFERENCE_POLL_INTERVAL": "200ms",
	"SHUTDOWN_TIMEOUT":        "10s",
}

var (
	binary string
	client = &http.Client{Timeout: 15 * time.Second}
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "wager-e2e")
	if err != nil {
		panic(err)
	}

	binary = filepath.Join(dir, "server")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}

	build := exec.Command("go", "build", "-o", binary, "../../cmd/server")
	build.Stderr = os.Stderr

	if err := build.Run(); err != nil {
		panic(fmt.Sprintf("build server: %v", err))
	}

	for key, value := range baseEnv {
		if os.Getenv(key) == "" {
			os.Setenv(key, value)
		}
	}

	code := m.Run()

	os.RemoveAll(dir)
	os.Exit(code)
}

type process struct {
	t    *testing.T
	cmd  *exec.Cmd
	url  string
	logs *bytes.Buffer
	dead bool
}

func startProcess(t *testing.T) *process {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()

	p := &process{t: t, url: "http://" + addr, logs: &bytes.Buffer{}}
	p.cmd = exec.Command(binary)
	p.cmd.Env = append(os.Environ(), "HTTP_ADDR="+addr)
	p.cmd.Stdout = p.logs
	p.cmd.Stderr = p.logs

	if err := p.cmd.Start(); err != nil {
		t.Fatalf("start process: %v", err)
	}

	t.Cleanup(p.kill)

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		res, err := client.Get(p.url + "/health/ready")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return p
			}
		}

		time.Sleep(100 * time.Millisecond)
	}

	t.Fatalf("process did not become ready:\n%s", p.logs.String())

	return nil
}

func (p *process) kill() {
	if p.dead {
		return
	}

	p.dead = true
	_ = p.cmd.Process.Kill()
	_ = p.cmd.Wait()
}

func startCluster(t *testing.T, n int) []*process {
	t.Helper()

	cluster := make([]*process, n)
	for i := range cluster {
		cluster[i] = startProcess(t)
	}

	return cluster
}

func token(t *testing.T, clientID string) string {
	t.Helper()

	res, err := client.PostForm(tokenURL, url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {clientID + "-secret"},
	})
	if err != nil {
		t.Fatalf("token request: %v", err)
	}
	defer res.Body.Close()

	var body struct {
		AccessToken string `json:"access_token"`
	}

	if err := json.NewDecoder(res.Body).Decode(&body); err != nil || body.AccessToken == "" {
		t.Fatalf("token for %s: status %d, %v", clientID, res.StatusCode, err)
	}

	return body.AccessToken
}

type response struct {
	status int
	body   map[string]any
}

func (r response) str(key string) string {
	value, _ := r.body[key].(string)
	return value
}

func (r response) amount(key string) string {
	money, _ := r.body[key].(map[string]any)
	value, _ := money["amount"].(string)

	return value
}

func (r response) errorCode() string {
	detail, _ := r.body["error"].(map[string]any)
	code, _ := detail["code"].(string)

	return code
}

func call(t *testing.T, method, target, bearer string, headers map[string]string, payload any) response {
	t.Helper()

	var reader io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequest(method, target, reader)
	if err != nil {
		t.Fatal(err)
	}

	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	res, err := client.Do(req)
	if err != nil {
		t.Errorf("%s %s: %v", method, target, err)
		return response{}
	}
	defer res.Body.Close()

	out := response{status: res.StatusCode}
	_ = json.NewDecoder(res.Body).Decode(&out.body)

	return out
}

type scenario struct {
	t        *testing.T
	internal string
	provider string
	walletID string
	playerID string
	run      string
}

func newScenario(t *testing.T, p *process, balance string) *scenario {
	t.Helper()

	s := &scenario{
		t:        t,
		internal: token(t, "wallet-service"),
		provider: token(t, "provider-a"),
		playerID: uuid.NewString(),
		run:      uuid.NewString()[:8],
	}

	res := call(t, http.MethodPost, p.url+"/wallets", s.internal, nil, map[string]any{
		"playerId":       s.playerID,
		"initialBalance": map[string]string{"amount": balance, "currency": "BRL"},
	})
	if res.status != http.StatusCreated {
		t.Fatalf("open wallet = %d %v", res.status, res.body)
	}

	s.walletID = res.str("id")

	return s
}

func (s *scenario) operation(externalID, kind, amount, reference string) map[string]any {
	op := map[string]any{
		"providerId":            "provider-a",
		"externalTransactionId": externalID + "-" + s.run,
		"playerId":              s.playerID,
		"walletId":              s.walletID,
		"roundId":               "round-987",
		"gameId":                "fortune-chimp",
		"kind":                  kind,
		"money":                 map[string]string{"amount": amount, "currency": "BRL"},
	}

	if reference != "" {
		op["referenceExternalTransactionId"] = reference + "-" + s.run
	}

	return op
}

func (s *scenario) key(externalID string) string {
	return "provider-a:" + externalID + "-" + s.run
}

func (s *scenario) submit(p *process, externalID, kind, amount, reference string) response {
	s.t.Helper()

	return call(s.t, http.MethodPost, p.url+"/wagering/transactions", s.provider,
		map[string]string{"Idempotency-Key": s.key(externalID)},
		s.operation(externalID, kind, amount, reference))
}

func (s *scenario) reconciled(p *process, balance string, entries int) {
	s.t.Helper()

	res := call(s.t, http.MethodPost, p.url+"/wallets/"+s.walletID+"/reconciliation", s.internal, nil, nil)
	if res.status != http.StatusOK {
		s.t.Fatalf("reconciliation = %d %v", res.status, res.body)
	}

	if consistent, _ := res.body["consistent"].(bool); !consistent {
		s.t.Errorf("wallet inconsistent: %v", res.body)
	}
	if got := res.amount("storedBalance"); got != balance {
		s.t.Errorf("stored balance = %s, want %s", got, balance)
	}
	if got, _ := res.body["checkedEntries"].(float64); int(got) != entries {
		s.t.Errorf("ledger entries = %v, want %d", got, entries)
	}
}

func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(200 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for: %s", what)
}

func database(t *testing.T) *pgxpool.Pool {
	t.Helper()

	pool, err := pgxpool.New(context.Background(), os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	return pool
}

func (s *scenario) pendingEvents(pool *pgxpool.Pool) (pending, total int) {
	s.t.Helper()

	err := pool.QueryRow(context.Background(), `
		SELECT COUNT(*) FILTER (WHERE o.published_at IS NULL), COUNT(*)
		  FROM outbox_events o
		  LEFT JOIN wager_transactions w ON w.id = o.aggregate_id
		 WHERE o.aggregate_id = $1 OR w.wallet_id = $1`, s.walletID).Scan(&pending, &total)
	if err != nil {
		s.t.Fatal(err)
	}

	return pending, total
}

func TestAuthenticationAndProviderIsolation(t *testing.T) {
	p := startProcess(t)
	s := newScenario(t, p, "100.00")

	other := token(t, "provider-b")
	bet := s.submit(p, "bet-1", "BET", "25.00", "")
	if bet.status != http.StatusOK {
		t.Fatalf("bet = %d %v", bet.status, bet.body)
	}

	transactionURL := p.url + "/wagering/transactions/" + bet.str("transactionId")
	walletURL := p.url + "/wallets/" + s.walletID

	parts := strings.Split(s.provider, ".")
	forged, _ := json.Marshal(map[string]any{
		"iss": "http://localhost:8081/realms/wagering", "aud": "wagering-api", "exp": time.Now().Add(time.Hour).Unix(),
		"provider_id": "provider-a", "realm_access": map[string]any{"roles": []string{"provider", "internal"}},
	})
	tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString(forged) + "." + parts[2]

	expired := token(t, "provider-short-lived")
	time.Sleep(3 * time.Second)

	unauthenticated := map[string]string{
		"missing token":   "",
		"garbage token":   "not.a.token",
		"wrong audience":  token(t, "other-audience"),
		"tampered claims": tampered,
		"expired token":   expired,
	}

	for name, bearer := range unauthenticated {
		for _, target := range []string{transactionURL, walletURL} {
			if res := call(t, http.MethodGet, target, bearer, nil, nil); res.status != http.StatusUnauthorized {
				t.Errorf("%s on %s = %d, want 401", name, target, res.status)
			}
		}

		res := call(t, http.MethodPost, p.url+"/wagering/transactions", bearer,
			map[string]string{"Idempotency-Key": s.key("bet-unauth")}, s.operation("bet-unauth", "BET", "10.00", ""))
		if res.status != http.StatusUnauthorized {
			t.Errorf("%s on submit = %d, want 401", name, res.status)
		}
	}

	for _, check := range []struct {
		name, method, target string
	}{
		{"read wallet", http.MethodGet, walletURL},
		{"read ledger", http.MethodGet, walletURL + "/ledger"},
		{"reconcile", http.MethodPost, walletURL + "/reconciliation"},
		{"open wallet", http.MethodPost, p.url + "/wallets"},
	} {
		if res := call(t, check.method, check.target, s.provider, nil, map[string]any{}); res.status != http.StatusForbidden {
			t.Errorf("provider %s = %d, want 403", check.name, res.status)
		}
	}

	if res := call(t, http.MethodPost, p.url+"/wagering/transactions", s.internal,
		map[string]string{"Idempotency-Key": s.key("bet-internal")}, s.operation("bet-internal", "BET", "10.00", "")); res.status != http.StatusForbidden {
		t.Errorf("internal submit = %d, want 403", res.status)
	}

	if res := call(t, http.MethodPost, p.url+"/wagering/transactions", other,
		map[string]string{"Idempotency-Key": s.key("bet-b")}, s.operation("bet-b", "BET", "10.00", "")); res.status != http.StatusForbidden {
		t.Errorf("provider-b acting as provider-a = %d, want 403", res.status)
	}

	replay := call(t, http.MethodPost, p.url+"/wagering/transactions", other,
		map[string]string{"Idempotency-Key": s.key("bet-1")}, s.operation("bet-1", "BET", "25.00", ""))
	if replay.status != http.StatusForbidden || replay.body["balance"] != nil {
		t.Errorf("provider-b replay of provider-a bet = %d %v, want 403 without data", replay.status, replay.body)
	}

	if res := call(t, http.MethodGet, transactionURL, other, nil, nil); res.status != http.StatusNotFound {
		t.Errorf("provider-b reading provider-a transaction = %d, want 404", res.status)
	}

	byExternal := p.url + "/providers/provider-a/wagering/transactions/bet-1-" + s.run
	if res := call(t, http.MethodGet, byExternal, other, nil, nil); res.status != http.StatusForbidden {
		t.Errorf("provider-b reading provider-a external id = %d, want 403", res.status)
	}

	for name, bearer := range map[string]string{"owner": s.provider, "internal": s.internal} {
		if res := call(t, http.MethodGet, transactionURL, bearer, nil, nil); res.status != http.StatusOK || res.str("status") != "PROCESSED" {
			t.Errorf("%s reading the transaction = %d %v", name, res.status, res.body)
		}
	}
	if res := call(t, http.MethodGet, byExternal, s.provider, nil, nil); res.status != http.StatusOK {
		t.Errorf("owner reading by external id = %d", res.status)
	}

	s.reconciled(p, "75.00", 2)
}

func TestSameBetFiftyTimesAcrossProcesses(t *testing.T) {
	cluster := startCluster(t, 3)
	s := newScenario(t, cluster[0], "100.00")

	const attempts = 50

	results := make([]response, attempts)

	var wg sync.WaitGroup
	for i := range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = s.submit(cluster[i%len(cluster)], "bet-1", "BET", "25.00", "")
		}()
	}
	wg.Wait()

	originals := 0
	for i, res := range results {
		if res.status != http.StatusOK || res.str("status") != "PROCESSED" || res.amount("balance") != "75.00" {
			t.Fatalf("attempt %d = %d %v", i, res.status, res.body)
		}
		if res.str("transactionId") != results[0].str("transactionId") {
			t.Errorf("attempt %d returned another transaction", i)
		}
		if replay, _ := res.body["idempotentReplay"].(bool); !replay {
			originals++
		}
	}

	if originals != 1 {
		t.Errorf("non-replay responses = %d, want 1", originals)
	}

	s.reconciled(cluster[1], "75.00", 2)
}

func TestTwoBetsRaceAcrossProcesses(t *testing.T) {
	cluster := startCluster(t, 3)
	s := newScenario(t, cluster[0], "100.00")

	const resends = 6

	results := make([]response, 2*resends)

	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = s.submit(cluster[i%len(cluster)], fmt.Sprintf("bet-%d", i%2), "BET", "80.00", "")
		}()
	}
	wg.Wait()

	outcomes := map[string]string{}
	for i, res := range results {
		bet := fmt.Sprintf("bet-%d", i%2)

		summary := fmt.Sprintf("%d %s %s", res.status, res.str("status"), res.str("failureCode"))
		if previous, seen := outcomes[bet]; seen && previous != summary {
			t.Errorf("%s diverged between resends: %q vs %q", bet, previous, summary)
		}

		outcomes[bet] = summary
	}

	processed, rejected := 0, 0
	for _, summary := range outcomes {
		switch summary {
		case "200 PROCESSED ":
			processed++
		case "422 REJECTED INSUFFICIENT_FUNDS":
			rejected++
		}
	}

	if processed != 1 || rejected != 1 {
		t.Fatalf("outcomes = %v, want one PROCESSED and one REJECTED/INSUFFICIENT_FUNDS", outcomes)
	}

	s.reconciled(cluster[2], "20.00", 2)
}

func sqsClient(t *testing.T) (*sqs.Client, string) {
	t.Helper()

	cfg, err := awsconfig.LoadDefaultConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	sqsAPI := sqs.NewFromConfig(cfg)

	out, err := sqsAPI.GetQueueUrl(context.Background(), &sqs.GetQueueUrlInput{QueueName: aws.String(inputQueue)})
	if err != nil {
		t.Fatalf("queue %s: %v (run sqs-init)", inputQueue, err)
	}

	return sqsAPI, aws.ToString(out.QueueUrl)
}

func (s *scenario) enqueue(sqsAPI *sqs.Client, queueURL, messageID, externalID, kind, amount, reference string) {
	s.t.Helper()

	data := s.operation(externalID, kind, amount, reference)
	data["idempotencyKey"] = s.key(externalID)

	body, err := json.Marshal(map[string]any{
		"messageId":  messageID,
		"type":       "WagerTransactionRequested",
		"occurredAt": time.Now().UTC().Format(time.RFC3339Nano),
		"data":       data,
	})
	if err != nil {
		s.t.Fatal(err)
	}

	_, err = sqsAPI.SendMessage(context.Background(), &sqs.SendMessageInput{
		QueueUrl:               aws.String(queueURL),
		MessageBody:            aws.String(string(body)),
		MessageGroupId:         aws.String(s.walletID),
		MessageDeduplicationId: aws.String(messageID),
	})
	if err != nil {
		s.t.Fatalf("send message: %v", err)
	}
}

func TestSameOperationThroughHTTPAndSQS(t *testing.T) {
	cluster := startCluster(t, 3)
	s := newScenario(t, cluster[0], "100.00")
	sqsAPI, queueURL := sqsClient(t)
	pool := database(t)

	const perChannel = 10

	var wg sync.WaitGroup
	for i := range perChannel {
		wg.Add(2)

		go func() {
			defer wg.Done()
			s.enqueue(sqsAPI, queueURL, fmt.Sprintf("msg-%s-%d", s.run, i), "bet-1", "BET", "25.00", "")
		}()

		go func() {
			defer wg.Done()
			if res := s.submit(cluster[i%len(cluster)], "bet-1", "BET", "25.00", ""); res.status != http.StatusOK {
				t.Errorf("http submit %d = %d %v", i, res.status, res.body)
			}
		}()
	}
	wg.Wait()

	eventually(t, 60*time.Second, "all messages consumed", func() bool {
		var completed int

		err := pool.QueryRow(context.Background(),
			`SELECT COUNT(*) FROM inbox_messages WHERE message_id LIKE $1 AND completed_at IS NOT NULL`,
			"msg-"+s.run+"-%").Scan(&completed)

		return err == nil && completed == perChannel
	})

	var transactions int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM wager_transactions WHERE wallet_id = $1 AND kind = 'BET'`, s.walletID).Scan(&transactions); err != nil {
		t.Fatal(err)
	}
	if transactions != 1 {
		t.Errorf("bet transactions = %d, want 1", transactions)
	}

	s.reconciled(cluster[1], "75.00", 2)
}

func TestRestartPreservesIdempotencyAndPendingReferences(t *testing.T) {
	cluster := startCluster(t, 3)
	s := newScenario(t, cluster[0], "100.00")

	if res := s.submit(cluster[0], "bet-1", "BET", "25.00", ""); res.status != http.StatusOK {
		t.Fatalf("bet-1 = %d %v", res.status, res.body)
	}

	pending := s.submit(cluster[1], "refund-2", "REFUND", "40.00", "bet-2")
	if pending.status != http.StatusAccepted || pending.str("status") != "PENDING_REFERENCE" {
		t.Fatalf("refund before reference = %d %v", pending.status, pending.body)
	}

	for _, p := range cluster {
		p.kill()
	}

	survivor := startProcess(t)

	replay := s.submit(survivor, "bet-1", "BET", "25.00", "")
	if replay.status != http.StatusOK || replay.amount("balance") != "75.00" || replay.body["idempotentReplay"] != true {
		t.Errorf("replay after restart = %d %v", replay.status, replay.body)
	}

	again := s.submit(survivor, "refund-2", "REFUND", "40.00", "bet-2")
	if again.str("transactionId") != pending.str("transactionId") || again.body["idempotentReplay"] != true {
		t.Errorf("pending replay after restart = %d %v", again.status, again.body)
	}

	if res := s.submit(survivor, "bet-2", "BET", "40.00", ""); res.status != http.StatusOK || res.amount("balance") != "35.00" {
		t.Fatalf("bet-2 = %d %v", res.status, res.body)
	}

	refundURL := survivor.url + "/wagering/transactions/" + pending.str("transactionId")

	eventually(t, 30*time.Second, "pending refund resolved after restart", func() bool {
		res := call(t, http.MethodGet, refundURL, s.provider, nil, nil)
		return res.str("status") == "PROCESSED" && res.amount("balance") == "75.00"
	})

	s.reconciled(survivor, "75.00", 4)
}

func TestAbruptKillLosesNothing(t *testing.T) {
	cluster := startCluster(t, 3)
	s := newScenario(t, cluster[0], "100.00")
	sqsAPI, queueURL := sqsClient(t)
	pool := database(t)

	const bets = 30

	for i := range bets {
		s.enqueue(sqsAPI, queueURL, fmt.Sprintf("msg-%s-%d", s.run, i), fmt.Sprintf("bet-%d", i), "BET", "1.00", "")
	}

	time.Sleep(300 * time.Millisecond)
	cluster[0].kill()
	time.Sleep(300 * time.Millisecond)
	cluster[1].kill()

	survivor := cluster[2]

	eventually(t, 90*time.Second, "every bet processed exactly once", func() bool {
		var processed int

		err := pool.QueryRow(context.Background(), `
			SELECT COUNT(*) FROM wager_transactions
			 WHERE wallet_id = $1 AND kind = 'BET' AND status = 'PROCESSED'`, s.walletID).Scan(&processed)

		return err == nil && processed == bets
	})

	s.reconciled(survivor, "70.00", 1+bets)

	eventually(t, 60*time.Second, "every committed event published", func() bool {
		pending, total := s.pendingEvents(pool)
		return pending == 0 && total == 2+2*bets
	})

	for i := range bets {
		res := s.submit(survivor, fmt.Sprintf("bet-%d", i), "BET", "1.00", "")
		if res.status != http.StatusOK || res.body["idempotentReplay"] != true {
			t.Fatalf("resend of bet-%d = %d %v", i, res.status, res.body)
		}
	}

	s.reconciled(survivor, "70.00", 1+bets)
}
