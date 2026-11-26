// Added by the mouse9527/multica fork: cross-workspace read-only access (M-307).

package crossread

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Every route the middleware can record must be storable: valid UTF-8 and at
// most maxRouteLen bytes (so at most maxRouteLen characters), or the audit
// write fails and the refusal it records turns into a 503.
func TestAuditRouteIsAlwaysStorable(t *testing.T) {
	cases := map[string]string{
		"short":                       "GET /api/issues/*",
		"long ASCII":                  "PUT /" + strings.Repeat("a", 600),
		"multibyte across the cut":    "PUT /api/issues/" + strings.Repeat("中", 300),
		"invalid UTF-8 from the path": "PUT /api/issues/\xff\xfe",
		"long and invalid":            "PUT /" + strings.Repeat("\xff", 700),
		"NUL from %00":                "PUT /api/issues/a\x00b",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			got := auditRoute(in)
			if !utf8.ValidString(got) || len(got) > maxRouteLen || got == "" || strings.ContainsRune(got, 0) {
				t.Fatalf("auditRoute(%q) = %q (%d bytes, valid=%v)", in, got, len(got), utf8.ValidString(got))
			}
			if utf8.ValidString(in) && len(in) <= maxRouteLen && !strings.ContainsRune(in, 0) && got != in {
				t.Fatalf("a storable route was changed: %q -> %q", in, got)
			}
		})
	}
}
