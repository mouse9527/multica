// Added by the mouse9527/multica fork: Evolution P3, proposals (M-137); P4
// (M-138) keeps the patched field; P6 (M-140) adds team proposals and
// condense proposals.

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
	// TargetTeam is the workspace's team as a whole: creating or merging
	// agents and routing work between agents and squads. Its id is the
	// workspace's.
	TargetTeam = "team"

	EvidenceComment = "comment"
	EvidenceChat    = "chat"
	EvidenceIssue   = "issue"
)

// Risk tiers.
var proposalRisks = map[string]bool{"low": true, "medium": true, "high": true}

// ProposalFields is the field whitelist per target type: a proposal changes
// exactly one of these. Squad and team proposals are text only; nothing
// applies them.
var ProposalFields = map[string][]string{
	TargetAgent:    {"instructions", "description", "skills"},
	TargetWorkflow: {"steps", "name", "description"},
	TargetSquad:    {"instructions", "description", "members", "leader"},
	TargetTeam:     {"new_agent", "merge_agents", "routing"},
}

// CondenseTopic is the topic of every condense proposal, set by the server:
// one open condense proposal per agent, found again by its fingerprint.
const CondenseTopic = "condense instructions"

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
	RejectAmbiguous        = "change_is_ambiguous"
	RejectRunLimit         = "run_limit_reached"
	RejectRecentlyRejected = "recently_rejected"
	// RejectNotShorter: a condense proposal must leave the instructions
	// shorter than they are.
	RejectNotShorter = "change_not_shorter"
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

// Validate checks a proposal from an analysis run on its own: shapes,
// lengths, whitelists, quote rules, and that no text carries what redaction
// would remove. Checks that need the database (the target, the evidence)
// come after.
func (p *ProposalRequest) Validate() error {
	if err := p.validate(); err != nil {
		return err
	}
	if len(p.Evidence) == 0 {
		return reject(RejectEvidence, "evidence is required: link the comments, chats, or issues the proposal rests on")
	}
	return nil
}

// ValidateCondense checks a proposal from a condense review the way
// Validate does, with the review's own rules: it rewrites an agent's
// instructions, cites no evidence (the server attaches the measurement that
// put the agent on the review), and its topic is CondenseTopic.
func (p *ProposalRequest) ValidateCondense() error {
	if err := p.validate(); err != nil {
		return err
	}
	if p.Target.Type != TargetAgent || p.Field != "instructions" {
		return reject(RejectField, "a condense review proposes only shorter agent instructions: target.type agent, field instructions")
	}
	if p.Change.Before == "" {
		return reject(RejectInvalid, "change.before is required: copy the passage the proposal rewrites")
	}
	if len(p.Evidence) > 0 {
		return reject(RejectInvalid, "a condense proposal cites no evidence; the server attaches the measurement that put the agent on the review")
	}
	p.Topic = CondenseTopic
	return nil
}

func (p *ProposalRequest) validate() error {
	p.trim()
	if _, ok := ProposalFields[p.Target.Type]; !ok {
		return reject(RejectTarget, "target.type must be agent, workflow, squad, or team")
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
	if p.Target.Type == TargetTeam && p.Change.Before != "" {
		return reject(RejectInvalid, "a team proposal describes the change in change.after only; leave change.before empty")
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

// verbatimIndex finds stretches of source text (comments, chat messages)
// copied word for word into a proposal's fields. Case and spacing are
// ignored. Each field is matched on its own: a stretch split across two
// fields, or reworded, is not found; the prompt asks for summaries for that
// reason.
type verbatimIndex struct {
	names []string
	texts []string
	// hashes holds the rolling hash of every chatFragmentRunes-long window of
	// every field. Fields are joined with NUL, which no source contains, so no
	// window spans two fields.
	hashes map[uint64]struct{}
}

const verbatimHashBase = 1000003

func normalizeVerbatim(s string) []rune {
	return []rune(strings.ToLower(collapseSpace(s)))
}

func newVerbatimIndex(fields map[string]string) *verbatimIndex {
	x := &verbatimIndex{hashes: map[uint64]struct{}{}}
	for _, name := range sortedKeys(fields) {
		x.names = append(x.names, name)
		x.texts = append(x.texts, string(normalizeVerbatim(fields[name])))
	}
	forEachWindowHash([]rune(strings.Join(x.texts, "\x00")), func(_ int, h uint64) bool {
		x.hashes[h] = struct{}{}
		return true
	})
	return x
}

// forEachWindowHash calls fn with the start and rolling hash of every
// chatFragmentRunes-long window of r, until fn returns false.
func forEachWindowHash(r []rune, fn func(start int, h uint64) bool) {
	const w = chatFragmentRunes
	if len(r) < w {
		return
	}
	var h, pow uint64 = 0, 1
	for i := 0; i < w; i++ {
		h = h*verbatimHashBase + uint64(r[i])
		if i > 0 {
			pow *= verbatimHashBase
		}
	}
	for i := 0; ; i++ {
		if !fn(i, h) || i+w >= len(r) {
			return
		}
		h = (h-uint64(r[i])*pow)*verbatimHashBase + uint64(r[i+w])
	}
}

// field returns the first field that contains needle.
func (x *verbatimIndex) field(needle string) string {
	for i, text := range x.texts {
		if strings.Contains(text, needle) {
			return x.names[i]
		}
	}
	return ""
}

// find returns the field holding at least n runes of source in a row, or
// "" when none does.
func (x *verbatimIndex) find(source []rune, n int) string {
	if n <= 0 || len(source) < n {
		return ""
	}
	if n < chatFragmentRunes {
		return x.field(string(source))
	}
	need := n - chatFragmentRunes + 1 // consecutive window hits a match implies
	found, run := "", 0
	forEachWindowHash(source, func(i int, h uint64) bool {
		if _, ok := x.hashes[h]; !ok {
			run = 0
			return true
		}
		run++
		if run >= need {
			start := i + chatFragmentRunes - n
			found = x.field(string(source[start : i+chatFragmentRunes]))
		}
		return found == ""
	})
	return found
}

// chatFragmentFound reports whether text carries a stretch of a chat
// message: chatFragmentRunes characters in a row, or the whole message when
// it is shorter (but not trivially short).
func chatFragmentFound(text string, chat string) bool {
	return newVerbatimIndex(map[string]string{"text": text}).findChat(normalizeVerbatim(chat)) != ""
}

func (x *verbatimIndex) findChat(chat []rune) string {
	if len(chat) < minChatFragmentRunes {
		return ""
	}
	return x.find(chat, min(len(chat), chatFragmentRunes))
}

// VerbatimSource is text a proposal may not copy: a comment or a chat
// message, redacted the way the signal channel serves it.
type VerbatimSource struct {
	Type    string // EvidenceComment or EvidenceChat
	ID      string
	Content string
}

// CheckVerbatim rejects proposal text that copies a source word for word:
// any chatFragmentRunes characters of a chat message (or a whole short one),
// or more than MaxQuoteRunes characters of a comment. It covers every field
// the proposal publishes except change.before, which CheckTarget already
// ties to the target's current text.
func (p *ProposalRequest) CheckVerbatim(sources []VerbatimSource) error {
	x := newVerbatimIndex(p.texts(false))
	seen := map[string]bool{}
	for _, src := range sources {
		key := src.Type + ":" + src.ID
		if seen[key] {
			continue
		}
		seen[key] = true
		runes := normalizeVerbatim(src.Content)
		switch src.Type {
		case EvidenceChat:
			if name := x.findChat(runes); name != "" {
				return reject(RejectQuote, "%s repeats chat message %s word for word; summarize chats instead of pasting them", name, src.ID)
			}
		case EvidenceComment:
			if name := x.find(runes, MaxQuoteRunes+1); name != "" {
				return reject(RejectQuote, "%s copies more than %d characters of comment %s; summarize it, or quote at most %d characters in evidence[].quote", name, MaxQuoteRunes, src.ID, MaxQuoteRunes)
			}
		}
	}
	return nil
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
// independent items and quotes that are real excerpts. Copied text is
// CheckVerbatim's.
func (p *ProposalRequest) CheckEvidence(items []ResolvedEvidence, minIndependent int) error {
	contexts := map[string]bool{}
	for _, e := range items {
		contexts[e.Context] = true
	}
	if len(contexts) < minIndependent {
		return reject(RejectEvidence, "a proposal needs evidence from at least %d independent sources (different issues or chat sessions); it has %d", minIndependent, len(contexts))
	}
	for i, e := range items {
		if e.Quote != "" && !containsText(e.Content, e.Quote) {
			return reject(RejectQuote, "evidence[%d].quote is not an excerpt of that comment; copy it exactly, at most %d characters", i, MaxQuoteRunes)
		}
	}
	return nil
}

// VerbatimSources are the cited comments and chat messages.
func VerbatimSources(items []ResolvedEvidence) []VerbatimSource {
	out := []VerbatimSource{}
	for _, e := range items {
		if e.Type == EvidenceComment || e.Type == EvidenceChat {
			out = append(out, VerbatimSource{Type: e.Type, ID: e.ID, Content: e.Content})
		}
	}
	return out
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

// Patched is the target's field as the change leaves it, what apply (P4)
// writes. The change must pick out one place in the field, and a workflow's
// steps must stay JSON.
func (p *ProposalRequest) Patched(s TargetSnapshot) (string, error) {
	out, n := PatchText(s.Fields[p.Field], p.Change.Before, p.Change.After)
	switch {
	case n == 0:
		return "", reject(RejectStaleBefore, "change.before is not in the %s's current %s; copy the current text exactly", p.Target.Type, p.Field)
	case n > 1:
		return "", reject(RejectAmbiguous, "change.before occurs %d times in the %s's current %s; quote enough of it to pick out one place", n, p.Target.Type, p.Field)
	}
	if p.Target.Type == TargetWorkflow && p.Field == "steps" && !json.Valid([]byte(out)) {
		return "", reject(RejectInvalid, "the change leaves the workflow's steps as invalid JSON")
	}
	return out, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
