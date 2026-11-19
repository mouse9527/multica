// Added by the mouse9527/multica fork: Evolution P2 (M-136).

package evolution

import (
	"strings"
	"testing"
)

func TestRedactReplacesSensitiveText(t *testing.T) {
	cases := []struct {
		name, in, gone, want string
	}{
		{"multica token", "use mul_abcdefghijklmnop1234 to log in", "mul_abcdefghijklmnop1234", "[redacted:token]"},
		{"github token", "GH ghp_ABCDEFGHIJKLMNOPQRSTUVWX1234 here", "ghp_ABCDEFGHIJKLMNOPQRSTUVWX1234", "[redacted:token]"},
		{"api key", "export sk-ant-api03-abcdefghijklmnop", "sk-ant-api03-abcdefghijklmnop", "[redacted:token]"},
		{"aws key", "AKIAABCDEFGHIJKLMNOP", "AKIAABCDEFGHIJKLMNOP", "[redacted:token]"},
		{"jwt", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.c2lnbmF0dXJlMTIz", "eyJhbGciOiJIUzI1NiJ9", "[redacted:token]"},
		{"bearer", "Authorization: Bearer abcdefghijklmnopqrstu", "abcdefghijklmnopqrstu", "[redacted:token]"},
		{"key value", "password=hunter2hunter", "hunter2hunter", "password=[redacted:secret]"},
		{"url credentials", "git clone https://bob:s3cret@example.com/repo", "s3cret", "https://[redacted:credentials]@example.com/repo"},
		{"query string", "see https://example.com/doc?sig=abc&exp=1 now", "sig=abc", "https://example.com/doc?[redacted:query] now"},
		{"email", "mail alice@example.com please", "alice@example.com", "[redacted:email]"},
		{"cn mobile", "call 13800138000, thanks", "13800138000", "call [redacted:phone], thanks"},
		{"intl phone", "call +1 415 555 0100 now", "415 555 0100", "[redacted:phone]"},
		{"private ip", "db at 192.168.1.20:5432", "192.168.1.20", "[redacted:host]"},
		{"internal host", "open grafana.ops.internal now", "grafana.ops.internal", "[redacted:host]"},
		{"private key", "-----BEGIN RSA PRIVATE KEY-----\nMIIabc\n-----END RSA PRIVATE KEY-----", "MIIabc", "[redacted:private-key]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, n := Redact(tc.in)
			if strings.Contains(got, tc.gone) {
				t.Fatalf("Redact(%q) = %q, still contains %q", tc.in, got, tc.gone)
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("Redact(%q) = %q, want it to contain %q", tc.in, got, tc.want)
			}
			if n == 0 {
				t.Fatalf("Redact(%q) reported no replacement", tc.in)
			}
		})
	}
}

func TestRedactAdjacentPhonesAreAllReplaced(t *testing.T) {
	got, n := Redact("13800138000,13900139000")
	if got != "[redacted:phone],[redacted:phone]" || n != 2 {
		t.Fatalf("got %q (%d replacements)", got, n)
	}
}

func TestRedactLeavesOrdinaryTextAlone(t *testing.T) {
	for _, in := range []string{
		"M-136 moved to in_review on 2026-11-19 at 10:00",
		"task-assignment handler 01a1ed8e-bce0-7505-822b-f98c4ab3a6ca failed twice",
		"see https://github.com/mouse9527/multica/pull/113 for the diff",
		"commit 3c42201aec70ee518575b93235dfa59f5de5943a",
		"进化建议：把验收标准写进 Issue 正文",
	} {
		if got, n := Redact(in); got != in || n != 0 {
			t.Errorf("Redact(%q) = %q (%d replacements), want it unchanged", in, got, n)
		}
	}
}
