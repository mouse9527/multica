// This file belongs to the mouse9527/multica fork (not present upstream).
// M-312 / mouse9527/multica#209.

package protocol

// TaskMessageToolAPIRetry marks a task_message row that reports a model API
// retry: type "error", tool "api_retry". Chat and the task transcript render
// it like any error row; the tool value only tells the server that the row is
// progress, not output, so a turn that only retried still counts as having an
// empty transcript (HasTaskMessages in queries/chat_cancel.sql matches this
// literal).
const TaskMessageToolAPIRetry = "api_retry"
