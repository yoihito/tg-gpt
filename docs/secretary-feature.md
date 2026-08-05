# Secretary feature — design notes

Status: **exploratory / not yet decided**. This captures the design discussion so far, including
research findings and an open question about scope that should be resolved before implementation
starts. Nothing here is committed.

## Goal

Give the bot a "secretary" capability: help the user deal with incoming messages from other people
(triage, notes, advice, follow-up tracking) instead of only handling messages the user sends to the
bot directly.

## Open decision: how do messages reach the bot at all?

Two fundamentally different approaches were considered. **This has not been decided yet.**

### Option A — MTProto listener (full passive automation)

Log into the user's *personal* Telegram account via the MTProto user API (not the Bot API), using
`github.com/gotd/td`, and passively watch their DMs + an allowlist of group chats in real time,
with zero action required from the user. Multi-tenant: any user already in `ALLOWED_USER_ID` can
self-link their own account through a conversational `/link_secretary` wizard in the bot chat
(phone number → login code → optional 2FA password).

This was designed in detail (schema, services, auth flow, security fixes) — see
[Appendix: Option A implementation plan](#appendix-option-a-implementation-plan-mtproto-listener)
below. A principal-engineer review of that design surfaced and fixed several real bugs (a secret-leak
through the existing request logger, a channel/goroutine leak in the auth flow, a race on concurrent
link attempts) — those fixes are folded into the appendix.

**Cost of this approach**: a new third-party dependency, per-user long-lived MTProto sessions held
by the bot process, a nontrivial interactive auth wizard, and it sits close to Telegram's API terms
around automating a personal account (read-only listening is fine per
[core.telegram.org/api/terms](https://core.telegram.org/api/terms)'s "no actions on the user's
behalf without consent" clause, but it's still meaningfully more surface area/risk than anything
else in this codebase).

### Option B — forward-to-bot (manual capture)

The user forwards (or pastes) messages they want handled to the bot directly, using the **existing
Bot API** — no MTProto, no session credentials, no new dependency, none of Option A's risk surface.
The bot processes a forwarded message the same way Option A's pipeline would (log it, generate
advice/notes), just triggered by an explicit user action instead of passive monitoring.

**Trade-off**: not passive — nothing is captured unless the user forwards it. But the research below
suggests this may not be a big loss in practice: most of the actual value (commitment tracking,
digests, on-demand queries) doesn't require full-inbox real-time visibility, and forwarding sidesteps
every risk Option A introduces. This has not been designed in detail yet — flagged here as the
likely-simpler v1 candidate pending a decision.

**Recommendation on the table**: start with Option B, revisit Option A only if manual forwarding
turns out to be too much friction in practice.

## Research: what should "secretary" actually produce?

Independent research (email/message-triage products, executive-assistant practice, existing
Telegram/WhatsApp personal-assistant bots) was done to ground the feature in prior art instead of
guessing. Full findings:

**Feature ideas, each grounded in an existing product pattern:**

| # | Feature | Rationale | Grounded in |
|---|---|---|---|
| 1 | One-line rolling summary per thread/chat, updated as messages arrive (not per-message) | Scan many chats fast | Superhuman Auto Summarize |
| 2 | Priority/urgency **tiers** (urgent / needs-response / FYI), not numeric scores | Coarse buckets are more actionable and more trusted than fine-grained scores | EA triage playbooks; Superhuman auto-categorization |
| 3 | **Commitment tracking** — detect when *the user* promises something ("I'll send that Friday") and follow up later | Most-cited underserved EA behavior — outbound commitments get lost | EA "follow-up management" pattern; commitment-extraction research |
| 4 | Deadline/date normalization ("let's do Friday" → concrete date, optionally a reminder) | High-precision, low-judgment, doesn't require "advice" | Granola, Fireflies, email task-extraction tools |
| 5 | Contact/relationship memory ("3rd message from your landlord about rent this month") | Makes it feel like an assistant, not a filter | SaneBox per-sender learning |
| 6 | Bundle low-signal chats into a digest instead of surfacing individually | Cuts noise from high-volume, rarely-urgent chats | SaneBox SaneLater/SaneNews, Shortwave Bundles |
| 7 | Periodic (e.g. morning) digest rather than a running feed | Batches low-urgency material into one touchpoint | Telegram userbot "Iva"; Shortwave/SaneBox default to batched review |
| 8 | Draft-reply suggestions, shown but never sent | Saves typing time without an assistant impersonating the user | Superhuman Snippets, Gmail Smart Reply/Help Me Write |
| 9 | Mention/relevance detection in groups — flag only messages that actually concern the user | Blanket "note everything" is the most-cited failure mode for group chats | Telegram chat-summarizer projects |
| 10 | On-demand query over history ("what did I promise the landlord", "catch me up on Foo") | Pull complement to the push stuff | Gmail/Gemini inbox Q&A; Iva memory search |

**Cadence — per idea, not globally:**
- **Real-time**: only narrow, high-confidence triggers — explicit commitments/deadlines, direct
  mentions, urgent-tier flags. "Someone is waiting on you right now" cases.
- **Batched digest**: thread summaries, group bundling — the dominant pattern across every mature
  product surveyed. Almost nothing in prior art pushes a notification per incoming message.
- **On-demand**: relationship lookups, draft generation, full history search — pull, not push.

**Key implication**: reacting in real time to *every* qualifying message (the original design
assumption) has essentially no precedent in the products surveyed. Real-time should be reserved for
a narrow trigger set; everything else should be batched or pulled.

**Failure modes to design around:**
1. Notification fatigue from reacting to everything.
2. Generic, low-signal "AI opinion" summaries that erode trust — prefer narrow structured extraction
   (decisions, commitments, dates) over open-ended commentary.
3. Over-precise urgency scoring — coarse tiers age better than granular scores.
4. Losing track of the user's own outbound commitments (underserved elsewhere — worth prioritizing).
5. Privacy/trust erosion from silently monitoring chats/group members who aren't the bot's user.

## Next steps

1. Decide Option A vs. B (or a hybrid — e.g. Option B now, Option A later if forwarding proves too
   manual).
2. Once the capture mechanism is decided, re-scope *what gets produced* using the cadence guidance
   above instead of "real-time generic advice on every message" — likely: real-time only for
   commitment/deadline/mention triggers, everything else batched into a digest, plus an on-demand
   query command.
3. Re-run planning once scope is settled; the detailed appendix below is Option A's design if that
   path is chosen, but should be re-scoped for cadence (digest vs. real-time) rather than implemented
   as originally written.

---

## Appendix: Option A implementation plan (MTProto listener)

*Fully designed and reviewed, but not decided as the chosen approach — see "Open decision" above.
Preserved here in case Option A is selected; would need re-scoping for cadence per the research
findings before implementation (this version assumes real-time-per-message, which the research
suggests reconsidering).*

### Context

The bot already supports multiple users — `ALLOWED_USER_ID` in `main.go:66-75` is a comma-separated
allowlist, not a single owner. The secretary feature should follow that same multi-user model: any
already-allowed bot user can self-link their own personal account, entirely through bot conversation
— no shared CLI login step, no single hardcoded owner.

- Scope per user: DMs always watched once linked, plus an explicit per-user allowlist of group chat
  IDs.
- Storage: a dedicated `secretary_notes` table, kept separate from `fact_memory`/`episodic_memory`/
  `trace_events` (those are "the user's conversation with the assistant"; secretary notes are
  messages from other people — read-only listening is fine per Telegram's API terms, but blending it
  into the assistant's memory of the user is not). No cross-promotion into facts for v1.
- Access: only users already in `ALLOWED_USER_ID` can run `/link_secretary`.
- The MTProto client only ever reads updates and separately sends messages *to the linking user* via
  the existing Bot API bot. It never sends, reacts, or marks-read on the personal account.

### New dependency

`github.com/gotd/td` — the standard Go MTProto client library. Verified against current docs
(pkg.go.dev + gotd examples):
- `telegram.NewClient(appID int, appHash string, telegram.Options{SessionStorage, UpdateHandler})`
- `session.FileStorage{Path: ...}` persists a login session to disk (aliased `telegram.FileSessionStorage`)
  — one file per linked user.
- `tg.NewUpdateDispatcher()` + `dispatcher.OnNewMessage(func(ctx, e tg.Entities, u *tg.UpdateNewMessage) error {...})`
  / `dispatcher.OnNewChannelMessage(...)`.
- `github.com/gotd/td/telegram/updates` (`updates.New(updates.Config{Handler: dispatcher})`, wired as
  `telegram.Options.UpdateHandler`) — gap recovery/ordering so a missed update isn't silently lost.
- `client.Run(ctx, func(ctx) error {...})` blocks for the connection's lifetime; check
  `client.Auth().Status(ctx)`, and once authorized call
  `gapsManager.Run(ctx, client.API(), user.ID, updates.AuthOptions{})`.
- Login: `auth.NewFlow(customAuthenticator, auth.SendCodeOptions{}).Run(ctx, client.Auth())`, where
  `customAuthenticator` implements `auth.UserAuthenticator` (`Phone`, `Code`, `Password`,
  `AcceptTermsOfService`, `SignUp`). There's no built-in interactive terminal authenticator — write
  one that blocks on channels fed by bot chat replies, since this needs to be interactive *through
  Telegram*, not stdin.
- Message shape: `u.Message.(*tg.Message)`, `msg.Out` (skip the account's own outgoing messages),
  `msg.Message` (text), `msg.GetPeerID()` (`tg.PeerUser`/`tg.PeerChat`/`tg.PeerChannel` for scope
  filtering).

`TG_API_ID` / `TG_API_HASH` (from https://my.telegram.org) are **app-level** credentials that
identify the client application, not a specific account — one registration covers every linked user.
These stay as global env vars, same convention as `TOKEN`/`OPENAI_API_KEY`. `TG_SESSION_DIR`
(default `data/secretary-sessions`) holds one session file per user: `<dir>/<user_id>.json`.

### Database

Three new tables, following the existing inline-`const`-DDL convention in
`internal/database/database.go` (`Migrate()` / `schemaMigrations` slice, `reminders`/
`episodic_memory` as style templates):

```sql
CREATE TABLE IF NOT EXISTS secretary_links (
    user_id INTEGER PRIMARY KEY,
    status TEXT NOT NULL CHECK (status IN ('linking','active','revoked')),
    session_path TEXT NOT NULL,
    created_at INTEGER DEFAULT (strftime('%s', 'now')),
    linked_at INTEGER,
    FOREIGN KEY (user_id) REFERENCES users(id)
);

CREATE TABLE IF NOT EXISTS secretary_watched_chats (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL,
    chat_id INTEGER NOT NULL,
    chat_title TEXT,
    created_at INTEGER DEFAULT (strftime('%s', 'now')),
    FOREIGN KEY (user_id) REFERENCES users(id),
    UNIQUE (user_id, chat_id)
);

CREATE TABLE IF NOT EXISTS secretary_notes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL,
    chat_id INTEGER NOT NULL,
    chat_type TEXT NOT NULL CHECK (chat_type IN ('private','group','channel')),
    chat_title TEXT,
    sender_id INTEGER NOT NULL,
    sender_name TEXT,
    message_text TEXT NOT NULL,
    tg_message_id INTEGER NOT NULL,
    advice TEXT,
    created_at INTEGER DEFAULT (strftime('%s', 'now')),
    FOREIGN KEY (user_id) REFERENCES users(id),
    UNIQUE (chat_id, tg_message_id)
);
CREATE INDEX IF NOT EXISTS idx_secretary_notes_user ON secretary_notes(user_id, created_at);
```

Notes:
- `secretary_links.user_id` is the PK — one link per bot user. `status='linking'` is transient (set
  while a `/link_secretary` flow is in progress) so a crash mid-flow doesn't leave a phantom "active"
  row with no real session.
- We deliberately do **not** persist the phone number or 2FA password anywhere — they're only held
  transiently in memory for the duration of the login flow, never written to the DB or logs.
- `secretary_watched_chats` replaces what would otherwise be a global env-var group allowlist — each
  user manages their own via bot commands. DMs need no row; they're implicitly always watched once
  `secretary_links.status = 'active'`.
- `UNIQUE(chat_id, tg_message_id)` on `secretary_notes` + `INSERT OR IGNORE` guards against the
  `updates` manager redelivering an update during gap recovery.

New files: `internal/models/secretary_link.go`, `secretary_watched_chat.go`, `secretary_note.go`,
and matching `internal/repositories/secretary_link_repo.go`, `secretary_watched_chat_repo.go`,
`secretary_note_repo.go` — same shape as `reminder_repo.go` (explicit column lists, shared scan
helpers, wrapped errors, `RowsAffected`-based ownership checks).

### Linking flow — the core new piece

This has to happen **through the bot chat**, since there's no other channel to reach the user
interactively. It's a 2–3 step wizard (phone → code → password-if-2FA) layered on top of the
existing text handler.

**`internal/services/secretary_link_manager.go`** — new, in-memory (process-local, not DB-backed)
manager:
```go
type SecretaryLinkManager struct {
    mu       sync.Mutex
    sessions map[int64]*linkSession // keyed by bot user ID
    bot      *tele.Bot
    ...
}

type linkSession struct {
    phoneCh, codeCh, passwordCh chan string
    cancel                      context.CancelFunc
}
```
- `StartLink(ctx context.Context, userID, chatID int64) error` — **first, under `mu`, checks
  `sessions[userID]` for an existing in-progress flow** and rejects ("already linking — use /cancel
  first") rather than silently overwriting it; a second concurrent `/link_secretary` must never spawn
  a second goroutine against the same session file. Then builds the per-user `telegram.Client`
  (session path from `secretary_link_repo`), inserts a `secretary_links` row with
  `status='linking'`, wraps a **`context.WithTimeout(ctx, 5*time.Minute)`** as the flow's context, and
  spawns a goroutine running `client.Run(flowCtx, func(ctx) error { return auth.NewFlow(chatAuthenticator{...}, auth.SendCodeOptions{}).Run(ctx, client.Auth()) })`.
  `chatAuthenticator` implements `auth.UserAuthenticator`: its `Phone`/`Code`/`Password` methods each
  send a prompt via `bot.Send`, then
  **`select { case <-ctx.Done(): return "", ctx.Err(); case v := <-phoneCh: return v, nil }`**
  (not a bare channel receive — a bare `<-phoneCh` is not unblocked by context cancellation and would
  leak the goroutine forever past the 5-minute cap). On timeout/cancel/error, clean up
  `sessions[userID]` and mark the `secretary_links` row back to a non-`linking` terminal state.
- `SubmitInput(userID int64, text string) bool` — routes `text` to whichever channel the active
  `linkSession` for that user is currently waiting on, returns `true` if one was active. Returns
  `false` (no active flow) so normal handling proceeds untouched.
- `Cancel(userID int64) bool` — calls the session's `cancel()` (which the `select` above turns into a
  clean unblock), removes it from `sessions`; wire into the existing `/cancel` command alongside its
  current `rateLimiter.CancelRequest`/`conversationRunner.CancelCurrentDialog` calls.
- On success (`client.Self(ctx)` resolves), updates `secretary_links` to `status='active'`,
  `linked_at=now`, sends a confirmation via the bot, and calls
  `SecretaryManager.StartListenerForUser(ctx, userID)` to begin real-time listening immediately — no
  restart needed.

**Security-critical wiring point — this is NOT just "call SubmitInput first inside HandleText".**
`cmd/main/main.go:141` registers `b.Use(middleware.Logger())` as **global** telebot middleware, and
telebot runs all `b.Use(...)` middleware for every update *before* any `protected.Handle(...)` route
(including `HandleText`) ever executes. `internal/middleware/logger.go` `json.Marshal(c.Update())`s
the entire incoming update — including the raw message text — and logs it at Info level via
`slog.InfoContext` to stderr. That means a phone number or 2FA password typed during
`/link_secretary` would be written to plaintext logs regardless of anything `HandleText` does
internally; short-circuiting inside `HandleText` is too late.

The fix: register a **new middleware that runs before `middleware.Logger()`** — e.g.
`b.Use(secretaryLinkManager.InterceptMiddleware())` placed immediately after the
`requestContext`/`request_id` middleware and *before* `b.Use(middleware.Logger())` in `main.go`. This
middleware calls `SubmitInput(c.Sender().ID, c.Text())`; if it returns `true` (an active link flow
consumed this message), the middleware returns `nil` immediately without calling `next(c)` — so
`Logger()`, `authenticator.Middleware()`, and `HandleText` never see that update at all, and nothing
about it is logged or reaches `trace_events`/the LLM. If it returns `false`, the middleware calls
`next(c)` and the chain proceeds exactly as today.

### New service: `internal/services/secretary_manager.go`

Multi-user analog of a single listener — holds one running MTProto client per linked user:
```go
type SecretaryManager struct {
    linkRepo     *repositories.SecretaryLinkRepo
    watchedRepo  *repositories.SecretaryWatchedChatRepo
    notesRepo    *repositories.SecretaryNoteRepo
    userRepo     *repositories.UserRepo
    textService  *TextService
    bot          *tele.Bot
    apiID        int
    apiHash      string
    listeners    map[int64]*userListener // keyed by user ID
    mu           sync.Mutex
}
```
- `StartAll(ctx context.Context) error` — called once at boot (after DB migrate, alongside
  `reminderService.StartScheduler`): loads every `secretary_links` row with `status='active'` and
  calls `StartListenerForUser` for each, reusing the existing session file (no interactivity — the
  session is already authorized).
- `StartListenerForUser(ctx context.Context, userID int64) error` — builds the `telegram.Client` +
  dispatcher for that user's session file, wires `OnNewMessage`/`OnNewChannelMessage` handlers that:
  skip `msg.Out`; always process `PeerUser` (DMs); for `PeerChat`/`PeerChannel`, only process if
  present in that user's `secretary_watched_chats`; skip empty `msg.Message` (v1 — media-only
  messages are out of scope for now). On a match: `notesRepo.Insert(...)` (via `INSERT OR IGNORE`, so
  redelivered updates are cheap no-ops), then if actually inserted,
  `go s.generateAndSendAdvice(ctx, userID, note)` — off the dispatcher's synchronous path so a slow
  LLM call never blocks update processing. The handler itself always returns `nil` to the dispatcher
  regardless of downstream errors (logged, not propagated).
- `generateAndSendAdvice`: calls `textService.GenerateSecretaryAdvice(ctx, owner, note)` (new method,
  below), stores the result via `notesRepo.SetAdvice`, sends it to the linked user via the
  **existing Bot API bot**: `bot.Send(&tele.User{ID: user.ChatId}, formattedText, &tele.SendOptions{ParseMode: tele.ModeMarkdown})`.
  Reuse/extract `reminder_service.go`'s chunk+markdown-fallback `sendBotMessage` helper (move it to
  `telegram_utils` so both services share it) rather than duplicating.
- `StopAll(ctx context.Context) error` — cancels every running listener's context and waits,
  mirroring `ReminderService.StopScheduler`'s shutdown shape.
- **Session invalidation**: a linked session can go stale server-side at any time — the user revokes
  it from Telegram Settings > Devices, or changes their 2FA password — with no local signal until
  `client.Run`/the auth flow surfaces an auth-invalidated error. The listener loop must catch that
  error class specifically, set `secretary_links.status='revoked'`, stop that user's goroutine, and
  send a one-time bot notification ("Your secretary link was revoked — run /link_secretary to
  relink"). Without this, `status='active'` silently drifts from reality and `StartAll` would keep
  retrying a dead session forever on every restart.

### New `TextService` method — read-only advice generation

Key design point: **do not** reuse `handleLLMRequestWithTools`/`RunScheduledAction`, because they
call `BeginTurn`/`PrepareUserForInput`/`AppendModelMsg`, writing into the user's **live dialog** trace
and touching `LastInteraction`. That would silently inject secretary content into the user's real
conversation history and interfere with dialog-timeout rollover — exactly what "dedicated table" was
meant to prevent.

Add to `internal/services/text_service.go`:
```go
func (h *TextService) GenerateSecretaryAdvice(ctx context.Context, user models.User, note models.SecretaryNote) (string, error) {
    mctx := TurnContext{UserID: user.Id, DialogID: user.CurrentDialogId} // no BeginTurn — nothing written to trace
    retrieved, err := h.memoryManager.Retrieve(ctx, mctx, note.MessageText) // read-only: prefs/facts/episodes/recent trace for context
    if err != nil {
        return "", err
    }
    systemHeader := fmt.Sprintf(SecretaryAdvicePrompt, time.Now().Format(time.RFC3339))
    history := h.memoryManager.AssemblePrompt(systemHeader, retrieved)
    history = append(history, llm.Message{Role: llm.RoleUser, Content: formatSecretaryPrompt(note)})

    modelToUse := user.CurrentModel
    if !h.client.IsClientRegistered(modelToUse) {
        modelToUse = h.defaultModel
    }
    stream, err := h.client.Stream(ctx, llm.Request{Model: modelToUse, Messages: history})
    if err != nil {
        return "", err
    }
    defer stream.Close()
    accumulator := adapters.NewStreamAccumulator()
    for stream.Next() {
        accumulator.AddEvent(stream.Event())
    }
    if err := stream.Err(); err != nil && !errors.Is(err, io.EOF) {
        return "", err
    }
    return accumulator.AccumulatedResponse(), nil
}
```
No `Tools`/`ToolChoice` (v1 is a plain completion — the multi-iteration tool-calling loop in
`runAttachedTurnWithTools` isn't needed for a first cut; web-search-for-advice can be added later).
`SecretaryAdvicePrompt` is a new prompt constant near `AssistantPrompt`, instructing the model it's
looking at a message *from someone else* to the user and should give brief, actionable notes — not
impersonate a reply. This reuses `MemoryManager.Retrieve` + `AssemblePrompt` + `LLMClientProxy.Stream`
+ `adapters.NewStreamAccumulator()` exactly as existing turn-handling does, so advice is automatically
informed by the user's existing facts/preferences — without writing anything back into that store.

### Bot commands (`internal/delivery/tgbot/handlers.go` + `main.go`'s `SetCommands`)

- `/link_secretary` — starts `SecretaryLinkManager.StartLink`. If already linked, report status
  instead of restarting.
- `/secretary_status` — shows linked/not-linked, and the user's `secretary_watched_chats`.
- `/secretary_watch <chat_id> [title]` — inserts into `secretary_watched_chats` for the calling user.
- `/secretary_unwatch <chat_id>` — removes it.
- `/cancel` (existing) — extended to also call `secretaryLinkManager.Cancel(user.Id)`.

v1 keeps chat-ID entry manual (the user finds their group's numeric ID themselves, e.g. via any
existing ID-lookup bot on their own account); a friendlier "pick from your recent groups" flow is
future work, not needed for a first cut.

### Wiring in `cmd/main/main.go`

Alongside the existing `reminderService`/`textService` construction: build the three new repos,
`SecretaryLinkManager`, and `SecretaryManager`; call `secretaryManager.StartAll(ctx)` next to
`reminderService.StartScheduler(ctx)` (before the blocking `b.Start()`), with a matching deferred
`StopAll`. Pass `secretaryLinkManager` into `tgbot.RegisterHandlers` so `HandleText` can call
`SubmitInput`. If `TG_API_ID`/`TG_API_HASH` aren't set, `StartAll` logs a warning and no-ops rather
than failing the whole bot — this feature is opt-in per deployment, not just per user.

### Files touched

- `go.mod`/`go.sum` — add `github.com/gotd/td`.
- `internal/database/database.go` — three new table DDLs + append to `schemaMigrations`.
- `internal/models/secretary_link.go`, `secretary_watched_chat.go`, `secretary_note.go` — new.
- `internal/repositories/secretary_link_repo.go`, `secretary_watched_chat_repo.go`,
  `secretary_note_repo.go` — new.
- `internal/services/secretary_link_manager.go` — new (the phone/code/password wizard).
- `internal/services/secretary_manager.go` — new (per-user MTProto listeners).
- `internal/services/text_service.go` — add `GenerateSecretaryAdvice` + `SecretaryAdvicePrompt` +
  `formatSecretaryPrompt`.
- `internal/delivery/tgbot/handlers.go` — wire `SubmitInput` into `HandleText`, add the new commands,
  extend `/cancel`.
- `internal/telegram_utils/telegram_utils.go` — extract the shared chunk+markdown-fallback send
  helper (currently private to `reminder_service.go`).
- `cmd/main/main.go` — construct/start/stop the new repos/managers, register new commands.

### Suggested implementation sequence

A principal-level review of this design flagged that bundling everything into one pass is riskier
than necessary — the schema/repos are low-risk and mechanical, while the chat-based auth wizard is
the newest, least-proven pattern in the codebase and is where the concurrency/leak bugs above live.
Recommend landing this in four smaller steps rather than one:

1. **Schema + repos + models only** (`secretary_links`, `secretary_watched_chats`, `secretary_notes`
   tables, matching repos/models) — no behavior change, easy to review in isolation.
2. **`TextService.GenerateSecretaryAdvice`**, tested against a manually-seeded `secretary_notes` row
   (no MTProto client involved yet) — proves the read-only-memory-reuse design and the prompt before
   any live listener exists.
3. **`SecretaryManager`** MTProto listener, bootstrapped against a session file created out-of-band (a
   throwaway script/manual step) rather than through the chat wizard — proves the gotd integration,
   dispatcher wiring, and watched-chat filtering independent of the auth UX.
4. **`SecretaryLinkManager`** chat-based wizard last, once (2) and (3) are proven — this is where the
   `middleware.Logger()` ordering fix, the `select`-on-`ctx.Done()` requirement, and the
   concurrent-`/link_secretary` guard all apply, and it's easiest to get right when it's the only new
   moving part in that pass.

### Verification

1. `go build ./...` and `go vet ./...` — must pass.
2. `go test ./...` — existing suite must still pass (no behavior change to existing paths).
3. Manual, since this needs real Telegram credentials: set `TG_API_ID`/`TG_API_HASH` (from
   my.telegram.org, one-time app registration), start the bot, run `/link_secretary` from an allowed
   user, complete the phone/code (and 2FA if applicable) prompts through the chat, confirm
   `secretary_links.status` becomes `active`.
4. From a second Telegram account, DM the now-linked personal account and confirm a `secretary_notes`
   row appears and an advice message arrives via the bot within seconds.
5. `/secretary_watch <group_id>`, then post in that group from a second account, confirm the same
   end-to-end flow fires; confirm an *unwatched* group produces no note.
6. Confirm the linking flow never leaks into `trace_events` — inspect the DB after a
   `/link_secretary` run and verify no phone number or password appears anywhere, and that the user's
   normal `/new_chat`/chat history is unaffected by secretary activity.
