// Added by the mouse9527/multica fork: quiet Super Agent batch replies (M-210).

package agentrouter

import (
	"strings"
	"testing"
	"time"
)

func TestQuietReply(t *testing.T) {
	for _, c := range []struct {
		content string
		want    bool
	}{
		{"[无需你处理]\nM-20 开发在推进，PR 还没开。", true},
		{"[无需你处理]", true},
		{"  **[无需你处理]**  \r\nM-20 在推进。", true},
		{"\n\n[无需你处理]\n", true},
		{"API Error: 504 Gateway Timeout", true},
		{"  API Error: 529 {\"type\":\"error\"}", true},
		{"需要你处理：目前没有。", false},
		{"M-20 验收通过，请批准合并。\n[无需你处理]", false},
		{"[无需你处理] 但请批准 M-20 合并", false},
		{"M-20 失败了：API Error: 504", false},
		{"", false},
	} {
		if got := quietReply(c.content); got != c.want {
			t.Errorf("quietReply(%q) = %v, want %v", c.content, got, c.want)
		}
	}
}

// With quiet replies off the batch keeps the wording it had before, so the
// agent is never told about a marker the server ignores; on, it names the
// marker the server looks for.
func TestChatBatchNoteNamesTheQuietMarkerOnlyWhenOn(t *testing.T) {
	in := noteInput{sub: dispatchSub{ID: "sub-1"}, anchorKey: "RT-1", scopeLabel: "issue tree RT-1", now: time.Now(), chat: true}
	off := buildNote(in)
	if !strings.Contains(off, "Any text you reply here notifies the owner") ||
		!strings.Contains(off, "finish without any text reply") || strings.Contains(off, QuietReplyMarker) {
		t.Fatalf("batch with quiet replies off:\n%s", off)
	}
	in.quiet = true
	on := buildNote(in)
	if !strings.Contains(on, "first line is exactly `"+QuietReplyMarker+"`") ||
		!strings.Contains(on, "never use that line") || strings.Contains(on, "finish without any text reply") {
		t.Fatalf("batch with quiet replies on:\n%s", on)
	}
	in.chat = false
	if anchor := buildNote(in); strings.Contains(anchor, QuietReplyMarker) {
		t.Fatalf("anchor run note names the chat marker:\n%s", anchor)
	}
}
