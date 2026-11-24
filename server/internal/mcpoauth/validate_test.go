// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

package mcpoauth

import (
	"errors"
	"strings"
	"testing"
)

func validInput() ServerInput {
	return ServerInput{
		WorkspaceID: "00000000-0000-0000-0000-000000000001",
		AgentID:     "00000000-0000-0000-0000-000000000002",
		CreatedBy:   "00000000-0000-0000-0000-000000000003",
		Name:        "gateway",
		URL:         "https://mcp.example.com/mcp",
	}
}

func TestValidateServerInput(t *testing.T) {
	if err := validateServerInput(validInput()); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
	cases := map[string]func(*ServerInput){
		"empty name":            func(in *ServerInput) { in.Name = "" },
		"name with space":       func(in *ServerInput) { in.Name = "my server" },
		"name starting with -":  func(in *ServerInput) { in.Name = "-x" },
		"name too long":         func(in *ServerInput) { in.Name = strings.Repeat("a", 65) },
		"http url":              func(in *ServerInput) { in.URL = "http://mcp.example.com/mcp" },
		"url with userinfo":     func(in *ServerInput) { in.URL = "https://u:p@mcp.example.com/mcp" },
		"url with query":        func(in *ServerInput) { in.URL = "https://mcp.example.com/mcp?k=v" },
		"url with fragment":     func(in *ServerInput) { in.URL = "https://mcp.example.com/mcp#x" },
		"url without host":      func(in *ServerInput) { in.URL = "https:///mcp" },
		"scope with newline":    func(in *ServerInput) { in.Scope = "a\nb" },
		"authorization header":  func(in *ServerInput) { in.ExtraHeaders = map[string]string{"Authorization": "x"} },
		"host header":           func(in *ServerInput) { in.ExtraHeaders = map[string]string{"host": "x"} },
		"header name with :":    func(in *ServerInput) { in.ExtraHeaders = map[string]string{"x:y": "x"} },
		"header value newline":  func(in *ServerInput) { in.ExtraHeaders = map[string]string{"x-key": "a\r\nb"} },
		"empty header value":    func(in *ServerInput) { in.ExtraHeaders = map[string]string{"x-key": ""} },
		"header given twice":    func(in *ServerInput) { in.ExtraHeaders = map[string]string{"X-Key": "a", "x-key": "b"} },
		"too many headers":      func(in *ServerInput) { in.ExtraHeaders = manyHeaders(maxExtraHeaders + 1) },
		"header value too long": func(in *ServerInput) { in.ExtraHeaders = map[string]string{"x-key": strings.Repeat("v", 4097)} },
	}
	for name, mutate := range cases {
		in := validInput()
		mutate(&in)
		err := validateServerInput(in)
		if !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: err = %v, want ErrInvalidInput", name, err)
		}
	}
}

// The error for a bad header value must not repeat the value: it is usually a
// key.
func TestHeaderValueNotEchoedInError(t *testing.T) {
	in := validInput()
	in.ExtraHeaders = map[string]string{"x-key": "sk-secret-value\n"}
	err := validateServerInput(in)
	if err == nil || strings.Contains(err.Error(), "sk-secret-value") {
		t.Fatalf("err = %v", err)
	}
}

func manyHeaders(n int) map[string]string {
	out := map[string]string{}
	for i := range n {
		out["x-h"+strings.Repeat("a", i+1)] = "v"
	}
	return out
}
