// Added by the mouse9527/multica fork: Router page settings (M-105).

export * from "./types";
export {
  createRouterSubscription,
  deleteRouterSubscription,
  listRouterChats,
  listRouterDispatches,
  listRouterSubscriptionChanges,
  listRouterSubscriptions,
  setRouterSubscriptionPaused,
  updateRouterSubscription,
} from "./api";
export {
  groupRouters,
  isSubscriptionPaused,
  latestDispatch,
  reconciledThrough,
  routerStatus,
  routerSubscriptionsOfChat,
  unboundSubscriptions,
} from "./group";
export {
  ROUTER_POLL_MS,
  routerChangesOptions,
  routerChatsOptions,
  routerKeys,
  routerLastDispatchOptions,
  routerSubscriptionsOptions,
  useLastRouterDispatch,
} from "./queries";
export {
  addRouterScopes,
  findScopeSubscription,
  RouterScopeTakenError,
  releaseRouter,
  setRouterPaused,
  updateRouterSettings,
  useAddRouterScopes,
  useReleaseRouter,
  useRemoveRouterScope,
  useSetRouterPaused,
  useUpdateRouterSettings,
} from "./mutations";
export type { AddRouterScopesInput, AddRouterScopesResult, RouterReleaseMode } from "./mutations";

/** Frontend flag: on only while router_v1, router_dispatch and router_chat all are. */
export const ROUTER_CHAT_FLAG = "router_chat";
