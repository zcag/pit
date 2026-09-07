package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/zcag/pit/internal/model"
	"github.com/zcag/pit/internal/store"
)

// MCP over the Streamable HTTP transport, hand-rolled JSON-RPC 2.0 — the same
// shape a sibling project uses, and small enough that a dependency would cost more than it
// saves.
//
// The tool surface has one deliberate hole: nothing here can sift. Agents
// propose, annotate, edit and read; the lane an idea rests in stays the owner's
// call, because a board an agent can sort is no longer evidence of what the
// owner thinks. An agent may push a card back to "unsorted" — that is asking
// for a decision, not making one.

const (
	mcpProtocolVersion = "2024-11-05"
	serverName         = "pit"
)

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func rpcResult(id, r any) *rpcMsg { return &rpcMsg{JSONRPC: "2.0", ID: id, Result: r} }
func rpcFail(id any, code int, msg string) *rpcMsg {
	return &rpcMsg{JSONRPC: "2.0", ID: id, Error: &rpcErr{Code: code, Message: msg}}
}

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func obj(props map[string]any, required ...string) map[string]any {
	// A nil map marshals to JSON null, and `"properties": null` is not a valid
	// schema — strict clients reject the entire tools/list over it, so a
	// no-argument tool takes the whole surface down with it.
	if props == nil {
		props = map[string]any{}
	}
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}
func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func num(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }
func lanemap(desc string) map[string]any {
	return map[string]any{"type": "object", "description": desc,
		"additionalProperties": map[string]any{"type": "string"}}
}

var mcpTools = []tool{
	{
		Name: "list_boards",
		Description: "List every board with its lane labels and how full each lane is. " +
			"A board with a big 'unsorted' count is one the owner still owes a sift. Start here.",
		InputSchema: obj(nil),
	},
	{
		Name: "create_board",
		Description: "Create a board. Lanes are fixed for every board — unsorted (the sift queue), " +
			"in, maybe, unsure, out — and can only be renamed for display.",
		InputSchema: obj(map[string]any{
			"name":        str("Display name, e.g. 'Side Projects'"),
			"description": str("What this board is for"),
			"labels":      lanemap("Display names per lane, e.g. {\"in\":\"drafting\"}"),
		}, "name"),
	},
	{
		Name:        "update_board",
		Description: "Edit a board's name, description or lane labels. Omitted fields are left alone.",
		InputSchema: obj(map[string]any{
			"board":       str("Board slug or id"),
			"name":        str("New display name"),
			"description": str("New description"),
			"labels":      lanemap("Lane display names to set; an empty string resets one to its own name"),
		}, "board"),
	},
	{
		Name: "add_ideas",
		Description: "Add one or many ideas to a board in a single atomic batch. This is the " +
			"main way an agent contributes. Ideas land unsorted, waiting for the owner to sift them.",
		InputSchema: obj(map[string]any{
			"board": str("Board slug or id"),
			"ideas": map[string]any{
				"type": "array",
				"items": obj(map[string]any{
					"title": str("Short statement of the idea"),
					"note":  str("One line saying what it is"),
					"body":  str("The full case for it — the sift shows this, so it is what the decision is made on"),
				}, "title"),
			},
		}, "board", "ideas"),
	},
	{
		Name: "list_ideas",
		Description: "List a board's ideas, best lane first, each with its lane and position. " +
			"Optionally filter by lane (unsorted, in, maybe, unsure, out) or source ('me' or 'agent').",
		InputSchema: obj(map[string]any{
			"board":  str("Board slug or id"),
			"lane":   str("Only ideas in this lane"),
			"source": str("'me' or 'agent'"),
		}, "board"),
	},
	{
		Name:        "get_idea",
		Description: "Get one idea in full: body, comment thread, lane, position.",
		InputSchema: obj(map[string]any{"id": str("Idea id")}, "id"),
	},
	{
		Name: "update_idea",
		Description: "Edit an idea's title, note or body. Omitted fields are left alone. Lanes " +
			"cannot be set — sifting is the owner's judgement — but an idea can be pushed back " +
			"to 'unsorted' to ask for that judgement again.",
		InputSchema: obj(map[string]any{
			"id":    str("Idea id"),
			"title": str("New title"),
			"note":  str("New one-line note"),
			"body":  str("New body"),
			"lane":  str("Only 'unsorted' is accepted: put this card back in the sift queue"),
		}, "id"),
	},
	{
		Name: "comment",
		Description: "Append a note to an idea's thread. This is where an agent argues its case — " +
			"the only channel it has for influencing where a card lands.",
		InputSchema: obj(map[string]any{
			"id":   str("Idea id"),
			"text": str("The note"),
		}, "id", "text"),
	},
	{
		Name: "argue_against",
		Description: "Load a board's 'in' lane together with the brief for attacking it. " +
			"Everything else an agent writes here is advocacy for adding, so nothing in the " +
			"loop ever tests what the owner kept. This call writes nothing itself: it hands " +
			"back the kept cards and what an objection has to contain, and you then post one " +
			"per card with `comment`. Invoke it when asked to, not on your own initiative.",
		InputSchema: obj(map[string]any{
			"board": str("Board slug or id"),
			"lane":  str("Which lane to attack; defaults to 'in'"),
		}, "board"),
	},
	{
		Name: "top",
		Description: "The short answer: what is in the 'in' lane, in the owner's order, plus how " +
			"much is still unsorted. A big unsorted count means this answer is provisional.",
		InputSchema: obj(map[string]any{
			"board": str("Board slug or id"),
			"k":     num("How many; defaults to 5"),
		}, "board"),
	},
}

func (s *Server) mcpHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet || r.Method == http.MethodDelete {
		// No server-initiated streaming: pit has no long-running work to push.
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var in rpcMsg
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeRPC(w, rpcFail(nil, -32700, "parse error"))
		return
	}
	defer r.Body.Close()

	// Notifications carry no id and expect no body.
	if in.ID == nil && strings.HasPrefix(in.Method, "notifications/") {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	switch in.Method {
	case "initialize":
		writeRPC(w, rpcResult(in.ID, map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": serverName, "version": Version},
		}))
	case "ping":
		writeRPC(w, rpcResult(in.ID, map[string]any{}))
	case "tools/list":
		writeRPC(w, rpcResult(in.ID, map[string]any{"tools": mcpTools}))
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(in.Params, &p); err != nil {
			writeRPC(w, rpcFail(in.ID, -32602, "invalid params"))
			return
		}
		out, err := s.callTool(p.Name, p.Arguments)
		if err != nil {
			// Tool errors are results with isError, not transport errors — the
			// model needs to read them and try something else.
			writeRPC(w, rpcResult(in.ID, map[string]any{
				"content": []any{map[string]any{"type": "text", "text": err.Error()}},
				"isError": true,
			}))
			return
		}
		writeRPC(w, rpcResult(in.ID, map[string]any{
			"content": []any{map[string]any{"type": "text", "text": out}},
		}))
	default:
		writeRPC(w, rpcFail(in.ID, -32601, "unknown method: "+in.Method))
	}
}

func writeRPC(w http.ResponseWriter, m *rpcMsg) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(m)
}

// Version is stamped at build time.
var Version = "dev"

func (s *Server) callTool(name string, raw json.RawMessage) (string, error) {
	var a map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", fmt.Errorf("bad arguments: %w", err)
		}
	}
	st := s.cfg.Store

	switch name {
	case "list_boards":
		bs, err := st.ListBoards()
		if err != nil {
			return "", err
		}
		out := make([]boardView, 0, len(bs))
		for _, b := range bs {
			v, err := s.view(b)
			if err != nil {
				return "", err
			}
			out = append(out, v)
		}
		return jsonText(map[string]any{"boards": out, "lanes": model.Lanes})

	case "create_board":
		b := model.Board{
			Name:        argStr(a, "name"),
			Description: argStr(a, "description"),
			Labels:      argLabels(a, "labels"),
		}
		if err := st.CreateBoard(&b); err != nil {
			return "", err
		}
		return jsonText(b)

	case "update_board":
		b, err := st.UpdateBoard(argStr(a, "board"),
			argPtr(a, "name"), argPtr(a, "description"), argLabels(a, "labels"))
		if err != nil {
			return "", err
		}
		return jsonText(b)

	case "add_ideas":
		b, err := st.Board(argStr(a, "board"))
		if err != nil {
			return "", err
		}
		raw, _ := a["ideas"].([]any)
		if len(raw) == 0 {
			return "", fmt.Errorf("ideas must be a non-empty array")
		}
		batch := make([]model.Idea, 0, len(raw))
		for _, r := range raw {
			m, _ := r.(map[string]any)
			batch = append(batch, model.Idea{
				Title: argStr(m, "title"), Note: argStr(m, "note"), Body: argStr(m, "body"),
				Lane: model.Unsorted, Source: model.SourceAgent,
			})
		}
		out, err := st.AddIdeas(b.ID, batch)
		if err != nil {
			return "", err
		}
		return jsonText(map[string]any{"added": len(out), "lane": model.Unsorted, "ideas": out})

	case "list_ideas":
		b, err := st.Board(argStr(a, "board"))
		if err != nil {
			return "", err
		}
		ideas, err := st.ListIdeas(b.ID)
		if err != nil {
			return "", err
		}
		// Counts describe the board, not the slice being returned: an agent asking
		// for one lane still needs to know what is waiting in the others.
		counts := store.Counts(ideas)
		if l := argStr(a, "lane"); l != "" {
			ideas = store.InLane(ideas, model.Lane(l))
		}
		if src := argStr(a, "source"); src != "" {
			ideas = filter(ideas, func(i model.Idea) bool { return string(i.Source) == src })
		}
		return jsonText(map[string]any{"board": b.Slug, "labels": b.Labels,
			"counts": counts, "ideas": ideas})

	case "get_idea":
		it, err := st.Idea(argStr(a, "id"))
		if err != nil {
			return "", err
		}
		return jsonText(it)

	case "update_idea":
		it, err := st.UpdateIdea(argStr(a, "id"), store.IdeaPatch{
			Title: argPtr(a, "title"), Note: argPtr(a, "note"), Body: argPtr(a, "body"),
		})
		if err != nil {
			return "", err
		}
		if l := argStr(a, "lane"); l != "" && model.Lane(l) != it.Lane {
			if model.Lane(l) != model.Unsorted {
				return "", fmt.Errorf("only the owner sifts: an agent may move a card to 'unsorted' " +
					"to ask for a decision, but not into in/maybe/unsure/out. Argue for it with `comment` instead")
			}
			if it, err = st.Move(it.BoardID, it.ID, model.Unsorted, nil, model.NoReason); err != nil {
				return "", err
			}
		}
		return jsonText(it)

	case "comment":
		c, err := st.AddComment(argStr(a, "id"), model.SourceAgent, argStr(a, "text"))
		if err != nil {
			return "", err
		}
		return jsonText(c)

	case "argue_against":
		b, err := st.Board(argStr(a, "board"))
		if err != nil {
			return "", err
		}
		lane := model.Lane(argStr(a, "lane"))
		if lane == "" {
			lane = model.In
		}
		ideas, err := st.ListIdeas(b.ID)
		if err != nil {
			return "", err
		}
		return jsonText(map[string]any{
			"board": b.Slug, "lane": lane, "label": b.Label(lane),
			"brief": argueBrief,
			"ideas": store.InLane(ideas, lane),
		})

	case "top":
		b, err := st.Board(argStr(a, "board"))
		if err != nil {
			return "", err
		}
		k := TopK
		if n := argInt(a, "k"); n > 0 {
			k = n
		}
		ideas, err := st.ListIdeas(b.ID)
		if err != nil {
			return "", err
		}
		return jsonText(topPayload(b, ideas, k))
	}
	return "", fmt.Errorf("unknown tool: %s", name)
}

// argueBrief is the whole point of argue_against: the tool has no model of its
// own, so what it can usefully hand back is the lane plus the shape an honest
// objection takes.
const argueBrief = `Post exactly one comment per card below, with ` + "`comment`" + `, each containing:
  - what would have to be true for this to be worth doing — stated as a condition that could turn out false;
  - what it competes with: which of the other cards on this list it takes time from;
  - what it quietly costs: the maintenance, the surface, the thing it makes harder later.
One honest objection, not a balanced review — the case for the card is already on the card.
Say nothing you do not believe: "this one survives, and here is the strongest thing against it anyway"
is a good comment. Do not sift, and do not move anything: which lane a card rests in is the owner's call.`

func jsonText(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	return string(b), err
}

func argStr(a map[string]any, k string) string {
	s, _ := a[k].(string)
	return s
}

// argPtr distinguishes "absent" from "set to empty", which is what makes a
// sparse patch safe: an agent editing a body must not blank the note.
func argPtr(a map[string]any, k string) *string {
	v, ok := a[k]
	if !ok {
		return nil
	}
	s, ok := v.(string)
	if !ok {
		return nil
	}
	return &s
}

func argLabels(a map[string]any, k string) map[string]string {
	raw, _ := a[k].(map[string]any)
	if raw == nil {
		return nil
	}
	out := make(map[string]string, len(raw))
	for lane, v := range raw {
		if s, ok := v.(string); ok {
			out[lane] = s
		}
	}
	return out
}

func argInt(a map[string]any, k string) int {
	switch v := a[k].(type) {
	case float64:
		return int(v)
	case string:
		n, _ := strconv.Atoi(v)
		return n
	}
	return 0
}
