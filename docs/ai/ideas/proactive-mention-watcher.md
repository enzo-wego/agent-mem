# Idea — Proactive "this is about your work" mention watcher

Status: captured (not designed, not implemented)
Date: 2026-05-28
Owner: Enzo

## One-liner

When agent-mem ingests new content (Slack message, Jira comment, PR review, CF
page edit, PagerDuty incident, etc.) that's **related to something you're
actively working on**, send you a Slack DM with: who said it, where, a
short summary, and a one-click link.

## Why this matters

Right now, the graph is **pull-only** — you have to ask EnzoBot a question
to surface relevant context. But the graph already sees every artefact
flowing through Slack/Jira/GH/CF/PD/DD/Sentry/GWS. It can detect when those
artefacts intersect with your current work *as it happens*, and proactively
notify you instead of waiting for you to ask.

Concrete pain it solves:

- You're heads-down on PAY-2128. Someone files PAY-2200 in a different
  channel with the same root cause; you find out three days later when
  oncall pages you. **Proactive ping** would have flagged it within minutes.
- A partner (Tabby/Checkout/TripleA) is discussed in a channel you're not in.
  You only learn after the postmortem is written.
- A code-review comment lands on a PR you've touched, but Slack notifications
  are too noisy so you miss it.

## What "related to your work" means — sources of intent

The system needs to know what you currently care about. Three layers, in
priority order:

1. **Open Jira tickets assigned to you** (status ≠ Done/Closed/Cancelled)
   - Source: `acli jira workitem search 'assignee = currentUser() AND
     statusCategory != Done'` once per N minutes, cached
   - Each open ticket → an active "watch" with the ticket's key + summary +
     extracted entities (partner names from the title/description)

2. **User-configured subjects** (configurable via dashboard / DM command)
   - "Watch any thread mentioning `partner:tabby` for 7 days"
   - "Watch anything in channel `payments-ops` for 30 days"
   - "Watch `feature:auto_refund` indefinitely until I say stop"
   - Stored in `graph.user_watches` (new table)

3. **PRs you authored or reviewed in the last 14 days** (auto-watch)
   - Auto-derived from `graph.nodes` where author_person_id = you AND
     type = gh_pr AND updated_at > now - 14d

## Trigger conditions

When a new artifact is ingested (insert flow), check it against active
watches:

| Watch type | Match logic |
|---|---|
| Open Jira ticket | New artifact has an edge to `jira:<your-ticket-key>` OR new artifact body mentions ticket key OR new artifact shares ≥ 2 entity nodes with the ticket (e.g. both reference `partner:tabby` + `currency:TRY`) |
| Subject (partner/feature/channel/entity) | New artifact has an edge to that entity node |
| Auto-watch (your PR) | New artifact has an edge to `gh_pr:<your-pr-url>` |

Match strength = number of independent links × time-decay × person-score
of the artifact author (using the existing scoring).

## Notification format

DM via EnzoBot to the Slack user:

```
:bell: Looks like new context for *PAY-2128* arrived

*Surbhi Babbar* in <#payments-cko> — 2 minutes ago:
> "I moved VISA, MC & Apple Pay for TRY to Wego MENA"

Why I think this is related:
• shares entity *currency:TRY*
• shares entity *partner:checkout*
• author is in *@payments-ops* with you

<https://wego.slack.com/archives/.../p...|View thread>  ·  `mute 1h`  `not relevant`
```

Actions:
- `mute 1h` — suppress further pings on this ticket/subject for 1h
- `not relevant` — feedback signal; reduces future matches with this same
  pattern (a small thumbs-down/thumbs-up store, used to tune match
  thresholds per user)

## Where this lives in the architecture

This is **mostly a process-layer addition**, with a small CCR change:

- New worker handler `check_watch_matches` (agent-mem):
  enqueued at the END of `fetch_body` / `describe_attachment` for every
  newly-ingested node. Reads active watches, computes match strength
  against this node, if above threshold → enqueues a `send_dm` job.
- New worker handler `send_dm` (agent-mem):
  POSTs to a CCR webhook endpoint (or directly to Slack via bot token —
  agent-mem already owns the token in v1). Payload includes the rendered
  Markdown block + the action buttons.
- New CCR endpoint `POST /graph/dm` (or directly use Slack chat.postMessage
  from agent-mem):
  Receives the DM payload, sends to the user via Slack Web API. Handles
  the `mute` and `not relevant` button clicks as Slack interactivity
  payloads.
- New tables:
  - `graph.user_watches(id, eeid, kind, target, expires_at, created_at)`
  - `graph.watch_matches(id, watch_id, node_id, score, reasons JSONB,
    notified_at, action TEXT)`  — store every match for audit + dedup
  - `graph.user_mutes(eeid, target, until)`  — short-term suppressions
- Dashboard page:
  - **Watches**: list active watches, create/edit/delete, mute durations
  - **Notifications**: history of matches per user with the feedback they gave

## Configurable knobs (per user)

- `notify_minimum_score` — threshold for sending a DM (default 0.5)
- `notify_max_per_hour` — rate-limit (default 5 — anti-spam)
- `notify_quiet_hours` — e.g. 22:00–08:00 in user TZ (default off)
- `notify_channels` — Slack DM only / also a personal log channel / nothing
- `auto_watch_my_open_jira` — default true
- `auto_watch_my_recent_prs` — default true
- `subject_watches` — managed via dashboard or `/watch` DM command

## Open questions

1. **Match scoring threshold** — too high and we miss real signal; too low
   and it spams. Start with: at least 2 independent entity links OR direct
   ticket key reference. Iterate with thumbs-up/down feedback.

2. **Slack rate limits** — Slack DMs have per-user-per-app rate caps. The
   `notify_max_per_hour` cap should keep us well under them, but should
   verify against Slack's tier 4 chat.postMessage limit.

3. **Privacy / scope** — a watch crosses channels: if you have an open Jira
   ticket but a relevant thread is in a private channel you're not a
   member of, do we notify? The Phase 1 ACL says "filter at retrieval
   against the asker's accessible_scopes". Same rule applies here — only
   notify on artefacts you'd be allowed to see via `/resolve`. **No leakage
   out of ACL.**

4. **Bot-noise filter** — bot messages (PagerDuty, JirAbot, etc.) often
   contain the keywords. Decide: do those count as "someone is talking
   about your task"? Probably yes for PagerDuty alerts, no for routine
   JirAbot field-change notifications. Per-source rule list.

5. **Stale watches** — auto-watches based on Jira tickets should
   auto-expire when the ticket transitions to Done. Need a periodic
   reconciliation job (`refresh_watches`) that polls Jira for status
   changes on watched tickets.

6. **Notification dedup** — if 10 messages in the same thread mention
   your ticket, send 1 DM with "10 new updates" not 10 separate DMs.
   Coalesce per `(user, watch, slack_thread)` over a 5-minute window.

7. **Conflict with EnzoBot's existing PagerDuty pings** — EnzoBot
   already pings on-call about incidents. Don't double-ping the same
   on-call person about the same incident artefact.

## Rough scope estimate

| Component | Effort |
|---|---|
| `graph.user_watches` + `graph.watch_matches` schema | 1 migration |
| `check_watch_matches` worker handler | small (~80 lines) |
| `refresh_watches` worker (poll Jira for status, expire auto-watches) | small |
| `send_dm` worker (Slack chat.postMessage) | small |
| Match scoring + threshold logic | medium — needs careful tuning |
| Slack interactivity handler in CCR (mute / not relevant buttons) | medium — CCR needs to handle Slack action callbacks |
| Dashboard pages: Watches + Notifications history | medium |
| Initial tests + a soak period with thresholds | medium-large |

Estimated: **1 small PR for schema + scaffolding, 1 medium PR for the
match engine, 1 medium PR for CCR interactivity + dashboard.** Total ~2 weeks
of focused work.

## Dependencies

This idea sits on top of:

- Phase 1–3 (ingest + process + read) — must be merged and running
- Phase 4 (CCR forwarder) — provides the live ingest stream
- Phase 5 (CCR prompt injection) — not required, but the asker_eeid
  resolver from there is reused for "who am I notifying"

Cannot start before all of those are deployed.

## Future extensions

- **Cross-team notification**: notify the whole team channel when a
  ticket they collectively own gets new context (not just the assignee)
- **Confidence levels**: "high confidence — direct ticket reference" vs
  "low confidence — semantic neighbour only" — different message phrasing
- **Daily digest mode**: instead of real-time DMs, a 9am summary of
  "what happened on your tickets yesterday"
- **Standup helper**: auto-generate the user's standup notes from their
  watch matches over the previous 24h
