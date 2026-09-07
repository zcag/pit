# pit

An idea sifter. Dump ideas in, go through them one card at a time, and come out
with a short list of what you actually meant to keep.

You pit ideas against your own judgement. It is also the hole you dump them into.

<https://pit.example.com>

## Why it is not a kanban board

A board gives you buckets and no verdict, so you end up with sixty cards in
"backlog" and still no idea what to do. The hard half is deciding, and deciding
is easy per card and impossible in bulk — forty cards on a screen is a wall you
stare at; one card in the middle of the screen is a question you answer.

**The sift** is the mechanic. One idea, full screen, four keys:

```
                    ↑  maybe
      ←  out       [card]       →  in
                    ↓  unsure

     space  skip — ask me again later this pass
     u      undo the last call
     n      fix the one-liner while it is in front of you
```

Horizontal is the call, vertical is the defer. The two lanes that take most of
the votes get the axis your hands know blind and the swipe everyone already
knows; the two exceptions get the bars above and below, warmer one up.

Twenty ideas takes about a minute. `unsure` is not a failure state — it is the
honest answer often enough to deserve a key, and it leaves you a pile you can
sift again later instead of a decision you faked.

## Lanes are fixed

Every board has the same five lanes, and only their display names change:

| | |
|---|---|
| `unsorted` | the queue the sift draws from; where everything lands |
| `in` | yes |
| `maybe` | a real idea, second tier |
| `unsure` | you looked and could not call it — owed a second pass |
| `out` | off the board. Buried, never deleted — a killed idea is what stops you having it again |

## A burial says how it ended

`out` used to mean two things at once. A card you rejected and a card you built
both landed there and sat indistinguishable, which is fine for clearing the
stage and wrong for everything that reads the pile afterwards — including you,
next month, wondering whether you said no to this or shipped it.

So a burial carries a reason: nothing (the plain no), **`done`** (you built it)
or **`handed`** (it became work somewhere else — a todo, a ticket, another
repo). On the board, `D` and `H` on a focused card bury it as one of those in a
keypress, and the drawer's chip sets or corrects it afterwards.

**`done` then gets its own column.** The board shows six sections over five
lanes: `out` holds what you said no to, `done` holds what left because it
became real, and the reason on the burial is what tells them apart. It takes
the stage like any other (`#/<board>/lane/done`), the header counts it, the
status line reads `7 live · 9 done · 19 out`, and a card carries its chip even
as a title-only token in the rail — which is the whole point, because a rail
token is all you see of a pile you are not looking at. Dropping a card on
`done` marks it; dropping it on `out` un-marks it, which is how you say a
shipped thing was actually a no.

It is deliberately **not** a sixth lane, even though it is a sixth column. A
lane is somewhere the sift can put a card with one key, and "I built this" is
not a judgement you make in a queue — it is something you record later about a
card you already kept. The sift's four keys are untouched, `←` still means no
without asking, and the API, the counts and every agent still see five lanes. A reason only exists
in `out`: dig a card up and it is un-shipped, because it is back to being a
question.

This used to be a per-board vocabulary of arbitrary length. It isn't any more,
because a lane you cannot vote into is a lane that fills up and never empties:
four keys, four lanes, plus the queue. A board renames one — a writing board
calls `in` *drafting* — and the sift, the API and every agent still speak one
vocabulary.

Order *within* a lane is yours: drag a card and it goes where you put it —
with a mouse, with a finger (press and hold, then move), or with `J`/`K` on a
focused card, because the order of `in` is the whole answer and it should not
need a pointer. There is no computed ranking any more (see below), so `in` in
the order you put it is the answer.

## The log is the truth

Every vote and every drag appends to `sifts` — from lane, to lane, the position
it left, and both reasons (the one it recorded and the one it replaced). Nothing is ever overwritten in place, which makes undo a
one-row write (`voided = 1`, put the card back) rather than a guess, and lets
`u` walk back a whole pass one card at a time.

## The duel is shelved

pit used to rank by pairwise duel with Glicko-2 and a bootstrap stopping rule.
It was good at the last mile and useless at the first: 40 ideas needs ~100
comparisons, and the real boards sat at zero duels forever because nobody starts
a hundred-question quiz on a pile that is 60% junk. The sift answers the
question the duels were being asked to answer, in a minute.

So it is off, not deleted. `internal/rank` (Glicko-2, adaptive pairing,
stability) is intact and still tested, the `duels` table and its rows survive
the migration, and the git history has the store and UI wiring. If ordering the
`in` lane by hand ever stops being enough, it comes back — for the five cards it
was always meant for, not for the whole dump.

## Agents

An agent cannot set a reason either, for the same cause: nothing in the tool
surface reaches `/sift` or `/place`, so "this shipped" stays something you say
about your own board.

Agents propose, annotate, edit and read. **Agents cannot sift** — there is
deliberately no tool for it, and `update_idea` will refuse any lane but
`unsorted`. Which lane a card rests in is the record of a judgement you made; if
an agent could set one, the board would stop being evidence of what *you* think,
which is the only thing it is for. Pushing a card back to `unsorted` is allowed:
that is asking for a decision, not making one.

Everything an agent writes is advocacy for adding, so nothing in the loop ever
argues the other way and the `in` lane never gets tested except by your own
second look. `argue_against` is the call that fixes that: it writes nothing
itself, it hands back the kept cards plus the shape an honest objection takes —
what would have to be true, what it competes with, what it quietly costs — and
the agent posts one per card as a comment. You invoke it; nothing watches.

Connect from Claude Code:

```sh
claude mcp add --transport http pit https://pit.example.com/mcp \
  --header "Authorization: Bearer $PIT_API_KEY"
```

From a hosted client (Claude.ai / ChatGPT "Connect"), use the URL alone —
`/mcp` also accepts an OAuth token from the configured authorization server and
serves RFC 9728 Protected Resource Metadata so the host can discover it.

| Tool | |
|---|---|
| `list_boards` | boards, lane labels, and how full each lane is |
| `create_board` / `update_board` | including the description and lane labels |
| `add_ideas` | bulk, atomic; the main way an agent contributes |
| `list_ideas` | best lane first, with lane and position; filter by lane or source |
| `get_idea` | body, comment thread, lane |
| `update_idea` | title, note, body — and `unsorted` as the only lane it may set |
| `comment` | where an agent argues its case |
| `argue_against` | the `in` lane plus the brief for attacking it, one objection per card |
| `top` | the `in` lane, plus how much is still unsorted |

## REST

Everything the UI does. `Authorization: Bearer <key>`, or the session cookie.

```
GET    /api/all?lane=in                   every board's kept lane, side by side
GET    /api/boards                        list, each with its lane counts
POST   /api/boards                        create
GET    /api/boards/{board}                one board + counts
PATCH  /api/boards/{board}                name, description, labels
DELETE /api/boards/{board}

GET    /api/boards/{board}/ideas          best lane first; ?lane= ?source=
POST   /api/boards/{board}/ideas          bulk add
GET    /api/boards/{board}/top?k=5        the short answer
POST   /api/boards/{board}/sift           {idea, lane, reason} — one vote
POST   /api/boards/{board}/sift/undo      void the last call, restore the card
GET    /api/boards/{board}/log?n=50       what you decided, newest first
GET    /api/boards/{board}/shares         live share links for this board
POST   /api/boards/{board}/shares         {lane} — mint one, or return the one that exists
DELETE /api/shares/{token}                revoke

GET    /s/{token}                         the shared lane, rendered — the one route with no key

GET    /api/ideas/{id}
PATCH  /api/ideas/{id}                    title, note, body, lane
DELETE /api/ideas/{id}
POST   /api/ideas/{id}/place              {lane, pos, reason} — a drag, or a burial
POST   /api/ideas/{id}/comments
```

## UI

One lane holds the stage and the rest are a rail beside it. The stage is `in`,
because after a sift that is the answer and the answer should not be a narrow
column at the far right: its cards are numbered in your order, wide enough to
read, and carry the first lines of the body. Clicking any rail lane's header
puts *it* on the stage — which is how you read the maybes, what shipped, or the
graveyard, without leaving the board (`#/<board>/lane/<lane>`). `S` opens the sift, `Esc` leaves
it, arrows vote, `space` skips, `U` undoes anywhere, and `D` / `H` on a focused
card bury it as done or handed off. `/` filters every lane at
once as you type — the graveyard is the pile you actually need to look things
up in — and `C` copies the stage to the clipboard as numbered markdown (`shift`
for the bodies too), because whatever comes out of a sift ends up somewhere
else. A card's comment thread travels
with it into the sift rather than being announced as a count: the case for an
idea matters in the second you are about to bury it, not afterwards on a board
you may never open. In the sift the card is
sized to its text rather than the text to the card — it is measured after render
and scaled until it fits, so a one-line idea comes out big, a long one dense, and
neither asks you to scroll. The two card edges behind it are the rest of the
queue, and they thin out as you go. Drag to reorder within a
lane or to move between them; a column's `sift` button re-sifts just that lane,
which is how the `unsure` pile gets a second pass. On a phone the sift is the
whole screen and the four votes are swipes. Clicking the description opens the
board itself — its name, its lane labels, and the one destructive button in the
app: deleting a board takes every idea on it and asks twice, naming the count.
`#/all` is the one view that is not about a board: every board's `in` lane on
one stage, grouped, each still ordered by hand — because "what have I actually
committed to" does not respect board boundaries. Nothing is merged or ranked
across them, and a card cannot be dragged from one board's answer into
another's. The stage's `share` button mints a read-only link to that one lane
(`/s/<token>`) — the only route in pit that answers without the key, so a
shortlist can be shown to someone instead of screenshotted; the token is the
whole credential and the same button revokes it.
Every view has an address:
`#/<board>`, `#/<board>/<ideaId>`, `#/<board>/sift[/<lane>]`, `#/all`.

## Running it

```sh
make run                     # localhost:8802, throwaway db, key "dev"
make test
make deploy                  # cross-compile + rsync + restart on the server
make install-service         # first time only; fill deploy/pit.service first
```

Config is env-only:

| | |
|---|---|
| `PIT_ADDR` | listen address (default `:8802`) |
| `PIT_DB` | SQLite path (default `~/.local/share/pit/pit.db`) |
| `PIT_API_KEY` | bearer key; also the web login password. Unset ⇒ **no auth** |
| `PIT_OAUTH_ISSUER` | authorization server; JWKS at `{issuer}/oauth2/jwks` |
| `PIT_MCP_RESOURCE` | this endpoint's public URL — the enforced audience |
| `PIT_OAUTH_ALLOWED_EMAIL` / `_SUB` | single-user allowlist; **set one** |

Issuer and resource both unset ⇒ OAuth off, `/mcp` stays API-key-only.

The resource-server layer behind those four is
[zcag/mcpauth](https://github.com/zcag/mcpauth) — token verification, the RFC
9728 metadata, and the discovery challenge, with nothing in it that knows what
pit is. The original keeps its own older copy on purpose; pit is the consumer.

Opening a pre-lanes database migrates it in place: buckets map onto lanes
(`shortlist`→`in`, `parked`→`unsure`, the intake bucket→`unsorted`, the terminal
one→`out`), a custom bucket name survives as that lane's label, and the Glicko
order becomes the hand order of the new board. It is a table rebuild — take a
copy of the file first.

## Known gaps

- **No dedupe on insert.** Agents re-propose the same idea constantly; embedding
  each new idea and matching it against live *and* buried ones is the fix, and
  it matters more now that the queue is the front door.
- **A skip is session-only.** Skipping rotates the card to the back of this
  pass; reopen the sift and it is in the queue again, unmarked. If "I keep
  skipping this one" turns out to be a signal, it needs a counter.
- **A no-argument tool must still emit `"properties": {}`.** In Go a nil map
  marshals to `null`, and one invalid schema makes a strict client reject the
  *entire* tools/list — every tool vanishes because of the one that takes no
  input. `internal/api/mcp_test.go` guards it.
- **The identity gate has to name the claim the token actually carries.** This
  AuthKit app delegates sign-in to an external identity provider and issues `sub="1"` with no `email`, so
  `PIT_OAUTH_ALLOWED_EMAIL` alone rejects every connect while the token itself
  verifies perfectly. `deploy/pit.service.template` says so; the deployed unit
  had drifted from it for days. Connect once and read the identity off the
  journal (`grep 'first verified-token identity'`) before pinning either.
