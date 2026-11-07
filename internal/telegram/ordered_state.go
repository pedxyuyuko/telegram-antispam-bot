package telegram

import (
	"bytes"
	"encoding/json"
	"sort"
)

// SORS presents state values in JSON traversal order. Enrichment keeps existing
// object keys and array positions in place, appending only newly added fields.
func marshalStateOrder(original json.RawMessage, updated any) (json.RawMessage, error) {
	if len(original) == 0 {
		return json.Marshal(updated)
	}
	switch value := updated.(type) {
	case map[string]any:
		decoder := json.NewDecoder(bytes.NewReader(original))
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		if token != json.Delim('{') {
			return json.Marshal(updated)
		}
		var out bytes.Buffer
		out.WriteByte('{')
		seen := map[string]bool{}
		first := true
		write := func(key string, old json.RawMessage) error {
			item, exists := value[key]
			if !exists || seen[key] {
				return nil
			}
			seen[key] = true
			raw, err := marshalStateOrder(old, item)
			if err != nil {
				return err
			}
			encodedKey, _ := json.Marshal(key)
			if !first {
				out.WriteByte(',')
			}
			first = false
			out.Write(encodedKey)
			out.WriteByte(':')
			out.Write(raw)
			return nil
		}
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			var old json.RawMessage
			if err := decoder.Decode(&old); err != nil {
				return nil, err
			}
			if err := write(token.(string), old); err != nil {
				return nil, err
			}
		}
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
		var added []string
		for key := range value {
			if !seen[key] {
				added = append(added, key)
			}
		}
		sort.Strings(added)
		for _, key := range added {
			if err := write(key, nil); err != nil {
				return nil, err
			}
		}
		out.WriteByte('}')
		return out.Bytes(), nil
	case []any:
		var old []json.RawMessage
		if json.Unmarshal(original, &old) != nil {
			return json.Marshal(updated)
		}
		items := make([]json.RawMessage, len(value))
		for i, item := range value {
			var previous json.RawMessage
			if i < len(old) {
				previous = old[i]
			}
			raw, err := marshalStateOrder(previous, item)
			if err != nil {
				return nil, err
			}
			items[i] = raw
		}
		return json.Marshal(items)
	default:
		return json.Marshal(updated)
	}
}
