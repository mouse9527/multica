// @vitest-environment node
// Added by the mouse9527/multica fork: GitHub-native issues. Upstream has no
// such file.
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiClient } from "../api/client";
import { defaultGitHubCreateRepo } from "./queries";

afterEach(() => {
  vi.unstubAllGlobals();
});

function respond(status: number, body: unknown) {
  return vi.fn().mockResolvedValue(
    new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } }),
  );
}

describe("getProjectGitHubSettings", () => {
  it("reads the project's repositories and default", async () => {
    const fetchMock = respond(200, {
      repos: [{ resource_id: "r1", repo: "acme/widgets" }],
      default_resource_id: "r1",
    });
    vi.stubGlobal("fetch", fetchMock);
    const settings = await new ApiClient("https://api.example.test").getProjectGitHubSettings("p1");
    expect(String(fetchMock.mock.calls[0]?.[0])).toBe("https://api.example.test/api/projects/p1/github-settings");
    expect(settings).toEqual({ repos: [{ resource_id: "r1", repo: "acme/widgets" }], default_resource_id: "r1" });
  });

  it("offers nothing when the server has the feature off", async () => {
    vi.stubGlobal("fetch", respond(404, { error: "not found" }));
    await expect(new ApiClient("https://api.example.test").getProjectGitHubSettings("p1")).resolves.toBeNull();
  });

  it("falls back to no repositories on a malformed response", async () => {
    vi.stubGlobal("fetch", respond(200, { repos: "nope" }));
    await expect(new ApiClient("https://api.example.test").getProjectGitHubSettings("p1")).resolves.toEqual({
      repos: [],
      default_resource_id: null,
    });
  });

  it("tolerates missing fields", async () => {
    vi.stubGlobal("fetch", respond(200, {}));
    await expect(new ApiClient("https://api.example.test").getProjectGitHubSettings("p1")).resolves.toEqual({
      repos: [],
      default_resource_id: null,
    });
  });
});

describe("defaultGitHubCreateRepo", () => {
  const widgets = { resource_id: "r1", repo: "acme/widgets" };
  const gadgets = { resource_id: "r2", repo: "acme/gadgets" };

  it("selects the default, else the only repository, else none", () => {
    expect(defaultGitHubCreateRepo(null)).toBeNull();
    expect(defaultGitHubCreateRepo({ repos: [], default_resource_id: null })).toBeNull();
    expect(defaultGitHubCreateRepo({ repos: [widgets], default_resource_id: null })).toBe("acme/widgets");
    expect(defaultGitHubCreateRepo({ repos: [widgets, gadgets], default_resource_id: null })).toBeNull();
    expect(defaultGitHubCreateRepo({ repos: [widgets, gadgets], default_resource_id: "r2" })).toBe("acme/gadgets");
    // A default that no longer names one of the repositories is ignored.
    expect(defaultGitHubCreateRepo({ repos: [widgets], default_resource_id: "gone" })).toBe("acme/widgets");
  });
});
