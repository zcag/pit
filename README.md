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
| `out` | no. Buried, never deleted — a killed idea is what stops you having it again |

This used to be a per-board vocabulary of arbitrary length. It isn't any more,
because a lane you cannot vote into is a lane that fills up and never empties:
four keys, four lanes, plus the queue. A board renames one — a writing board
calls `in` *drafting* — and the sift, the API and every agent still speak one
vocabulary.

Order *within* a lane is yours: drag a card and it goes where you put it. There
is no computed ranking any more (see below), so `in` in the order you dragged it
is the answer.

## The log is the truth

Every vote and every drag appends to `sifts` — from lane, to lane, and the
position it left. Nothing is ever overwritten in place, which makes undo a
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

Agents propose, annotate, edit and read. **Agents cannot sift** — there is
deliberately no tool for it, and `update_idea` will refuse any lane but
`unsorted`. Which lane a card rests in is the record of a judgement you made; if
an agent could set one, the board would stop being evidence of what *you* think,
which is the only thing it is for. Pushing a card back to `unsorted` is allowed:
that is asking for a decision, not making one.

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
| `top` | the `in` lane, plus how much is still unsorted |

## REST

Everything the UI does. `Authorization: Bearer <key>`, or the session cookie.

```
GET    /api/boards                        list, each with its lane counts
POST   /api/boards                        create
GET    /api/boards/{board}                one board + counts
PATCH  /api/boards/{board}                name, description, labels
DELETE /api/boards/{board}

GET    /api/boards/{board}/ideas          best lane first; ?lane= ?source=
POST   /api/boards/{board}/ideas          bulk add
GET    /api/boards/{board}/top?k=5        the short answer
POST   /api/boards/{board}/sift           {idea, lane} — one vote
POST   /api/boards/{board}/sift/undo      void the last call, restore the card
GET    /api/boards/{board}/log?n=50       what you decided, newest first

GET    /api/ideas/{id}
PATCH  /api/ideas/{id}                    title, note, body, lane
DELETE /api/ideas/{id}
POST   /api/ideas/{id}/place              {lane, pos} — a drag
POST   /api/ideas/{id}/comments
```

## UI

The queue, then the four lanes it drains into. `S` opens the sift, `Esc` leaves
it, arrows vote, `space` skips, `U` undoes anywhere. In the sift the card is
sized to its text rather than the text to the card — it is measured after render
and scaled until it fits, so a one-line idea comes out big, a long one dense, and
neither asks you to scroll. The two card edges behind it are the rest of the
queue, and they thin out as you go. Drag to reorder within a
lane or to move between them; a column's `sift` button re-sifts just that lane,
which is how the `unsure` pile gets a second pass. On a phone the sift is the
whole screen and the four votes are swipes. Clicking the description opens the
board itself — its name, its lane labels, and the one destructive button in the
app: deleting a board takes every idea on it and asks twice, naming the count.
Every view has an address:
`#/<board>`, `#/<board>/<ideaId>`, `#/<board>/sift[/<lane>]`.

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

Opening a pre-lanes database migrates it in place: buckets map onto lanes
(`shortlist`→`in`, `parked`→`unsure`, the intake bucket→`unsorted`, the terminal
one→`out`), a custom bucket name survives as that lane's label, and the Glicko
order becomes the hand order of the new board. It is a table rebuild — take a
copy of the file first.

## Known gaps

- **No dedupe on insert.** Agents re-propose the same idea constantly; embedding
  each new idea and matching it against live *and* buried ones is the fix, and
  it matters more now that the queue is the front door.
- **Drag is desktop-only** (native HTML5 DnD). The phone story is the sift.
- **A skip is session-only.** Skipping rotates the card to the back of this
  pass; reopen the sift and it is in the queue again, unmarked. If "I keep
  skipping this one" turns out to be a signal, it needs a counter.
- **A no-argument tool must still emit `"properties": {}`.** In Go a nil map
  marshals to `null`, and one invalid schema makes a strict client reject the
  *entire* tools/list — every tool vanishes because of the one that takes no
  input. `internal/api/mcp_test.go` guards it.
- **The OAuth resource-server layer is a port of another project's.** Once it has two
  proven consumers it should become a shared module rather than a second copy.
