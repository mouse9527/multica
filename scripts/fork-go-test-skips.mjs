#!/usr/bin/env node
// Fork-only script (not present upstream): the gate of the `backend-db-no-skip`
// check in .github/workflows/ci-fork.yml. See docs/fork-ci.md.
//
// Reads a `go test -json` stream and fails when a test skipped because the
// database was unavailable. CI provides Postgres at DATABASE_URL, so such a
// skip is never expected there; without this gate it passes as "ok" and the
// database cases silently do not run. Failing tests are reported too, so the
// log stays readable even though the raw stream is JSON.
import { appendFileSync, createReadStream, realpathSync } from "node:fs";
import { createInterface } from "node:readline";
import { fileURLToPath } from "node:url";

// A skip whose reason mentions the database, unless the reason is one CI's
// Postgres image legitimately cannot satisfy.
const DATABASE_REASON = /database|postgres|DATABASE_URL|\bDB\b|not migrated/i;
const ALLOWED_DATABASE_REASONS = [
  // pgvector/pgvector images do not ship the pg_bigm extension.
  /pg_bigm/,
];
// Package TestMain functions that cannot reach the database print this line
// and exit 0 without running a single test.
const PACKAGE_SKIP = /^Skipping .*(could not connect|database|not reachable|seed failed)/i;

const SKIP_NOISE = /^(=== (RUN|PAUSE|CONT|NAME)|--- SKIP:)/;

export function isDatabaseSkip(reason) {
  return DATABASE_REASON.test(reason) && !ALLOWED_DATABASE_REASONS.some((pattern) => pattern.test(reason));
}

export function summarize(events) {
  const outputs = new Map();
  const packages = new Map();
  const databaseSkips = [];
  const otherSkips = [];
  const failures = [];
  const packageSkips = [];

  const pkg = (name) => {
    if (!packages.has(name)) packages.set(name, { pass: 0, fail: 0, skip: 0 });
    return packages.get(name);
  };

  for (const event of events) {
    if (!event.Package) continue;
    const key = `${event.Package}\u0000${event.Test ?? ""}`;
    if (event.Action === "output") {
      if (!outputs.has(key)) outputs.set(key, []);
      outputs.get(key).push(event.Output ?? "");
      if (!event.Test && PACKAGE_SKIP.test((event.Output ?? "").trim())) {
        packageSkips.push({ package: event.Package, reason: event.Output.trim() });
      }
      continue;
    }
    if (!event.Test) continue;
    const counts = pkg(event.Package);
    if (event.Action === "pass") counts.pass += 1;
    if (event.Action === "fail") {
      counts.fail += 1;
      failures.push({ package: event.Package, test: event.Test, output: outputs.get(key) ?? [] });
    }
    if (event.Action === "skip") {
      counts.skip += 1;
      const reason = (outputs.get(key) ?? [])
        .map((line) => line.trim())
        .filter((line) => line && !SKIP_NOISE.test(line))
        .join(" ");
      const skip = { package: event.Package, test: event.Test, reason };
      (isDatabaseSkip(reason) ? databaseSkips : otherSkips).push(skip);
    }
  }

  let pass = 0;
  let fail = 0;
  let skip = 0;
  for (const counts of packages.values()) {
    pass += counts.pass;
    fail += counts.fail;
    skip += counts.skip;
  }
  return { packages, pass, fail, skip, databaseSkips, otherSkips, failures, packageSkips };
}

export function verdict(summary) {
  const problems = [];
  if (summary.pass === 0) problems.push("no test passed; the stream is empty or truncated");
  for (const s of summary.packageSkips) problems.push(`${s.package}: ${s.reason}`);
  for (const s of summary.databaseSkips) problems.push(`${s.package} ${s.test}: skipped: ${s.reason}`);
  return problems;
}

export function report(summary, problems) {
  const lines = [
    "## backend-db-no-skip",
    "",
    `Packages with tests: ${summary.packages.size} · passed ${summary.pass} · failed ${summary.fail} · skipped ${summary.skip}`,
    `Database-related skips: ${summary.databaseSkips.length + summary.packageSkips.length}`,
    "",
  ];
  if (problems.length > 0) {
    lines.push("### Problems", "", ...problems.map((p) => `- ${p}`), "");
  }
  if (summary.otherSkips.length > 0) {
    lines.push(`<details><summary>Other skips (${summary.otherSkips.length})</summary>`, "");
    for (const s of summary.otherSkips) lines.push(`- \`${s.package}\` ${s.test}: ${s.reason || "(no reason)"}`);
    lines.push("", "</details>", "");
  }
  return lines.join("\n");
}

async function readEvents(file) {
  const events = [];
  const lines = createInterface({ input: createReadStream(file), crlfDelay: Infinity });
  for await (const line of lines) {
    if (!line.startsWith("{")) continue;
    try {
      events.push(JSON.parse(line));
    } catch {
      // Build errors and other non-JSON lines are reported by go test itself.
    }
  }
  return events;
}

if (process.argv[1] && realpathSync(process.argv[1]) === realpathSync(fileURLToPath(import.meta.url))) {
  const file = process.argv[2];
  if (!file) {
    console.error("usage: fork-go-test-skips.mjs <go-test.json>");
    process.exit(2);
  }
  const summary = summarize(await readEvents(file));
  for (const f of summary.failures) {
    console.log(`--- FAIL: ${f.package} ${f.test}`);
    process.stdout.write(f.output.slice(-60).join(""));
  }
  const problems = verdict(summary);
  const text = report(summary, problems);
  console.log(text);
  if (process.env.GITHUB_STEP_SUMMARY) appendFileSync(process.env.GITHUB_STEP_SUMMARY, `${text}\n`);
  if (problems.length > 0) process.exit(1);
}
