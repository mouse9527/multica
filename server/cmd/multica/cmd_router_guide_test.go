// Added by the mouse9527/multica fork: the Super Agent guide in the CLI (M-150).

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
