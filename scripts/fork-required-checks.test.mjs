// Fork-only test (not present upstream). The ruleset in
// .github/rulesets/main-required-checks.json makes checks required by name;
// GitHub cannot tell that a name no longer exists, or that a job may stop
// reporting, and either one leaves every pull request waiting or the gate
// passing on a skip. These invariants keep the ruleset and the workflows that
// produce its checks in agreement. See docs/fork-ci.md.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

// The GitHub Actions app: only check runs it creates satisfy a context, so a
// commit status posted through the API under the same name does not.
const GITHUB_ACTIONS_APP_ID = 15368;

const ruleset = JSON.parse(readFileSync(new URL("../.github/rulesets/main-required-checks.json", import.meta.url), "utf8"));
const requiredRule = ruleset.rules.find((rule) => rule.type === "required_status_checks");

function readWorkflow(file) {
  const source = readFileSync(new URL(`../.github/workflows/${file}`, import.meta.url), "utf8");
  const triggers = source.slice(source.indexOf("\non:\n"), source.indexOf("\njobs:\n"));
  const jobSource = source.slice(source.indexOf("\njobs:\n") + "\njobs:\n".length);
  const headings = [...jobSource.matchAll(/^  ([\w-]+):$/gm)];
  const jobs = Object.fromEntries(headings.map((match, index) => [
    match[1], jobSource.slice(match.index, headings[index + 1]?.index),
  ]));
  return { file, triggers, jobs };
}

const workflows = ["ci.yml", "ci-fork.yml"].map(readWorkflow);

function producer(context) {
  const found = workflows.filter((workflow) => Object.hasOwn(workflow.jobs, context));
  assert.equal(found.length, 1, `${context}: expected exactly one job with this id, found ${found.length}`);
  return { workflow: found[0], job: found[0].jobs[context] };
}

test("the ruleset enforces the required checks on main without bypass", () => {
  assert.equal(ruleset.enforcement, "active");
  assert.equal(ruleset.target, "branch");
  assert.deepEqual(ruleset.conditions.ref_name.include, ["~DEFAULT_BRANCH"]);
  assert.deepEqual(ruleset.bypass_actors, []);
  assert.ok(requiredRule, "missing required_status_checks rule");
  // Matches the merge rule: the source branch must contain the latest main.
  assert.equal(requiredRule.parameters.strict_required_status_checks_policy, true);
  assert.ok(requiredRule.parameters.required_status_checks.length > 0);
});

for (const check of requiredRule.parameters.required_status_checks) {
  test(`required check ${check.context} is reported by GitHub Actions on every pull request`, () => {
    assert.equal(check.integration_id, GITHUB_ACTIONS_APP_ID);
    const { workflow, job } = producer(check.context);
    // A job-level name would change the reported check name.
    assert.doesNotMatch(job, /^    name:/m, `${workflow.file} ${check.context}: name override`);
    // A path-filtered workflow never reports on unrelated pull requests, so the
    // required check would wait forever.
    assert.match(workflow.triggers, /^  pull_request:\n    branches: \[main\]$/m, `${workflow.file}: pull_request on main`);
    assert.doesNotMatch(workflow.triggers, /paths(-ignore)?:/, `${workflow.file}: path-filtered trigger`);
    // A skipped job satisfies a required check, so the job may only be
    // conditioned on not being cancelled; a matrix reports per-leg names.
    const condition = job.match(/^    if: (.+)$/m)?.[1];
    assert.ok(condition === undefined || condition === "${{ !cancelled() }}", `${workflow.file} ${check.context}: if: ${condition}`);
    // A job whose dependency is skipped is skipped too, unless it runs on
    // !cancelled() like the ci.yml aggregates that validate those skips.
    if (/^    needs:/m.test(job)) {
      assert.equal(condition, "${{ !cancelled() }}", `${workflow.file} ${check.context}: needs without if: \${{ !cancelled() }}`);
    }
    assert.doesNotMatch(job, /^    strategy:/m, `${workflow.file} ${check.context}: matrix`);
    assert.doesNotMatch(job, /continue-on-error/, `${workflow.file} ${check.context}: continue-on-error`);
  });
}
