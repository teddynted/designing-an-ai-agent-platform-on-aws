package router

import (
	"fmt"

	"github.com/teddynted/designing-an-ai-agent-platform-on-aws/internal/llm"
)

// The strategies this milestone ships.
const (
	// StrategyFixed sends everything to [Config.Default].
	StrategyFixed = "fixed"

	// StrategyPurpose routes by what the inference is FOR — [llm.Purpose] — through
	// [Config.Rules], falling through to [Config.Default].
	StrategyPurpose = "purpose"
)

// Candidate is a provider, described to a strategy in terms it can reason about.
//
// Note what a strategy is NOT given: the [llm.Provider] itself. It cannot call one, cannot
// probe one, cannot time one out, and cannot accidentally make a routing decision take two
// seconds. It is handed a name and a set of facts and it returns a name — which is what
// makes every strategy in this package a pure function, and every routing test a table.
type Candidate struct {
	Name         string
	Capabilities llm.Capabilities

	// Healthy is false when the provider has been failing (see [Health]). A strategy may
	// read it, and the ones here do not: health is about *availability*, which the router
	// handles by ordering the fallback chain, and mixing it into *preference* would give
	// two mechanisms an opinion on the same question.
	Healthy bool
}

// Decision is a strategy's answer, and the reason for it.
//
// The Reason is not decoration. It goes into the log line, and it is the difference between
// "this request went to Bedrock" and "this request went to Bedrock because
// LLM_ROUTER_RULES sends release-notes there" — which is the only one of those two
// sentences that anybody can act on at 3am.
type Decision struct {
	Provider string
	Reason   string
}

// Strategy chooses a provider for a request.
//
// # It cannot fail, and that is the design
//
// Select returns no error. It is given a non-empty list of candidates that are ALREADY
// known to be capable of serving the request — the router has applied the constraints
// before the strategy is consulted — so there is no failure left for it to have. The worst
// a strategy can do is express a preference for something that is not on the list, and the
// contract for that is explicit: it must pick a candidate anyway.
//
// This matters more than it looks. A routing layer sits in front of every inference the
// platform makes; a strategy that could return an error would be a new way for the whole
// inference plane to go down, added in exchange for nothing. So the type system removes the
// option.
//
// # And it cannot see the providers
//
// A [Candidate] is a name and some facts. A strategy has no way to call a model, so a
// routing decision cannot become slow, cannot become expensive, and cannot itself fail over
// — and "the router made a network call to decide where to make a network call" is a
// sentence nobody will ever have to debug.
type Strategy interface {
	// Name is the strategy, for the log: "fixed", "purpose".
	Name() string

	// Select chooses from candidates, which is non-empty and every entry of which can
	// serve the request. An implementation MUST return the name of one of them.
	Select(req llm.Request, candidates []Candidate) Decision
}

// NewStrategy builds the strategy the configuration names.
func NewStrategy(cfg Config) (Strategy, error) {
	switch cfg.Strategy {
	case StrategyFixed:
		return Fixed{Provider: cfg.Default}, nil
	case StrategyPurpose:
		return ByPurpose{Rules: cfg.Rules, Default: cfg.Default}, nil
	default:
		// Unreachable through ConfigFromEnv, which validates. Present because a caller can
		// construct a Config by hand, and a silent nil Strategy would be a nil dereference
		// on the first inference rather than an error at start-up.
		return nil, fmt.Errorf("%w: no strategy called %q", ErrConfig, cfg.Strategy)
	}
}

// Fixed sends every request to one provider.
//
// # This one strategy is four of the six the brief asked for
//
// "Always use Ollama", "always use Amazon Bedrock", "provider selected by configuration"
// and — with [ByPurpose] — "provider selected by workflow" are not four mechanisms. They
// are one mechanism and four values of one environment variable:
//
//	LLM_ROUTER_STRATEGY=fixed  LLM_ROUTER_DEFAULT=ollama    # always Ollama
//	LLM_ROUTER_STRATEGY=fixed  LLM_ROUTER_DEFAULT=bedrock   # always Bedrock
//
// Shipping four classes that differ only in a string would make the routing layer look
// richer than it is, and it would be the same code four times. The honest version is this
// one, and the extensibility that matters is not "how many strategies are there" but
// whether a fifth can be added without touching anything else — which is what [Strategy]
// being a two-method interface is for.
//
// # It is a preference, and it bends
//
// If the request needs tools and Ollama cannot do tools, Ollama is not among the candidates
// and this strategy picks whatever is — because a *preference* that cannot be honoured
// should give way to one that can. The thing that does NOT bend is a constraint:
// [llm.Request.RequireLocal] removes Bedrock from the candidates entirely, and if that
// leaves nothing the request is refused rather than rerouted.
//
// If you want "Ollama, and no exceptions, ever", the way to say it is not a strategy — it
// is `LLM_ROUTER_PROVIDERS=ollama`, which does not build a Bedrock client at all. See
// [EnvProviders].
type Fixed struct {
	Provider string
}

func (f Fixed) Name() string { return StrategyFixed }

func (f Fixed) Select(_ llm.Request, candidates []Candidate) Decision {
	for _, c := range candidates {
		if c.Name == f.Provider {
			return Decision{Provider: c.Name, Reason: "the configured default provider"}
		}
	}

	// The configured provider cannot serve this request — it has no tools, or the prompt
	// does not fit its window — and the router has already excluded it. Preferences bend:
	// take the first candidate that CAN, and say plainly in the log that we did, because a
	// request that quietly went somewhere other than where the configuration points is
	// exactly the kind of thing an operator should be told about rather than discover.
	return Decision{
		Provider: candidates[0].Name,
		Reason: fmt.Sprintf("the configured default (%s) cannot serve this request, so it went "+
			"to %s instead", f.Provider, candidates[0].Name),
	}
}

// ByPurpose routes on what the inference is FOR.
//
// [llm.Purpose] is already on every request — "release-notes", "diff-summary",
// "change-triage" — and it has been since Milestone 7, where it exists so that logs can
// answer "what is this platform spending its tokens on?". It turns out to be exactly the
// right routing key as well, and that is not a coincidence: the question "what is this
// for?" is the question that decides both what it costs and where it should run.
//
//	LLM_ROUTER_RULES=release-notes=bedrock,diff-summary=ollama
//
// # "By workflow" and "by task type" are this
//
// The brief asks for routing by workflow and routing by task type as separate strategies.
// They are the same lookup, because the workflow is what SETS the purpose: n8n triggers
// `release-notes`, and `release-notes` is the purpose the request carries. Adding a
// `Request.Workflow` field to route on would mean adding a field that nothing populates, in
// order to look like it does something the platform can already do.
//
// # The economics this is really for
//
// The reason to route by purpose is that the platform's inference work is not homogeneous
// and the providers are not interchangeable:
//
//   - "Summarise this diff" runs a hundred times a day, is worth nothing individually, and
//     a 7B local model does it perfectly well. Sending it to Claude is paying a specialist
//     to do arithmetic.
//   - "Draft the release notes" runs once, is read by everyone, and is the one where a
//     frontier model's output is visibly better.
//
// The GPU is already paid for, hourly, whether it is busy or idle. So the cheap work is
// *free* on Ollama, in the strict sense that not doing it there saves nothing — and the
// expensive work is worth paying for. That asymmetry is the entire commercial argument for
// a hybrid platform, and this is the four lines of code that collect on it.
type ByPurpose struct {
	Rules   map[llm.Purpose]string
	Default string
}

func (b ByPurpose) Name() string { return StrategyPurpose }

func (b ByPurpose) Select(req llm.Request, candidates []Candidate) Decision {
	if want, ruled := b.Rules[req.Purpose]; ruled {
		for _, c := range candidates {
			if c.Name == want {
				return Decision{
					Provider: c.Name,
					Reason:   fmt.Sprintf("purpose %q is routed to %s", req.Purpose, want),
				}
			}
		}
		// There is a rule and it points at a provider that cannot serve this request. Same
		// answer as Fixed, for the same reason, and said just as loudly.
		return Decision{
			Provider: candidates[0].Name,
			Reason: fmt.Sprintf("purpose %q is routed to %s, which cannot serve this request, "+
				"so it went to %s instead", req.Purpose, want, candidates[0].Name),
		}
	}

	for _, c := range candidates {
		if c.Name == b.Default {
			return Decision{
				Provider: c.Name,
				Reason:   fmt.Sprintf("no rule for purpose %q, so the default", req.Purpose),
			}
		}
	}
	return Decision{
		Provider: candidates[0].Name,
		Reason: fmt.Sprintf("no rule for purpose %q and the default (%s) cannot serve this "+
			"request, so it went to %s instead", req.Purpose, b.Default, candidates[0].Name),
	}
}
