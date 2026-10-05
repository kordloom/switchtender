<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# Schedule a job

A schedule uses a cron expression or an RFC 5545 recurrence rule, and it can fire a stored template,
a single run, a split, or a whole pipeline.

## Schedule a template

1. Save the work first as a [template](tutorial-save-a-template.md), so the schedule stays a one-line
   reference instead of a copy of every field.
2. Open Schedules and add a schedule.
3. Give it a cron expression for the cadence, for example `0 2 * * *` for 02:00 every day, and point
   it at the template.
4. Save. It fires on the schedule and each firing shows up in Runs like any other.

## From the API

    curl -s -X POST localhost:8080/v1/schedules \
      -H "Authorization: Bearer $ST_TOKEN" \
      -H 'content-type: application/json' \
      -d '{"cron":"0 2 * * *","template_id":"tpl_abc123"}'

Use a real template id: the API stores the schedule either way, and one naming a template that does
not exist fires nothing. The Doctor page, at `/ui/doctor`, and `GET /v1/doctor` report a schedule in
that state by id. A `name` is optional and worth setting, since it is what that report
and the schedules list call it.

To schedule without a template, send `playbook` and `inventory` inline, add `shards` for a split, or
`steps` for a pipeline. A worker or the server must be running for a schedule to fire. On a Team
license, add capacity with `switchtender worker`.

A schedule over a smart or constructed inventory can match no hosts at a fire. That fire is
skipped, not failed, as [when a fire matches no hosts](#when-a-fire-matches-no-hosts) explains.

By default the cron expression reads in the server's local time, and the server writes its zone's
name onto the schedule when it is created or imported, such as `America/Chicago`, so every server of
a highly available group reads it the same way. The published container image runs in UTC, so a
schedule created there without a zone is pinned to `UTC`. Add a `timezone`, an IANA name such as
`America/New_York`, to pin it to a zone of your choosing and let it follow that zone's
daylight-saving shifts, so a nightly window stays put across the year.

A schedule an earlier release stored without a zone is read in UTC, and the first server of this
release to open the database writes `UTC` onto it, so no server reads it in a zone of its own. That
is the zone the published image already ran those schedules in. On a host install whose clock is
not on UTC, those schedules now fire by UTC, so set each one's `timezone` to the zone it should fire
in.

    curl -s -X POST localhost:8080/v1/schedules \
      -H "Authorization: Bearer $ST_TOKEN" \
      -H 'content-type: application/json' \
      -d '{"cron":"0 2 * * *","timezone":"America/New_York","template_id":"tpl_abc123"}'

## Schedule a template with a survey

Nobody is there to answer a survey when a schedule fires, so every question takes its default: a
plain question's default becomes an extra var, and a secret question's sealed default rides onto the
run still sealed, opened only inside the execution, the same as when a person leaves the field
blank. A required question takes its default too, held to the question's own rules.

A required question with no default has no answer, so the schedule refuses to fire rather than run
without it. No run is created. The schedules list shows "did not run" with the reason, which names
the question, the audit trail records the refusal as its own entry naming the question, and the
Doctor page reports the schedule as broken before its first fire comes due. Give the question a
default in the template editor and the next fire runs. A webhook trigger and a pull request plan
fire templates the same way and refuse the same way.

## When a fire matches no hosts

A template whose inventory is smart or constructed is resolved each time a fire comes due. When the
inventory matches no host at that moment, the fire is skipped, not failed: no run starts, and the
schedule fires again on its cadence. A skip is recorded where it is hard to miss:

- The schedule dialog previews the chosen template's inventory and warns when it matches nothing
  right now.
- The schedules list shows the last fire as `skipped: no hosts matched`, apart from a failure, and a
  schedule whose last three fires in a row were skipped carries the badge `matched no hosts for the
  last N fires`.
- The Doctor page, at `/ui/doctor`, and `GET /v1/doctor` warn about the same schedules and link to
  the inventory's host preview.
- Every notification target attached to the schedule for `skipped` or for `failure` is told, once,
  through chat, webhook, ntfy, and email targets. A skip is not an incident, so like a hold it never
  pages, texts, or annotates a dashboard.
- Each skip is its own entry on the audit chain, `SCHEDULE /schedules/<id>/skipped`, after the entry
  for the fire.

A schedule's `last_skip` holds the reason the last fire was skipped and `skipped_fires` how many
fires in a row were. A fire that starts a run, or fails, clears both.

## Schedule what cron cannot say

A cron expression has no way to say the last Friday of each quarter, every other Tuesday, every
third day, run six times and stop, or run nightly except on a holiday. A recurrence rule says all of
them. It is the same RFC 5545 form AWX stores its schedules in, so a rule copied out of AWX works as
written.

In the schedule dialog, set Cadence to Recurrence rule and write the rule. Send it as `rrule`
instead of `cron` from the API. A schedule carries one of the two, never both.

    curl -s -X POST localhost:8080/v1/schedules \
      -H "Authorization: Bearer $ST_TOKEN" \
      -H 'content-type: application/json' \
      -d '{"rrule":"DTSTART;TZID=America/New_York:20260102T170000\nRRULE:FREQ=MONTHLY;BYMONTH=3,6,9,12;BYDAY=-1FR","template_id":"tpl_abc123"}'

A rule is a `DTSTART` line and one or more `RRULE` lines, separated by line breaks or spaces:

- `DTSTART` sets the first day and the time of day. A `TZID` on it, as in
  `DTSTART;TZID=America/New_York:20260102T170000`, sets the zone the rule is read in, and the
  schedule takes that zone. A trailing `Z` means UTC. With neither, the rule reads in the schedule's
  `timezone`, which a schedule created without one takes from the server's local zone. A `TZID`
  that disagrees with the schedule's `timezone` is refused rather than resolved in favor of either.
- `RRULE` takes every RFC 5545 part: `FREQ` from `MINUTELY` to `YEARLY`, `INTERVAL`, `COUNT`,
  `UNTIL`, `WKST`, `BYSETPOS`, `BYMONTH`, `BYMONTHDAY`, `BYYEARDAY`, `BYWEEKNO`, `BYDAY` with an
  ordinal such as `-1FR` for the last Friday, `BYHOUR`, `BYMINUTE`, and `BYSECOND`. `FREQ=SECONDLY`
  is refused, since the scheduler checks for due work every 15 seconds.
- `EXRULE` takes the dates one rule generates back out of the others, `EXDATE` takes out single
  dates, and `RDATE` adds single dates.

Some rules worth having:

| Cadence | Rule |
|---------|------|
| Last Friday of each quarter at 17:00 | `RRULE:FREQ=MONTHLY;BYMONTH=3,6,9,12;BYDAY=-1FR` |
| Last weekday of each quarter | `RRULE:FREQ=MONTHLY;BYMONTH=3,6,9,12;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-1` |
| Every other Tuesday | `RRULE:FREQ=WEEKLY;INTERVAL=2;BYDAY=TU` |
| Every third day | `RRULE:FREQ=DAILY;INTERVAL=3` |
| Weekdays, except the first Monday of the month | `RRULE:FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR` and `EXRULE:FREQ=MONTHLY;BYDAY=1MO` |
| Nightly, but not on 25 December | `RRULE:FREQ=DAILY` and `EXDATE;TZID=America/New_York:20261225T020000` |
| Six runs, then stop | `RRULE:FREQ=DAILY;COUNT=6` |

The time of day comes from the `DTSTART` unless `BYHOUR` and `BYMINUTE` say otherwise. The `DTSTART`
itself fires only when it matches the rule, which is how AWX evaluates its schedules, so a rule for
the last Friday of the quarter with a `DTSTART` on a Monday starts at the first last Friday after
it.

The dialog previews the next five fires as the rule is typed, on the rule's own clock, and the
preview endpoint answers the same question from the API:

    curl -s -G localhost:8080/v1/schedules/preview \
      -H "Authorization: Bearer $ST_TOKEN" \
      --data-urlencode 'rrule=DTSTART;TZID=America/New_York:20260102T170000 RRULE:FREQ=MONTHLY;BYMONTH=3,6,9,12;BYDAY=-1FR'

### Daylight saving

A rule is expanded on the wall clock of its zone, which keeps a 09:00 rule at 09:00 local through
every change of the clocks. The two nights a year the clocks move are handled the way RFC 5545
defines them:

- When the clocks go back, a wall time that happens twice fires once, at the first of the two. A
  nightly 01:30 job fires once that night, and an hourly rule fires each named hour once rather than
  running the repeated hour twice.
- When the clocks go forward, a wall time that does not exist is read with the offset in force
  before the gap, so a 02:30 rule fires at 03:30 that night rather than being dropped. A schedule's
  `spring_forward`, the dialog's "If the time doesn't exist", can instead run it when the clock
  jumps, as a cron schedule does by default, or skip that day.

### When a rule ends

A rule with a `COUNT` or an `UNTIL` fires its last occurrence and then stops: the schedule stays,
with no next run, and the Doctor page lists it as finished. Saving a rule that has already fired its
last time is refused, since the schedule would never run.

### Running more than one server

Every server in a highly available group runs the scheduler, and a due recurrence is claimed the
same way a due cron schedule is: by a compare-and-set on its next run time in the shared database,
so exactly one server fires it. The last occurrence of a bounded rule is claimed by clearing that
time under the same compare-and-set, so it fires once too.

A server that stops while it is firing an occurrence, before the run exists, hands the occurrence
back: its next run time is set back to that occurrence, and the schedule's last error says the fire
was interrupted. The next scheduler to check fires it, this server after a restart or another server
of the group. Each scheduled run carries a key naming its schedule and occurrence, so a fire that
takes up a handed back occurrence finds the run the interrupted fire made, if it made one, rather
than starting a second.

A tick that runs late on the night the clocks go back, after a restart, a database failover, or with
a `--schedule-interval` longer than a minute, still fires a time that happens twice only once.

### Imported schedules

An AWX import carries each schedule's rule across. A rule a cron expression says exactly, checked
against the rule's own next fires, becomes cron, which is easier to read and edit. Every other rule
comes across as the recurrence it is, where it used to be skipped. A Rundeck job on a Quartz form
cron cannot read, such as the third Friday (`6#3`) or the last day of the month (`L`), comes across
as a recurrence too.

An AWX schedule that answers its template's survey fires a copy of the template whose questions
default to those answers, so it fires with the same answers it did in AWX. A secret answer cannot
come across, because an export never carries one readably, so a schedule that gave one arrives
switched off and the import report names the question. Give that question a default on the copy,
then switch the schedule on.

Next: give the job its secrets with [set a secret](tutorial-set-a-secret.md).
