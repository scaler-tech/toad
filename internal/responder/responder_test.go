package responder

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/scaler-tech/toad/internal/agent"
	"github.com/scaler-tech/toad/internal/config"
)

func testConv(surface string) Conversation {
	return Conversation{
		Messages: []Message{
			{Role: "user", Text: "why are exports slow?"},
			{Role: "toad", Text: "The cap was removed."},
			{Role: "user", Text: "update the ticket please"},
		},
		PriorFindings: "Investigated 2h ago: facetsForScope has no page cap.",
		TicketContext: "<linked_tickets>\nDAT-5107: Exports slow\n</linked_tickets>",
		Surface:       surface,
		Repo:          &config.RepoConfig{Name: "scaler-mono", Path: "/tmp/repo"},
	}
}

func TestRespond_PromptCarriesConversationAndContext(t *testing.T) {
	mock := &agent.MockProvider{RunResult: &agent.RunResult{Result: `{"reply":"ok"}`}}
	e := New(mock, "sonnet", time.Minute, config.VCSConfig{Platform: "github"})
	if _, err := e.Respond(context.Background(), testConv(SurfaceLinear)); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	p := mock.RunCalls[0].Prompt
	for _, want := range []string{
		"why are exports slow?",
		"update the ticket please",
		"Investigated 2h ago",
		"<linked_tickets>",
		"answer", // the answer-from-knowledge-first instruction
		`{"reply"`,
		agent.ProseStyleRules,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if mock.RunCalls[0].Permissions != agent.PermissionReadOnly {
		t.Error("responder must run read-only")
	}
	if len(mock.RunCalls[0].AllowedBashCommands) == 0 {
		t.Error("github platform should allow gh read-only commands")
	}
}

func TestRespond_SurfaceSelectsFormattingRules(t *testing.T) {
	mock := &agent.MockProvider{RunResult: &agent.RunResult{Result: `{"reply":"ok"}`}}
	e := New(mock, "sonnet", time.Minute, config.VCSConfig{})
	e.Respond(context.Background(), testConv(SurfaceSlack))
	slackPrompt := mock.RunCalls[0].Prompt
	if !strings.Contains(slackPrompt, "2000 characters") || !strings.Contains(slackPrompt, "*bold*") {
		t.Error("slack surface must carry mrkdwn + length rules")
	}
	e.Respond(context.Background(), testConv(SurfaceLinear))
	linearPrompt := mock.RunCalls[1].Prompt
	if strings.Contains(linearPrompt, "2000 characters") {
		t.Error("linear surface must not carry the Slack length cap")
	}
}

func TestRespond_ParsesEnvelope(t *testing.T) {
	mock := &agent.MockProvider{RunResult: &agent.RunResult{Result: `{"reply":"Updated.","ticket_update":{"issue":"DAT-5107","comment":"summary"},"did_investigate":false}`}}
	e := New(mock, "sonnet", time.Minute, config.VCSConfig{})
	env, err := e.Respond(context.Background(), testConv(SurfaceLinear))
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if env.Reply != "Updated." || env.TicketUpdate == nil || env.TicketUpdate.Comment != "summary" {
		t.Errorf("envelope = %+v", env)
	}
}

func TestRespond_RetriesOnceOnEmpty(t *testing.T) {
	mock := &agent.MockProvider{RunResults: []*agent.RunResult{
		{Result: "   "},
		{Result: `{"reply":"second try"}`},
	}}
	e := New(mock, "sonnet", time.Minute, config.VCSConfig{})
	env, err := e.Respond(context.Background(), testConv(SurfaceSlack))
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if env.Reply != "second try" || len(mock.RunCalls) != 2 {
		t.Errorf("reply=%q calls=%d", env.Reply, len(mock.RunCalls))
	}
}

func TestRespond_TicketEditInstructionOnlyWhenTicketPresent(t *testing.T) {
	mock := &agent.MockProvider{RunResult: &agent.RunResult{Result: `{"reply":"ok"}`}}
	e := New(mock, "sonnet", time.Minute, config.VCSConfig{})
	conv := testConv(SurfaceLinear)
	conv.TicketContext = ""
	e.Respond(context.Background(), conv)
	if strings.Contains(mock.RunCalls[0].Prompt, "ticket_update") &&
		!strings.Contains(mock.RunCalls[0].Prompt, "no ticket is in play") {
		t.Error("with no ticket context the prompt must tell the agent ticket_update does not apply")
	}
}
