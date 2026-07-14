package threadusage

type exactKey struct {
	messageID string
	requestID string
}

// Dedup drops replayed usage lines using OpenUsage/ccusage semantics:
// primary key (message.id, requestId), plus message-id sidechain replay detection.
// Preference: non-sidechain, then larger token total, then richer (has speed) records.
func Dedup(entries []Record) []Record {
	deduped := make([]Record, 0, len(entries))
	exactIndex := make(map[exactKey]int, len(entries))
	messageIndex := make(map[string][]int, len(entries))

	for _, entry := range entries {
		if entry.MessageID == "" {
			deduped = append(deduped, entry)
			continue
		}
		key := exactKey{messageID: entry.MessageID, requestID: entry.RequestID}
		collision := -1
		if idx, ok := exactIndex[key]; ok {
			collision = idx
		} else if indexes := messageIndex[entry.MessageID]; len(indexes) > 0 {
			for _, idx := range indexes {
				if entry.isSidechain || deduped[idx].isSidechain {
					collision = idx
					break
				}
			}
		}

		if collision >= 0 {
			if shouldReplace(entry, deduped[collision]) {
				old := deduped[collision]
				if old.MessageID != "" {
					delete(exactIndex, exactKey{messageID: old.MessageID, requestID: old.RequestID})
				}
				deduped[collision] = entry
				exactIndex[key] = collision
			}
			continue
		}

		idx := len(deduped)
		deduped = append(deduped, entry)
		exactIndex[key] = idx
		messageIndex[entry.MessageID] = append(messageIndex[entry.MessageID], idx)
	}
	return deduped
}

func shouldReplace(candidate, existing Record) bool {
	if candidate.isSidechain != existing.isSidechain {
		// Prefer non-sidechain (parent) entry.
		return existing.isSidechain
	}
	if candidate.TotalTokens() != existing.TotalTokens() {
		return candidate.TotalTokens() > existing.TotalTokens()
	}
	return candidate.hasSpeed && !existing.hasSpeed
}
