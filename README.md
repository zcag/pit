# pit

An idea ranker. Dump ideas in, sift them by pairwise duel, find out what to
focus on — and get a defensible answer to *can I stop now?*

You pit ideas against each other. It is also the hole you dump them into.

<https://pit.example.com>

## Why it is not a kanban board

A board gives you buckets and no order, so you end up with sixty cards in
"backlog" and still no idea what to do. The hard half is the sifting.

**Pairwise duels** are the mechanic: *A or B?* You never hold forty ideas in your
head, only two. Glicko-2 over the answers gives a real order, and — because
Glicko-2 carries an uncertainty per idea — a principled reason to stop.

Two axes that compose:

- **Buckets** are the decided state: `shortlist → maybe → inbox → parked → dead`
  (per board; a writing board wants `drafting` where a project board wants `doing`)
- **Duels** rank *within* the board, and the board updates live as you go

## When the duel ends

Sorting everything is the wrong target. Nobody needs #9 against #10, and a full
noisy sort costs more comparisons than anyone will sit through. The target is
the **top 5 as a set**, and the stopping rule is: *if I had asked slightly
different questions, would I have landed on the same five?*

- **Stability** — the duel log is resampled with replacement, ratings recomputed
  from scratch each time, and we count how often the top 5 comes out the same.
  95% agreement and the board is **settled**.
- **Contested** — the ideas whose uncertainty still straddles the cut line.
  Everything else is safely in or safely out; asking about it is a wasted click.
- **Adaptive pairing** — the server picks pairs that are close, uncertain, and
  near the boundary. Asking whether #1 beats #12 teaches nothing.

A board also goes unsettled on its own when new ideas arrive, because a new
idea has maximum uncertainty by definition. So pit is never "done" — it is
*settled until something changes*.

## The duel log is the truth

Ratings on an idea are a cache. `Recompute` rebuilds every one of them from the
append-only duel log, which is what makes the rating system swappable without a
migration, and what makes undo a one-row write (`voided = 1`, then recompute)
rather than an inverse-Glicko problem.

## Agents

Agents propose, annotate, edit and read. **Agents cannot write a rating** —
there is deliberately no tool for it. A rating is the record of a judgement you
made; if an agent could set one, the board would stop being evidence of what
*you* think, which is the only thing it is for.

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
| `list_boards` | boards, buckets, descriptions, and whether each is settled |
| `create_board` / `update_board` | including the description and bucket vocabulary |
| `add_ideas` | bulk, atomic; the main way an agent contributes |
| `list_ideas` | ranked, with rating and uncertainty; filter by bucket or source |
| `get_idea` | body, comment thread, duel record |
| `update_idea` | title, note, body, bucket — never the rating |
| `comment` | where an agent argues its case |
| `top` | the short answer, with the settled flag |

## REST

Everything the UI does. `Authorization: Bearer <key>`, or the session cookie.

```
GET    /api/boards                        list, each with its verdict
POST   /api/boards                        create
GET    /api/boards/{board}                one board + verdict
PATCH  /api/boards/{board}                name, description, buckets, terminal, intake
DELETE /api/boards/{board}

GET    /api/boards/{board}/ideas          ranked
POST   /api/boards/{board}/ideas          bulk add
GET    /api/boards/{board}/top?k=5        the short answer
GET    /api/boards/{board}/pair[?bucket=] the next duel worth asking
POST   /api/boards/{board}/duels          {winner, loser}
POST   /api/boards/{board}/duels/undo     void the last duel, recompute

GET    /api/ideas/{id}
PATCH  /api/ideas/{id}                    title, note, body, bucket
DELETE /api/ideas/{id}
POST   /api/ideas/{id}/place              {rating, bucket} — a drag; UI only
POST   /api/ideas/{id}/comments
```

## UI

Vertical buckets, drag between them, click a card for body and notes. `D` opens
the fullscreen duel, `Esc` leaves it, `←`/`→` pick. Every view has an address:
`#/<board>`, `#/<board>/<ideaId>`, `#/<board>/duel[/<bucket>]`.

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

## Known gaps

- **No sweep mode yet.** 40 ideas needs ~100 duels; sweep the junk first and 12
  ideas needs ~25, so a keyboard triage pass is what makes the duel finishable
  on a big dump. It is the next thing to build.
- **No dedupe on insert.** Agents re-propose the same idea constantly; embedding
  each new idea and matching it against live *and* buried ones is the fix.
- **Drag is desktop-only** (native HTML5 DnD). The phone story is the duel view.
- **A hand-placed rating loses to the next duel.** Whether dragging should
  instead *pin* an idea out of the pairing pool is an open call.
- **A no-argument tool must still emit `"properties": {}`.** In Go a nil map
  marshals to `null`, and one invalid schema makes a strict client reject the
  *entire* tools/list — every tool vanishes because of the one that takes no
  input. `internal/api/mcp_test.go` guards it.
- **The OAuth resource-server layer is a port of another project's.** Once it has two
  proven consumers it should become a shared module rather than a second copy.
