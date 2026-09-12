package forum

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

func validName(name string) bool {
	if name == "" || len(name) > 64 || !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return false
		}
	}
	return true
}

// RegisterName binds a model-chosen name to a stable routing ID permanently.
func (b *Board) RegisterName(id, name string) error {
	for _, r := range name {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return fmt.Errorf("name must be single-line without control/format characters")
		}
	}
	name = strings.TrimSpace(name)
	if !validName(name) {
		return fmt.Errorf("name must be a nonempty single line, at most 64 UTF-8 bytes, with no control/format characters")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	a, ok := b.agents[id]
	if !ok || a.Stage == "user" || a.Stage == "system" {
		return fmt.Errorf("this identity cannot register an agent name")
	}
	if a.Name != "" {
		if a.Name == name {
			return nil
		}
		return fmt.Errorf("name already registered as %q; renaming is not allowed", a.Name)
	}
	for _, reserved := range []string{"user", "system", "coordinator"} {
		if strings.EqualFold(name, reserved) {
			return fmt.Errorf("name is reserved; choose another name")
		}
	}
	for otherID, other := range b.agents {
		if strings.EqualFold(name, otherID) || (other.Name != "" && strings.EqualFold(name, other.Name)) {
			return fmt.Errorf("name is already taken or reserved; choose another name")
		}
	}
	a.Name = name
	b.agents[id] = a
	for i := range b.msgs {
		if b.msgs[i].AgentID == id {
			b.bytes += len(name) - len(b.msgs[i].AgentName)
			b.msgs[i].AgentName = name
		}
	}
	b.trimLocked()
	b.signalLocked()
	return nil
}

func (b *Board) Name(id string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.agents[id].Name
}

// RestoreNames is used on a fresh board before it is exposed to workers.
func (b *Board) RestoreNames(names map[string]string) error {
	for id := range names {
		b.mu.Lock()
		_, exists := b.agents[id]
		b.mu.Unlock()
		if !exists {
			b.Register(id, "audit") // Historical verifier; stable ID remains reserved.
			b.SetStatus(id, "completed")
		}
	}
	for id, name := range names {
		if err := b.RegisterName(id, name); err != nil {
			return fmt.Errorf("restore name for %s: %w", id, err)
		}
	}
	return nil
}
