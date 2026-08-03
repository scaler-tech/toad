package digest

import (
	"fmt"
	"log/slog"
)

// dedupChannel collapses messages with identical text within a channel.
// The first occurrence is kept; duplicates are removed and their count appended.
func dedupChannel(msgs []Message) []Message {
	type entry struct {
		idx   int // index into result slice
		count int
	}
	seen := make(map[string]*entry)
	var result []Message

	for _, msg := range msgs {
		if e, ok := seen[msg.Text]; ok {
			e.count++
		} else {
			seen[msg.Text] = &entry{idx: len(result), count: 1}
			result = append(result, msg)
		}
	}

	// Append duplicate counts
	for text, e := range seen {
		if e.count > 1 {
			result[e.idx].Text = fmt.Sprintf("%s (x%d duplicates)", text, e.count)
		}
	}

	return result
}

// buildChunks groups messages by channel, deduplicates within each channel,
// and packs them into chunks. A single channel is NEVER split — Haiku needs
// full channel context to correlate messages about the same underlying issue.
// MaxChunkSize only governs coalescing of small channels into mixed chunks.
func (e *Engine) buildChunks(msgs []Message) []chunk {
	maxSize := e.cfg.MaxChunkSize
	if maxSize <= 0 {
		maxSize = 50
	}

	// Group by channel
	byChannel := make(map[string][]Message)
	channelOrder := []string{} // preserve insertion order
	for _, msg := range msgs {
		key := msg.ChannelName
		if _, exists := byChannel[key]; !exists {
			channelOrder = append(channelOrder, key)
		}
		byChannel[key] = append(byChannel[key], msg)
	}

	// rawByChannel retains the pre-dedup originals per channel, one entry per
	// message (no "(xN duplicates)" text collapsing) — used only so a failed
	// chunk's requeueOrDropFailedChunk call can put the ORIGINAL messages back
	// in the buffer, not the dedup-annotated copies. Requeuing the annotated
	// copies would defeat a later dedup pass: the annotated text ("foo (x3
	// duplicates)") would no longer match a fresh, un-annotated "foo" that
	// arrives before the next flush, so they'd stop collapsing together.
	rawByChannel := make(map[string][]Message, len(byChannel))

	// Dedup within each channel and log significant reductions
	for ch, chMsgs := range byChannel {
		raw := make([]Message, len(chMsgs))
		copy(raw, chMsgs)
		rawByChannel[ch] = raw

		deduped := dedupChannel(chMsgs)
		if len(chMsgs) != len(deduped) {
			slog.Info("digest dedup", "channel", ch,
				"before", len(chMsgs), "after", len(deduped))
		}
		byChannel[ch] = deduped
	}

	var chunks []chunk

	// Large channels get their own dedicated chunk (never split)
	var smallChannels []string
	for _, ch := range channelOrder {
		chMsgs := byChannel[ch]
		if len(chMsgs) >= maxSize {
			label := fmt.Sprintf("#%s (%d msgs)", ch, len(chMsgs))
			chunks = append(chunks, chunk{messages: chMsgs, raw: rawByChannel[ch], label: label})
		} else {
			smallChannels = append(smallChannels, ch)
		}
	}

	// Coalesce small channels into mixed chunks up to maxSize
	var current []Message
	var currentRaw []Message
	var currentChannels int
	for _, ch := range smallChannels {
		chMsgs := byChannel[ch]
		if len(current)+len(chMsgs) > maxSize && len(current) > 0 {
			label := fmt.Sprintf("mixed (%d msgs, %d channels)", len(current), currentChannels)
			if currentChannels == 1 {
				label = fmt.Sprintf("#%s (%d msgs)", current[0].ChannelName, len(current))
			}
			chunks = append(chunks, chunk{messages: current, raw: currentRaw, label: label})
			current = nil
			currentRaw = nil
			currentChannels = 0
		}
		current = append(current, chMsgs...)
		currentRaw = append(currentRaw, rawByChannel[ch]...)
		currentChannels++
	}
	if len(current) > 0 {
		label := fmt.Sprintf("mixed (%d msgs, %d channels)", len(current), currentChannels)
		if currentChannels == 1 {
			label = fmt.Sprintf("#%s (%d msgs)", current[0].ChannelName, len(current))
		}
		chunks = append(chunks, chunk{messages: current, raw: currentRaw, label: label})
	}

	return chunks
}
