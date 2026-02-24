package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/bubbles/v2/textinput"
	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/hergen/toad/internal/tui"
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Set up toad in the current directory",
	Long:  "Interactive setup wizard that creates a comprehensive .toad.yaml config file.",
	RunE:  runInit,
}

func init() {
	rootCmd.AddCommand(initCmd)
}

// ── Steps ────────────────────────────────────────────

type wizardStep int

const (
	stepWelcome wizardStep = iota
	stepSlack
	stepRepo
	stepRepoCommands
	stepToadKing
	stepAdvancedAsk
	stepAdvanced
	stepSummary
	stepDone
)

var stepNames = []string{"Slack", "Repo", "Toad King", "Finish"}

func stepIndex(s wizardStep) int {
	switch s {
	case stepWelcome, stepSlack:
		return 0
	case stepRepo, stepRepoCommands:
		return 1
	case stepToadKing:
		return 2
	default:
		return 3
	}
}

// ── Model ────────────────────────────────────────────

type wizardModel struct {
	step     wizardStep
	cursor   int
	width    int
	height   int
	quitting bool
	err      string

	// Slack
	appTokenInput textinput.Model
	botTokenInput textinput.Model
	focusedInput  int // 0=app, 1=bot

	// Repo
	repoPathInput textinput.Model
	repoNameInput textinput.Model
	detected      repoDefaults

	// Repo commands
	testCmdInput    textinput.Model
	lintCmdInput    textinput.Model
	branchOptions   []string
	branchCursor    int
	cmdFocusedField int // 0=test, 1=lint, 2=branch

	// Toad King
	toadKingCursor int // 0=dry-run, 1=live, 2=off

	// Advanced ask
	advancedCursor int // 0=no, 1=yes

	// Advanced settings
	advSection    int // which advanced section
	advCursor     int // cursor within section
	channelsInput textinput.Model
	emojiInput    textinput.Model
	keywordsInput textinput.Model
	claudeModel   int // 0=sonnet, 1=opus, 2=haiku
	triageModel   int // 0=haiku, 1=sonnet
	autoSpawn     bool
	autoMerge     bool
	labelsInput   textinput.Model
	logLevel      int // 0=debug, 1=info, 2=warn, 3=error

	// Result
	configWritten bool
}

func newTextInput(placeholder string, width int) textinput.Model {
	ti := textinput.New()
	ti.Prompt = ""
	ti.Placeholder = placeholder
	ti.SetWidth(width)
	ti.CharLimit = 500
	return ti
}

func newWizardModel() wizardModel {
	cwd, _ := os.Getwd()

	appToken := newTextInput("xapp-1-...", 60)
	botToken := newTextInput("xoxb-...", 60)
	botToken.EchoMode = textinput.EchoPassword

	repoPath := newTextInput(cwd, 60)
	repoPath.SetValue(cwd)

	repoName := newTextInput(filepath.Base(cwd), 40)
	repoName.SetValue(filepath.Base(cwd))

	testCmd := newTextInput("e.g. go test ./...", 60)
	lintCmd := newTextInput("e.g. golangci-lint run", 60)

	channels := newTextInput("C0123456789, C9876543210 (leave empty for all)", 60)

	emoji := newTextInput("frog", 30)
	emoji.SetValue("frog")

	keywords := newTextInput("toad fix, toad help", 60)
	keywords.SetValue("toad fix, toad help")

	labels := newTextInput("toad, automated", 40)

	return wizardModel{
		step:          stepWelcome,
		width:         80,
		height:        24,
		appTokenInput: appToken,
		botTokenInput: botToken,
		repoPathInput: repoPath,
		repoNameInput: repoName,
		testCmdInput:  testCmd,
		lintCmdInput:  lintCmd,
		channelsInput: channels,
		emojiInput:    emoji,
		keywordsInput: keywords,
		labelsInput:   labels,
		logLevel:      1, // info
	}
}

// ── Init ─────────────────────────────────────────────

func (m wizardModel) Init() tea.Cmd {
	return nil
}

// ── Update ───────────────────────────────────────────

func (m wizardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.KeyMsg:
		// Global quit
		if msg.String() == "ctrl+c" {
			m.quitting = true
			return m, tea.Quit
		}

		m.err = ""

		switch m.step {
		case stepWelcome:
			return m.updateWelcome(msg)
		case stepSlack:
			return m.updateSlack(msg)
		case stepRepo:
			return m.updateRepo(msg)
		case stepRepoCommands:
			return m.updateRepoCommands(msg)
		case stepToadKing:
			return m.updateToadKing(msg)
		case stepAdvancedAsk:
			return m.updateAdvancedAsk(msg)
		case stepAdvanced:
			return m.updateAdvanced(msg)
		case stepSummary:
			return m.updateSummary(msg)
		}
	}

	return m, nil
}

// ── Step updates ─────────────────────────────────────

func (m wizardModel) updateWelcome(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter", " ":
		m.step = stepSlack
		m.appTokenInput.Focus()
		return m, nil
	case "q", "esc":
		m.quitting = true
		return m, tea.Quit
	}
	return m, nil
}

func (m wizardModel) updateSlack(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "tab", "down":
		if m.focusedInput == 0 {
			m.focusedInput = 1
			m.appTokenInput.Blur()
			m.botTokenInput.Focus()
		}
		return m, nil
	case "shift+tab", "up":
		if m.focusedInput == 1 {
			m.focusedInput = 0
			m.botTokenInput.Blur()
			m.appTokenInput.Focus()
		}
		return m, nil
	case "enter":
		app := m.appTokenInput.Value()
		bot := m.botTokenInput.Value()
		if !strings.HasPrefix(app, "xapp-") {
			m.err = "App token must start with xapp-"
			return m, nil
		}
		if !strings.HasPrefix(bot, "xoxb-") {
			m.err = "Bot token must start with xoxb-"
			return m, nil
		}
		m.appTokenInput.Blur()
		m.botTokenInput.Blur()
		m.step = stepRepo
		m.repoPathInput.Focus()
		m.focusedInput = 0
		return m, nil
	case "esc":
		m.step = stepWelcome
		m.appTokenInput.Blur()
		m.botTokenInput.Blur()
		return m, nil
	default:
		var cmd tea.Cmd
		if m.focusedInput == 0 {
			m.appTokenInput, cmd = m.appTokenInput.Update(msg)
		} else {
			m.botTokenInput, cmd = m.botTokenInput.Update(msg)
		}
		return m, cmd
	}
}

func (m wizardModel) updateRepo(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "tab", "down":
		if m.focusedInput == 0 {
			m.focusedInput = 1
			m.repoPathInput.Blur()
			m.repoNameInput.Focus()
		}
		return m, nil
	case "shift+tab", "up":
		if m.focusedInput == 1 {
			m.focusedInput = 0
			m.repoNameInput.Blur()
			m.repoPathInput.Focus()
		}
		return m, nil
	case "enter":
		path := m.repoPathInput.Value()
		name := m.repoNameInput.Value()
		if err := validateRepoPath(path); err != nil {
			m.err = err.Error()
			return m, nil
		}
		if strings.TrimSpace(name) == "" {
			m.err = "Repo name cannot be empty"
			return m, nil
		}
		m.repoPathInput.Blur()
		m.repoNameInput.Blur()

		// Auto-detect
		abs, _ := filepath.Abs(path)
		m.repoPathInput.SetValue(abs)
		m.detected = detectRepoDefaults(abs)
		m.testCmdInput.SetValue(m.detected.TestCommand)
		m.lintCmdInput.SetValue(m.detected.LintCommand)

		// Build branch options
		m.branchOptions = []string{"main", "master", "develop"}
		if b := m.detected.DefaultBranch; b != "main" && b != "master" && b != "develop" {
			m.branchOptions = append([]string{b}, m.branchOptions...)
		}
		// Select detected branch
		for i, b := range m.branchOptions {
			if b == m.detected.DefaultBranch {
				m.branchCursor = i
				break
			}
		}

		m.step = stepRepoCommands
		m.testCmdInput.Focus()
		m.cmdFocusedField = 0
		return m, nil
	case "esc":
		m.step = stepSlack
		m.repoPathInput.Blur()
		m.repoNameInput.Blur()
		m.appTokenInput.Focus()
		m.focusedInput = 0
		return m, nil
	default:
		var cmd tea.Cmd
		if m.focusedInput == 0 {
			m.repoPathInput, cmd = m.repoPathInput.Update(msg)
		} else {
			m.repoNameInput, cmd = m.repoNameInput.Update(msg)
		}
		return m, cmd
	}
}

func (m wizardModel) updateRepoCommands(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "tab":
		m.testCmdInput.Blur()
		m.lintCmdInput.Blur()
		m.cmdFocusedField = (m.cmdFocusedField + 1) % 3
		switch m.cmdFocusedField {
		case 0:
			m.testCmdInput.Focus()
		case 1:
			m.lintCmdInput.Focus()
		}
		return m, nil
	case "shift+tab":
		m.testCmdInput.Blur()
		m.lintCmdInput.Blur()
		m.cmdFocusedField = (m.cmdFocusedField + 2) % 3
		switch m.cmdFocusedField {
		case 0:
			m.testCmdInput.Focus()
		case 1:
			m.lintCmdInput.Focus()
		}
		return m, nil
	case "enter":
		if m.cmdFocusedField == 2 {
			// On branch selector, enter confirms
			m.step = stepToadKing
			m.testCmdInput.Blur()
			m.lintCmdInput.Blur()
			return m, nil
		}
		// Tab forward from text inputs
		m.testCmdInput.Blur()
		m.lintCmdInput.Blur()
		m.cmdFocusedField = (m.cmdFocusedField + 1) % 3
		switch m.cmdFocusedField {
		case 0:
			m.testCmdInput.Focus()
		case 1:
			m.lintCmdInput.Focus()
		}
		return m, nil
	case "up":
		if m.cmdFocusedField == 2 {
			if m.branchCursor > 0 {
				m.branchCursor--
			}
			return m, nil
		}
		m.testCmdInput.Blur()
		m.lintCmdInput.Blur()
		m.cmdFocusedField = (m.cmdFocusedField + 2) % 3
		switch m.cmdFocusedField {
		case 0:
			m.testCmdInput.Focus()
		case 1:
			m.lintCmdInput.Focus()
		}
		return m, nil
	case "down":
		if m.cmdFocusedField == 2 {
			if m.branchCursor < len(m.branchOptions)-1 {
				m.branchCursor++
			}
			return m, nil
		}
		m.testCmdInput.Blur()
		m.lintCmdInput.Blur()
		m.cmdFocusedField = (m.cmdFocusedField + 1) % 3
		switch m.cmdFocusedField {
		case 0:
			m.testCmdInput.Focus()
		case 1:
			m.lintCmdInput.Focus()
		}
		return m, nil
	case "esc":
		m.step = stepRepo
		m.testCmdInput.Blur()
		m.lintCmdInput.Blur()
		m.repoPathInput.Focus()
		m.focusedInput = 0
		return m, nil
	default:
		var cmd tea.Cmd
		switch m.cmdFocusedField {
		case 0:
			m.testCmdInput, cmd = m.testCmdInput.Update(msg)
		case 1:
			m.lintCmdInput, cmd = m.lintCmdInput.Update(msg)
		}
		return m, cmd
	}
}

func (m wizardModel) updateToadKing(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.toadKingCursor > 0 {
			m.toadKingCursor--
		}
	case "down", "j":
		if m.toadKingCursor < 2 {
			m.toadKingCursor++
		}
	case "enter":
		m.step = stepAdvancedAsk
	case "esc":
		m.step = stepRepoCommands
		m.testCmdInput.Focus()
		m.cmdFocusedField = 0
	}
	return m, nil
}

func (m wizardModel) updateAdvancedAsk(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "down", "k", "j":
		m.advancedCursor = 1 - m.advancedCursor
	case "enter":
		if m.advancedCursor == 1 {
			m.step = stepAdvanced
			m.advSection = 0
			m.advCursor = 0
			m.channelsInput.Focus()
		} else {
			m.step = stepSummary
		}
	case "esc":
		m.step = stepToadKing
	}
	return m, nil
}

func (m wizardModel) updateAdvanced(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		if m.advSection > 0 {
			m.advSection--
			m.advCursor = 0
			m.blurAllAdvanced()
			m.focusAdvancedField()
		} else {
			m.step = stepAdvancedAsk
			m.blurAllAdvanced()
		}
		return m, nil
	}

	switch m.advSection {
	case 0: // Channels & Triggers
		return m.updateAdvTriggers(msg)
	case 1: // AI Models
		return m.updateAdvModels(msg)
	case 2: // Repo Options
		return m.updateAdvRepoOpts(msg)
	case 3: // Log Level
		return m.updateAdvLog(msg)
	}
	return m, nil
}

func (m wizardModel) updateAdvTriggers(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "tab", "down":
		m.blurAllAdvanced()
		m.advCursor = (m.advCursor + 1) % 3
		m.focusAdvancedField()
		return m, nil
	case "shift+tab", "up":
		m.blurAllAdvanced()
		m.advCursor = (m.advCursor + 2) % 3
		m.focusAdvancedField()
		return m, nil
	case "enter":
		m.blurAllAdvanced()
		m.advSection = 1
		m.advCursor = 0
		return m, nil
	default:
		var cmd tea.Cmd
		switch m.advCursor {
		case 0:
			m.channelsInput, cmd = m.channelsInput.Update(msg)
		case 1:
			m.emojiInput, cmd = m.emojiInput.Update(msg)
		case 2:
			m.keywordsInput, cmd = m.keywordsInput.Update(msg)
		}
		return m, cmd
	}
}

func (m wizardModel) updateAdvModels(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "tab", "down":
		m.advCursor = (m.advCursor + 1) % 3
	case "shift+tab", "up":
		m.advCursor = (m.advCursor + 2) % 3
	case "left":
		switch m.advCursor {
		case 0:
			if m.claudeModel > 0 {
				m.claudeModel--
			}
		case 1:
			if m.triageModel > 0 {
				m.triageModel--
			}
		case 2:
			m.autoSpawn = !m.autoSpawn
		}
	case "right":
		switch m.advCursor {
		case 0:
			if m.claudeModel < 2 {
				m.claudeModel++
			}
		case 1:
			if m.triageModel < 1 {
				m.triageModel++
			}
		case 2:
			m.autoSpawn = !m.autoSpawn
		}
	case " ":
		if m.advCursor == 2 {
			m.autoSpawn = !m.autoSpawn
		}
	case "enter":
		m.advSection = 2
		m.advCursor = 0
	}
	return m, nil
}

func (m wizardModel) updateAdvRepoOpts(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "tab", "down":
		m.blurAllAdvanced()
		m.advCursor = (m.advCursor + 1) % 2
		if m.advCursor == 1 {
			m.labelsInput.Focus()
		}
	case "shift+tab", "up":
		m.blurAllAdvanced()
		m.advCursor = (m.advCursor + 1) % 2
		if m.advCursor == 1 {
			m.labelsInput.Focus()
		}
	case " ":
		if m.advCursor == 0 {
			m.autoMerge = !m.autoMerge
		}
	case "left", "right":
		if m.advCursor == 0 {
			m.autoMerge = !m.autoMerge
		}
	case "enter":
		m.blurAllAdvanced()
		m.advSection = 3
		m.advCursor = 0
	default:
		if m.advCursor == 1 {
			var cmd tea.Cmd
			m.labelsInput, cmd = m.labelsInput.Update(msg)
			return m, cmd
		}
	}
	return m, nil
}

func (m wizardModel) updateAdvLog(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.logLevel > 0 {
			m.logLevel--
		}
	case "down", "j":
		if m.logLevel < 3 {
			m.logLevel++
		}
	case "enter":
		m.step = stepSummary
	}
	return m, nil
}

func (m *wizardModel) blurAllAdvanced() {
	m.channelsInput.Blur()
	m.emojiInput.Blur()
	m.keywordsInput.Blur()
	m.labelsInput.Blur()
}

func (m *wizardModel) focusAdvancedField() {
	if m.advSection == 0 {
		switch m.advCursor {
		case 0:
			m.channelsInput.Focus()
		case 1:
			m.emojiInput.Focus()
		case 2:
			m.keywordsInput.Focus()
		}
	}
}

func (m wizardModel) updateSummary(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter", "y":
		if err := m.writeConfig(); err != nil {
			m.err = err.Error()
			return m, nil
		}
		m.configWritten = true
		m.step = stepDone
		return m, tea.Quit
	case "esc", "n":
		m.step = stepAdvancedAsk
	}
	return m, nil
}

// ── Config writing ───────────────────────────────────

func (m *wizardModel) writeConfig() error {
	toadKingModes := []string{"dry-run", "live", "off"}
	toadKingMode := toadKingModes[m.toadKingCursor]

	claudeModels := []string{"sonnet", "opus", "haiku"}
	triageModels := []string{"haiku", "sonnet"}
	logLevels := []string{"debug", "info", "warn", "error"}

	digestEnabled := toadKingMode != "off"
	digestDryRun := toadKingMode != "live"

	absPath, _ := filepath.Abs(m.repoPathInput.Value())

	data := templateData{
		Slack: slackTemplateData{
			AppToken: m.appTokenInput.Value(),
			BotToken: m.botTokenInput.Value(),
			Channels: parseCSV(m.channelsInput.Value()),
			Emoji:    m.emojiInput.Value(),
			Keywords: parseCSV(m.keywordsInput.Value()),
		},
		Repos: []repoTemplateData{{
			Name:          strings.TrimSpace(m.repoNameInput.Value()),
			Path:          absPath,
			TestCommand:   m.testCmdInput.Value(),
			LintCommand:   m.lintCmdInput.Value(),
			DefaultBranch: m.branchOptions[m.branchCursor],
			AutoMerge:     m.autoMerge,
			PRLabels:      parseCSV(m.labelsInput.Value()),
		}},
		Limits: limitsTemplateData{
			MaxConcurrent:   2,
			MaxTurns:        30,
			TimeoutMinutes:  10,
			MaxFilesChanged: 5,
			MaxRetries:      1,
		},
		Triage: triageTemplateData{
			Model:     triageModels[m.triageModel],
			AutoSpawn: m.autoSpawn,
		},
		Claude: claudeTemplateData{
			Model: claudeModels[m.claudeModel],
		},
		Digest: digestTemplateData{
			Enabled: digestEnabled,
			DryRun:  digestDryRun,
		},
		IssueTracker: issueTrackerTemplateData{},
		Log: logTemplateData{
			Level: logLevels[m.logLevel],
		},
	}

	configData, err := renderConfig(data)
	if err != nil {
		return fmt.Errorf("rendering config: %w", err)
	}

	return os.WriteFile(".toad.yaml", configData, 0o600)
}

// ── View ─────────────────────────────────────────────

func (m wizardModel) View() tea.View {
	if m.quitting {
		return tea.NewView("")
	}

	w := m.contentWidth()
	var b strings.Builder

	// Progress bar (except welcome and done)
	if m.step != stepWelcome && m.step != stepDone {
		b.WriteString(tui.RenderProgressBar(stepNames, stepIndex(m.step)))
		b.WriteString("\n\n")
	}

	// Step content
	switch m.step {
	case stepWelcome:
		b.WriteString(m.viewWelcome())
	case stepSlack:
		b.WriteString(m.viewSlack())
	case stepRepo:
		b.WriteString(m.viewRepo())
	case stepRepoCommands:
		b.WriteString(m.viewRepoCommands())
	case stepToadKing:
		b.WriteString(m.viewToadKing())
	case stepAdvancedAsk:
		b.WriteString(m.viewAdvancedAsk())
	case stepAdvanced:
		b.WriteString(m.viewAdvanced())
	case stepSummary:
		b.WriteString(m.viewSummary())
	case stepDone:
		b.WriteString(m.viewDone())
	}

	// Error display
	if m.err != "" {
		b.WriteString("\n\n")
		b.WriteString(tui.ErrorStyle.Render("✗ " + m.err))
	}

	// Help text at bottom
	b.WriteString("\n\n")
	b.WriteString(m.helpText())

	// Wrap in bordered box
	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(tui.ColorBorder).
		Padding(1, 3).
		Width(w)

	box := boxStyle.Render(b.String())

	// Center horizontally and vertically
	boxLines := strings.Count(box, "\n") + 1
	topPad := (m.height - boxLines) / 2
	if topPad < 0 {
		topPad = 0
	}

	centered := lipgloss.NewStyle().
		Width(m.width).
		Align(lipgloss.Center).
		PaddingTop(topPad).
		Render(box)

	v := tea.NewView(centered)
	v.AltScreen = true
	return v
}

func (m wizardModel) contentWidth() int {
	w := m.width - 8 // margin for centering
	if w > 80 {
		w = 80
	}
	if w < 40 {
		w = 40
	}
	return w
}

// ── Step views ───────────────────────────────────────

const toadLogo = `
 ████████╗ ██████╗  █████╗ ██████╗
 ╚══██╔══╝██╔═══██╗██╔══██╗██╔══██╗
    ██║   ██║   ██║███████║██║  ██║
    ██║   ██║   ██║██╔══██║██║  ██║
    ██║   ╚██████╔╝██║  ██║██████╔╝
    ╚═╝    ╚═════╝ ╚═╝  ╚═╝╚═════╝`

const toadFrog = `
         ╭━━━╮ ╭━━━╮
        ┃ ● ┃━┃ ● ┃
         ╰┳━━━━━━┳╯
    ╭━━━━━┻━━━━━━┻━━━━━╮
    ┃  ╭──╮      ╭──╮  ┃
    ┃  ╰──╯  ^^  ╰──╯  ┃
    ╰━━┳━━━━━━━━━━━━┳━━╯
       ┃  ╱╲    ╱╲  ┃
       ╰━╱  ╲━━╱  ╲━╯`

func (m wizardModel) viewWelcome() string {
	var b strings.Builder

	b.WriteString(tui.SelectedStyle.Render(toadLogo))
	b.WriteString("\n")
	b.WriteString(tui.DimStyle.Render(toadFrog))
	b.WriteString("\n\n")
	b.WriteString("AI-powered coding assistant that lives in Slack.\n")
	b.WriteString("Monitors channels, answers questions, and fixes bugs\n")
	b.WriteString("by autonomously creating pull requests.\n")
	b.WriteString("\n")
	b.WriteString(tui.DimStyle.Render("Press Enter to start setup."))

	return b.String()
}

func (m wizardModel) viewSlack() string {
	var b strings.Builder

	b.WriteString(tui.TitleStyle.Render("Slack Connection"))
	b.WriteString("\n\n")
	b.WriteString(tui.DimStyle.Render("Create a Slack app at https://api.slack.com/apps"))
	b.WriteString("\n")
	b.WriteString(tui.DimStyle.Render("Enable Socket Mode, add Bot Token Scopes, subscribe to events."))
	b.WriteString("\n\n")

	b.WriteString(m.fieldLabel("App-Level Token", m.focusedInput == 0))
	b.WriteString("\n")
	b.WriteString(m.inputBorderStyle(m.focusedInput == 0).Render(m.appTokenInput.View()))
	b.WriteString("\n\n")

	b.WriteString(m.fieldLabel("Bot User OAuth Token", m.focusedInput == 1))
	b.WriteString("\n")
	b.WriteString(m.inputBorderStyle(m.focusedInput == 1).Render(m.botTokenInput.View()))

	return b.String()
}

func (m wizardModel) viewRepo() string {
	var b strings.Builder

	b.WriteString(tui.TitleStyle.Render("Repository"))
	b.WriteString("\n\n")
	b.WriteString(tui.DimStyle.Render("Configure the repo Toad will work on."))
	b.WriteString("\n\n")

	b.WriteString(m.fieldLabel("Repo path", m.focusedInput == 0))
	b.WriteString("\n")
	b.WriteString(m.inputBorderStyle(m.focusedInput == 0).Render(m.repoPathInput.View()))
	b.WriteString("\n\n")

	b.WriteString(m.fieldLabel("Repo name", m.focusedInput == 1))
	b.WriteString("\n")
	b.WriteString(m.inputBorderStyle(m.focusedInput == 1).Render(m.repoNameInput.View()))

	return b.String()
}

func (m wizardModel) viewRepoCommands() string {
	var b strings.Builder

	b.WriteString(tui.TitleStyle.Render("Validation Commands"))
	b.WriteString("\n\n")

	if m.detected.Stack != "" {
		info := fmt.Sprintf("Detected %s project", m.detected.Stack)
		if m.detected.Module != "" {
			info += fmt.Sprintf(" (%s)", m.detected.Module)
		}
		b.WriteString(tui.SuccessStyle.Render("✓ " + info))
	} else {
		b.WriteString(tui.DimStyle.Render("Configure test and lint commands."))
	}
	b.WriteString("\n\n")

	b.WriteString(m.fieldLabel("Test command", m.cmdFocusedField == 0))
	b.WriteString("\n")
	b.WriteString(m.inputBorderStyle(m.cmdFocusedField == 0).Render(m.testCmdInput.View()))
	b.WriteString("\n\n")

	b.WriteString(m.fieldLabel("Lint command", m.cmdFocusedField == 1))
	b.WriteString("\n")
	b.WriteString(m.inputBorderStyle(m.cmdFocusedField == 1).Render(m.lintCmdInput.View()))
	b.WriteString("\n\n")

	b.WriteString(m.fieldLabel("Default branch", m.cmdFocusedField == 2))
	b.WriteString("\n")
	for i, branch := range m.branchOptions {
		b.WriteString("  ")
		if m.cmdFocusedField == 2 && i == m.branchCursor {
			b.WriteString(tui.SelectedStyle.Render("● " + branch))
		} else if i == m.branchCursor {
			b.WriteString(tui.DimStyle.Render("● " + branch))
		} else {
			b.WriteString(tui.DimStyle.Render("○ " + branch))
		}
		b.WriteString("\n")
	}

	return b.String()
}

func (m wizardModel) viewToadKing() string {
	var b strings.Builder

	b.WriteString(tui.TitleStyle.Render("Toad King"))
	b.WriteString("\n\n")
	b.WriteString("Toad King passively monitors your Slack channels\n")
	b.WriteString("and auto-identifies bugs that could be fixed.\n")
	b.WriteString("\n")

	options := []struct {
		label string
		desc  string
	}{
		{"Dry-run", "monitor and report opportunities (recommended)"},
		{"Live", "auto-fix high-confidence bugs"},
		{"Off", "disable passive monitoring"},
	}

	for i, opt := range options {
		if i == m.toadKingCursor {
			b.WriteString(tui.CursorStyle.Render("▸ "))
			b.WriteString(tui.SelectedStyle.Render(opt.label))
			b.WriteString(tui.DimStyle.Render(" — " + opt.desc))
		} else {
			b.WriteString("  ")
			b.WriteString(opt.label)
			b.WriteString(tui.DimStyle.Render(" — " + opt.desc))
		}
		b.WriteString("\n")
	}

	return b.String()
}

func (m wizardModel) viewAdvancedAsk() string {
	var b strings.Builder

	b.WriteString(tui.TitleStyle.Render("Advanced Settings"))
	b.WriteString("\n\n")
	b.WriteString(tui.DimStyle.Render("Channels, triggers, AI models, and more."))
	b.WriteString("\n")
	b.WriteString(tui.DimStyle.Render("Defaults work great for most setups."))
	b.WriteString("\n\n")

	options := []string{"Use defaults and finish", "Customize settings"}
	for i, opt := range options {
		if i == m.advancedCursor {
			b.WriteString(tui.CursorStyle.Render("▸ "))
			b.WriteString(tui.SelectedStyle.Render(opt))
		} else {
			b.WriteString("  " + opt)
		}
		b.WriteString("\n")
	}

	return b.String()
}

func (m wizardModel) viewAdvanced() string {
	switch m.advSection {
	case 0:
		return m.viewAdvTriggers()
	case 1:
		return m.viewAdvModels()
	case 2:
		return m.viewAdvRepoOpts()
	case 3:
		return m.viewAdvLog()
	}
	return ""
}

func (m wizardModel) viewAdvTriggers() string {
	var b strings.Builder

	b.WriteString(tui.TitleStyle.Render("Channels & Triggers"))
	b.WriteString("  ")
	b.WriteString(tui.DimStyle.Render("(1/4)"))
	b.WriteString("\n\n")

	fields := []struct {
		label string
		view  string
	}{
		{"Channel IDs", m.channelsInput.View()},
		{"Trigger emoji", m.emojiInput.View()},
		{"Trigger keywords", m.keywordsInput.View()},
	}

	for i, f := range fields {
		b.WriteString(m.fieldLabel(f.label, i == m.advCursor))
		b.WriteString("\n")
		b.WriteString(m.inputBorderStyle(i == m.advCursor).Render(f.view))
		b.WriteString("\n\n")
	}

	return b.String()
}

func (m wizardModel) viewAdvModels() string {
	var b strings.Builder

	claudeModels := []string{"sonnet", "opus", "haiku"}
	triageModels := []string{"haiku", "sonnet"}

	b.WriteString(tui.TitleStyle.Render("AI Models"))
	b.WriteString("  ")
	b.WriteString(tui.DimStyle.Render("(2/4)"))
	b.WriteString("\n\n")

	b.WriteString(m.fieldLabel("Tadpole model", m.advCursor == 0))
	b.WriteString("  ")
	for i, model := range claudeModels {
		if i == m.claudeModel {
			b.WriteString(tui.SelectedStyle.Render("[" + model + "]"))
		} else {
			b.WriteString(tui.DimStyle.Render(" " + model + " "))
		}
		b.WriteString(" ")
	}
	b.WriteString("\n\n")

	b.WriteString(m.fieldLabel("Triage model", m.advCursor == 1))
	b.WriteString("  ")
	for i, model := range triageModels {
		if i == m.triageModel {
			b.WriteString(tui.SelectedStyle.Render("[" + model + "]"))
		} else {
			b.WriteString(tui.DimStyle.Render(" " + model + " "))
		}
		b.WriteString(" ")
	}
	b.WriteString("\n\n")

	b.WriteString(m.fieldLabel("Auto-spawn", m.advCursor == 2))
	b.WriteString("  ")
	if m.autoSpawn {
		b.WriteString(tui.SelectedStyle.Render("[on]"))
		b.WriteString(tui.DimStyle.Render("  off "))
	} else {
		b.WriteString(tui.DimStyle.Render(" on  "))
		b.WriteString(tui.SelectedStyle.Render("[off]"))
	}
	b.WriteString("\n")
	b.WriteString(tui.DimStyle.Render("Skip trigger — auto-spawn for any detected bug"))

	return b.String()
}

func (m wizardModel) viewAdvRepoOpts() string {
	var b strings.Builder

	b.WriteString(tui.TitleStyle.Render("Repo Options"))
	b.WriteString("  ")
	b.WriteString(tui.DimStyle.Render("(3/4)"))
	b.WriteString("\n\n")

	b.WriteString(m.fieldLabel("Auto-merge PRs", m.advCursor == 0))
	b.WriteString("  ")
	if m.autoMerge {
		b.WriteString(tui.SelectedStyle.Render("[on]"))
		b.WriteString(tui.DimStyle.Render("  off "))
	} else {
		b.WriteString(tui.DimStyle.Render(" on  "))
		b.WriteString(tui.SelectedStyle.Render("[off]"))
	}
	b.WriteString("\n\n")

	b.WriteString(m.fieldLabel("PR labels", m.advCursor == 1))
	b.WriteString("\n")
	b.WriteString(m.inputBorderStyle(m.advCursor == 1).Render(m.labelsInput.View()))

	return b.String()
}

func (m wizardModel) viewAdvLog() string {
	var b strings.Builder

	levels := []string{"debug", "info", "warn", "error"}

	b.WriteString(tui.TitleStyle.Render("Log Level"))
	b.WriteString("  ")
	b.WriteString(tui.DimStyle.Render("(4/4)"))
	b.WriteString("\n\n")

	for i, level := range levels {
		if i == m.logLevel {
			b.WriteString(tui.CursorStyle.Render("▸ "))
			b.WriteString(tui.SelectedStyle.Render(level))
		} else {
			b.WriteString("  " + level)
		}
		if level == "info" {
			b.WriteString(tui.DimStyle.Render(" (default)"))
		}
		b.WriteString("\n")
	}

	return b.String()
}

func (m wizardModel) viewSummary() string {
	var b strings.Builder

	toadKingModes := []string{"dry-run", "live", "off"}
	claudeModels := []string{"sonnet", "opus", "haiku"}

	b.WriteString(tui.TitleStyle.Render("Review & Save"))
	b.WriteString("\n\n")

	var box strings.Builder
	box.WriteString(m.summaryLine("Repo", m.repoNameInput.Value()))
	box.WriteString(m.summaryLine("Path", m.repoPathInput.Value()))
	if m.detected.Stack != "" {
		box.WriteString(m.summaryLine("Stack", m.detected.Stack))
	}
	box.WriteString(m.summaryLine("Branch", m.branchOptions[m.branchCursor]))
	box.WriteString(m.summaryLine("Test", m.testCmdInput.Value()))
	box.WriteString(m.summaryLine("Lint", m.lintCmdInput.Value()))
	box.WriteString(m.summaryLine("Toad King", toadKingModes[m.toadKingCursor]))
	box.WriteString(m.summaryLine("Model", claudeModels[m.claudeModel]))

	// Inner summary box
	innerBox := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(tui.ColorSubtle).
		Padding(1, 2).
		Width(m.contentWidth() - 12).
		Render(box.String())

	b.WriteString(innerBox)
	b.WriteString("\n\n")
	b.WriteString("Save to ")
	b.WriteString(tui.SelectedStyle.Render(".toad.yaml"))
	b.WriteString("?")

	return b.String()
}

func (m wizardModel) summaryLine(key, value string) string {
	return fmt.Sprintf("%-12s %s\n", tui.DimStyle.Render(key), value)
}

func (m wizardModel) viewDone() string {
	var b strings.Builder
	b.WriteString(tui.SuccessStyle.Render("✓ Config written to .toad.yaml"))
	b.WriteString("\n\n")
	b.WriteString("Start toad with: ")
	b.WriteString(tui.SelectedStyle.Render("toad"))
	return b.String()
}

// ── Help text ────────────────────────────────────────

func (m wizardModel) helpText() string {
	switch m.step {
	case stepWelcome:
		return tui.HelpStyle.Render("Enter start  •  Esc quit")
	case stepSlack, stepRepo:
		return tui.HelpStyle.Render("Tab/↓ next field  •  Enter continue  •  Esc back")
	case stepRepoCommands:
		return tui.HelpStyle.Render("Tab next field  •  ↑/↓ select branch  •  Enter continue  •  Esc back")
	case stepToadKing:
		return tui.HelpStyle.Render("↑/↓ select  •  Enter continue  •  Esc back")
	case stepAdvancedAsk:
		return tui.HelpStyle.Render("↑/↓ select  •  Enter continue  •  Esc back")
	case stepAdvanced:
		switch m.advSection {
		case 0:
			return tui.HelpStyle.Render("Tab next field  •  Enter next section  •  Esc back")
		case 1, 2:
			return tui.HelpStyle.Render("Tab next  •  ←/→ change  •  Enter next section  •  Esc back")
		case 3:
			return tui.HelpStyle.Render("↑/↓ select  •  Enter finish  •  Esc back")
		}
	case stepSummary:
		return tui.HelpStyle.Render("Enter/y save  •  Esc/n back")
	}
	return ""
}

// ── Style helpers ────────────────────────────────────

func (m wizardModel) fieldLabel(label string, focused bool) string {
	if focused {
		return tui.CursorStyle.Render("▸ ") + tui.TitleStyle.Render(label)
	}
	return "  " + label
}

func (m wizardModel) inputBorderStyle(focused bool) lipgloss.Style {
	borderColor := tui.ColorBorder
	if focused {
		borderColor = tui.ColorPrimary
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor).
		Padding(0, 1).
		Width(m.contentWidth() - 10)
}

// ── Validators ───────────────────────────────────────

func validateRepoPath(s string) error {
	abs, err := filepath.Abs(s)
	if err != nil {
		return fmt.Errorf("invalid path: %w", err)
	}
	info, err := os.Stat(abs)
	if os.IsNotExist(err) {
		return fmt.Errorf("path does not exist: %s", abs)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory: %s", abs)
	}
	if _, err := os.Stat(filepath.Join(abs, ".git")); os.IsNotExist(err) {
		return fmt.Errorf("not a git repository: %s", abs)
	}
	return nil
}

// ── Helpers ──────────────────────────────────────────

func parseCSV(s string) []string {
	var result []string
	for _, part := range strings.Split(s, ",") {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// ── Entry point ──────────────────────────────────────

func runInit(cmd *cobra.Command, args []string) error {
	// Overwrite check
	if _, err := os.Stat(".toad.yaml"); err == nil {
		fmt.Printf("  .toad.yaml already exists. Overwrite? [y/N] ")
		var answer string
		fmt.Scanln(&answer)
		if answer != "y" && answer != "Y" {
			fmt.Println("  Canceled.")
			return nil
		}
	}

	m := newWizardModel()
	p := tea.NewProgram(m)
	result, err := p.Run()
	if err != nil {
		return fmt.Errorf("wizard error: %w", err)
	}

	final := result.(wizardModel)
	if final.quitting && !final.configWritten {
		fmt.Println("  Setup canceled.")
	}

	return nil
}
