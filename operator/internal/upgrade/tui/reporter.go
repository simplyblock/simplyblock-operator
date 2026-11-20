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

func (r *Reporter) Stage(stage upgrade.Stage) {
	r.program.Send(stageMsg{stage: stage})
}

func (r *Reporter) Section(label string, total int) {
	r.program.Send(sectionMsg{label: label, total: total})
}

func (r *Reporter) Work(total int) {
	r.program.Send(workMsg{total: total})
}

func (r *Reporter) Item(label string) {
	r.program.Send(itemMsg{label: label})
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

// Plan renders the hierarchy as one block, so a redrawing bar cannot land in
// the middle of it.
func (r *Reporter) Plan(plan upgrade.Plan) {
	if len(plan.Tasks) == 0 {
		r.program.Send(printMsg{
			text:       fmt.Sprintf("  %s has nothing to do on this cluster.", plan.Stage),
			blankAfter: true,
		})
		r.Findings(plan.Findings)
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "  %s\n", styleSection.Render("What "+string(plan.Stage)+" would do"))

	for _, phase := range plan.Phases() {
		if phase != "" {
			fmt.Fprintf(&b, "\n  %s\n", stylePhase.Render(phase.Describe()))
		}
		for _, task := range plan.InPhase(phase) {
			head := "    " + styleRuleName.Render(string(task.Step))
			if task.Blocked != "" {
				head += styleDim.Render("  (not implemented)")
			}
			fmt.Fprintf(&b, "\n%s\n      %s\n", head, styleDim.Render(task.Summary))

			if task.Collapsed() {
				continue
			}
			for _, action := range task.Subtasks {
				fmt.Fprintf(&b, "      %s\n", action)
			}
		}
	}
	r.program.Send(printMsg{text: strings.TrimRight(b.String(), "\n")})

	r.Findings(plan.Findings)

	var summary strings.Builder
	for _, line := range plan.Summary() {
		fmt.Fprintf(&summary, "%s\n", line)
	}
	if errs := plan.Findings.Errors(); len(errs) > 0 {
		fmt.Fprintf(&summary, "\n%s\n", styleError.Render(fmt.Sprintf(
			"%s failed: %d violations. No changes were made.", plan.Stage, len(errs))))
	}
	r.program.Send(printMsg{text: strings.TrimRight(summary.String(), "\n")})
}

// Block sends the lines as one message, so a redrawing bar cannot land in the
// middle of a tree.
func (r *Reporter) Block(heading string, lines []string) {
	var b strings.Builder
	if heading != "" {
		fmt.Fprintf(&b, "  %s\n\n", styleSection.Render(heading))
	}
	for _, line := range lines {
		fmt.Fprintf(&b, "  %s\n", line)
	}
	r.program.Send(printMsg{text: strings.TrimRight(b.String(), "\n"), blankAfter: true})
}

func (r *Reporter) Progress(format string, args ...any) {
	r.program.Send(printMsg{text: "  " + fmt.Sprintf(format, args...)})
}

// Close drains what is queued, stops the program, and waits for the terminal to
// be given back.
//
// It does not quit directly. bubbletea runs the command a message returns in a
// goroutine, so a quit sent while prints are outstanding races them, and the
// last line of a run is the one most worth keeping. The model quits itself once
// its queue is empty, which is the only point at which nothing is in flight.
func (r *Reporter) Close() error {
	r.closeOnce.Do(func() {
		r.program.Send(closeMsg{})
		<-r.done
	})
	return nil
}

// The messages the reporter sends. They are the Reporter interface, restated as
// values so the model is the only thing that renders.
type (
	stageMsg struct {
		stage upgrade.Stage
	}
	sectionMsg struct {
		label string
		total int
	}
	phaseMsg struct {
		phase upgrade.Phase
		steps int
	}
	workMsg struct {
		total int
	}
	itemMsg struct {
		label string
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

	// closeMsg asks the model to quit once it has printed what it holds.
	closeMsg struct{}

	// flushedMsg reports that the queued lines reached the terminal, and is
	// what lets the next flush start. One flush in flight at a time is what
	// keeps the scrollback in order.
	flushedMsg struct{}
)

// model is the live view: what is running, and how far along the stage is.
type model struct {
	spinner  spinner.Model
	progress progress.Model

	stage   upgrade.Stage
	phase   upgrade.Phase
	section string

	// total and completed size and fill the section's bar.
	total     int
	completed int

	current     upgrade.ID
	description string

	// item, itemsTotal, and itemsDone are the rule's own walk. They are shown
	// beside the spinner rather than as a second bar, because a run that
	// nests two bars is one where neither is legible.
	item       string
	itemsTotal int
	itemsDone  int

	width int

	// pending holds lines waiting to be printed, flushing keeps one print in
	// flight, and closing records that the run asked to stop while lines were
	// still queued.
	//
	// One print in flight at a time is what keeps the scrollback in order.
	// bubbletea runs the command a message returns in a goroutine, so two
	// Println commands issued from two updates race, and the line a discoverer
	// finished with can reach the terminal after the summary that follows it.
	pending  []string
	flushing bool
	closing  bool
}

func newModel() *model {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = styleSpinner

	return &model{
		spinner:  s,
		progress: progress.New(progress.WithDefaultGradient(), progress.WithWidth(40)),
		width:    80,
	}
}

func (m *model) Init() tea.Cmd { return m.spinner.Tick }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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
		m.stage = msg.stage
		m.phase, m.section, m.current = "", "", ""
		m.total, m.completed = 0, 0
		return m, tea.Batch(m.enqueue(styleStage.Render(fmt.Sprintf("── %s ──", msg.stage))), m.progress.SetPercent(0))

	case sectionMsg:
		m.section, m.total, m.completed = msg.label, msg.total, 0
		m.current, m.item = "", ""
		return m, tea.Batch(m.enqueue(stylePhase.Render(fmt.Sprintf("  %s", msg.label))), m.progress.SetPercent(0))

	case phaseMsg:
		m.phase, m.section = msg.phase, msg.phase.Describe()
		m.total, m.completed = msg.steps, 0
		m.current, m.item = "", ""
		return m, tea.Batch(m.enqueue(stylePhase.Render(fmt.Sprintf("  %s", msg.phase.Describe()))), m.progress.SetPercent(0))

	case ruleMsg:
		m.current, m.description = msg.id, msg.description
		m.item, m.itemsTotal, m.itemsDone = "", 0, 0
		return m, nil

	case workMsg:
		m.itemsTotal, m.itemsDone = msg.total, 0
		return m, nil

	case itemMsg:
		m.item = msg.label
		m.itemsDone++
		return m, nil

	case outcomeMsg:
		m.completed++
		m.current, m.item = "", ""
		m.itemsTotal, m.itemsDone = 0, 0
		return m, tea.Batch(m.enqueue(renderOutcome(msg)), m.progress.SetPercent(m.fraction()))

	case printMsg:
		if msg.blankAfter {
			return m, m.enqueue(msg.text + "\n")
		}
		return m, m.enqueue(msg.text)

	case flushedMsg:
		m.flushing = false
		if cmd := m.flush(); cmd != nil {
			return m, cmd
		}
		if m.closing {
			return m, tea.Quit
		}
		return m, nil

	case closeMsg:
		m.closing = true
		if m.flushing || len(m.pending) > 0 {
			// Quit once the queue drains, so the last line of a run is not the
			// one a race drops.
			return m, nil
		}
		return m, tea.Quit

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

// enqueue adds a line to the scrollback and starts a print when none is in
// flight. Returning nil is normal: the line goes out with the flush already
// running.
func (m *model) enqueue(line string) tea.Cmd {
	m.pending = append(m.pending, line)
	return m.flush()
}

// flush prints everything queued as one Println, which is atomic, and reports
// back so the next flush can start.
func (m *model) flush() tea.Cmd {
	if m.flushing || len(m.pending) == 0 {
		return nil
	}

	text := strings.Join(m.pending, "\n")
	m.pending, m.flushing = nil, true
	return tea.Sequence(
		tea.Println(text),
		func() tea.Msg { return flushedMsg{} },
	)
}

// fraction is how far the current stage or phase has got, and is zero for work
// that was not countable in advance.
func (m *model) fraction() float64 {
	if m.total <= 0 {
		return 0
	}
	return float64(m.completed) / float64(m.total)
}

// View is the live part: one line naming the activity, and one bar under it.
//
// The activity is the section rather than the rule, because a section is a
// sentence a user recognizes and a rule identity is a slug this tool made up.
// The rule and whatever it is walking follow it, dimmed, and they are what
// changes often enough to tell a slow run from a hung one.
func (m *model) View() string {
	if m.closing || (m.section == "" && m.current == "") {
		// A run that has stopped leaves its last frame on screen otherwise, and
		// a half-drawn progress bar under a finished report reads as a run that
		// hung.
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s %s%s\n",
		m.spinner.View(), styleSection.Render(m.section+"…"), styleDim.Render(m.detail()))
	if m.total > 0 {
		fmt.Fprintf(&b, "  %s %d/%d\n", m.progress.View(), m.completed, m.total)
	}
	return b.String()
}

// detail is what follows the activity: the rule that is running, and the item
// it is on. On a large cluster this is the only thing that moves for minutes at
// a time, which is what makes a slow run distinguishable from a hung one.
func (m *model) detail() string {
	if m.current == "" {
		return ""
	}

	switch {
	case m.item != "" && m.itemsTotal > 0:
		return fmt.Sprintf("  %s  %d/%d %s", m.current, m.itemsDone, m.itemsTotal, m.item)
	case m.item != "":
		return fmt.Sprintf("  %s  %s", m.current, m.item)
	default:
		return fmt.Sprintf("  %s", m.current)
	}
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
	styleStage    = lipgloss.NewStyle().Bold(true)
	styleSection  = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "27", Dark: "39"})
	stylePhase    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "27", Dark: "39"})
	styleRuleName = lipgloss.NewStyle().Bold(true)
	styleDim      = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "245", Dark: "241"})
	styleSpinner  = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "27", Dark: "39"})
	styleDone     = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "28", Dark: "42"})
	styleSkipped  = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "245", Dark: "241"})
	styleError    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "160", Dark: "203"})
	styleWarning  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "130", Dark: "214"})
	styleInfo     = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "245", Dark: "247"})
)
