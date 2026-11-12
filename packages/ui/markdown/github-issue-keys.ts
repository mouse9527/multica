// Added by the mouse9527/multica fork: autolinking GitHub-native issue keys
// (`owner/repo#N`) in readonly markdown. Upstream has no such file.

import {
  detectLinks,
  findCodeRanges,
  findMarkdownLinkRanges,
  isInsideCode,
  rangesOverlap,
} from './linkify'

/**
 * A GitHub-native issue is identified as `owner/repo#N`. This module rewrites
 * such keys in prose into `mention://issue/<encoded key>` links, the same
 * transport `preprocessIssueIdentifiers` uses for `PREFIX-N`, so the views
 * layer resolves them against the current workspace and renders a chip on a
 * hit. A key that resolves to nothing — untracked, another workspace, or a
 * server with the GitHub issue feature off — goes back to plain text, so
 * over-detection costs at most one deduped lookup.
 *
 * The key is percent-encoded in the link: a raw `#` would start the URL
 * fragment and a raw `/` would read as a path separator.
 */

// Same shape the server accepts (server/internal/githubissues/issuekey): a
// GitHub login, `/`, a repository name, `#`, a number.
const KEY = '[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}#[0-9]{1,10}'

export const GITHUB_ISSUE_KEY_PATTERN = new RegExp(`^${KEY}$`)

// Standalone token with its key in group 1: not part of a longer path
// (`a/owner/repo#1`), a dotted host (`github.com/...`), a scoped package
// (`@scope/pkg#1`), or a word; and not followed by more word characters
// (`#12abc`). Exported as a source so the editor autolink can combine it with
// its `PREFIX-N` detector.
export const GITHUB_ISSUE_KEY_TOKEN_SOURCE = `(?<![A-Za-z0-9_.\\/#@-])(${KEY})(?![A-Za-z0-9_-])`

const KEY_RE = new RegExp(GITHUB_ISSUE_KEY_TOKEN_SOURCE, 'g')

/** True when `value` is a GitHub issue key such as `owner/repo#12`. */
export function isGitHubIssueKey(value: string): boolean {
  return GITHUB_ISSUE_KEY_PATTERN.test(value)
}

/**
 * The GitHub issue key carried by the id segment of a `mention://issue/<id>`
 * link written by `preprocessGitHubIssueKeys`, or `null` for any other id (a
 * UUID or a `PREFIX-N` identifier).
 */
export function githubIssueKeyFromMentionId(id: string): string | null {
  if (!id.includes('%')) return null
  let decoded: string
  try {
    decoded = decodeURIComponent(id)
  } catch {
    return null
  }
  return isGitHubIssueKey(decoded) ? decoded : null
}

/**
 * Rewrite bare `owner/repo#N` keys into `[owner/repo#N](mention://issue/owner%2Frepo%23N)`.
 *
 * Skipped contexts (never rewritten), as for `PREFIX-N`:
 *   - fenced code blocks, inline code, and math
 *   - existing markdown links / images (label or destination)
 *   - detected URLs / emails / file paths
 *   - path or dotted continuations such as `owner/repo#1/x` or `owner/repo#1.2`
 */
export function preprocessGitHubIssueKeys(text: string): string {
  if (!text.includes('#') || !text.includes('/')) return text

  const codeRanges = findCodeRanges(text)
  const linkRanges = findMarkdownLinkRanges(text)
  const detectedLinks = detectLinks(text)

  KEY_RE.lastIndex = 0
  let result = ''
  let lastIndex = 0
  let match: RegExpExecArray | null

  while ((match = KEY_RE.exec(text)) !== null) {
    const key = match[1]
    if (!key) continue
    const start = match.index
    const end = start + key.length
    const range = { start, end }

    if (isInsideCode(start, codeRanges)) continue
    if (linkRanges.some((r) => rangesOverlap(range, r))) continue
    if (detectedLinks.some((l) => rangesOverlap(range, l))) continue

    const after = text[end]
    if (after === '/') continue
    if (after === '.' && /[A-Za-z0-9]/.test(text[end + 1] ?? '')) continue

    result += text.slice(lastIndex, start)
    result += `[${key}](mention://issue/${encodeURIComponent(key)})`
    lastIndex = end
  }

  if (lastIndex === 0) return text
  result += text.slice(lastIndex)
  return result
}
