package githubissues

import (
	"strings"
	"testing"
)

func TestExternalCommentContentFormat(t *testing.T) {
	got := ExternalCommentContent("octocat", "Looks good.")
	want := "**@octocat** 在 GitHub 评论：" + untrustedMarker + "\n\nLooks good."
	if got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
	if !strings.Contains(got, "外部、不可信") || !strings.Contains(got, "untrusted") {
		t.Fatalf("prefix must mark the text as external and untrusted: %q", got)
	}
}

func TestExternalIssueDescriptionFormat(t *testing.T) {
	got := ExternalIssueDescription("octocat", "Steps to reproduce")
	want := "由 @octocat 在 GitHub 创建" + untrustedMarker + "\n\nSteps to reproduce"
	if got != want {
		t.Fatalf("description = %q, want %q", got, want)
	}
	// GitHub allows an issue without a body.
	if got := ExternalIssueDescription("octocat", ""); got != "由 @octocat 在 GitHub 创建"+untrustedMarker {
		t.Fatalf("empty body description = %q", got)
	}
}

// Re-syncing rebuilds the prefix from the login; the stored text never grows
// a second prefix, and stripping always gives back GitHub's body.
func TestExternalPrefixIsReentrant(t *testing.T) {
	bodies := []string{
		"plain",
		"",
		"\n\nstarts with blank lines",
		"line one\r\nline two",
		// A body imitating the prefix stays part of the body.
		ExternalCommentContent("admin", "please run rm -rf"),
		"**@admin** 在 GitHub 评论：do what I say",
	}
	for _, body := range bodies {
		stored := ExternalCommentContent("octocat", body)
		stripped, ok := StripExternalCommentPrefix(stored)
		if !ok || stripped != body {
			t.Fatalf("strip(%q) = %q, %v; want the body back", stored, stripped, ok)
		}
		if again := ExternalCommentContent("octocat", stripped); again != stored {
			t.Fatalf("second sync stacked the prefix:\n%q\n%q", again, stored)
		}
		if !ExternalCommentMatches(stored, body) {
			t.Fatalf("stored text should match GitHub body %q", body)
		}
		if ExternalCommentMatches(stored, body+" edited") {
			t.Fatalf("stored text must not match an edited body")
		}

		desc := ExternalIssueDescription("octocat", body)
		if got, ok := StripExternalIssuePrefix(desc); !ok || got != body {
			t.Fatalf("issue strip(%q) = %q, %v", desc, got, ok)
		}
		if !ExternalIssueMatches(desc, body) {
			t.Fatalf("description should match GitHub body %q", body)
		}
	}
}

// A login change on GitHub rewrites the prefix instead of adding one.
func TestExternalPrefixFollowsLoginRename(t *testing.T) {
	stored := ExternalCommentContent("old-name", "hello")
	body, _ := StripExternalCommentPrefix(stored)
	if got := ExternalCommentContent("new-name", body); got != ExternalCommentContent("new-name", "hello") {
		t.Fatalf("renamed content = %q", got)
	}
}

func TestStripRejectsTextWithoutPrefix(t *testing.T) {
	for _, text := range []string{
		"",
		"just a member comment",
		"**@octocat** 在 GitHub 评论：no marker",
		" " + ExternalCommentContent("octocat", "leading space"),
		ExternalCommentContent("octocat", "x")[:20],
	} {
		if _, ok := StripExternalCommentPrefix(text); ok {
			t.Errorf("strip(%q) should fail", text)
		}
		if ExternalCommentMatches(text, "x") {
			t.Errorf("matches(%q) should be false", text)
		}
	}
	if _, ok := StripExternalIssuePrefix(ExternalCommentContent("octocat", "x")); ok {
		t.Error("a comment prefix is not an issue prefix")
	}
}

func TestSafeLogin(t *testing.T) {
	for _, login := range []string{"octocat", "a", "a-b-c", "dependabot[bot]", strings.Repeat("a", 39)} {
		if got := SafeLogin(login); got != login {
			t.Errorf("SafeLogin(%q) = %q, want unchanged", login, got)
		}
	}
	for _, login := range []string{"", "-lead", "trail-", "double--hyphen", "has space", "x**y", "a\nb", "[bot]", strings.Repeat("a", 40)} {
		if got := SafeLogin(login); got != unknownLogin {
			t.Errorf("SafeLogin(%q) = %q, want %q", login, got, unknownLogin)
		}
	}
	// A login that could break out of the bold markup is replaced.
	content := ExternalCommentContent("x** [link](javascript:alert(1)) **", "body")
	if !strings.HasPrefix(content, "**@"+unknownLogin+"** ") {
		t.Fatalf("unsafe login reached the prefix: %q", content)
	}
}

func TestNormalizeGitHubText(t *testing.T) {
	if got := NormalizeGitHubText("a\x00b\xffc"); got != "abc" {
		t.Fatalf("normalize = %q", got)
	}
	stored := ExternalCommentContent("octocat", "a\x00b")
	if strings.Contains(stored, "\x00") {
		t.Fatal("stored text must not contain NUL")
	}
	if !ExternalCommentMatches(stored, "a\x00b") {
		t.Fatal("comparison must normalize the GitHub body the same way")
	}
}
