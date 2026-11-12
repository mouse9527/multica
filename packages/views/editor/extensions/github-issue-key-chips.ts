// Added by the mouse9527/multica fork: GitHub-native issue keys (`owner/repo#N`)
// as issue chips in the editable editor. Upstream has no such file.
/**
 * Shows every `owner/repo#N` of an issue this workspace tracks as an issue
 * chip — in content that was loaded (an issue body written by an agent or
 * imported from GitHub) as well as in text the user types or pastes.
 *
 * The chip is a mention node whose id is the percent-encoded key, and it
 * serialises back to the bare key (see `githubKeyMentionMarkdown`). So turning
 * text into a chip never changes the saved Markdown: no `mention://` link is
 * written to storage or copied to GitHub, where it could not be opened. That is
 * what makes it safe to convert loaded content, which the `PREFIX-N` autolink
 * deliberately does not do (its mentions carry a UUID and would rewrite the
 * body on the next save).
 *
 * Conversions are applied with `preventUpdate` and outside the undo history:
 * the document's Markdown is unchanged, so there is nothing to save or undo.
 * A key that resolves to nothing — untracked, another workspace, or a server
 * with the GitHub issue feature off (404) — stays plain text.
 */
import { Extension } from "@tiptap/core";
import { Plugin, PluginKey } from "@tiptap/pm/state";
import type { EditorState } from "@tiptap/pm/state";
import type { EditorView } from "@tiptap/pm/view";
import type { NodeType } from "@tiptap/pm/model";
import type { RefObject } from "react";
import {
  GITHUB_ISSUE_KEY_TOKEN_SOURCE,
  githubIssueKeyFromMentionId,
} from "@multica/ui/markdown";
import type { IssueIdentifierResolver } from "./issue-identifier-autolink";

/** Mention attributes for the chip that stands for `key`. */
export function githubKeyMentionAttrs(key: string) {
  return { id: encodeURIComponent(key), label: key, type: "issue" };
}

/**
 * The bare key a mention node serialises to when it stands for a GitHub key,
 * or `null` for any other mention.
 */
export function githubKeyMentionMarkdown(
  attrs: Record<string, unknown> | null | undefined,
): string | null {
  if (attrs?.type !== "issue" || typeof attrs.id !== "string") return null;
  return githubIssueKeyFromMentionId(attrs.id);
}

interface KeyRange {
  key: string;
  from: number;
  to: number;
}

const KEY_RE = new RegExp(GITHUB_ISSUE_KEY_TOKEN_SOURCE, "g");

/**
 * Standalone keys in plain prose. Skips code, links and code blocks, the same
 * contexts the `PREFIX-N` autolink skips, plus path and dotted continuations
 * (`owner/repo#1/x`, `owner/repo#1.2`). A key the caret sits right behind is
 * still being typed and is left alone until the caret moves on.
 */
function collectKeys(state: EditorState): KeyRange[] {
  const out: KeyRange[] = [];
  const { selection } = state;
  const caret = selection.empty ? selection.from : -1;
  state.doc.descendants((node, pos, parent) => {
    if (!node.isText || !node.text) return;
    if (node.marks.some((m) => m.type.name === "code" || m.type.name === "link")) return;
    if (parent?.type.name === "codeBlock") return;
    const text = node.text;
    KEY_RE.lastIndex = 0;
    let m: RegExpExecArray | null;
    while ((m = KEY_RE.exec(text)) !== null) {
      const key = m[1];
      if (!key) continue;
      const end = m.index + key.length;
      const after = text[end];
      if (after === "/") continue;
      if (after === "." && /[A-Za-z0-9]/.test(text[end + 1] ?? "")) continue;
      const from = pos + m.index;
      const to = pos + end;
      if (to === caret) continue;
      out.push({ key, from, to });
    }
  });
  return out;
}

const pluginKey = new PluginKey("githubIssueKeyChips");

export function createGitHubIssueKeyChipsExtension(options: {
  /** Same resolver as the `PREFIX-N` autolink; a hit means the key is tracked. */
  resolveRef: RefObject<IssueIdentifierResolver | undefined>;
}): Extension {
  return Extension.create({
    name: "githubIssueKeyChips",

    addProseMirrorPlugins() {
      const maybeMentionType = this.editor.schema.nodes.mention;
      if (!maybeMentionType) return [];
      const mentionType: NodeType = maybeMentionType;
      const resolveRef = options.resolveRef;

      return [
        new Plugin({
          key: pluginKey,
          view(view) {
            // key → tracked; resolved at most once per editor.
            const tracked = new Map<string, boolean>();
            const inFlight = new Set<string>();
            let destroyed = false;
            let scheduled = false;

            function scheduleApply(): void {
              if (scheduled || destroyed) return;
              scheduled = true;
              void Promise.resolve().then(() => {
                scheduled = false;
                if (!destroyed) applyTracked(view);
              });
            }

            function applyTracked(v: EditorView): void {
              const hits = collectKeys(v.state).filter((r) => tracked.get(r.key));
              if (hits.length === 0) return;
              const { tr } = v.state;
              // Right-to-left so earlier ranges stay valid.
              for (const r of hits.sort((a, b) => b.from - a.from)) {
                tr.replaceWith(r.from, r.to, mentionType.create(githubKeyMentionAttrs(r.key)));
              }
              tr.setMeta("addToHistory", false);
              tr.setMeta("preventUpdate", true);
              v.dispatch(tr);
            }

            function scan(): void {
              if (destroyed) return;
              const resolve = resolveRef.current;
              if (!resolve) return;
              let anyHit = false;
              for (const { key } of collectKeys(view.state)) {
                if (tracked.has(key)) {
                  anyHit ||= tracked.get(key) === true;
                  continue;
                }
                if (inFlight.has(key)) continue;
                inFlight.add(key);
                Promise.resolve(resolve(key))
                  .then((ref) => tracked.set(key, Boolean(ref)))
                  // A failed lookup counts as untracked so we don't spin.
                  .catch(() => tracked.set(key, false))
                  .finally(() => {
                    inFlight.delete(key);
                    scheduleApply();
                  });
              }
              if (anyHit) scheduleApply();
            }

            // The initial document is content that was loaded, not typed.
            void Promise.resolve().then(scan);

            return {
              update(v, prevState) {
                if (v.state.doc.eq(prevState.doc) && v.state.selection.eq(prevState.selection)) {
                  return;
                }
                scan();
              },
              destroy() {
                destroyed = true;
              },
            };
          },
        }),
      ];
    },
  });
}
