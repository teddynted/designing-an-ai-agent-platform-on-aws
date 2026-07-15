package observability

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Status is the outcome of a health check.
type Status string

const (
	// StatusUp — the thing works.
	StatusUp Status = "up"
	// StatusDown — the thing does not, and depending on which probe found it, the
	// right response is either "restart me" or "stop sending me traffic".
	StatusDown Status = "down"
)

// A Check answers one question — "is n8n reachable?", "is the model loaded?" — with
// an error or nil. The name is what appears in the health response, so it should be
// the dependency's name ("n8n", "openclaw"), not a description.
//
// The distinction the whole health story rests on is *which* question a check
// answers, and it is not a property of the check — it is where you register it.
// The same "is n8n reachable" check is a *readiness* concern (stop routing to me
// until it is back) and almost never a *liveness* concern (do not restart me just
// because a dependency blipped — restarting will not fix n8n). See [Health].
type Check interface {
	Name() string
	Check(ctx context.Context) error
}

// CheckFunc adapts a function to a [Check].
type CheckFunc struct {
	CheckName string
	Fn        func(ctx context.Context) error
}

func (c CheckFunc) Name() string                    { return c.CheckName }
func (c CheckFunc) Check(ctx context.Context) error { return c.Fn(ctx) }

// HTTPCheck probes a URL and calls it healthy on a 2xx. It is the check most of the
// platform's dependencies need, because OpenClaw and n8n are HTTP services deployed
// by their own repositories — reachability over HTTP is exactly the fact a readiness
// probe wants.
//
// It deliberately does not send credentials. A health probe's job is "can I reach
// it and does it answer", not "can I use it" — mixing an auth check into liveness is
// how a rotated token turns into a restart loop.
type HTTPCheck struct {
	CheckName string
	URL       string
	// Path is appended to URL, e.g. "/healthz". Many services expose a dedicated
	// health path that is cheaper than their real endpoints.
	Path   string
	Client *http.Client
}

func (c HTTPCheck) Name() string { return c.CheckName }

func (c HTTPCheck) Check(ctx context.Context) error {
	client := c.Client
	if client == nil {
		// A short default: a health probe that hangs is worse than one that fails,
		// because a hung probe fails the *probe's* timeout, later, less clearly.
		client = &http.Client{Timeout: 5 * time.Second}
	}
	url := c.URL + c.Path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return &statusError{code: res.StatusCode}
	}
	return nil
}

type statusError struct{ code int }

func (e *statusError) Error() string {
	return http.StatusText(e.code) + " (" + itoa(e.code) + ")"
}

// Health aggregates checks into the two probes an orchestrator actually asks for.
//
//   - Liveness ("/healthz"): is this process itself healthy — not wedged, not
//     deadlocked? A failing liveness probe means *restart me*. Dependencies do NOT
//     belong here: restarting a process does not fix the database it cannot reach,
//     it just adds a crash loop to an outage.
//   - Readiness ("/readyz"): can this process do useful work right now — are its
//     dependencies reachable? A failing readiness probe means *stop routing traffic
//     to me*, and is the correct home for "is n8n up", "is the model loaded".
//
// Getting these two backwards is one of the most common and most damaging
// operational mistakes, which is why they are separate registries with separate
// meanings rather than one list with a flag.
type Health struct {
	mu        sync.RWMutex
	liveness  []Check
	readiness []Check
	started   time.Time
	now       func() time.Time
}

// NewHealth returns a Health with the process start time recorded, so the readiness
// response can report uptime — a small thing that answers "did it just restart?" at
// a glance.
func NewHealth() *Health {
	return &Health{started: time.Now(), now: time.Now}
}

// AddLiveness registers a check that, if it fails, means the process should be
// restarted. Use it sparingly: most things are readiness concerns.
func (h *Health) AddLiveness(c Check) *Health {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.liveness = append(h.liveness, c)
	return h
}

// AddReadiness registers a dependency check that, if it fails, means traffic should
// be routed elsewhere.
func (h *Health) AddReadiness(c Check) *Health {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.readiness = append(h.readiness, c)
	return h
}

// Report is the JSON body of a probe response.
type Report struct {
	Status    Status        `json:"status"`
	UptimeSec int64         `json:"uptimeSeconds"`
	Checks    []CheckResult `json:"checks,omitempty"`
}

// CheckResult is one dependency's outcome within a [Report].
type CheckResult struct {
	Name      string `json:"name"`
	Status    Status `json:"status"`
	Error     string `json:"error,omitempty"`
	LatencyMS int64  `json:"latencyMs"`
}

// Live runs the liveness checks. With none registered it is trivially up, which is
// the correct default: a process that is answering the probe at all is, by that
// fact, alive.
func (h *Health) Live(ctx context.Context) Report {
	h.mu.RLock()
	checks := h.liveness
	h.mu.RUnlock()
	return h.run(ctx, checks)
}

// Ready runs the readiness checks.
func (h *Health) Ready(ctx context.Context) Report {
	h.mu.RLock()
	checks := h.readiness
	h.mu.RUnlock()
	return h.run(ctx, checks)
}

func (h *Health) run(ctx context.Context, checks []Check) Report {
	rep := Report{
		Status:    StatusUp,
		UptimeSec: int64(h.now().Sub(h.started).Seconds()),
	}
	for _, c := range checks {
		start := h.now()
		err := c.Check(ctx)
		res := CheckResult{
			Name:      c.Name(),
			Status:    StatusUp,
			LatencyMS: h.now().Sub(start).Milliseconds(),
		}
		if err != nil {
			res.Status = StatusDown
			res.Error = err.Error()
			rep.Status = StatusDown
		}
		rep.Checks = append(rep.Checks, res)
	}
	return rep
}

// Handler serves the two probes: GET /healthz for liveness, GET /readyz for
// readiness. Each returns 200 when up and 503 when down, so a load balancer or an
// orchestrator reads the status code, and a human reads the JSON body that explains
// which dependency failed and how long it took.
//
// 503 (not 500) on a down readiness probe is deliberate: it means "unavailable, try
// elsewhere", which is exactly what a caller should do, and what a 500 ("I broke")
// does not communicate.
func (h *Health) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeReport(w, h.Live(r.Context()))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		writeReport(w, h.Ready(r.Context()))
	})
	return mux
}

func writeReport(w http.ResponseWriter, rep Report) {
	w.Header().Set("Content-Type", "application/json")
	code := http.StatusOK
	if rep.Status != StatusUp {
		code = http.StatusServiceUnavailable
	}
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(rep)
}

// itoa is a tiny dependency-free int-to-string, so this file (and the package's
// leaf status) does not lean on strconv just for a status code.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
