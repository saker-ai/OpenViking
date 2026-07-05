package console

import (
	"strings"
	"testing"
)

func TestNewModel_EmptyView(t *testing.T) {
	m := NewModel()
	v := m.View()
	if !strings.Contains(v, "vikingbot console") {
		t.Errorf("view missing header: %q", v)
	}
	if !strings.Contains(v, "channels: (none)") {
		t.Errorf("view missing empty channels: %q", v)
	}
}

func TestModel_UpdateAndRender(t *testing.T) {
	m := NewModel()
	m.SetState(State{
		Channels: []string{"telegram", "slack"},
		Messages: []string{"[telegram] hello", "[slack] hi"},
		Replies:  []string{"hi back"},
	})
	v := m.View()
	if !strings.Contains(v, "telegram, slack") {
		t.Errorf("channels not rendered: %q", v)
	}
	if !strings.Contains(v, "[telegram] hello") {
		t.Errorf("message not rendered: %q", v)
	}
	if !strings.Contains(v, "hi back") {
		t.Errorf("reply not rendered: %q", v)
	}
}

func TestModel_HistoryCapped(t *testing.T) {
	m := NewModel()
	msgs := make([]string, 200)
	for i := range msgs {
		msgs[i] = "msg"
	}
	m.SetState(State{Messages: msgs})
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.state.Messages) > 100 {
		t.Errorf("messages = %d, want <= 100", len(m.state.Messages))
	}
}
