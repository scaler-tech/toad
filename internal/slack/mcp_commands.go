package slack

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"github.com/scaler-tech/toad/internal/config"
	"github.com/scaler-tech/toad/internal/state"
)

// SlashCommandHandler processes /toad slash commands.
type SlashCommandHandler struct {
	db  *state.DB
	api *slack.Client
	cfg config.MCPConfig
}

// NewSlashCommandHandler creates a new handler for /toad commands.
func NewSlashCommandHandler(db *state.DB, api *slack.Client, cfg config.MCPConfig) *SlashCommandHandler {
	return &SlashCommandHandler{
		db:  db,
		api: api,
		cfg: cfg,
	}
}

// handleSlashCommand dispatches /toad slash commands.
func handleSlashCommand(c *Client, cmd slack.SlashCommand) {
	if c.mcpHandler == nil {
		slog.Debug("slash command received but no handler configured")
		return
	}

	args := strings.Fields(strings.ToLower(strings.TrimSpace(cmd.Text)))
	if len(args) == 0 {
		c.mcpHandler.handleHelp(cmd)
		return
	}

	switch args[0] {
	case "mcp":
		if !c.mcpHandler.cfg.Enabled {
			c.mcpHandler.ephemeral(cmd, "MCP server is not enabled. Add `mcp.enabled: true` to your toad config to use MCP commands.")
			return
		}
		if len(args) < 2 {
			c.mcpHandler.handleMCPHelp(cmd)
			return
		}
		switch args[1] {
		case "connect":
			c.mcpHandler.handleMCPConnect(cmd)
		case "revoke":
			c.mcpHandler.handleMCPRevoke(cmd)
		case "status":
			c.mcpHandler.handleMCPStatus(cmd)
		case "ping":
			c.mcpHandler.handleMCPPing(cmd)
		default:
			c.mcpHandler.handleMCPHelp(cmd)
		}
	case "github":
		if len(args) < 2 {
			c.mcpHandler.handleGitHubHelp(cmd)
			return
		}
		switch args[1] {
		case "add":
			if len(args) < 3 {
				c.mcpHandler.ephemeral(cmd, "Usage: `/toad github add <github-username>`")
				return
			}
			c.mcpHandler.handleGitHubAdd(cmd, args[2])
		case "list":
			c.mcpHandler.handleGitHubList(cmd)
		case "remove":
			if len(args) < 3 {
				c.mcpHandler.ephemeral(cmd, "Usage: `/toad github remove <github-username>`")
				return
			}
			c.mcpHandler.handleGitHubRemove(cmd, args[2])
		default:
			c.mcpHandler.handleGitHubHelp(cmd)
		}
	case "status":
		c.mcpHandler.handleStatus(cmd)
	case "joke":
		c.mcpHandler.handleJoke(cmd)
	case "help":
		c.mcpHandler.handleHelp(cmd)
	default:
		c.mcpHandler.ephemeral(cmd, fmt.Sprintf("Unknown command: `/toad %s`. Try `/toad help` to see what I can do.", strings.Join(args, " ")))
	}
}

// --- /toad status ---

func (h *SlashCommandHandler) handleStatus(cmd slack.SlashCommand) {
	stats, err := h.db.ReadDaemonStats()
	if err != nil {
		slog.Error("failed to read daemon stats", "error", err)
		h.ephemeral(cmd, "Sorry, I couldn't read daemon status.")
		return
	}
	if stats == nil {
		h.ephemeral(cmd, "Toad daemon is not running (no heartbeat found).")
		return
	}

	uptime := time.Since(stats.StartedAt).Truncate(time.Second)
	age := time.Since(stats.Heartbeat).Truncate(time.Second)

	status := "running"
	if stats.Draining {
		status = "draining"
	}
	if age > 30*time.Second {
		status = fmt.Sprintf("stale (last heartbeat %s ago)", age)
	}

	text := fmt.Sprintf("*Toad Daemon Status*\n"+
		"• Status: *%s*\n"+
		"• Version: %s\n"+
		"• Uptime: %s\n"+
		"• Ribbits: %d\n"+
		"• Triages: %d (bug: %d, feature: %d, question: %d)",
		status,
		stats.Version,
		uptime,
		stats.Ribbits,
		stats.Triages,
		stats.TriageByCategory["bug"],
		stats.TriageByCategory["feature"],
		stats.TriageByCategory["question"],
	)

	if stats.DigestEnabled {
		mode := ""
		if stats.DigestDryRun {
			mode = " (dry run)"
		}
		text += fmt.Sprintf("\n• Digest: enabled%s — %d processed, %d opportunities", mode, stats.DigestProcessed, stats.DigestOpps)
	}

	h.ephemeral(cmd, text)
}

// --- /toad mcp ... ---

func (h *SlashCommandHandler) handleMCPConnect(cmd slack.SlashCommand) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		slog.Error("failed to generate MCP token", "error", err)
		h.ephemeral(cmd, "Sorry, I couldn't generate a token. Please try again.")
		return
	}
	token := "toad_" + hex.EncodeToString(b)

	role := "user"
	for _, dev := range h.cfg.Devs {
		if dev == cmd.UserID {
			role = "dev"
			break
		}
	}

	// Revoke any existing tokens before issuing a new one
	_ = h.db.RevokeMCPToken(cmd.UserID)

	tok := &state.MCPToken{
		Token:       token,
		SlackUserID: cmd.UserID,
		SlackUser:   cmd.UserName,
		Role:        role,
		CreatedAt:   time.Now(),
	}
	if err := h.db.SaveMCPToken(tok); err != nil {
		slog.Error("failed to save MCP token", "error", err)
		h.ephemeral(cmd, "Sorry, I couldn't save your token. Please try again.")
		return
	}

	slog.Info("MCP token issued", "user", cmd.UserName, "role", role)

	endpoint := fmt.Sprintf("%s://%s:%d/mcp", h.mcpScheme(), h.cfg.Host, h.cfg.Port)

	desktopSnippet := fmt.Sprintf(`{
  "mcpServers": {
    "toad": {
      "url": "%s",
      "headers": {
        "Authorization": "Bearer %s"
      }
    }
  }
}`, endpoint, token)

	codeCmd := fmt.Sprintf("claude mcp add toad %s --transport http -H \"Authorization: Bearer %s\"", endpoint, token)

	text := fmt.Sprintf("Your MCP token has been created (role: *%s*).\n\n"+
		"*Claude Code* — run this in your terminal:\n```\n%s\n```\n\n"+
		"*Claude Desktop* — add this to your config (`claude_desktop_config.json`):\n```\n%s\n```\n\n"+
		":warning: Keep this token secret — it grants access to toad on your behalf.",
		role, codeCmd, desktopSnippet)
	if h.cfg.Message != "" {
		text += "\n\n" + h.cfg.Message
	}
	h.ephemeral(cmd, text)
}

func (h *SlashCommandHandler) handleMCPRevoke(cmd slack.SlashCommand) {
	if err := h.db.RevokeMCPToken(cmd.UserID); err != nil {
		slog.Error("failed to revoke MCP token", "error", err)
		h.ephemeral(cmd, "Sorry, I couldn't revoke your token. Please try again.")
		return
	}

	slog.Info("MCP token revoked", "user", cmd.UserName)
	h.ephemeral(cmd, "Your MCP token has been revoked. Use `/toad mcp connect` to generate a new one.")
}

func (h *SlashCommandHandler) handleMCPStatus(cmd slack.SlashCommand) {
	tok, err := h.db.GetMCPTokenByUser(cmd.UserID)
	if err != nil {
		slog.Error("failed to look up MCP token", "error", err)
		h.ephemeral(cmd, "Sorry, I couldn't check your token status.")
		return
	}

	if tok == nil {
		h.ephemeral(cmd, "You don't have an MCP token. Run `/toad mcp connect` to create one.")
		return
	}

	lastUsed := "never"
	if !tok.LastUsedAt.IsZero() {
		lastUsed = tok.LastUsedAt.Format(time.RFC3339)
	}

	text := fmt.Sprintf("*MCP Token Status*\n"+
		"• Role: *%s*\n"+
		"• Created: %s\n"+
		"• Last used: %s\n"+
		"• Endpoint: `%s://%s:%d/mcp`",
		tok.Role,
		tok.CreatedAt.Format(time.RFC3339),
		lastUsed,
		h.mcpScheme(), h.cfg.Host, h.cfg.Port,
	)
	h.ephemeral(cmd, text)
}

func (h *SlashCommandHandler) handleMCPPing(cmd slack.SlashCommand) {
	h.ephemeral(cmd, fmt.Sprintf("MCP server is running at `%s://%s:%d/mcp`", h.mcpScheme(), h.cfg.Host, h.cfg.Port))
}

func (h *SlashCommandHandler) handleMCPHelp(cmd slack.SlashCommand) {
	h.ephemeral(cmd, "*Toad MCP Commands*\n"+
		"• `/toad mcp connect` — Generate an MCP token for Claude Desktop/Code\n"+
		"• `/toad mcp revoke` — Revoke your MCP token\n"+
		"• `/toad mcp status` — Check your token and endpoint\n"+
		"• `/toad mcp ping` — Check if the MCP server is running")
}

// --- /toad joke ---

//nolint:lll // joke lines are naturally long
var frogJokes = []string{
	// Classic puns
	"What do frogs do with paper? Rip-it. :frog:",
	"What kind of shoes do frogs wear? Open toad sandals. :sandal:",
	"Why are frogs so happy? They eat whatever bugs them. :bug:",
	"What did the frog order at the restaurant? French flies. :french_fries:",
	"What do you call a frog with no legs? Unhoppy. :disappointed:",
	"Why did the frog take the bus to work? His car got toad. :bus:",
	"What kind of music do frogs listen to? Hip hop. :headphones:",
	"What happens when two frogs collide? They get tongue tied. :tongue:",
	"What do frogs drink? Croaka-Cola. :cup_with_straw:",
	"What do you call a girl with a frog on her head? Lily. :lily_pad:",
	"What's a frog's favorite candy? Lollihops. :lollipop:",
	"What's a toad's favorite ballet? Swamp Lake. :dancer:",
	"What do you get when you cross a frog with a rabbit? A bunny ribbit. :rabbit:",

	// Workplace humor
	"Why did the toad become a programmer? He was great at debugging. :computer:",
	"What's a frog's favorite version control system? Git ribbit. :git:",
	"Why do frogs make great QA testers? They're always finding bugs. :lady_beetle:",
	"How do frogs deploy code? They push to the lily pad. :rocket:",
	"What's a frog's favorite IDE feature? Auto-croak-plete. :keyboard:",
	"Why was the frog fired from the help desk? He kept telling people to restart their lily pads. :telephone_receiver:",
	"What do you call a frog who works in IT? A tech-toad. :technologist:",
	"Why don't frogs use JIRA? They prefer kanban — it's more agile. :clipboard:",
	"How does a frog review a pull request? One hop at a time. :eyes:",

	// Nature & lifestyle
	"What did the frog say about the book? Reddit. :book:",
	"What's a frog's favorite game? Leapfrog — they take it very seriously. :video_game:",
	"Why did the frog go to the hospital? He needed a hopperation. :hospital:",
	"What do frogs wear in summer? Jumpsuits. :shirt:",
	"What's a frog's favorite year? A leap year. :calendar:",
	"Why don't frogs ever get lost? They always know where to find the nearest pad. :compass:",
	"What do you call a frog spy? A croak-and-dagger agent. :male_detective:",
	"What's a toad's favorite TV show? Game of Ponds. :tv:",
	"Why did the frog become a lifeguard? He was already outstanding in his field — the swamp. :ocean:",
	"What do frogs do when they're sad? They drown their sorrows in the pond. :sweat_drops:",

	// Philosophy & wisdom
	"A frog walks into a library and says 'reddit, reddit, reddit'. The librarian says 'you've been here three times today'. :books:",
	"What did the zen frog say? Time's fun when you're having flies. :fly:",
	"Why did the philosophical frog sit on the log? To ponder the meaning of lily. :thought_balloon:",

	// Relationship & social
	"What did the frog say to his date? You make my heart leap. :heart:",
	"What do you call a frog who's always complaining? A grumpy toad. :rage:",
	"Why did the toad break up with the mushroom? There wasn't enough room for both of them on the log. :mushroom:",
	"What did the father frog say to his son? Time flies when you're catching them. :watch:",
	"How do frogs communicate over long distances? With a tele-croak. :phone:",

	// Food & drink
	"What's a frog's favorite hot drink? Croako. :coffee:",
	"What do frogs eat with their burgers? Flies and a shake. :hamburger:",
	"What's a toad's favorite snack? Flies and chips. :fries:",
	"What beer do frogs prefer? Bud Weis-ribbit. :beers:",

	// Science & education
	"Why are frogs so good at math? They love log-arithms. :abacus:",
	"What subject do frogs study at school? Biology — it's the only class where dissecting the teacher is acceptable. :microscope:",
	"Why did the tadpole feel lonely? Because he was going through a phase. :crescent_moon:",
	"What's a frog's blood type? Bee positive. :bee:",
	"Why did the frog go to night school? He wanted a toad-al education. :mortar_board:",

	// Meta & self-aware
	"I tried to write a frog joke but it didn't work. It just wasn't ribbiting enough. :writing_hand:",
	"What did the toad say to the other toad after hearing a joke? That was ribbiting! :joy:",
	"Why do frog jokes always work? They just have a certain _je ne sais croak_. :sparkles:",
	"What's the difference between a frog and a toad? About three product meetings. :memo:",
	"What's a frog's least favorite day? Fry-day. :grimacing:",

	// Tech & modern
	"How do frogs keep up with the news? They check the web — they're great at catching things on it. :spider_web:",
	"What social media do frogs use? TikToad. :iphone:",
	"What's a frog's WiFi password? Lily-pad-123. :signal_strength:",
	"Why did the frog's startup fail? Too many bugs in production. :chart_with_downwards_trend:",
	"What's a frog's favorite cryptocurrency? Dogecoin — just kidding, it's Ribbit-coin. :moneybag:",

	// Seasonal & situational
	"What do frogs say on New Year's? Hoppy New Year! :tada:",
	"What do frogs say at Halloween? Warts new? :jack_o_lantern:",
	"What did the frog wear to the party? A jumpsuit and a bow-toad. :bowtie:",
	"Why don't frogs play cricket? They're afraid of catching flies out. :cricket_game:",
	"What do you call a frog in January? A frogsicle. :cold_face:",
}

func (h *SlashCommandHandler) handleJoke(cmd slack.SlashCommand) {
	n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(frogJokes))))
	h.post(cmd, frogJokes[n.Int64()])
}

// --- /toad help ---

func (h *SlashCommandHandler) handleHelp(cmd slack.SlashCommand) {
	text := "*Toad Commands*\n" +
		"• `/toad status` — Daemon status, version, and stats\n"
	if h.cfg.Enabled {
		text += "• `/toad mcp connect` — Generate an MCP token\n" +
			"• `/toad mcp revoke` — Revoke your MCP token\n" +
			"• `/toad mcp status` — Check your MCP token\n" +
			"• `/toad mcp ping` — Check MCP server liveness\n"
	}
	text += "• `/toad github add|list|remove` — Link GitHub accounts for @mentions\n" +
		"• `/toad joke` — Tell a frog joke\n" +
		"• `/toad help` — Show this message"
	h.ephemeral(cmd, text)
}

// --- /toad github ---

func (h *SlashCommandHandler) handleGitHubAdd(cmd slack.SlashCommand, login string) {
	if err := h.db.AddGitHubMapping(cmd.UserID, login); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint") {
			h.ephemeral(cmd, fmt.Sprintf("GitHub account `%s` is already linked to another Slack user.", login))
			return
		}
		h.ephemeral(cmd, "Failed to add mapping: "+err.Error())
		return
	}
	h.ephemeral(cmd, fmt.Sprintf(":white_check_mark: Linked GitHub account `%s` to your Slack profile.", login))
}

func (h *SlashCommandHandler) handleGitHubList(cmd slack.SlashCommand) {
	logins, err := h.db.ListGitHubMappings(cmd.UserID)
	if err != nil {
		h.ephemeral(cmd, "Failed to list mappings: "+err.Error())
		return
	}
	if len(logins) == 0 {
		h.ephemeral(cmd, "No GitHub accounts linked. Use `/toad github add <username>` to link one.")
		return
	}
	lines := make([]string, len(logins))
	for i, l := range logins {
		lines[i] = fmt.Sprintf("• `%s`", l)
	}
	h.ephemeral(cmd, ":link: Your linked GitHub accounts:\n"+strings.Join(lines, "\n"))
}

func (h *SlashCommandHandler) handleGitHubRemove(cmd slack.SlashCommand, login string) {
	if err := h.db.RemoveGitHubMapping(cmd.UserID, login); err != nil {
		h.ephemeral(cmd, "Failed to remove mapping: "+err.Error())
		return
	}
	h.ephemeral(cmd, fmt.Sprintf(":white_check_mark: Unlinked GitHub account `%s`.", login))
}

func (h *SlashCommandHandler) handleGitHubHelp(cmd slack.SlashCommand) {
	h.ephemeral(cmd, "*GitHub account linking*\n\n"+
		"Link your GitHub account so toad can @mention you in investigation findings.\n\n"+
		"• `/toad github add <username>` — link a GitHub account\n"+
		"• `/toad github list` — show your linked accounts\n"+
		"• `/toad github remove <username>` — unlink an account")
}

// --- helpers ---

func (h *SlashCommandHandler) mcpScheme() string {
	if h.cfg.TLS {
		return "https"
	}
	return "http"
}

func (h *SlashCommandHandler) post(cmd slack.SlashCommand, text string) {
	_, _, err := h.api.PostMessage(
		cmd.ChannelID,
		slack.MsgOptionText(text, false),
	)
	if err != nil {
		slog.Error("failed to send message", "error", err, "channel", cmd.ChannelID)
	}
}

func (h *SlashCommandHandler) ephemeral(cmd slack.SlashCommand, text string) {
	_, err := h.api.PostEphemeral(
		cmd.ChannelID,
		cmd.UserID,
		slack.MsgOptionText(text, false),
	)
	if err != nil {
		slog.Error("failed to send ephemeral response", "error", err, "user", cmd.UserID)
	}
}
