// The Reporter implementation, and the bubbletea program behind it. Every
// reporter method is a message send, so a rule that reports from a goroutine of
// its own cannot race the render, and the model is the only thing that touches
// the terminal.

package tui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-isatty"

	"github.com/simplyblock/simplyblock-operator/internal/upgrade"
)

// Reporter renders a run at a terminal.
type Reporter struct {
	program *tea.Program

	// done closes when the program has stopped, so Close can wait for the
	// final frame rather than returning while the terminal is still being
	// written to.
	done chan struct{}

	// closeOnce keeps Close idempotent: a run that fails closes the reporter on
	// the error path, and the deferred close runs anyway.
	closeOnce sync.Once
}

// Interactive reports whether a terminal is attached, which is what decides
// between this reporter and the plain text one. A tool that draws a progress
// bar into a Job's log produces a log nobody can read.
func Interactive(out *os.File) bool {
	return isatty.IsTerminal(out.Fd()) || isatty.IsCygwinTerminal(out.Fd())
}

// New starts the terminal program and returns the reporter that feeds it.
func New(out io.Writer) *Reporter {
	model := newModel()
	program := tea.NewProgram(model, tea.WithOutput(out))

	r := &Reporter{program: program, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		// A terminal that fails mid-run is not a reason to fail a migration, so
		// the error is dropped here and the run continues unrendered.
		_, _ = program.Run()
	}()
	return r
}

func (r *Reporter) Stage(stage upgrade.Stage, steps int) {
	r.program.Send(stageMsg{stage: stage, steps: steps})
}

func (r *Reporter) Phase(phase upgrade.Phase, steps int) {
	r.program.Send(phaseMsg{phase: phase, steps: steps})
}

func (r *Reporter) Rule(rule upgrade.Rule) {
	r.program.Send(ruleMsg{id: rule.ID(), description: rule.Description()})
}

func (r *Reporter) Outcome(rule upgrade.Rule, outcome upgrade.Outcome, detail string) {
	r.program.Send(outcomeMsg{id: rule.ID(), outcome: outcome, detail: detail})
}

func (r *Reporter) Findings(findings upgrade.Findings) {
	for _, finding := range findings {
		r.program.Send(printMsg{text: styleFinding(finding), blankAfter: true})
	}
}

func (r *Reporter) Action(action upgrade.Action) {
	r.program.Send(printMsg{text: "  " + action.String()})
}

func (r *Reporter) Plan(plan upgrade.Plan) {
	for _, action := range plan.Actions {
		r.Action(action)
	}
	r.Findings(plan.Findings)

	var b strings.Builder
	for _, line := range plan.Summary() {
		fmt.Fprintf(&b, "%s\n", line)
	}
	if errs := plan.Findings.Errors(); len(errs) > 0 {
		fmt.Fprintf(&b, "\n%s\n", styleError.Render(fmt.Sprintf(
			"%s failed: %d violations. No changes were made.", plan.Stage, len(errs))))
	}
	r.program.Send(printMsg{text: strings.TrimRight(b.String(), "\n")})
}

func (r *Reporter) Progress(format string, args ...any) {
	r.program.Send(printMsg{text: "  " + fmt.Sprintf(format, args...)})
}

// Close stops the program and waits for the terminal to be given back.
func (r *Reporter) Close() error {
	r.closeOnce.Do(func() {
		r.program.Quit()
		<-r.done
	})
	return nil
}

// The messages the reporter sends. They are the Reporter interface, restated as
// values so the model is the only thing that renders.
type (
	stageMsg struct {
		stage upgrade.Stage
		steps int
	}
	phaseMsg struct {
		phase upgrade.Phase
		steps int
	}
	ruleMsg struct {
		id          upgrade.ID
		description string
	}
	outcomeMsg struct {
		id      upgrade.ID
		outcome upgrade.Outcome
		detail  string
	}
	printMsg struct {
		text       string
		blankAfter bool
	}
)

// model is the live view: what is running, and how far along the stage is.
type model struct {
	spinner  spinner.Model
	progress progress.Model

	stage upgrade.Stage
	phase upgrade.Phase

	total     int
	completed int

	current     upgrade.ID
	description string

	width int
}

func newModel() model {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = styleSpinner

	return model{
		spinner:  s,
		progress: progress.New(progress.WithDefaultGradient(), progress.WithWidth(40)),
		width:    80,
	}
}

func (m model) Init() tea.Cmd { return m.spinner.Tick }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.progress.Width = min(msg.Width-20, 60)
		return m, nil

	case tea.KeyMsg:
		// Ctrl-C stops the render, and the run's own context handles the rest.
		// Quitting here rather than swallowing the key is what keeps the
		// terminal usable after an interrupted migration.
		if msg.Type == tea.KeyCtrlC {
			return m, tea.Quit
		}
		return m, nil

	case stageMsg:
		m.stage, m.total, m.completed = msg.stage, msg.steps, 0
		m.phase, m.current = "", ""
		return m, tea.Batch(
			tea.Println(styleStage.Render(fmt.Sprintf("── %s ──", msg.stage))),
			m.progress.SetPercent(0),
		)

	case phaseMsg:
		m.phase, m.total, m.completed = msg.phase, msg.steps, 0
		m.current = ""
		return m, tea.Batch(
			tea.Println(stylePhase.Render(fmt.Sprintf("  %s", msg.phase))),
			m.progress.SetPercent(0),
		)

	case ruleMsg:
		m.current, m.description = msg.id, msg.description
		return m, nil

	case outcomeMsg:
		m.completed++
		m.current = ""
		return m, tea.Batch(
			tea.Println(renderOutcome(msg)),
			m.progress.SetPercent(m.fraction()),
		)

	case printMsg:
		if msg.blankAfter {
			return m, tea.Println(msg.text + "\n")
		}
		return m, tea.Println(msg.text)

	case progress.FrameMsg:
		updated, cmd := m.progress.Update(msg)
		m.progress = updated.(progress.Model)
		return m, cmd

	default:
		s, cmd := m.spinner.Update(msg)
		m.spinner = s
		return m, cmd
	}
}

// fraction is how far the current stage or phase has got, and is zero for work
// that was not countable in advance.
func (m model) fraction() float64 {
	if m.total <= 0 {
		return 0
	}
	return float64(m.completed) / float64(m.total)
}

func (m model) View() string {
	if m.total <= 0 && m.current == "" {
		return ""
	}

	var b strings.Builder
	if m.total > 0 {
		fmt.Fprintf(&b, "%s %d/%d\n", m.progress.View(), m.completed, m.total)
	}
	if m.current != "" {
		fmt.Fprintf(&b, "%s %s %s\n",
			m.spinner.View(), styleRule.Render(string(m.current)), styleDim.Render(m.description))
	}
	return b.String()
}

// renderOutcome is the line an outcome leaves in the scrollback.
func renderOutcome(msg outcomeMsg) string {
	var symbol string
	switch msg.outcome {
	case upgrade.OutcomeSkipped:
		symbol = styleSkipped.Render("~")
	case upgrade.OutcomeFailed:
		symbol = styleError.Render("✗")
	default:
		symbol = styleDone.Render("✓")
	}

	line := fmt.Sprintf("  %s %s", symbol, msg.id)
	if msg.detail != "" {
		line += styleDim.Render(": " + firstLine(msg.detail))
	}
	return line
}

// firstLine keeps a multi-line error from breaking the live view apart. The
// whole error still reaches the log, which is where it is diagnosed from.
func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	return s
}

// styleFinding renders a finding with its severity colored, keeping the layout
// [upgrade.Finding.String] already produces.
func styleFinding(finding upgrade.Finding) string {
	rendered := finding.String()
	style := styleInfo
	switch finding.Severity {
	case upgrade.SeverityError:
		style = styleError
	case upgrade.SeverityWarning:
		style = styleWarning
	}

	head, rest, found := strings.Cut(rendered, " ")
	if !found {
		return style.Render(rendered)
	}
	return style.Render(head) + " " + rest
}

// The palette. Adaptive colors so the report is legible on a light terminal as
// well as a dark one.
var (
	styleStage   = lipgloss.NewStyle().Bold(true)
	stylePhase   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "27", Dark: "39"})
	styleRule    = lipgloss.NewStyle().Bold(true)
	styleDim     = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "245", Dark: "241"})
	styleSpinner = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "27", Dark: "39"})
	styleDone    = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "28", Dark: "42"})
	styleSkipped = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "245", Dark: "241"})
	styleError   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "160", Dark: "203"})
	styleWarning = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "130", Dark: "214"})
	styleInfo    = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "245", Dark: "247"})
)
