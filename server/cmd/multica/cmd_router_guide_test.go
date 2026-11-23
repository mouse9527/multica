// Added by the mouse9527/multica fork: the Super Agent guide in the CLI (M-150), issues a Super Agent creates (M-175), quiet batch replies (M-210), the reply format (M-206), and wakeups on issues in scope (M-208).

package main

import (
	"bytes"
	"strings"
	"testing"
)

// M-148 replayed without any skill: an agent asked to "make this chat a Super
// Agent and subscribe X" must get from `multica --help` to the right command
// through the CLI alone — root help names router as the Super Agent command,
// `multica router --help` sends it to the guide, and the guide maps the
// request to `router subscribe ... --anchor-chat current` with a confirmation
// step first.
func TestHelpLeadsToTheSuperAgentGuide(t *testing.T) {
	if root := renderHelp(t); !strings.Contains(root, "Super Agent") {
		t.Fatalf("`multica --help` does not mention Super Agent:\n%s", root)
	}
	if got := renderHelp(t, "router"); !strings.Contains(got, "multica router guide") || !strings.Contains(got, "guide:") {
		t.Errorf("`multica router --help` must point at and list `multica router guide`:\n%s", got)
	}

	guide := runRouterGuide(t)
	for _, want := range []string{
		"把这个 chat 设为 Super Agent",
		"multica router subscribe --project <项目ID> --anchor-chat current",
		"multica router subscribe --issue M-12 --anchor-chat current",
		"multica router get --chat current",
		"复述并等确认",
		"Super Agent 的边界",
		"不是 issue 的通知订阅",
	} {
		if !strings.Contains(guide, want) {
			t.Errorf("`multica router guide` lacks %q", want)
		}
	}
}

// The owner often reads only the "需要你处理" list at the end of a reply, so
// the guide makes every decision item stand on its own and shows a bare
// "A, B or C?" list as the wrong way (M-162).
func TestRouterGuideMakesOwnerDecisionsSelfContained(t *testing.T) {
	guide := runRouterGuide(t)
	for _, want := range []string{
		"## 请 Owner 决定的事：每一条单独看也能拍板",
		"为什么要 Owner 定",
		"每个选项的内容和后果",
		"推荐哪个",
		"批准范围不含什么",
		"**全部未结**的事项，不多也不少",
		"反例",
		"正例",
	} {
		if !strings.Contains(guide, want) {
			t.Errorf("`multica router guide` lacks %q", want)
		}
	}
}

// A Super Agent chat gets the upstream chat prompt too, which asks for a chat
// wakeup whenever work is handed off; the guide keeps issues in scope to the
// batch so one change does not wake the Super Agent twice (M-164).
func TestRouterGuideSplitsChatWakeupsFromBatches(t *testing.T) {
	guide := runRouterGuide(t)
	start := strings.Index(guide, "## chat wakeup 和事件批次的分工")
	if start < 0 {
		t.Fatal("`multica router guide` lacks the chat wakeup and batch section")
	}
	section := guide[start:]
	if end := strings.Index(section[1:], "\n## "); end >= 0 {
		section = section[:end+1]
	}
	for _, want := range []string{
		"订阅范围内的 issue：不建 wakeup，只靠批次",
		"不用 `--every` 轮询范围内的东西",
		"这些仍然可以建 wakeup",
		"`--at` / `--after`",
		"`--until-pr",
		// M-208: a timed wakeup on an issue in scope looked like a duplicate.
		"在回复里说清是哪一类提醒",
		"不是 M-206 的事件重复",
		"wakeup 触发的那一轮，先读 issue 当前状态再汇报",
		"反例",
		"正例",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("the chat wakeup and batch section lacks %q", want)
		}
	}
}

// A Super Agent that created an issue told the owner "I subscribed" while the
// issue was in no scope: it was only the issue's `creator` subscriber (M-175).
// The batch rule and the per-turn note send the agent to this section.
func TestRouterGuideSaysWhetherACreatedIssueIsInScope(t *testing.T) {
	guide := runRouterGuide(t)
	start := strings.Index(guide, "## 自己新建的 issue 在不在范围里")
	if start < 0 {
		t.Fatal("`multica router guide` lacks the section on issues a Super Agent creates")
	}
	section := guide[start:]
	if end := strings.Index(section[1:], "\n## "); end >= 0 {
		section = section[:end+1]
	}
	for _, want := range []string{
		"**「订阅」只指这个 Super Agent 的范围**",
		"原因 `creator`",
		"chat wakeup",
		"`--parent`",
		"`multica router scope <订阅ID>`",
		"照实说",
		"Owner 同意后再加",
		"multica router subscribe --issue M-30 --anchor-chat current",
		"反例",
		"正例",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("the created-issue section lacks %q", want)
		}
	}
}

// The reporting format came from one workspace's notes and the agent's memory,
// so it was lost in another workspace; the guide now carries it for all
// (M-206), and the decision section no longer drops the list when it is empty.
func TestRouterGuideStatesTheReplyFormat(t *testing.T) {
	guide := runRouterGuide(t)
	start := strings.Index(guide, "## 每次回复的格式")
	if start < 0 {
		t.Fatal("`multica router guide` lacks the reply format section")
	}
	section := guide[start:]
	if end := strings.Index(section[1:], "\n## "); end >= 0 {
		section = section[:end+1]
	}
	for _, want := range []string{
		"末尾单独一节「需要你处理」",
		"目前没有",
		"事件批次里没有新东西要告诉 Owner",
		"不写回复（这一轮不输出文字）",
		"只为了把旧的未结事项再列一遍也不行",
		"包括 `[无需你处理]` 开头的静默回复",
		"跟 Owner 用的语言走",
		"Owner 所在时区",
		"`…Z` 时间是 UTC",
		"写明是哪个时区",
		"合并了不等于修好了",
		"已合并，未上线，线上仍会出现",
		"把「是否部署」列进「需要你处理」",
		"已合并，未确认是否已上线",
		"列出**全部**未结事项",
		"`multica notify owner`",
		"转述前先核对",
		"反例",
		"正例",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("the reply format section lacks %q", want)
		}
	}
	if strings.Contains(guide, "没有未结事项时，不写这一段") {
		t.Error("the decision section still drops the list when nothing is open; it should say 目前没有")
	}
	// A quiet batch must not become a reason to leave open items off a reply
	// that is sent: the owner reads only the list at the end (M-162).
	for _, banned := range []string{"再发一遍", "等下次有话要说时一起列", "按未上线说", "默认不回复"} {
		if strings.Contains(section, banned) {
			t.Errorf("the reply format section still says %q", banned)
		}
	}
}

// The guide prints the embedded file verbatim: editing it and releasing the
// CLI is the whole update, and there is no second copy to keep in step.
func TestRouterGuidePrintsTheEmbeddedFile(t *testing.T) {
	if strings.TrimSpace(routerGuide) == "" {
		t.Fatal("router_guide.md is not embedded")
	}
	if got := runRouterGuide(t); got != routerGuide {
		t.Error("`multica router guide` output differs from router_guide.md")
	}
	if strings.HasPrefix(routerGuide, "---") || strings.Contains(routerGuide, "<!--") {
		t.Error("router_guide.md is printed to agents as is; keep skill frontmatter and HTML comments out of it")
	}
}

func runRouterGuide(t *testing.T) string {
	t.Helper()
	var out bytes.Buffer
	routerGuideCmd.SetOut(&out)
	t.Cleanup(func() { routerGuideCmd.SetOut(nil) })
	if err := routerGuideCmd.RunE(routerGuideCmd, nil); err != nil {
		t.Fatalf("multica router guide: %v", err)
	}
	return out.String()
}

// Every text reply in a chat alerts the owner, and the runtime often makes a
// batch turn end with text, so the guide names the line that keeps a batch reply quiet,
// says when not to use it, and shows using it on a decision as the wrong way
// (M-210). The marker must match agentrouter.QuietReplyMarker.
func TestRouterGuideExplainsQuietBatchReplies(t *testing.T) {
	guide := runRouterGuide(t)
	start := strings.Index(guide, "## 没事可报的批次")
	if start < 0 {
		t.Fatal("`multica router guide` lacks the quiet batch section")
	}
	section := guide[start:]
	if end := strings.Index(section[1:], "\n## "); end >= 0 {
		section = section[:end+1]
	}
	for _, want := range []string{
		"`[无需你处理]`",
		"router_chat_quiet",
		"不要写这一行",
		"Owner 自己发起的轮次",
		"API Error:",
		"反例",
		"正例",
		// M-206: quiet skips the alert, never the open items.
		"末尾照样按「每次回复的格式」列出全部未结事项",
		"静默只表示不弹通知",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("the quiet batch section lacks %q:\n%s", want, section)
		}
	}
	if strings.Contains(section, "也不用把未结的决定事项再列一遍") {
		t.Error("the quiet batch section still lets a quiet reply drop the open items")
	}
	// M-215: with the flag off the guide still says to write no reply, so the
	// section's premise must not say a batch turn can never end without text.
	if !strings.Contains(section, "没事可报就不写回复（这一轮不输出文字）") {
		t.Error("the quiet batch section lacks what to do with router_chat_quiet off")
	}
	if strings.Contains(section, "不能不写字就结束") {
		t.Error("the quiet batch section still says a batch turn cannot end without text")
	}
}
