package storage

// PollInteractions atomically returns and deletes all interactions for a hook.
func (m *SQLiteManager) PollInteractions(hookID string) []*Interaction {
	if !m.Has(hookID) {
		return []*Interaction{}
	}

	tx, err := m.db.Begin()
	if err != nil {
		m.logger.Error("failed to begin poll transaction", "error", err, "hook_id", hookID)
		return []*Interaction{}
	}
	defer tx.Rollback()

	interactions, err := queryInteractions(tx, hookID, -1)
	if err != nil {
		m.logger.Error("failed to read interactions", "error", err, "hook_id", hookID)
		return []*Interaction{}
	}

	if len(interactions) > 0 {
		if _, err := tx.Exec(`DELETE FROM interactions WHERE hook_id = ?`, hookID); err != nil {
			m.logger.Error("failed to clear interactions", "error", err, "hook_id", hookID)
			return []*Interaction{}
		}
	}

	if err := tx.Commit(); err != nil {
		m.logger.Error("failed to commit poll", "error", err, "hook_id", hookID)
		return []*Interaction{}
	}
	return interactions
}

// PollInteractionsBatch polls several hooks. Unknown hooks yield a not-found
// error, matching the in-memory manager's contract.
func (m *SQLiteManager) PollInteractionsBatch(hookIDs []string) map[string]*PollResult {
	results := make(map[string]*PollResult, len(hookIDs))
	ids := make([]string, 0, len(hookIDs))
	for _, id := range hookIDs {
		if _, seen := results[id]; seen {
			continue
		}
		results[id] = &PollResult{Interactions: []*Interaction{}}
		if !m.Has(id) {
			results[id].Error = "Hook not found"
			continue
		}
		ids = append(ids, id)
	}
	// Bound writer occupancy instead of holding one transaction for the entire
	// API batch. Polling a chunk remains atomic with respect to captures.
	const chunkSize = 64
	for start := 0; start < len(ids); start += chunkSize {
		chunk := ids[start:min(start+chunkSize, len(ids))]
		polled, err := m.pollChunk(chunk)
		if err != nil {
			m.logger.Error("failed to poll interaction batch", "error", err)
			for _, id := range chunk {
				results[id] = &PollResult{Error: "storage error"}
			}
			continue
		}
		for id, interactions := range polled {
			results[id] = &PollResult{Interactions: interactions}
		}
	}
	return results
}

// pollChunk decodes before deleting/committing, so a corrupt row cannot be
// silently drained. On any failure the whole chunk stays available for retry.
func (m *SQLiteManager) pollChunk(ids []string) (map[string][]*Interaction, error) {
	tx, err := m.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	results := make(map[string][]*Interaction, len(ids))
	for _, id := range ids {
		interactions, err := queryInteractions(tx, id, -1)
		if err != nil {
			return nil, err
		}
		if len(interactions) > 0 {
			if _, err := tx.Exec(`DELETE FROM interactions WHERE hook_id = ?`, id); err != nil {
				return nil, err
			}
		}
		results[id] = interactions
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return results, nil
}
