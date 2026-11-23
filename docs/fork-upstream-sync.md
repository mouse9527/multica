# 同步上游时 fork 侧要补的文件

本文件属于 `mouse9527/multica` fork（上游没有）。这里列出同步上游时，因为上游新加了某个文件，fork 要在同步 PR 里**新增**的 fork 文件。它们不改上游文件，但在上游文件合入之前无法编译，所以不能预先放进 `main`。

## 私有项目路由登记（M-166）

上游 `server/cmd/server/private_project_routes_test.go`（`TestEveryRouteByProjectObjectIDHidesPrivateProjects`，fork `main` 上还没有）遍历全部路由，要求每个路由参数都登记过。fork 的登记写在 `server/cmd/server/fork_private_project_routes_test.go`，不进上游的表：

- `subscriptions/{id}`（Super Agent 订阅的 9 个路由）登记为「不在项目内的对象」：订阅是工作区级对象，`issue_tree` 范围可以跨多个项目；它触达的 issue 和事件对项目外的调用者按 issue 过滤掉，而不是整条订阅隐藏（`TestRouterSubscriptionFiltersPrivateProjects`）。
- `GET /api/router/issues/{id}/covering` 用的是上游已登记的 `issues/{id}`，会被探测；但上游的探测路由不开功能开关，`/api/router` 全部 404，项目成员那一侧的对照必然失败，所以列为「不探测」，由 `TestRouterIssueCoveringHidesPrivateProjects` 在开着 `router_v1` 时做同样的探测。

`main` 上由 `TestForkPrivateProjectRouteRegistrations` 保证登记和路由一致：fork 新加的 `/api/router/`、`/api/evolution/` 路由参数没登记，这个测试就失败。

**同步 PR 里要做的**：上游的测试文件合入后，新增 `server/cmd/server/fork_private_project_routes_hook_test.go`：

```go
// Added by the mouse9527/multica fork: merges the fork's route registrations
// (fork_private_project_routes_test.go) into upstream's
// TestEveryRouteByProjectObjectIDHidesPrivateProjects tables (M-166).

package main

func init() {
	for key, why := range forkRouteParamsOutsideProjects {
		routeParamsOutsideProjects[key] = outsideProjectParam{why: why}
	}
	for key, why := range forkPrivateProjectRoutesNotProbed {
		privateProjectRoutesNotProbed[key] = why
	}
}
```

包级变量在所有 `init` 之前初始化，所以这里改到的是上游已经建好的表；上游文件一行不改。上游如果改了这几个表的名字或类型，按新名字改这个文件即可。
