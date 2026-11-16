// Fork-only test (not present upstream) for scripts/fork-go-test-skips.mjs.
import assert from "node:assert/strict";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { isDatabaseSkip, summarize, verdict } from "./fork-go-test-skips.mjs";

const script = fileURLToPath(new URL("./fork-go-test-skips.mjs", import.meta.url));

const out = (Package, Test, Output) => ({ Action: "output", Package, Test, Output });
const end = (Action, Package, Test) => ({ Action, Package, Test });

function skipped(Package, Test, reason) {
  return [
    out(Package, Test, `=== RUN   ${Test}\n`),
    out(Package, Test, `    x_test.go:12: ${reason}\n`),
    out(Package, Test, `--- SKIP: ${Test} (0.00s)\n`),
    end("skip", Package, Test),
  ];
}

const passing = [out("p/a", "TestOK", "=== RUN   TestOK\n"), end("pass", "p/a", "TestOK")];

for (const reason of [
  "database not available",
  "no database connection",
  "integration test requires Postgres at DATABASE_URL",
  "handler test DB not configured",
  "channel_installation not present (database not migrated)",
  "DATABASE_URL not set; skipping live-Postgres telemetry test",
]) {
  test(`database skip fails the gate: ${reason}`, () => {
    assert.equal(isDatabaseSkip(reason), true);
    const problems = verdict(summarize([...passing, ...skipped("p/b", "TestDB", reason)]));
    assert.equal(problems.length, 1);
    assert.match(problems[0], /p\/b TestDB/);
  });
}

for (const reason of [
  "Postgres does not provide pg_bigm; install it to run the real-opclass integration test",
  "requires windows",
  "symlink privilege not held",
]) {
  test(`other skip passes the gate: ${reason}`, () => {
    assert.equal(isDatabaseSkip(reason), false);
    const summary = summarize([...passing, ...skipped("p/b", "TestOther", reason)]);
    assert.deepEqual(verdict(summary), []);
    assert.equal(summary.otherSkips.length, 1);
  });
}

test("a TestMain that cannot reach the database fails the gate", () => {
  const problems = verdict(summarize([
    ...passing,
    out("p/handler", undefined, "Skipping tests: database not reachable: dial tcp: refused\n"),
    out("p/handler", undefined, "ok  \tp/handler\t0.01s\n"),
    end("pass", "p/handler"),
  ]));
  assert.equal(problems.length, 1);
  assert.match(problems[0], /p\/handler: Skipping tests/);
});

test("a package-level skip announced in other words still fails the gate", () => {
  const problems = verdict(summarize([
    ...passing,
    out("p/new", undefined, "SKIP: DATABASE_URL not set\n"),
    out("p/new", undefined, "ok  \tp/new\t0.01s\n"),
    end("pass", "p/new"),
  ]));
  assert.equal(problems.length, 1);
  assert.match(problems[0], /p\/new: SKIP: DATABASE_URL not set/);
});

test("package-level output that is not a skip passes the gate", () => {
  const problems = verdict(summarize([
    ...passing,
    out("p/db", undefined, "migrated test database in 1.2s\n"),
    out("p/db", undefined, "ok  \tp/db\t0.01s\n"),
    end("pass", "p/db"),
  ]));
  assert.deepEqual(problems, []);
});

test("an empty stream fails the gate", () => {
  assert.match(verdict(summarize([]))[0], /no test passed/);
});

test("subtest skip reasons are attributed to the subtest", () => {
  const summary = summarize([
    ...passing,
    out("p/c", "TestParent", "=== RUN   TestParent\n"),
    ...skipped("p/c", "TestParent/case", "database unavailable"),
    end("pass", "p/c", "TestParent"),
  ]);
  assert.deepEqual(summary.databaseSkips.map((s) => s.test), ["TestParent/case"]);
});

test("the CLI exits non-zero on a database skip and zero otherwise", () => {
  const dir = mkdtempSync(join(tmpdir(), "fork-go-test-skips-"));
  try {
    const write = (name, events) => {
      const file = join(dir, name);
      writeFileSync(file, `${events.map((e) => JSON.stringify(e)).join("\n")}\nnot json\n`);
      return file;
    };
    const env = { ...process.env, GITHUB_STEP_SUMMARY: "" };
    const bad = spawnSync(process.execPath, [script, write("bad.json", [...passing, ...skipped("p/b", "TestDB", "no database: refused")])], { env, encoding: "utf8" });
    assert.equal(bad.status, 1, bad.stdout + bad.stderr);
    const good = spawnSync(process.execPath, [script, write("good.json", passing)], { env, encoding: "utf8" });
    assert.equal(good.status, 0, good.stdout + good.stderr);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
