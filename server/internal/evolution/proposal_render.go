// Added by the mouse9527/multica fork: Evolution P3, the proposal issue body (M-137).

package evolution

import (
	"fmt"
	"strings"
	"time"
)

// Proposal issues are written in the language the agent was enabled in
// (its built-in name); Chinese and English have their own headings, the
// rest use English.
func proposalLanguage(agentName string) string {
	if agentName == "进化" {
		return "zh"
	}
	return "en"
}

var proposalTitlePrefix = map[string]string{
	"en": "Evolution proposal",
	"zh": "进化提案",
}

type proposalCopy struct {
	target, field, baseHash, problem, change, evidence, effect, risk, rollback string
	comment, chat, issue, message, session                                     string
	risks                                                                      map[string]string
	footer                                                                     string
	moreEvidence, moreEvidenceIntro                                            string
}

var proposalCopies = map[string]proposalCopy{
	"en": {
		target: "Target", field: "Field", baseHash: "base hash",
		problem: "Problem and cause", change: "Change", evidence: "Evidence",
		effect: "Expected effect", risk: "Risk", rollback: "Undo",
		comment: "Comment", chat: "Chat", issue: "Issue", message: "message", session: "session",
		risks:             map[string]string{"low": "Low", "medium": "Medium", "high": "High"},
		footer:            "Proposed by Evolution in analysis run %s. This is a proposal only: nothing changes until an owner or admin approves it.",
		moreEvidence:      "More evidence",
		moreEvidenceIntro: "Analysis run %s found the same problem again.",
	},
	"zh": {
		target: "对象", field: "字段", baseHash: "基线哈希",
		problem: "问题与归因", change: "改动", evidence: "证据",
		effect: "预期效果与度量", risk: "风险档位", rollback: "回滚",
		comment: "评论", chat: "对话", issue: "Issue", message: "消息", session: "会话",
		risks:             map[string]string{"low": "低", "medium": "中", "high": "高"},
		footer:            "由进化在分析运行 %s 中提出。这只是提案：所有者或管理员批准前，不会改动任何配置。",
		moreEvidence:      "新证据",
		moreEvidenceIntro: "分析运行 %s 再次发现了同一问题。",
	},
}

func issueLink(identifier, issueID string) string {
	return fmt.Sprintf("[%s](mention://issue/%s)", identifier, issueID)
}

// fence returns a code fence longer than any backtick run in s.
func fence(s string) string {
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return strings.Repeat("`", max(3, longest+1))
}

// renderDiff writes before as removed lines and after as added lines.
func renderDiff(before, after string) string {
	var b strings.Builder
	f := fence(before + after)
	b.WriteString(f + "diff\n")
	for _, side := range []struct{ mark, text string }{{"-", before}, {"+", after}} {
		if side.text == "" {
			continue
		}
		for _, line := range strings.Split(side.text, "\n") {
			if line == "" {
				b.WriteString(side.mark + "\n")
				continue
			}
			b.WriteString(side.mark + " " + line + "\n")
		}
	}
	b.WriteString(f + "\n")
	return b.String()
}

// renderEvidence writes one line per item. Items carry ids, never authors.
func renderEvidence(c proposalCopy, items []ResolvedEvidence, loc *time.Location) string {
	var b strings.Builder
	for _, e := range items {
		day := e.CreatedAt.In(loc).Format(time.DateOnly)
		parts := []string{}
		switch e.Type {
		case EvidenceComment:
			parts = append(parts, c.comment, issueLink(e.IssueIdentifier, e.IssueID), day, fmt.Sprintf("%s `%s`", strings.ToLower(c.comment), e.ID))
			if e.Quote != "" {
				parts = append(parts, "“"+collapseSpace(e.Quote)+"”")
			}
		case EvidenceChat:
			parts = append(parts, c.chat, fmt.Sprintf("%s `%s`", c.session, e.ChatSessionID), fmt.Sprintf("%s `%s`", c.message, e.ID), day)
		case EvidenceIssue:
			parts = append(parts, c.issue, issueLink(e.IssueIdentifier, e.IssueID), day)
		}
		if e.Note != "" {
			parts = append(parts, e.Note)
		}
		b.WriteString("- " + strings.Join(parts, " · ") + "\n")
	}
	return b.String()
}

// renderProposal is the proposal issue's body, in the section order of the
// multica-evolution skill.
func renderProposal(lang string, p *ProposalRequest, snap TargetSnapshot, evidence []ResolvedEvidence, runIssueID, runIdentifier string, loc *time.Location) string {
	c := proposalCopies[lang]
	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n\n%s · **%s** · `%s`\n%s: `%s` · %s `%s`\n\n", c.target,
		p.Target.Type, snap.Name, p.Target.ID, c.field, p.Field, c.baseHash, snap.ContentHash()[:12])
	fmt.Fprintf(&b, "## %s\n\n%s\n\n", c.problem, p.Problem)
	fmt.Fprintf(&b, "## %s\n\n%s\n", c.change, renderDiff(p.Change.Before, p.Change.After))
	fmt.Fprintf(&b, "## %s\n\n%s\n", c.evidence, renderEvidence(c, evidence, loc))
	fmt.Fprintf(&b, "## %s\n\n%s\n\n", c.effect, p.ExpectedEffect)
	fmt.Fprintf(&b, "## %s\n\n%s · %s\n\n%s: %s\n\n", c.risk, c.risks[p.Risk], p.RiskReason, c.rollback, p.Rollback)
	b.WriteString("---\n\n")
	fmt.Fprintf(&b, c.footer+"\n", issueLink(runIdentifier, runIssueID))
	return b.String()
}

// renderMoreEvidence is the comment that adds evidence to an open proposal.
func renderMoreEvidence(lang string, evidence []ResolvedEvidence, runIssueID, runIdentifier string, loc *time.Location) string {
	c := proposalCopies[lang]
	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n\n", c.moreEvidence)
	fmt.Fprintf(&b, c.moreEvidenceIntro+"\n\n", issueLink(runIdentifier, runIssueID))
	b.WriteString(renderEvidence(c, evidence, loc))
	return b.String()
}
