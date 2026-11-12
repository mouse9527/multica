// Added by the mouse9527/multica fork: typed and pasted GitHub-native issue
// keys (`owner/repo#N`) in the editable editor. Upstream has no such file.
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

const TestMention = BaseMentionExtension.extend({
  addNodeView() {
    return () => ({ dom: document.createElement("span") });
  },
});

const resolveMock = vi.fn<IssueIdentifierResolver>();
const resolveRef: RefObject<IssueIdentifierResolver | undefined> = {
  current: (identifier) => resolveMock(identifier),
};

let editor: Editor | null = null;

function makeEditor(): Editor {
  const element = document.createElement("div");
  document.body.appendChild(element);
  return new Editor({
    element,
    extensions: [
      StarterKit.configure({ link: false }),
      Link.extend({ inclusive: false }).configure({ openOnClick: false, autolink: false }),
      TestMention,
      createIssueIdentifierAutolinkExtension({ resolveRef }),
      Markdown.configure({ indentation: { style: "space", size: 3 } }),
      createMarkdownPasteExtension(),
    ],
  });
}

function flush(): Promise<void> {
  return new Promise((r) => setTimeout(r, 0));
}

function typeAt1(ed: Editor, text: string): void {
  ed.view.dispatch(ed.state.tr.insertText(text, 1));
}

function paste(ed: Editor, text: string): void {
  ed.commands.focus("start");
  const event = new Event("paste", { bubbles: false, cancelable: true });
  Object.defineProperty(event, "clipboardData", {
    value: {
      files: [],
      getData: (type: string) => (type === "text/plain" ? text : ""),
    },
  });
  ed.view.dom.dispatchEvent(event);
}

beforeEach(() => {
  resolveMock.mockReset();
  (EditorView.prototype as unknown as { scrollToSelection: () => void }).scrollToSelection =
    () => {};
  Element.prototype.scrollIntoView = () => {};
});

afterEach(() => {
  editor?.destroy();
  editor = null;
  document.body.innerHTML = "";
});

describe("GitHub issue keys in the editor autolink", () => {
  it("converts a completed owner/repo#N into a canonical issue mention", async () => {
    resolveMock.mockResolvedValue({ id: "uuid-21", identifier: "mouse9527/multica#21" });
    editor = makeEditor();

    typeAt1(editor, "mouse9527/multica#21 ");
    await flush();

    expect(resolveMock).toHaveBeenCalledWith("mouse9527/multica#21");
    expect(editor.getMarkdown().trim()).toBe(
      "[mouse9527/multica#21](mention://issue/uuid-21)",
    );
  });

  it("leaves an untracked key as plain text", async () => {
    resolveMock.mockResolvedValue(null);
    editor = makeEditor();

    typeAt1(editor, "other/repo#9 ");
    await flush();

    expect(resolveMock).toHaveBeenCalledWith("other/repo#9");
    expect(editor.getMarkdown()).not.toContain("mention://issue");
  });

  it("converts a pasted key next to a local identifier", async () => {
    resolveMock.mockImplementation(async (identifier) => {
      const ids: Record<string, string> = {
        "BIPO-46": "uuid-46",
        "mouse9527/multica#21": "uuid-21",
      };
      const id = ids[identifier];
      return id ? { id, identifier } : null;
    });
    editor = makeEditor();

    paste(editor, "BIPO-46 waits on mouse9527/multica#21 now");
    await flush();

    expect(editor.getMarkdown().trim()).toBe(
      "[BIPO-46](mention://issue/uuid-46) waits on [mouse9527/multica#21](mention://issue/uuid-21) now",
    );
  });

  it("does not convert a key inside inline code", async () => {
    resolveMock.mockResolvedValue({ id: "uuid-21", identifier: "mouse9527/multica#21" });
    editor = makeEditor();

    const codeMark = editor.schema.marks.code!.create();
    editor.view.dispatch(
      editor.state.tr.insert(1, editor.schema.text("mouse9527/multica#21 ", [codeMark])),
    );
    await flush();

    expect(resolveMock).not.toHaveBeenCalled();
    expect(editor.getMarkdown()).not.toContain("mention://issue");
  });
});
