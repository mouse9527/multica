import type { LocaleResources } from "@multica/core/i18n";
import ruDocs from "./ru/docs.json";
import ruCommon from "./ru/common.json";
import ruAuth from "./ru/auth.json";
import ruSettings from "./ru/settings.json";
import ruIssues from "./ru/issues.json";
import ruAgents from "./ru/agents.json";
import ruEditor from "./ru/editor.json";
import ruOnboarding from "./ru/onboarding.json";
import ruInvite from "./ru/invite.json";
import ruLabels from "./ru/labels.json";
import ruMembers from "./ru/members.json";
import ruMyIssues from "./ru/my-issues.json";
import ruSearch from "./ru/search.json";
import ruInbox from "./ru/inbox.json";
import ruWorkspace from "./ru/workspace.json";
import ruProjects from "./ru/projects.json";
import ruAutopilots from "./ru/autopilots.json";
import ruSkills from "./ru/skills.json";
import ruChat from "./ru/chat.json";
import ruModals from "./ru/modals.json";
import ruRuntimes from "./ru/runtimes.json";
import ruLayout from "./ru/layout.json";
import ruUsage from "./ru/usage.json";
import ruUi from "./ru/ui.json";
import ruSquads from "./ru/squads.json";
import ruBilling from "./ru/billing.json";
// Modified by the mouse9527/multica fork (M-105): the Router namespace.
import ruRouter from "./ru/router.json";
// Modified by the mouse9527/multica fork (M-43): the password login namespace.
import ruPasswordauth from "./ru/passwordauth.json";
import ruEvolution from "./ru/evolution.json";
// mouse9527/multica fork (M-230): the OAuth MCP namespace.
import ruMcpOAuth from "./ru/mcpoauth.json";

const resources: LocaleResources = {
  docs: ruDocs,
  common: ruCommon,
  auth: ruAuth,
  settings: ruSettings,
  issues: ruIssues,
  agents: ruAgents,
  editor: ruEditor,
  onboarding: ruOnboarding,
  invite: ruInvite,
  labels: ruLabels,
  members: ruMembers,
  "my-issues": ruMyIssues,
  search: ruSearch,
  inbox: ruInbox,
  workspace: ruWorkspace,
  projects: ruProjects,
  autopilots: ruAutopilots,
  skills: ruSkills,
  chat: ruChat,
  modals: ruModals,
  runtimes: ruRuntimes,
  layout: ruLayout,
  usage: ruUsage,
  ui: ruUi,
  squads: ruSquads,
  billing: ruBilling,
  router: ruRouter,
  passwordauth: ruPasswordauth,
  evolution: ruEvolution,
  mcpoauth: ruMcpOAuth,
};

export default resources;
