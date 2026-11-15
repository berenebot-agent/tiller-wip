package server

import "encoding/json"

// omitTemperature lets the provider use its default when the target explicitly
// reports that temperature is unsupported. Other sampling controls stay intact.
func omitTemperature(body []byte) []byte {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil || raw == nil {
		return body
	}
	if _, ok := raw["temperature"]; !ok {
		return body
	}
	delete(raw, "temperature")
	out, err := json.Marshal(raw)
	if err != nil {
		return body
	}
	return out
}
