package responder

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/scaler-tech/toad/internal/agent"
	"github.com/scaler-tech/toad/internal/config"
)

const (
	SurfaceSlack  = "slack"
	SurfaceLinear = "linear"
)

// Message is one turn of the conversation.
type Message struct {
	Role string // "user" or "toad"
	Text string
}

// Conversation is the surface-neutral input: what was said, what toad
// already knows, and where the exchange is happening.
type Conversation struct {
	Messages []Message
	// Capabilities is surface-supplied reference material about toad itself
	// and the current request — e.g. ribbit's "about toad" blurb and triage
	// hints — that is NOT a ticket. Kept separate from TicketContext so that
	// field can genuinely mean "a ticket is in play in this conversation":
	// folding capability blurbs into TicketContext made it non-empty on
	// every Slack turn, permanently disabling buildPrompt's "no ticket is in
	// play" note (see buildPrompt's updateNote logic).
	Capabilities  string
	PriorFindings string
	TicketContext string
	Surface       string
	Repo          *config.RepoConfig
	RepoPaths     map[string]string
}

// Engine runs the responder agent.
type Engine struct {
	agent   agent.Provider
	model   string
	timeout time.Duration
	vcs     config.VCSConfig
}

func New(p agent.Provider, model string, timeout time.Duration, vcs config.VCSConfig) *Engine {
	return &Engine{agent: p, model: model, timeout: timeout, vcs: vcs}
}

// Respond runs one conversational turn and returns the parsed envelope.
func (e *Engine) Respond(ctx context.Context, conv Conversation) (*Envelope, error) {
	prompt := buildPrompt(conv)

	runOpts := agent.RunOpts{
		Prompt:      prompt,
		Model:       e.model,
		Timeout:     e.timeout,
		Permissions: agent.PermissionReadOnly,
	}
	if conv.Repo != nil {
		runOpts.WorkDir = conv.Repo.Path
	}
	for p := range conv.RepoPaths {
		runOpts.AdditionalDirs = append(runOpts.AdditionalDirs, p)
	}
	switch e.vcs.Platform {
	case "github":
		runOpts.AllowedBashCommands = []string{
			"gh pr view", "gh pr list", "gh pr diff", "gh pr checks",
			"gh issue view", "gh issue list",
			"gh search",
		}
	case "gitlab":
		runOpts.AllowedBashCommands = []string{
			"glab mr view", "glab mr list", "glab mr diff",
			"glab issue view", "glab issue list",
		}
	}

	result, err := e.agent.Run(ctx, runOpts)
	if err != nil {
		return nil, fmt.Errorf("responder call failed: %w", err)
	}
	// Retry once on empty output (same pattern as ribbit): the agent may
	// have spent its budget searching without emitting a final message.
	if strings.TrimSpace(result.Result) == "" {
		slog.Warn("responder empty, retrying once")
		result, err = e.agent.Run(ctx, runOpts)
		if err != nil {
			return nil, fmt.Errorf("responder retry failed: %w", err)
		}
		if strings.TrimSpace(result.Result) == "" {
			return nil, fmt.Errorf("agent returned empty result after retry")
		}
	}

	return ParseEnvelope(result.Result), nil
}
