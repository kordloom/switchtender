# Yardmaster Ansible callback plugin.
#
# Emits one JSON object per event to the file named by YARDMASTER_EVENTS_PATH while leaving the
# default stdout callback untouched, so a run keeps its readable log and gains a structured stream.
# Enabled by setting ANSIBLE_CALLBACKS_ENABLED=yardmaster and pointing ANSIBLE_CALLBACK_PLUGINS at
# the directory holding this file. The Go event parser in internal/event reads what this writes.

from __future__ import annotations

import json
import os
import time

from ansible.plugins.callback import CallbackBase


class CallbackModule(CallbackBase):
    """Writes structured run events as newline delimited JSON."""

    CALLBACK_VERSION = 2.0
    CALLBACK_TYPE = "notification"
    CALLBACK_NAME = "yardmaster"
    CALLBACK_NEEDS_ENABLED = True

    # MAX_FIELD caps each captured result field so a single event stays small.
    MAX_FIELD = 4000

    def __init__(self):
        super().__init__()
        self._play = ""
        self._task = ""
        self._handle = None
        path = os.environ.get("YARDMASTER_EVENTS_PATH")
        if path:
            self._handle = open(path, "a", encoding="utf-8")

    def _emit(self, event_type, **fields):
        if self._handle is None:
            return
        record = {"type": event_type, "ts": time.time()}
        record.update(fields)
        self._handle.write(json.dumps(record) + "\n")
        self._handle.flush()

    @staticmethod
    def _host(result):
        return result._host.get_name()

    @staticmethod
    def _changed(result):
        return bool(result._result.get("changed", False))

    def _summary(self, result):
        data = result._result
        out = {}
        truncated = False

        def take(key, value):
            nonlocal truncated
            text = value if isinstance(value, str) else json.dumps(value)
            if len(text) > self.MAX_FIELD:
                text = text[: self.MAX_FIELD]
                truncated = True
            out[key] = text

        for key in ("msg", "stdout", "stderr"):
            value = data.get(key)
            if value:
                take("message" if key == "msg" else key, value)
        if isinstance(data.get("rc"), int):
            out["rc"] = data["rc"]
        if data.get("diff"):
            take("diff", data["diff"])
        if truncated:
            out["truncated"] = True
        return out

    def v2_playbook_on_play_start(self, play):
        self._play = play.get_name()
        self._emit("play_start", play=self._play)

    def v2_playbook_on_task_start(self, task, is_conditional):
        self._task = task.get_name()
        self._emit("task_start", play=self._play, task=self._task)

    def v2_runner_on_ok(self, result):
        self._emit(
            "runner_ok",
            play=self._play,
            task=self._task,
            host=self._host(result),
            changed=self._changed(result),
            **self._summary(result),
        )

    def v2_runner_on_failed(self, result, ignore_errors=False):
        self._emit(
            "runner_failed",
            play=self._play,
            task=self._task,
            host=self._host(result),
            **self._summary(result),
        )

    def v2_runner_on_skipped(self, result):
        self._emit("runner_skipped", play=self._play, task=self._task, host=self._host(result))

    def v2_runner_on_unreachable(self, result):
        self._emit(
            "runner_unreachable",
            play=self._play,
            task=self._task,
            host=self._host(result),
            **self._summary(result),
        )

    def v2_playbook_on_stats(self, stats):
        summary = {}
        for host in sorted(stats.processed.keys()):
            totals = stats.summarize(host)
            summary[host] = {
                "ok": totals["ok"],
                "changed": totals["changed"],
                "failures": totals["failures"],
                "unreachable": totals["unreachable"],
                "skipped": totals["skipped"],
                "rescued": totals["rescued"],
                "ignored": totals["ignored"],
            }
        self._emit("stats", stats=summary)
