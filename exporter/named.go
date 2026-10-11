package exporter

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// TargetList holds the targets by name. In the configuration file it is a list, and every entry has a "name":
//
//	"targets": [ {"name": "lab", "server": "...", ...}, {"name": "prod", ...} ]
//
// The name identifies the entry in /probe?target=NAME and in EASEXPORTER_TARGET.
type TargetList map[string]Target

// MailMonitorList holds the mail monitor targets by name, a list with a "name" in every entry like the targets.
type MailMonitorList map[string]MailMonitorTarget

func (l *TargetList) UnmarshalJSON(data []byte) error {
	m, err := decodeNamed[Target](data, "targets")
	*l = m
	return err
}

func (l *MailMonitorList) UnmarshalJSON(data []byte) error {
	m, err := decodeNamed[MailMonitorTarget](data, "mail_monitor")
	*l = m
	return err
}

// decodeNamed reads a list of objects with a "name" into a map by name. The other keys are read strictly: an unknown one
// is an error. A missing or repeated name is an error as well, where a map in the file would silently keep the last.
func decodeNamed[T any](data []byte, what string) (map[string]T, error) {
	var list []map[string]json.RawMessage
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("%s must be a list of entries, each with a \"name\": %v", what, err)
	}
	out := make(map[string]T, len(list))
	for i, entry := range list {
		var name string
		if raw, ok := entry["name"]; !ok || json.Unmarshal(raw, &name) != nil || name == "" {
			return nil, fmt.Errorf("%s[%d]: needs a \"name\"", what, i)
		}
		delete(entry, "name")
		if _, again := out[name]; again {
			return nil, fmt.Errorf("%s: the name %q is used twice", what, name)
		}
		body, err := json.Marshal(entry)
		if err != nil {
			return nil, err
		}
		var v T
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("%s %q: %v", what, name, err)
		}
		out[name] = v
	}
	return out, nil
}
