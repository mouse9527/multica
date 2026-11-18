// Added by the mouse9527/multica fork: the built-in Evolution agent (M-135).

import { useFeatureEnabled } from "@multica/core/config";
import { EVOLUTION_FLAG } from "@multica/core/evolution";
import { Sparkles } from "@multica/ui/icons";
import { useT } from "../i18n";
import { EvolutionTab } from "./evolution-tab";

/** The settings page's Evolution entry; none while `evolution_v1` is off. */
export function useEvolutionSettingsTabs() {
  const { t } = useT("evolution");
  const enabled = useFeatureEnabled(EVOLUTION_FLAG, false);
  if (!enabled) return [];
  return [
    { value: "evolution", label: t(($) => $.tab), icon: Sparkles, content: <EvolutionTab />, adminOnly: true },
  ];
}
