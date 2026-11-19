// Added by the mouse9527/multica fork: Evolution P2, redaction (M-136).

package evolution

import (
	"regexp"
	"strings"
)

// redactionRule replaces every match of pattern. When keep is set, the
// replacement keeps the first submatch (the part that is not sensitive) in
// front of the placeholder; when group is set, only the first submatch is
// replaced and the rest of the match stays.
type redactionRule struct {
	pattern     *regexp.Regexp
	placeholder string
	keep        bool
	group       bool
}

// redactionRules run in order; earlier rules see the original text. They
// favour false positives over leaks: the agent reads for patterns, not for
// exact strings, and a redacted secret costs it nothing.
var redactionRules = []redactionRule{
	// PEM private keys, the whole block.
	{pattern: regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`), placeholder: "[redacted:private-key]"},
	// JSON web tokens.
	{pattern: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), placeholder: "[redacted:token]"},
	// Well-known token prefixes: Multica, GitHub, Slack, OpenAI/Anthropic-style
	// sk-/pk-/rk- keys, Google API keys, AWS access key ids.
	{pattern: regexp.MustCompile(`\b(?:mul_|mat_|mcn_|ghp_|gho_|ghu_|ghs_|ghr_|github_pat_|glpat-|xox[abposr]-|sk-|pk-|rk-|sk_live_|sk_test_|AIza)[A-Za-z0-9_\-]{12,}`), placeholder: "[redacted:token]"},
	{pattern: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), placeholder: "[redacted:token]"},
	// Authorization headers.
	{pattern: regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=\-]{12,}`), placeholder: "[redacted:token]"},
	// key=value / key: value pairs whose key names a secret.
	{pattern: regexp.MustCompile(`(?i)\b((?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|client[_-]?secret)["']?\s*[:=]\s*)["']?[^\s"',;]{4,}["']?`), placeholder: "[redacted:secret]", keep: true},
	// Credentials embedded in URLs: scheme://user:pass@host.
	{pattern: regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^\s/@:]+:[^\s/@]+@`), placeholder: "[redacted:credentials]@", keep: true},
	// URL query strings (signatures, tokens, tracking ids).
	{pattern: regexp.MustCompile(`(https?://[^\s?#<>"'()]+)\?[^\s#<>"'()]+`), placeholder: "?[redacted:query]", keep: true},
	// Email addresses.
	{pattern: regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`), placeholder: "[redacted:email]"},
	// Phone numbers: mainland China mobile numbers, and international numbers
	// written with a leading +.
	{pattern: regexp.MustCompile(`(?:^|[^0-9])(1[3-9][0-9]{9})(?:$|[^0-9])`), placeholder: "[redacted:phone]", group: true},
	{pattern: regexp.MustCompile(`\+[1-9][0-9]{0,2}[ \-]?(?:[0-9][ \-]?){6,13}[0-9]`), placeholder: "[redacted:phone]"},
	// Private network addresses and internal host names.
	{pattern: regexp.MustCompile(`\b(?:10\.\d{1,3}|192\.168|172\.(?:1[6-9]|2\d|3[01]))\.\d{1,3}\.\d{1,3}\b`), placeholder: "[redacted:host]"},
	{pattern: regexp.MustCompile(`(?i)\b[a-z0-9](?:[a-z0-9\-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9\-]*[a-z0-9])?)*\.(?:internal|intranet|corp|lan|local|home\.arpa)\b`), placeholder: "[redacted:host]"},
}

// Redact replaces secrets, credentials, contact details, internal hosts, and
// URL query strings in s with placeholders and reports how many it replaced.
func Redact(s string) (string, int) {
	total := 0
	for _, rule := range redactionRules {
		if rule.group {
			// Matches consume the delimiter after them, so a run of adjacent
			// matches needs another pass for every other one.
			for pass := 0; pass < 3; pass++ {
				var n int
				s, n = replaceGroup(rule.pattern, s, rule.placeholder)
				total += n
				if n == 0 {
					break
				}
			}
			continue
		}
		s = rule.pattern.ReplaceAllStringFunc(s, func(m string) string {
			total++
			if rule.keep {
				if sub := rule.pattern.FindStringSubmatch(m); len(sub) > 1 {
					return sub[1] + rule.placeholder
				}
			}
			return rule.placeholder
		})
	}
	return s, total
}

// replaceGroup replaces only the first submatch of every match, keeping the
// delimiters around it.
func replaceGroup(pattern *regexp.Regexp, s, placeholder string) (string, int) {
	locs := pattern.FindAllStringSubmatchIndex(s, -1)
	if len(locs) == 0 {
		return s, 0
	}
	var b strings.Builder
	last := 0
	for _, loc := range locs {
		b.WriteString(s[last:loc[2]])
		b.WriteString(placeholder)
		last = loc[3]
	}
	b.WriteString(s[last:])
	return b.String(), len(locs)
}
