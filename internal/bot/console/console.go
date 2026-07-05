// Package console is the vikingbot interactive control surface. It
// uses github.com/charmbracelet/bubbletea to render a TUI for
// inspecting channel status, recent messages, and agent replies.
//
// The console is optional — `vikingbot start` does not require it.
// Operators launch it via `vikingbot console` to peek at live state
// without tailing JSON logs.
package console

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/saker-ai/ctxhub/internal/bot/channels"
)

// State is the snapshot the console renders. The bot's main loop
// updates it via SetState; the console's bubbletea Model reads it via
// a copy under a mutex.
type State struct {
	Channels []string
	Messages []string
	Replies  []string
}

// Model implements bubbletea.Model for the vikingbot console.
type Model struct {
	mu    sync.RWMutex
	state State
	width int
}

// NewModel returns a fresh console Model.
func NewModel() *Model {
	return &Model{}
}

// SetState applies the given state delta to the model. It is called by
// the bot's main loop (not by bubbletea) to push live state.
func (m *Model) SetState(s State) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.Channels = append([]string(nil), s.Channels...)
	m.state.Messages = append([]string(nil), s.Messages...)
	m.state.Replies = append([]string(nil), s.Replies...)
	// Cap history at 100 entries each to bound memory.
	if len(m.state.Messages) > 100 {
		m.state.Messages = m.state.Messages[len(m.state.Messages)-100:]
	}
	if len(m.state.Replies) > 100 {
		m.state.Replies = m.state.Replies[len(m.state.Replies)-100:]
	}
}

// Update implements bubbletea.Model.Update. Handles window-size and quit.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case tea.KeyMsg:
		if strings.EqualFold(msg.String(), "q") || msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
	}
	return m, nil
}

// View renders the current state as a string.
func (m *Model) View() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var sb strings.Builder
	sb.WriteString("== vikingbot console ==\n")
	sb.WriteString("channels: ")
	if len(m.state.Channels) == 0 {
		sb.WriteString("(none)")
	} else {
		sb.WriteString(strings.Join(m.state.Channels, ", "))
	}
	sb.WriteString("\n\n-- recent messages --\n")
	for _, msg := range m.state.Messages {
		sb.WriteString("  ")
		sb.WriteString(msg)
		sb.WriteString("\n")
	}
	sb.WriteString("\n-- recent replies --\n")
	for _, r := range m.state.Replies {
		sb.WriteString("  ")
		sb.WriteString(r)
		sb.WriteString("\n")
	}
	return sb.String()
}

// Init implements bubbletea.Model.
func (m *Model) Init() tea.Cmd { return nil }

// Run launches the bubbletea program on stdin/stdout. It blocks until
// the user quits. The ctx is used to drive a state-refresh ticker;
// when ctx is canceled the program exits.
func Run(ctx context.Context, dispatcher *channels.Dispatcher, out io.Writer) error {
	if dispatcher == nil {
		return fmt.Errorf("console: dispatcher is nil")
	}
	m := NewModel()
	p := tea.NewProgram(m, tea.WithoutSignalHandler(), tea.WithOutput(out))
	go func() {
		<-ctx.Done()
		p.Quit()
	}()
	// Initial state push.
	m.SetState(State{Channels: dispatcher.Names()})
	_, err := p.Run()
	return err
}
