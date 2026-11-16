// Added by the mouse9527/multica fork: Router page settings (M-105).

import { useFeatureEnabled } from "@multica/core/config";
import { ROUTER_CHAT_FLAG } from "@multica/core/router";
import { Waves } from "@multica/ui/icons";
import { useT } from "../i18n";
import { RoutersTab } from "./routers-tab";

/** The settings page's Routers entry; none while `router_chat` is off. */
export function useRouterSettingsTabs() {
  const { t } = useT("router");
  const enabled = useFeatureEnabled(ROUTER_CHAT_FLAG, false);
  if (!enabled) return [];
  return [{ value: "routers", label: t(($) => $.list.tab), icon: Waves, content: <RoutersTab /> }];
}
