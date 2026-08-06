package cmd

import (
	"encoding/json"
	"log/slog"

	islack "github.com/scaler-tech/toad/internal/slack"
	"github.com/scaler-tech/toad/internal/state"
)

// knownChannelsSettingKey is the settings-table key the daemon publishes its
// channel inventory under (JSON-encoded []knownChannel, sorted by name). The
// dashboard process (`toad status --port N`) has no Slack connection of its
// own — it shares only the SQLite state DB with the daemon — so this is how
// it learns which channels exist to build the System tab's per-channel
// digest toggle list (see status.go's apiDataHandler, which merges this with
// state.DB.DisabledDigestChannels).
const knownChannelsSettingKey = "known_channels"

// knownChannel is the (id, name) pair persisted under knownChannelsSettingKey.
type knownChannel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// publishKnownChannels snapshots slackClient's channel id↔name cache
// (islack.Client.ChannelSnapshot, populated during auto-join and grown as
// messages/lookups happen) and writes it to the known_channels setting.
// Called once via slackClient.SetOnReady right after auto-join completes
// (root.go), and again every hour thereafter so channels toad joins later
// still show up without a restart. Best-effort: a write failure is logged
// and otherwise ignored — this is discovery data for the dashboard, not
// required for the digest gate itself (digestChannelGate reads
// digest_channel:<id> rows directly, independent of this snapshot).
func publishKnownChannels(db *state.DB, slackClient *islack.Client) {
	if db == nil || slackClient == nil {
		return
	}
	snap := slackClient.ChannelSnapshot()
	channels := make([]knownChannel, len(snap))
	for i, ch := range snap {
		channels[i] = knownChannel{ID: ch.ID, Name: ch.Name}
	}
	data, err := json.Marshal(channels)
	if err != nil {
		slog.Warn("publish known channels: marshal failed", "error", err)
		return
	}
	if err := db.SetSetting(knownChannelsSettingKey, string(data)); err != nil {
		slog.Warn("publish known channels: write failed", "error", err)
	}
}
