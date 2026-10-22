package runpod

import (
	"encoding/json"
	"strconv"
	"strings"
)

// sshEndpoint returns only programmable SSH endpoints that support command
// execution and file transfer. RunPod's ssh.runpod.io proxy is interactive
// terminal-only and must not be advertised to installers.
func (p *pod) sshEndpoint() (host string, port int, user string) {
	if direct := p.SSH.Direct; direct.Host != "" && direct.Port > 0 {
		return direct.Host, direct.Port, firstNonEmpty(direct.Username, "root")
	}

	host = p.PublicIP
	raws := make([]json.RawMessage, 0, 4)
	if len(p.Runtime) > 0 {
		var runtime struct {
			Ports        json.RawMessage `json:"ports"`
			PortMappings json.RawMessage `json:"portMappings"`
		}
		if err := json.Unmarshal(p.Runtime, &runtime); err == nil {
			raws = append(raws, runtime.Ports, runtime.PortMappings)
		}
	}
	raws = append(raws, p.PortMappings, p.Ports)
	for _, raw := range raws {
		if len(raw) == 0 {
			continue
		}
		var asMap map[string]json.RawMessage
		if err := json.Unmarshal(raw, &asMap); err == nil {
			for key, value := range asMap {
				if key == "22" || strings.HasPrefix(key, "22/") {
					if number, ok := jsonInt(value); ok {
						return host, int(number), "root"
					}
				}
			}
		}
		var mappings []struct {
			PrivatePort json.RawMessage `json:"privatePort"`
			PublicPort  json.RawMessage `json:"publicPort"`
			Private     json.RawMessage `json:"private"`
			Public      json.RawMessage `json:"public"`
			IP          string          `json:"ip"`
			Type        string          `json:"type"`
		}
		if err := json.Unmarshal(raw, &mappings); err == nil {
			for _, mapping := range mappings {
				privatePort, _ := jsonInt(mapping.PrivatePort)
				if privatePort == 0 {
					privatePort, _ = jsonInt(mapping.Private)
				}
				if privatePort != 22 || (mapping.Type != "" && !strings.EqualFold(mapping.Type, "tcp")) {
					continue
				}
				if mapping.IP != "" {
					host = mapping.IP
				}
				publicPort, _ := jsonInt(mapping.PublicPort)
				if publicPort == 0 {
					publicPort, _ = jsonInt(mapping.Public)
				}
				if host != "" && publicPort != 0 {
					return host, int(publicPort), "root"
				}
			}
		}
	}
	return "", 0, ""
}

func jsonInt(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		value, err := strconv.ParseInt(string(number), 10, 64)
		return value, err == nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return 0, false
	}
	value, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	return value, err == nil
}
