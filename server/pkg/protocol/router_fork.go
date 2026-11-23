// Added by the mouse9527/multica fork: Router v6, slice 1 (M-96).

package protocol

// ChatMessageKindRouterBatch is a batch of issue events the server wrote
// into a Router chat as a user-role input (role 'user', not said by the
// session's creator). It lives in its own file, not beside the upstream
// kinds in messages.go, so a kind upstream adds there does not collide with
// it (M-141).
const ChatMessageKindRouterBatch = "router_batch"
