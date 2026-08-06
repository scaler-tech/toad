package cmd

import (
	"encoding/json"
	"testing"

	"github.com/scaler-tech/toad/internal/config"
	islack "github.com/scaler-tech/toad/internal/slack"
)

func TestPublishKnownChannels_NilSafe(t *testing.T) {
	// Must not panic with either argument nil.
	publishKnownChannels(nil, nil)
	publishKnownChannels(openGateTestDB(t), nil)
	publishKnownChannels(nil, islack.NewClient(config.SlackConfig{}))
}

func TestPublishKnownChannels_WritesValidJSON(t *testing.T) {
	db := openGateTestDB(t)
	client := islack.NewClient(config.SlackConfig{})

	// The channel id↔name cache is populated during Run()'s auto-join (or by
	// message/lookup traffic) — internal/slack's own tests
	// (TestChannelSnapshot_SortedByName) cover that sorting contract in
	// isolation. Here we only need to verify publishKnownChannels writes
	// whatever ChannelSnapshot returns as valid JSON under the right key,
	// which the empty-cache case (a fresh client) already exercises.
	publishKnownChannels(db, client)

	raw, err := db.GetSetting(knownChannelsSettingKey)
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if raw == "" {
		t.Fatal("expected known_channels setting to be written")
	}
	var got []knownChannel
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty channel list from a fresh client, got %v", got)
	}
}

func TestPublishKnownChannels_UnpublishedSettingIsAbsent(t *testing.T) {
	db := openGateTestDB(t)
	raw, err := db.GetSetting(knownChannelsSettingKey)
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if raw != "" {
		t.Fatalf("expected no known_channels setting before publish, got %q", raw)
	}
}
