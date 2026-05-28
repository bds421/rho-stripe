package webhooks

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFetchStripeWebhookIPs_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"WEBHOOKS":["3.18.12.63","54.187.205.235"]}`))
	}))
	t.Cleanup(srv.Close)
	got, err := fetchFromURL(t.Context(), srv.URL, nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got.IPs) != 2 {
		t.Fatalf("IPs: %v", got.IPs)
	}
	cidrs := got.CIDRs()
	if len(cidrs) != 2 || cidrs[0] != "3.18.12.63/32" || cidrs[1] != "54.187.205.235/32" {
		t.Errorf("CIDRs: %v", cidrs)
	}
	if got.Source != srv.URL {
		t.Errorf("Source: %s, want %s", got.Source, srv.URL)
	}
}

func TestFetchStripeWebhookIPs_Rejects500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	_, err := fetchFromURL(t.Context(), srv.URL, nil)
	if err == nil || !strings.Contains(err.Error(), "status=500") {
		t.Fatalf("want 500 error; got %v", err)
	}
}

func TestFetchStripeWebhookIPs_RejectsEmptyList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"WEBHOOKS":[]}`))
	}))
	t.Cleanup(srv.Close)
	_, err := fetchFromURL(t.Context(), srv.URL, nil)
	if err == nil || !strings.Contains(err.Error(), "empty WEBHOOKS list") {
		t.Fatalf("want empty-list rejection; got %v", err)
	}
}

func TestFetchStripeWebhookIPs_RejectsMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	t.Cleanup(srv.Close)
	_, err := fetchFromURL(t.Context(), srv.URL, nil)
	if err == nil || !strings.Contains(err.Error(), "parse JSON") {
		t.Fatalf("want parse error; got %v", err)
	}
}

func TestIPRefresher_StartAppliesAllowlist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"WEBHOOKS":["10.0.0.1","10.0.0.2"]}`))
	}))
	t.Cleanup(srv.Close)

	wh := newFakeWebhooksForIPTest()

	// Use the exported Start path with the test server URL.
	r := newIPRefresherForURL(srv.URL, 24*time.Hour, nil)
	if err := r.Start(t.Context(), wh); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(r.Stop)

	got := wh.snapshotCIDRs()
	gotStrs := []string{}
	for _, n := range got {
		gotStrs = append(gotStrs, n.String())
	}
	if len(got) != 2 || gotStrs[0] != "10.0.0.1/32" || gotStrs[1] != "10.0.0.2/32" {
		t.Errorf("CIDRs not applied: %v", gotStrs)
	}
	last, ok := r.Last()
	if !ok || len(last.IPs) != 2 {
		t.Errorf("Last() did not record fetch")
	}
}

func TestIPRefresher_StartReturnsFetchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	wh := newFakeWebhooksForIPTest()
	r := newIPRefresherForURL(srv.URL, 24*time.Hour, nil)
	err := r.Start(t.Context(), wh)
	if err == nil {
		t.Fatal("Start should return the initial fetch error")
	}
}

func TestIPRefresher_StartTwiceRejects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"WEBHOOKS":["10.0.0.1"]}`))
	}))
	t.Cleanup(srv.Close)
	wh := newFakeWebhooksForIPTest()
	r := newIPRefresherForURL(srv.URL, 24*time.Hour, nil)
	if err := r.Start(t.Context(), wh); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	t.Cleanup(r.Stop)
	if err := r.Start(t.Context(), wh); err == nil || !strings.Contains(err.Error(), "already started") {
		t.Errorf("second Start should reject; got %v", err)
	}
}

func TestIPRefresher_OnErrorFiresOnBackgroundFailure(t *testing.T) {
	var calls atomic.Int32
	flip := atomic.Bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if flip.Load() {
			http.Error(w, "later failure", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"WEBHOOKS":["10.0.0.1"]}`))
	}))
	t.Cleanup(srv.Close)

	wh := newFakeWebhooksForIPTest()
	r := newIPRefresherForURL(srv.URL, 10*time.Millisecond, nil)
	var lastErr atomic.Value
	r.SetOnError(func(err error) {
		calls.Add(1)
		lastErr.Store(err.Error())
	})
	if err := r.Start(t.Context(), wh); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(r.Stop)
	flip.Store(true) // future ticks fail
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if calls.Load() > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("OnError never fired; last=%v", lastErr.Load())
}

// fetchFromURL + newIPRefresherForURL are tiny test-only seams that
// hit a caller-supplied URL instead of the real Stripe one.
func fetchFromURL(ctx context.Context, url string, client *http.Client) (FetchedIPs, error) {
	old := stripeWebhookIPsURL
	stripeWebhookIPsURL = url
	defer func() { stripeWebhookIPsURL = old }()
	return FetchStripeWebhookIPs(ctx, client)
}

func newIPRefresherForURL(url string, interval time.Duration, client *http.Client) *IPRefresher {
	old := stripeWebhookIPsURL
	stripeWebhookIPsURL = url
	// We don't restore here because the refresher's background loop
	// holds the URL; restore after tests via t.Cleanup.
	r := NewIPRefresher(client, interval)
	r.cleanup = func() { stripeWebhookIPsURL = old }
	return r
}

// fakeWebhooksForIPTest is the minimum needed to satisfy the IP refresher path.
type fakeWebhooksForIPTest = Webhooks

func newFakeWebhooksForIPTest() *fakeWebhooksForIPTest {
	return &Webhooks{}
}

func (w *Webhooks) snapshotCIDRs() []*net.IPNet {
	w.cidrMu.RLock()
	defer w.cidrMu.RUnlock()
	out := make([]*net.IPNet, len(w.allowedCIDRs))
	copy(out, w.allowedCIDRs)
	return out
}

// keep import clean
var _ = errors.New
