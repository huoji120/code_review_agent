package forum

import "fmt"

func (b *Board) joinThreadLocked(m Message) {
	members := b.participants[m.ThreadID]
	if members == nil {
		members = make(map[string]int64)
		b.participants[m.ThreadID] = members
	}
	if members[m.AgentID] == 0 {
		members[m.AgentID] = m.ID
	}
}

func (b *Board) RestoreParticipants(saved map[int64]map[string]int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for thread, members := range saved {
		current, retained := b.participants[thread]
		if !retained {
			continue
		}
		for id, joined := range members {
			if id == "" || joined < thread || joined > b.nextID {
				return fmt.Errorf("invalid forum participant checkpoint")
			}
			if current[id] == 0 || joined < current[id] {
				current[id] = joined
			}
		}
	}
	return nil
}

// Checkpoint atomically snapshots posts, permanent names, and retained-thread
// membership so concurrent posting/retention cannot split their provenance.
func (b *Board) Checkpoint() ([]Message, map[string]string, map[int64]map[string]int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	names := make(map[string]string)
	for id, a := range b.agents {
		if a.Name != "" {
			names[id] = a.Name
		}
	}
	participants := make(map[int64]map[string]int64, len(b.participants))
	for thread, members := range b.participants {
		copied := make(map[string]int64, len(members))
		for id, joined := range members {
			copied[id] = joined
		}
		participants[thread] = copied
	}
	return append([]Message(nil), b.msgs...), names, participants
}
