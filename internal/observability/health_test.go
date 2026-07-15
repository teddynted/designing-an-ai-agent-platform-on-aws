package observability

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func up(name string) Check {
	return CheckFunc{CheckName: name, Fn: func(ctx context.Context) error { return nil }}
}

func down(name string, err error) Check {
	return CheckFunc{CheckName: name, Fn: func(ctx context.Context) error { return err }}
}

func TestReadinessAggregatesChecks(t *testing.T) {
	h := NewHealth().
		AddReadiness(up("n8n")).
		AddReadiness(down("openclaw", errors.New("connection refused")))

	rep := h.Ready(context.Background())
	if rep.Status != StatusDown {
		t.Errorf("status = %v, want down (one dependency is down)", rep.Status)
	}
	if len(rep.Checks) != 2 {
		t.Fatalf("got %d checks, want 2", len(rep.Checks))
	}
	// The failing check must name itself and its error.
	var openclaw *CheckResult
	for i := range rep.Checks {
		if rep.Checks[i].Name == "openclaw" {
			openclaw = &rep.Checks[i]
		}
	}
	if openclaw == nil || openclaw.Status != StatusDown || openclaw.Error == "" {
		t.Errorf("openclaw result = %+v, want down with an error", openclaw)
	}
}

// Liveness with no checks is trivially up — a process answering the probe is, by
// that fact, alive.
func TestLivenessTriviallyUp(t *testing.T) {
	if got := NewHealth().Live(context.Background()).Status; got != StatusUp {
		t.Errorf("empty liveness = %v, want up", got)
	}
}

func TestHandlerStatusCodes(t *testing.T) {
	h := NewHealth().AddReadiness(down("dep", errors.New("nope")))
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	// Liveness up -> 200.
	res, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", res.StatusCode)
	}
	res.Body.Close()

	// Readiness down -> 503, with a JSON body naming the failed check.
	res, err = http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("/readyz = %d, want 503", res.StatusCode)
	}
	var rep Report
	json.NewDecoder(res.Body).Decode(&rep)
	if rep.Status != StatusDown || len(rep.Checks) != 1 || rep.Checks[0].Name != "dep" {
		t.Errorf("body = %+v, want down/dep", rep)
	}
}

func TestHTTPCheck(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer healthy.Close()
	sick := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer sick.Close()

	if err := (HTTPCheck{CheckName: "ok", URL: healthy.URL}).Check(context.Background()); err != nil {
		t.Errorf("healthy check failed: %v", err)
	}
	if err := (HTTPCheck{CheckName: "bad", URL: sick.URL}).Check(context.Background()); err == nil {
		t.Error("a 502 should be a failed check")
	}
}
