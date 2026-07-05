// Package tui implements `ov tui` — a small bubbletea-based resource
// browser. The user can navigate the resource tree and press Enter to
// view a resource's raw content streamed from /api/v1/content/<uri>.
//
// To avoid an import cycle with the parent cli package, tui defines a
// narrow Client interface and accepts a resolver function that returns
// one. *cli.Client satisfies the interface; the cli package wires it
// in at RunE time so the connection reflects the resolved config.
package tui

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// Client is the subset of *cli.Client the TUI needs. Defining it here
// keeps tui free of any dependency on the parent cli package.
type Client interface {
	GetJSON(ctx context.Context, path string, out any) error
	Raw(ctx context.Context, method, path string) (*http.Response, error)
	BaseURL() string
}

// Resolver returns a Client at RunE time. The cli package supplies a
// closure that calls rt.EnsureClient and yields rt.Client.
type Resolver func(cmd *cobra.Command) (Client, error)

// NewCmd returns the `ov tui` cobra command.
func NewCmd(resolve Resolver, out io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Enter the interactive TUI",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if resolve == nil {
				return fmt.Errorf("tui: client resolver is required (run `ov init` first)")
			}
			c, err := resolve(cmd)
			if err != nil {
				return err
			}
			if c == nil {
				return fmt.Errorf("tui: client is required (run `ov init` first)")
			}
			m := newModel(c)
			p := tea.NewProgram(m, tea.WithAltScreen())
			_, err = p.Run()
			return err
		},
	}
	_ = out // reserved for future non-TUI output
	return cmd
}

// item is a list.Item wrapping a domain.Resource.
type item struct {
	res domain.Resource
}

func (i item) Title() string { return i.res.Name }
func (i item) Description() string {
	return fmt.Sprintf("%s  size=%d  mod=%s", i.res.Type, i.res.Size, i.res.ModifiedAt.Format("2006-01-02"))
}
func (i item) FilterValue() string { return i.res.Name + " " + i.res.URI }

// model holds TUI state: the current resource list and a content view
// for the selected entry.
type model struct {
	list    list.Model
	client  Client
	path    string
	content string
	viewing bool
	width   int
	height  int
}

func newModel(client Client) model {
	delegate := list.NewDefaultDelegate()
	l := list.New(nil, delegate, 80, 24)
	l.Title = "OpenViking"
	return model{list: l, client: client, path: "/"}
}

func (m model) Init() tea.Cmd {
	return m.load(m.path)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.list.SetSize(msg.Width, msg.Height-4)
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "enter":
			if sel, ok := m.list.SelectedItem().(item); ok {
				return m, m.fetchContent(sel.res.URI)
			}
		case "esc":
			m.viewing = false
			m.content = ""
		}
	case loadMsg:
		m.list.SetItems(msg.items)
		m.list.Title = fmt.Sprintf("OpenViking: %s (%d)", m.path, len(msg.items))
	case contentMsg:
		m.viewing = true
		m.content = msg.content
		m.list.Title = fmt.Sprintf("OpenViking: viewing %s (esc to return)", msg.uri)
	}
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m model) View() string {
	if m.viewing {
		body := m.content
		if len(body) > 4000 {
			body = body[:4000] + "\n... (truncated)"
		}
		return lipgloss.NewStyle().Padding(1, 2).Render(body)
	}
	if m.width == 0 {
		return "Loading..."
	}
	return m.list.View()
}

// load returns a tea.Cmd that fetches the resource list at path.
func (m model) load(path string) tea.Cmd {
	return func() tea.Msg {
		items, err := fetchItems(context.Background(), m.client, path)
		if err != nil {
			return loadMsg{items: nil, err: err}
		}
		return loadMsg{items: items}
	}
}

func (m model) fetchContent(uri string) tea.Cmd {
	return func() tea.Msg {
		resp, err := m.client.Raw(context.Background(), http.MethodGet, "/api/v1/content/"+strings.TrimPrefix(uri, "/"))
		if err != nil {
			return contentMsg{uri: uri, content: "error: " + err.Error()}
		}
		defer resp.Body.Close()
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return contentMsg{uri: uri, content: string(buf)}
	}
}

type loadMsg struct {
	items []list.Item
	err   error
}

type contentMsg struct {
	uri     string
	content string
}

// fetchItems queries /api/v1/fs/ls?path=<path> and converts the
// response into list.Item values.
func fetchItems(ctx context.Context, client Client, path string) ([]list.Item, error) {
	q := ""
	if path != "" && path != "/" {
		q = "?path=" + path
	}
	var resp struct {
		Items []domain.Resource `json:"items"`
	}
	if err := client.GetJSON(ctx, "/api/v1/fs/ls"+q, &resp); err != nil {
		return nil, err
	}
	items := make([]list.Item, 0, len(resp.Items))
	for _, r := range resp.Items {
		items = append(items, item{res: r})
	}
	return items, nil
}
