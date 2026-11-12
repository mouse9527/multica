// Added by the mouse9527/multica fork: GitHub-native issue keys
// (`owner/repo#N`) as chips in the editable editor. Upstream has no such file.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { Editor } from "@tiptap/core";
import StarterKit from "@tiptap/starter-kit";
import Link from "@tiptap/extension-link";
import { Markdown } from "@tiptap/markdown";
import { EditorView } from "@tiptap/pm/view";
import type { RefObject } from "react";
import { BaseMentionExtension } from "./mention-extension";
import { createMarkdownPasteExtension } from "./markdown-paste";
import {
  createIssueIdentifierAutolinkExtension,
  type IssueIdentifierResolver,
} from "./issue-identifier-autolink";
import { createGitHubIssueKeyChipsExtension } from "./github-issue-key-chips";

// The real NodeView needs workspace providers; serialisation is what matters.
const TestMention = BaseMentionExtension.extend({
  addNodeView() {
    return () => ({ dom: document.createElement("span") });
  },
});

const TRACKED: Record<string, string> = {
  "mouse9527/multica#21": "uuid-21",
  "BIPO-46": "uuid-46",
};

const resolveMock = vi.fn<IssueIdentifierResolver>();
const resolveRef: RefObject<IssueIdentifierResolver | undefined> = {
  current: (identifier) => resolveMock(identifier),
};

let editor: Editor | null = null;
let updates = 0;

function makeEditor(content = ""): Editor {
  const element = document.createElement("div");
  document.body.appendChild(element);
  const ed = new Editor({
    element,
    content,
    contentType: "markdown",
    extensions: [
      StarterKit.configure({ link: false }),
      Link.extend({ inclusive: false }).configure({ openOnClick: false, autolink: false }),
      TestMention,
      createIssueIdentifierAutolinkExtension({ resolveRef }),
      createGitHubIssueKeyChipsExtension({ resolveRef }),
      Markdown.configure({ indentation: { style: "space", size: 3 } }),
      createMarkdownPasteExtension(),
    ],
  });
  ed.on("update", () => {
    updates++;
  });
  return ed;
}

function flush(): Promise<void> {
  return new Promise((r) => setTimeout(r, 0));
}

/** Issue mention nodes in the document, as `id` attributes. */
function mentionIds(ed: Editor): string[] {
  const ids: string[] = [];
  ed.state.doc.descendants((node) => {
    if (node.type.name === "mention") ids.push(String(node.attrs.id));
  });
  return ids;
}

beforeEach(() => {
  updates = 0;
  resolveMock.mockReset();
  resolveMock.mockImplementation(async (identifier) => {
    const id = TRACKED[identifier];
    return id ? { id, identifier } : null;
  });
  (EditorView.prototype as unknown as { scrollToSelection: () => void }).scrollToSelection =
    () => {};
  Element.prototype.scrollIntoView = () => {};
});

afterEach(() => {
  editor?.destroy();
  editor = null;
  document.body.innerHTML = "";
});

describe("GitHub issue key chips in the editor", () => {
  it("shows a tracked key in loaded content as a chip and saves it unchanged", async () => {
    const body = "Blocked by mouse9527/multica#21 for now.";
    editor = makeEditor(body);
    await flush();
    await flush();

    expect(resolveMock).toHaveBeenCalledWith("mouse9527/multica#21");
    expect(mentionIds(editor)).toEqual(["mouse9527%2Fmultica%2321"]);
    // Same Markdown as loaded: no mention link reaches storage or GitHub.
    expect(editor.getMarkdown().trim()).toBe(body);
    // Nothing to save: the conversion does not emit an update.
    expect(updates).toBe(0);
  });

  it("leaves an untracked key as plain text", async () => {
    editor = makeEditor("See other/repo#9 too.");
    await flush();
    await flush();

    expect(resolveMock).toHaveBeenCalledWith("other/repo#9");
    expect(mentionIds(editor)).toEqual([]);
    expect(editor.getMarkdown().trim()).toBe("See other/repo#9 too.");
  });

  it("does not convert keys in inline code or code blocks", async () => {
    const body = "Run `mouse9527/multica#21` here\n\n```\nmouse9527/multica#21\n```";
    editor = makeEditor(body);
    await flush();
    await flush();

    expect(resolveMock).not.toHaveBeenCalled();
    expect(mentionIds(editor)).toEqual([]);
  });

  it("keeps local identifiers and their mentions as they are", async () => {
    const body =
      "[BIPO-46](mention://issue/uuid-46) and BIPO-47 wait on mouse9527/multica#21";
    editor = makeEditor(body);
    await flush();
    await flush();

    expect(mentionIds(editor)).toEqual(["uuid-46", "mouse9527%2Fmultica%2321"]);
    // A bare local identifier in loaded content is not rewritten (upstream rule).
    expect(resolveMock).not.toHaveBeenCalledWith("BIPO-47");
    expect(editor.getMarkdown().trim()).toBe(body);
  });

  it("leaves formatted keys as text so the saved Markdown keeps its formatting", async () => {
    const body =
      "**mouse9527/multica#21** and *mouse9527/multica#21* and ~~mouse9527/multica#21~~ and **bold mouse9527/multica#21 text**";
    editor = makeEditor(body);
    await flush();
    await flush();

    expect(mentionIds(editor)).toEqual([]);
    expect(resolveMock).not.toHaveBeenCalled();
    expect(editor.getMarkdown().trim()).toBe(body);
    expect(updates).toBe(0);
  });

  it("converts an unformatted key between formatted text without touching the formatting", async () => {
    const body = "**bold** mouse9527/multica#21 *italic*";
    editor = makeEditor(body);
    await flush();
    await flush();

    expect(mentionIds(editor)).toEqual(["mouse9527%2Fmultica%2321"]);
    expect(editor.getMarkdown().trim()).toBe(body);
    expect(updates).toBe(0);
  });

  it("converts a typed key once the caret moves past it, saving plain text", async () => {
    editor = makeEditor();
    await flush();

    editor.view.dispatch(editor.state.tr.insertText("mouse9527/multica#21", 1));
    await flush();
    await flush();
    // Caret still right behind the key: it may still be growing.
    expect(mentionIds(editor)).toEqual([]);

    editor.view.dispatch(editor.state.tr.insertText(" ", editor.state.selection.from));
    await flush();
    await flush();

    expect(mentionIds(editor)).toEqual(["mouse9527%2Fmultica%2321"]);
    expect(editor.getMarkdown().trim()).toBe("mouse9527/multica#21");
  });
});
