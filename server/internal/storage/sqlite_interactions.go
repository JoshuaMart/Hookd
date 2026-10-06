package storage

import (
	"encoding/json"
)

// AddInteraction stores an interaction for a known hook. Interactions for hooks
// this store does not own are ignored (the composite routes them elsewhere).
func (m *SQLiteManager) AddInteraction(hookID string, interaction *Interaction) {
	if !m.Has(hookID) {
		return
	}

	m.truncateBody(interaction)
	data, err := json.Marshal(interaction.Data)
	if err != nil {
		m.logger.Error("failed to encode interaction data", "error", err, "hook_id", hookID)
		return
	}

	// The hook can be deleted (expiry) after the Has() check above; a missing
	// row then means it is gone, so dropping the interaction is correct.
	if err := m.insertInteraction(hookID, interaction, string(data)); err != nil {
		m.logger.Debug("failed to persist interaction", "error", err, "hook_id", hookID)
	}
}

// insertInteraction assigns the hook's next seq and stores the interaction.
func (m *SQLiteManager) insertInteraction(hookID string, interaction *Interaction, data string) error {
	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var seq int64
	if err := tx.QueryRow(
		`UPDATE hooks SET last_seq = last_seq + 1 WHERE id = ? RETURNING last_seq`, hookID,
	).Scan(&seq); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO interactions (id, hook_id, type, timestamp, source_ip, data, seq) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		interaction.ID, hookID, string(interaction.Type), interaction.Timestamp.UnixNano(), interaction.SourceIP, data, seq,
	); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	interaction.Seq = seq
	return nil
}

// truncateBody caps the stored HTTP body at maxBodyBytes, flagging the entry so
// clients know it was cut. Long-lived hooks can accumulate for weeks, so full
// multi-megabyte bodies are not kept on disk. The cut is made on a UTF-8 rune
// boundary so the stored body is never left with a mangled trailing rune.
func (m *SQLiteManager) truncateBody(interaction *Interaction) {
	body, ok := interaction.Data["body"].(string)
	if !ok {
		return
	}
	cut, truncated := TruncateBody(body, m.maxBodyBytes)
	if !truncated {
		return
	}
	interaction.Data["body"] = cut
	interaction.Data["truncated"] = true
}
