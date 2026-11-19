// Added by the mouse9527/multica fork: Evolution P3, proposals (M-137).

package evolution

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/multica-ai/multica/server/internal/util"
)

// Proposal target types and evidence types.
const (
	TargetAgent    = "agent"
	TargetWorkflow = "workflow"
	TargetSquad    = "squad"

	EvidenceComment = "comment"
	EvidenceChat    = "chat"
	EvidenceIssue   = "issue"
)

// Risk tiers.
var proposalRisks = map[string]bool{"low": true, "medium": true, "high": true}

// ProposalFields is the field whitelist per target type: a proposal changes
// exactly one of these. Squad proposals are text only; nothing applies them.
var ProposalFields = map[string][]string{
	TargetAgent:    {"instructions", "description", "skills"},
	TargetWorkflow: {"steps", "name", "description"},
	TargetSquad:    {"instructions", "description", "members", "leader"},
}

// forbiddenFields are refused with a reason of their own: permissions,
// secrets, environment, MCP configuration, runtimes, and visibility are never
// a proposal's business (M-132 §5).
var forbiddenFields = map[string]bool{
	"custom_env": true, "custom_args": true, "mcp_config": true, "env": true,
	"runtime": true, "runtime_id": true, "runtime_config": true, "runtime_mode": true,
	"permission_mode": true, "permissions": true, "visibility": true, "invocation_targets": true,
	"owner": true, "owner_id": true, "secrets": true, "credentials": true,
	"composio_toolkit_allowlist": true,
}

// Bounds of a proposal request. Text lengths are in characters (runes).
const (
	MaxQuoteRunes         = 120
	MaxEvidenceItems      = 20
	maxTitleRunes         = 120
	maxTopicRunes         = 80
	maxProblemRunes       = 4000
	maxChangeRunes        = 16000
	maxEffectRunes        = 2000
	maxRiskReasonRunes    = 1000
	maxRollbackRunes      = 1000
	maxEvidenceNoteRunes  = 300
	minTopicRunes         = 3
	minTitleRunes         = 3
	chatFragmentRunes     = 20
	minChatFragmentRunes  = 8
	minMemberNameRunes    = 2
	proposalFingerprintLn = 32
)

// ProposalRequest is the body of POST /api/evolution/proposals.
type ProposalRequest struct {
	Target ProposalTarget `json:"target"`
	// Field is the one whitelisted field the change touches.
	Field string `json:"field"`
	// Topic names the problem in a few words; with the target and field it
	// makes the fingerprint that finds the same proposal again.
	Topic          string             `json:"topic"`
	Title          string             `json:"title"`
	Problem        string             `json:"problem"`
	Change         ProposalChange     `json:"change"`
	Evidence       []ProposalEvidence `json:"evidence"`
	ExpectedEffect string             `json:"expected_effect"`
	Risk           string             `json:"risk"`
	RiskReason     string             `json:"risk_reason"`
	Rollback       string             `json:"rollback"`
}

type ProposalTarget struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// ProposalChange is the diff. Before is copied from the target as it is now
// (empty for a pure addition); the server checks it is there.
type ProposalChange struct {
	Before string `json:"before"`
	After  string `json:"after"`
}

// ProposalEvidence points at one comment, chat message, or issue. Quote is
// allowed on comments only.
type ProposalEvidence struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Note  string `json:"note"`
	Quote string `json:"quote"`
}

// Rejection codes.
const (
	RejectInvalid          = "invalid_proposal"
	RejectTarget           = "target_not_allowed"
	RejectField            = "field_not_allowed"
	RejectEvidence         = "insufficient_evidence"
	RejectQuote            = "quote_not_allowed"
	RejectSensitive        = "sensitive_content"
	RejectStaleBefore      = "change_does_not_match_target"
	RejectRunLimit         = "run_limit_reached"
	RejectRecentlyRejected = "recently_rejected"
)

// RejectedError is a proposal the server refuses; Message says why, for the
// agent to fix and resubmit.
type RejectedError struct {
	Code    string `json:"code"`
	Message string `json:"error"`
}

func (e *RejectedError) Error() string { return e.Code + ": " + e.Message }

func reject(code, format string, args ...any) error {
	return &RejectedError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// IsFieldAllowed reports whether field is on targetType's whitelist.
func IsFieldAllowed(targetType, field string) bool {
	for _, f := range ProposalFields[targetType] {
		if f == field {
			return true
		}
	}
	return false
}

// Validate checks the request on its own: shapes, lengths, whitelists, quote
// rules, and that no text carries what redaction would remove. Checks that
// need the database (the target, the evidence) come after.
func (p *ProposalRequest) Validate() error {
	p.trim()
	if _, ok := ProposalFields[p.Target.Type]; !ok {
		return reject(RejectTarget, "target.type must be agent, workflow, or squad")
	}
	if _, err := util.ParseUUID(p.Target.ID); err != nil {
		return reject(RejectInvalid, "target.id must be a uuid")
	}
	if forbiddenFields[strings.ToLower(p.Field)] {
		return reject(RejectField, "field %q is never proposed: permissions, secrets, environment, MCP configuration, runtimes, and visibility are out of bounds", p.Field)
	}
	if !IsFieldAllowed(p.Target.Type, p.Field) {
		return reject(RejectField, "field must be one of %s for a %s", strings.Join(ProposalFields[p.Target.Type], ", "), p.Target.Type)
	}
	for _, f := range []struct {
		name     string
		value    string
		min, max int
	}{
		{"topic", p.Topic, minTopicRunes, maxTopicRunes},
		{"title", p.Title, minTitleRunes, maxTitleRunes},
		{"problem", p.Problem, 1, maxProblemRunes},
		{"change.before", p.Change.Before, 0, maxChangeRunes},
		{"change.after", p.Change.After, 0, maxChangeRunes},
		{"expected_effect", p.ExpectedEffect, 1, maxEffectRunes},
		{"risk_reason", p.RiskReason, 1, maxRiskReasonRunes},
		{"rollback", p.Rollback, 1, maxRollbackRunes},
	} {
		n := utf8.RuneCountInString(f.value)
		if n < f.min {
			return reject(RejectInvalid, "%s is required", f.name)
		}
		if n > f.max {
			return reject(RejectInvalid, "%s is longer than %d characters", f.name, f.max)
		}
	}
	if strings.ContainsAny(p.Title, "\r\n") {
		return reject(RejectInvalid, "title must be one line")
	}
	if p.Change.Before == p.Change.After {
		return reject(RejectInvalid, "change.after must differ from change.before")
	}
	if !proposalRisks[p.Risk] {
		return reject(RejectInvalid, "risk must be low, medium, or high")
	}
	if len(p.Evidence) == 0 {
		return reject(RejectEvidence, "evidence is required: link the comments, chats, or issues the proposal rests on")
	}
	if len(p.Evidence) > MaxEvidenceItems {
		return reject(RejectInvalid, "evidence may list at most %d items", MaxEvidenceItems)
	}
	seen := map[string]bool{}
	for i, e := range p.Evidence {
		switch e.Type {
		case EvidenceComment, EvidenceChat, EvidenceIssue:
		default:
			return reject(RejectInvalid, "evidence[%d].type must be comment, chat, or issue", i)
		}
		if _, err := util.ParseUUID(e.ID); err != nil {
			return reject(RejectInvalid, "evidence[%d].id must be a uuid", i)
		}
		key := e.Type + ":" + e.ID
		if seen[key] {
			return reject(RejectInvalid, "evidence lists %s %s twice", e.Type, e.ID)
		}
		seen[key] = true
		if utf8.RuneCountInString(e.Note) > maxEvidenceNoteRunes {
			return reject(RejectInvalid, "evidence[%d].note is longer than %d characters", i, maxEvidenceNoteRunes)
		}
		if strings.ContainsAny(e.Note, "\r\n") {
			return reject(RejectInvalid, "evidence[%d].note must be one line", i)
		}
		if e.Quote == "" {
			continue
		}
		if e.Type != EvidenceComment {
			return reject(RejectQuote, "evidence[%d]: only comments may be quoted; summarize chats and issues in the note instead", i)
		}
		if utf8.RuneCountInString(e.Quote) > MaxQuoteRunes {
			return reject(RejectQuote, "evidence[%d].quote is longer than %d characters", i, MaxQuoteRunes)
		}
	}
	for name, text := range p.texts(true) {
		if _, n := Redact(text); n > 0 {
			return reject(RejectSensitive, "%s contains what looks like a secret, credential, contact detail, internal host, or URL query string; remove it", name)
		}
	}
	return nil
}

func (p *ProposalRequest) trim() {
	p.Target.Type = strings.TrimSpace(p.Target.Type)
	p.Target.ID = strings.ToLower(strings.TrimSpace(p.Target.ID))
	p.Field = strings.TrimSpace(p.Field)
	p.Topic = strings.TrimSpace(p.Topic)
	p.Title = strings.TrimSpace(p.Title)
	p.Problem = strings.TrimSpace(p.Problem)
	p.ExpectedEffect = strings.TrimSpace(p.ExpectedEffect)
	p.Risk = strings.ToLower(strings.TrimSpace(p.Risk))
	p.RiskReason = strings.TrimSpace(p.RiskReason)
	p.Rollback = strings.TrimSpace(p.Rollback)
	for i := range p.Evidence {
		e := &p.Evidence[i]
		e.Type = strings.TrimSpace(e.Type)
		e.ID = strings.ToLower(strings.TrimSpace(e.ID))
		e.Note = strings.TrimSpace(e.Note)
		e.Quote = strings.TrimSpace(e.Quote)
	}
}

// texts is every piece of text the proposal publishes, by field name.
// change.before is the target's current text, checked against the target
// rather than for chat fragments or names; withBefore includes it.
func (p *ProposalRequest) texts(withBefore bool) map[string]string {
	out := map[string]string{
		"topic": p.Topic, "title": p.Title, "problem": p.Problem, "change.after": p.Change.After,
		"expected_effect": p.ExpectedEffect, "risk_reason": p.RiskReason, "rollback": p.Rollback,
	}
	if withBefore {
		out["change.before"] = p.Change.Before
	}
	for i, e := range p.Evidence {
		out[fmt.Sprintf("evidence[%d].note", i)] = e.Note
		out[fmt.Sprintf("evidence[%d].quote", i)] = e.Quote
	}
	return out
}

var nonWord = regexp.MustCompile(`[^\p{L}\p{N}]+`)

// NormalizeTopic folds case, punctuation, and spacing so the same problem
// worded slightly differently keeps one fingerprint.
func NormalizeTopic(topic string) string {
	return strings.Trim(nonWord.ReplaceAllString(strings.ToLower(topic), "-"), "-")
}

// Fingerprint identifies a proposal: the same target, field, and topic is
// the same proposal.
func Fingerprint(targetType, targetID, field, topic string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{targetType, strings.ToLower(targetID), field, NormalizeTopic(topic)}, "\x1f")))
	return hex.EncodeToString(sum[:])[:proposalFingerprintLn]
}

// TargetSnapshot is a target's whitelisted fields as text, the way a
// proposal's change.before quotes them.
type TargetSnapshot struct {
	Type   string            `json:"type"`
	ID     string            `json:"id"`
	Name   string            `json:"-"`
	Fields map[string]string `json:"fields"`
}

// ContentHash is the hash of the snapshot's whitelisted fields. A proposal
// records it as its base; apply (P4) compares it with the target's hash at
// that time to see whether someone changed the target since.
func (s TargetSnapshot) ContentHash() string {
	raw, _ := json.Marshal(s) // map keys marshal sorted
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// collapseSpace folds every run of whitespace to one space.
func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// containsText reports whether needle occurs in haystack, ignoring how
// whitespace is laid out.
func containsText(haystack, needle string) bool {
	return strings.Contains(collapseSpace(haystack), collapseSpace(needle))
}

// chatFragmentFound reports whether text carries a stretch of a chat
// message: chatFragmentRunes characters in a row, or the whole message when
// it is shorter (but not trivially short). Case and spacing are ignored.
// Rewording defeats it; the prompt asks for summaries for that reason.
func chatFragmentFound(text string, chat string) bool {
	t := []rune(strings.ToLower(collapseSpace(text)))
	c := []rune(strings.ToLower(collapseSpace(chat)))
	if len(c) < minChatFragmentRunes {
		return false
	}
	if len(c) <= chatFragmentRunes {
		return strings.Contains(string(t), string(c))
	}
	windows := make(map[string]struct{}, len(t))
	for i := 0; i+chatFragmentRunes <= len(t); i++ {
		windows[string(t[i:i+chatFragmentRunes])] = struct{}{}
	}
	for i := 0; i+chatFragmentRunes <= len(c); i++ {
		if _, ok := windows[string(c[i:i+chatFragmentRunes])]; ok {
			return true
		}
	}
	return false
}

// nameFound reports whether text names a member. Names written in Latin
// letters match as whole words, case-insensitively; other names match
// anywhere.
func nameFound(text, name string) bool {
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) < minMemberNameRunes {
		return false
	}
	lowerText, lowerName := strings.ToLower(text), strings.ToLower(name)
	if !isLatin(name) {
		return strings.Contains(lowerText, lowerName)
	}
	for start := 0; ; {
		i := strings.Index(lowerText[start:], lowerName)
		if i < 0 {
			return false
		}
		i += start
		end := i + len(lowerName)
		before, _ := utf8.DecodeLastRuneInString(lowerText[:i])
		after, _ := utf8.DecodeRuneInString(lowerText[end:])
		if (i == 0 || !isWordRune(before)) && (end == len(lowerText) || !isWordRune(after)) {
			return true
		}
		start = i + 1
	}
}

func isLatin(s string) bool {
	for _, r := range s {
		if r > unicode.MaxLatin1 {
			return false
		}
	}
	return true
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

// ResolvedEvidence is one evidence item the server found and may cite.
type ResolvedEvidence struct {
	ProposalEvidence
	CreatedAt time.Time
	// Context is what makes two items independent: the issue a comment is
	// on, the issue itself, or the chat session.
	Context string
	// Content is the comment or chat text (redacted), for the quote and
	// fragment checks; empty for issues.
	Content         string
	IssueID         string
	IssueIdentifier string
	ChatSessionID   string
}

// CheckEvidence applies the rules that need the resolved evidence: enough
// independent items, quotes that are real excerpts, and no chat text pasted
// into the proposal.
func (p *ProposalRequest) CheckEvidence(items []ResolvedEvidence, minIndependent int) error {
	contexts := map[string]bool{}
	for _, e := range items {
		contexts[e.Context] = true
	}
	if len(contexts) < minIndependent {
		return reject(RejectEvidence, "a proposal needs evidence from at least %d independent sources (different issues or chat sessions); it has %d", minIndependent, len(contexts))
	}
	texts := p.texts(false)
	for i, e := range items {
		if e.Quote != "" && !containsText(e.Content, e.Quote) {
			return reject(RejectQuote, "evidence[%d].quote is not an excerpt of that comment; copy it exactly, at most %d characters", i, MaxQuoteRunes)
		}
		if e.Type != EvidenceChat {
			continue
		}
		for _, name := range sortedKeys(texts) {
			if chatFragmentFound(texts[name], e.Content) {
				return reject(RejectQuote, "%s repeats chat message %s word for word; summarize chats instead of pasting them", name, e.ID)
			}
		}
	}
	return nil
}

// CheckNames rejects text that names a member.
func (p *ProposalRequest) CheckNames(memberNames []string) error {
	texts := p.texts(false)
	for _, name := range sortedKeys(texts) {
		for _, member := range memberNames {
			if nameFound(texts[name], member) {
				return reject(RejectSensitive, "%s names a workspace member; say \"a member\" or describe their role", name)
			}
		}
	}
	return nil
}

// CheckTarget applies the rules that need the target as it is now: the
// change quotes it faithfully.
func (p *ProposalRequest) CheckTarget(s TargetSnapshot) error {
	current := s.Fields[p.Field]
	if p.Change.Before != "" && !containsText(current, p.Change.Before) {
		return reject(RejectStaleBefore, "change.before is not in the %s's current %s; copy the current text exactly", p.Target.Type, p.Field)
	}
	if p.Change.Before == "" && containsText(current, p.Change.After) {
		return reject(RejectStaleBefore, "change.after is already in the %s's current %s", p.Target.Type, p.Field)
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
