package githubissues

import (
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
)

// Every body Multica writes to GitHub ends in a hidden marker naming who asked
// for the write and the write intent behind it:
//
//	<!-- multica:v1 actor=agent:<agent-uuid> op=<intent-uuid> -->
//
// op is the intent id, the idempotency key GitHub's create endpoints lack: a
// write whose result is unknown is found again by looking for its op. An
// agent's write also carries a visible signature line above the marker,
// because every write reaches GitHub as the token owner. A member's write
// carries the marker only.
//
// The marker is never trusted on its own: anyone can paste one into a public
// repository. adopt.go accepts it only when the workspace holds an intent
// with that op whose target and GitHub id agree.

const (
	markerOpen  = "<!-- multica:v1 "
	markerClose = " -->"
	// signatureFormat is the visible line above an agent's marker.
	signatureFormat = "— %s（智能体），经 Multica"
)

var (
	uuidPattern = `[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`
	// trailingMarker matches the marker block at the very end of a body: an
	// optional blank line, an optional signature line, the marker and
	// trailing whitespace GitHub may add.
	// GitHub may hand an edited body back with CRLF line breaks.
	trailingMarker = regexp.MustCompile(`(?:\r?\n\r?\n)?(?:— [^\r\n]*（智能体），经 Multica\r?\n)?` +
		regexp.QuoteMeta(markerOpen) + `actor=(member|agent):(` + uuidPattern + `) op=(` + uuidPattern + `)` +
		regexp.QuoteMeta(markerClose) + `\s*$`)
	signatureNameCleaner = strings.NewReplacer("\r", " ", "\n", " ", "<", "‹", ">", "›")
)

// Actor is who asked Multica for a write: a member or an agent of the
// workspace.
type Actor struct {
	// Type is "member" or "agent", as github_write_intent.actor_type.
	Type string
	ID   pgtype.UUID
	// Name is the agent's display name for the signature line; unused for a
	// member.
	Name string
}

func (a Actor) valid() bool {
	return (a.Type == "member" || a.Type == "agent") && a.ID.Valid
}

// marker is a parsed trailing marker.
type marker struct {
	actorType string
	actorID   string
	op        string
}

// signedBody is text as written to GitHub for actor and intent op.
// stripMarker(signedBody(text, ...)) returns text unchanged.
func signedBody(text string, actor Actor, op pgtype.UUID) string {
	block := markerOpen + "actor=" + actor.Type + ":" + util.UUIDToString(actor.ID) +
		" op=" + util.UUIDToString(op) + markerClose
	if actor.Type == "agent" {
		name := strings.TrimSpace(signatureNameCleaner.Replace(actor.Name))
		if name == "" {
			name = "agent"
		}
		block = strings.Replace(signatureFormat, "%s", name, 1) + "\n" + block
	}
	if text == "" {
		return block
	}
	return text + "\n\n" + block
}

// parseMarker reads the marker at the end of body.
func parseMarker(body string) (marker, bool) {
	m := trailingMarker.FindStringSubmatch(body)
	if m == nil {
		return marker{}, false
	}
	return marker{actorType: m[1], actorID: m[2], op: m[3]}, true
}

// stripMarker removes the trailing marker block (and an agent's signature
// line) from a body Multica wrote. Callers strip only bodies whose marker
// they trust; other bodies are stored as GitHub returned them.
func stripMarker(body string) string {
	loc := trailingMarker.FindStringIndex(body)
	if loc == nil {
		return body
	}
	return body[:loc[0]]
}
