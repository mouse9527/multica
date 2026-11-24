// This file belongs to the mouse9527/multica fork (not present upstream): M-259.
//
// The runtime brief used to carry a paragraph about kept checkouts on every
// run that listed a repository. The lean brief (server/internal/daemon/execenv/
// runtime_config_lean.go) drops it, and `multica repo checkout` says it instead,
// only when a checkout really was kept. The upstream brief still carries the
// paragraph, so agents on it read the rule twice when a checkout is kept.

package main

// keptCheckoutDeliveryNote follows the "Kept the existing checkout" line. It
// names no source for the original target on purpose: the branch a kept
// checkout reports is the task's own branch, the head of a pull request.
const keptCheckoutDeliveryNote = "\nThis is work that began earlier, possibly before the project was last reconfigured. " +
	"The branch named above is where the work sits: the head of a pull request, never its base. " +
	"Keep delivering where it was already going — the base of its existing pull request, or the target the task states — and ask if neither settles it. " +
	"Do not retarget it to the project's current starting point, which may have changed since this work began."
