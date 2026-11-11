package githubissues

import (
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
)

func TestSignedBodyRoundTrips(t *testing.T) {
	agent := Actor{Type: "agent", ID: testUUID(7), Name: "研析\n<b>x</b>"}
	member := Actor{Type: "member", ID: testUUID(8)}
	op := testUUID(9)
	for _, text := range []string{"", "plain", "ends with newline\n", "line\n\n— fake（智能体），经 Multica"} {
		for _, actor := range []Actor{agent, member} {
			body := signedBody(text, actor, op)
			if got := stripMarker(body); got != text {
				t.Fatalf("strip(signed(%q, %s)) = %q", text, actor.Type, got)
			}
			m, ok := parseMarker(body)
			if !ok || m.actorType != actor.Type || m.actorID != util.UUIDToString(actor.ID) || m.op != util.UUIDToString(op) {
				t.Fatalf("parse(%q) = %+v %v", body, m, ok)
			}
			// GitHub may return an edited body with CRLF line breaks.
			if got := stripMarker(strings.ReplaceAll(body, "\n", "\r\n")); got != strings.ReplaceAll(text, "\n", "\r\n") {
				t.Fatalf("CRLF strip = %q", got)
			}
		}
	}
	if body := signedBody("hi", agent, op); !strings.Contains(body, "\n— 研析 ‹b›x‹/b›（智能体），经 Multica\n") {
		t.Fatalf("agent signature = %q", body)
	}
	if body := signedBody("hi", member, op); strings.Contains(body, "智能体") {
		t.Fatalf("member body has a signature: %q", body)
	}
}

func TestParseMarkerOnlyAtTheEnd(t *testing.T) {
	marker := signedBody("", Actor{Type: "member", ID: testUUID(1)}, testUUID(2))
	for _, body := range []string{
		marker + "\n\nmore text after it",
		"no marker",
		"<!-- multica:v1 actor=system:" + util.UUIDToString(testUUID(1)) + " op=" + util.UUIDToString(testUUID(2)) + " -->",
		"<!-- multica:v1 actor=member:not-a-uuid op=" + util.UUIDToString(testUUID(2)) + " -->",
	} {
		if m, ok := parseMarker(body); ok {
			t.Fatalf("parseMarker(%q) = %+v, want none", body, m)
		}
		if got := stripMarker(body); got != body {
			t.Fatalf("stripMarker changed %q", body)
		}
	}
}

func TestIssueMatchesDigest(t *testing.T) {
	reason := "completed"
	sent := map[string]any{"title": "T", "state": "closed", "state_reason": "completed", "labels": []string{"a", "b"}}
	digest := payloadDigest(sent)
	gh := githubIssue{Title: "T", State: "closed", StateReason: &reason}
	gh.Labels = []githubLabel{{Name: "b"}, {Name: "a"}}
	if !issueMatchesDigest(gh, digest) {
		t.Fatalf("matching issue did not match %s", digest)
	}
	gh.Title = "other"
	if issueMatchesDigest(gh, digest) {
		t.Fatalf("changed issue matched")
	}
}
