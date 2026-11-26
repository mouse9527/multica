// Added by the mouse9527/multica fork: Evolution P3, the proposal issue body
// (M-137); P6 condense and team proposals (M-140); metric evidence (M-296).

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
	// P6 (M-140).
	condenseFooter, byHand, measured string
	// M-296: a metric evidence line, with its sample and window.
	metric, metricLine string
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
		condenseFooter:    "Proposed by Evolution in condense review %s. This is a proposal only: nothing changes until an owner or admin approves it.",
		byHand:            "Carried out by hand: nothing applies this proposal. If approved, a person makes the change described above.",
		measured:          "Measured when the review opened: %d characters; of %d lines, %d (%d%%) repeat an earlier line. Listed at %d characters or %d%% repeated lines. After this change: %d characters.",
		metric:            "Metric",
		metricLine:        "%s (sample %d, %s to %s, recomputed by the server)",
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
		condenseFooter:    "由进化在精简检查 %s 中提出。这只是提案：所有者或管理员批准前，不会改动任何配置。",
		byHand:            "需手动执行：没有任何功能会应用这条提案。批准后由人按上面的描述去改。",
		measured:          "检查开始时测得：%d 字；%d 行中有 %d 行（%d%%）与前面的行重复。达到 %d 字或 %d%% 重复行即列入检查。改动后：%d 字。",
		metric:            "指标",
		metricLine:        "%s（样本 %d，%s 至 %s，服务端重算）",
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
		case EvidenceMetric:
			parts = append(parts, c.metric, fmt.Sprintf("`%s`", e.ID))
			if m := e.Metric; m != nil {
				parts = append(parts, fmt.Sprintf(c.metricLine, formatMetricValue(m.Value), m.Sample,
					m.WindowFrom.In(loc).Format(time.DateOnly), m.WindowUntil.In(loc).Format(time.DateOnly)))
			}
		}
		if e.Note != "" {
			parts = append(parts, e.Note)
		}
		b.WriteString("- " + strings.Join(parts, " · ") + "\n")
	}
	return b.String()
}

// proposalView is everything a proposal issue's body shows.
type proposalView struct {
	lang                      string
	p                         *ProposalRequest
	snap                      TargetSnapshot
	evidence                  []ResolvedEvidence
	runIssueID, runIdentifier string
	loc                       *time.Location
	// measure is set for a condense proposal: the review's measurement of
	// the agent, shown in place of evidence, with the length the change
	// leaves (proposedRunes) and the thresholds of cfg.
	measure       *CondenseCandidate
	proposedRunes int
	cfg           Config
}

// renderProposal is the body of a proposal from an analysis run.
func renderProposal(lang string, p *ProposalRequest, snap TargetSnapshot, evidence []ResolvedEvidence, runIssueID, runIdentifier string, loc *time.Location) string {
	return proposalView{lang: lang, p: p, snap: snap, evidence: evidence, runIssueID: runIssueID, runIdentifier: runIdentifier, loc: loc}.render()
}

// render is the proposal issue's body, in the section order of the
// multica-evolution skill.
func (v proposalView) render() string {
	c, p, snap := proposalCopies[v.lang], v.p, v.snap
	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n\n%s · **%s** · `%s`\n%s: `%s` · %s `%s`\n\n", c.target,
		p.Target.Type, snap.Name, p.Target.ID, c.field, p.Field, c.baseHash, snap.ContentHash()[:12])
	fmt.Fprintf(&b, "## %s\n\n%s\n\n", c.problem, p.Problem)
	fmt.Fprintf(&b, "## %s\n\n%s\n", c.change, renderDiff(p.Change.Before, p.Change.After))
	if p.Target.Type == TargetSquad || p.Target.Type == TargetTeam {
		fmt.Fprintf(&b, "%s\n\n", c.byHand)
	}
	if m := v.measure; m != nil {
		fmt.Fprintf(&b, "## %s\n\n"+c.measured+"\n\n", c.evidence, m.Runes, m.Lines, m.RepeatedLines, m.RepeatedPercent,
			v.cfg.CondenseMinRunes, v.cfg.CondenseRepeatPercent, v.proposedRunes)
	} else {
		fmt.Fprintf(&b, "## %s\n\n%s\n", c.evidence, renderEvidence(c, v.evidence, v.loc))
	}
	fmt.Fprintf(&b, "## %s\n\n%s\n\n", c.effect, p.ExpectedEffect)
	fmt.Fprintf(&b, "## %s\n\n%s · %s\n\n%s: %s\n\n", c.risk, c.risks[p.Risk], p.RiskReason, c.rollback, p.Rollback)
	b.WriteString("---\n\n")
	footer := c.footer
	if v.measure != nil {
		footer = c.condenseFooter
	}
	fmt.Fprintf(&b, footer+"\n", issueLink(v.runIdentifier, v.runIssueID))
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
