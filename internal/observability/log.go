package observability

import (
	"context"
	"io"
	"log/slog"
	"os"
)

// New builds the platform's standard logger from a [Config].
//
// It returns a plain *[slog.Logger] on purpose. Every package in this repository
// that logs already takes a *slog.Logger — internal/workflow, internal/agent,
// internal/llm — so adopting the platform standard is a change to *how the logger
// is constructed*, in one place per binary, and not a change to a single call
// site. The alternative (a bespoke Logger type with its own methods) would have
// made "use the standard" a rewrite, and a standard that is a rewrite is a
// standard nobody adopts.
//
// What the caller gets that a raw slog.New would not give them:
//
//   - The service name stamped on every line ([FieldService]), so one log group
//     can hold several binaries and still be filtered to one.
//   - Redaction ([redactAttr]) wired into the handler, so a credential or a prompt
//     cannot reach the log group even if a caller logs it by mistake.
//   - Context enrichment: InfoContext/ErrorContext pull the correlation [Fields]
//     off the context and stamp them, so a function deep in the stack logs the
//     right IDs without threading them through its signature.
func New(cfg Config) *slog.Logger {
	return newWithWriter(cfg, os.Stderr)
}

// newWithWriter is the seam a test writes through. Nothing else needs it: a binary
// logs to stderr, and where those bytes go from there is the platform's business,
// not the process's.
func newWithWriter(cfg Config, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{
		Level:       cfg.level(),
		ReplaceAttr: redactAttr,
		// AddSource is off by default: a file:line on every line is noise in
		// production and cost in a log group, and the correlation IDs are what
		// actually locate a problem. A caller who wants it can set LOG_SOURCE.
		AddSource: cfg.AddSource,
	}

	var base slog.Handler
	if cfg.textFormat() {
		// Text is for a terminal — a human running the CLI locally. JSON is for
		// everywhere a machine reads the line, which is everywhere else.
		base = slog.NewTextHandler(w, opts)
	} else {
		base = slog.NewJSONHandler(w, opts)
	}

	h := &contextHandler{base: base}
	log := slog.New(h)
	if cfg.Service != "" {
		log = log.With(FieldService, cfg.Service)
	}
	return log
}

// contextHandler enriches every line with the correlation [Fields] carried on the
// context, so the standard fields appear whether or not the call site remembered
// them.
//
// It wraps a base handler rather than replacing it, so it inherits the base's
// formatting, level filtering and redaction unchanged — it adds exactly one thing
// (the context attributes) and delegates the rest.
type contextHandler struct {
	base slog.Handler
}

// Handle is where the enrichment happens. It reads [FieldsFrom] the context —
// which also resolves the ambient X-Ray trace ID — and adds any that are set
// before handing the record to the base handler. Fields already present on the
// record win, so an explicit executionId on the call is never overwritten by a
// coarser one from the context.
func (h *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	f := FieldsFrom(ctx)
	if attrs := f.attrs(); len(attrs) > 0 {
		// Only add fields the record does not already carry, so a more specific
		// value at the call site is not clobbered by the context's.
		present := map[string]bool{}
		r.Attrs(func(a slog.Attr) bool {
			present[a.Key] = true
			return true
		})
		for i := 0; i+1 < len(attrs); i += 2 {
			key, _ := attrs[i].(string)
			if key == FieldService {
				// Service is stamped once, by the logger. Skip it here so it is not
				// duplicated on lines that also carry it from a context.
				continue
			}
			if !present[key] {
				r.AddAttrs(slog.String(key, attrs[i+1].(string)))
			}
		}
	}
	return h.base.Handle(ctx, r)
}

func (h *contextHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.base.Enabled(ctx, l)
}

func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &contextHandler{base: h.base.WithAttrs(attrs)}
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{base: h.base.WithGroup(name)}
}
