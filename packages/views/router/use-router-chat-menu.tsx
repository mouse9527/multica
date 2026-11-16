// Added by the mouse9527/multica fork: Router page settings (M-105).
//
// The chat header's hook into Router settings: a menu item and the dialog it
// opens. While `router_chat` is off it renders nothing and touches no query
// or store, so the header behaves as upstream. Only a chat's creator can
// make it their Router, so the item shows only on the member's own chats.

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { useAuthStore } from "@multica/core/auth";
import { useFeatureEnabled } from "@multica/core/config";
import { useWorkspaceId } from "@multica/core/hooks";
import { ROUTER_CHAT_FLAG, routerSubscriptionsOfChat, routerSubscriptionsOptions } from "@multica/core/router";
import type { Agent, ChatSession } from "@multica/core/types";
import { DropdownMenuItem } from "@multica/ui/components/ui/dropdown-menu";
import { Waves } from "@multica/ui/icons";
import { useT } from "../i18n";
import { RouterChatDialog } from "./router-chat-dialog";

export function useRouterChatMenu(session: ChatSession, agent: Agent | null) {
  const enabled = useFeatureEnabled(ROUTER_CHAT_FLAG, false);
  const [open, setOpen] = useState(false);
  if (!enabled || !agent) return { menuItem: null, dialog: null };
  return {
    menuItem: <RouterChatMenuItem session={session} onSelect={() => setOpen(true)} />,
    dialog: <RouterChatDialog session={session} agentName={agent.name} open={open} onOpenChange={setOpen} />,
  };
}

function RouterChatMenuItem({ session, onSelect }: { session: ChatSession; onSelect: () => void }) {
  const { t } = useT("router");
  const wsId = useWorkspaceId();
  const userId = useAuthStore((s) => s.user?.id);
  const own = Boolean(userId) && session.creator_id === userId;
  // Only the label depends on it, so no polling for a closed menu.
  const { data: subs } = useQuery({ ...routerSubscriptionsOptions(wsId), enabled: own, refetchInterval: false });
  const isRouter = routerSubscriptionsOfChat(subs ?? [], session.id).length > 0;
  // An archived chat cannot become a Router, but an existing one can still
  // be paused or released.
  if (!own || (session.status === "archived" && !isRouter)) return null;
  return (
    <DropdownMenuItem onClick={onSelect}>
      <Waves className="h-4 w-4" />
      {isRouter ? t(($) => $.menu.settings) : t(($) => $.menu.setup)}
    </DropdownMenuItem>
  );
}
