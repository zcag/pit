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
// The tool surface has one deliberate hole: nothing here can write a rating.
// Agents propose, annotate, edit and read; ranking stays the owner's, because a
// board an agent can rank is no longer evidence of what the owner thinks.

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

var mcpTools = []tool{
	{
		Name: "list_boards",
		Description: "List every board with its bucket vocabulary, description, idea counts, " +
			"and whether its ranking is settled. Start here.",
		InputSchema: obj(nil),
	},
	{
		Name:        "create_board",
		Description: "Create a board. Buckets default to shortlist/maybe/inbox/parked/dead.",
		InputSchema: obj(map[string]any{
			"name":        str("Display name, e.g. 'Side Projects'"),
			"description": str("What this board is for"),
			"buckets":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Ordered buckets, decided-most first"},
			"terminal":    str("The graveyard bucket; must be one of buckets. Defaults to 'dead'"),
			"intake":      str("Where new ideas land; must be one of buckets. Defaults to 'inbox'"),
		}, "name"),
	},
	{
		Name:        "update_board",
		Description: "Edit a board's name, description, buckets or terminal bucket. Omitted fields are left alone.",
		InputSchema: obj(map[string]any{
			"board":       str("Board slug or id"),
			"name":        str("New display name"),
			"description": str("New description"),
			"buckets":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"terminal":    str("New terminal bucket; must be one of buckets"),
			"intake":      str("New intake bucket; must be one of buckets"),
		}, "board"),
	},
	{
		Name: "add_ideas",
		Description: "Add one or many ideas to a board in a single atomic batch. This is the " +
			"main way an agent contributes. Ideas land in the board's inbox unless a bucket is given.",
		InputSchema: obj(map[string]any{
			"board": str("Board slug or id"),
			"ideas": map[string]any{
				"type": "array",
				"items": obj(map[string]any{
					"title":  str("Short statement of the idea"),
					"note":   str("One line saying what it is"),
					"body":   str("The full case for it"),
					"bucket": str("Target bucket; defaults to the board's inbox"),
				}, "title"),
			},
		}, "board", "ideas"),
	},
	{
		Name: "list_ideas",
		Description: "List a board's ideas, best-ranked first, with rating and uncertainty (rd). " +
			"Optionally filter by bucket or source ('me' or 'agent').",
		InputSchema: obj(map[string]any{
			"board":  str("Board slug or id"),
			"bucket": str("Only ideas in this bucket"),
			"source": str("'me' or 'agent'"),
		}, "board"),
	},
	{
		Name:        "get_idea",
		Description: "Get one idea in full: body, comment thread, rating, duel record.",
		InputSchema: obj(map[string]any{"id": str("Idea id")}, "id"),
	},
	{
		Name: "update_idea",
		Description: "Edit an idea's title, note, body or bucket. Omitted fields are left alone. " +
			"Ratings cannot be set — they are earned in duels by the board's owner.",
		InputSchema: obj(map[string]any{
			"id":     str("Idea id"),
			"title":  str("New title"),
			"note":   str("New one-line note"),
			"body":   str("New body"),
			"bucket": str("Move to this bucket; the terminal bucket buries it"),
		}, "id"),
	},
	{
		Name: "comment",
		Description: "Append a note to an idea's thread. This is where an agent argues its case — " +
			"the only channel it has for influencing a ranking.",
		InputSchema: obj(map[string]any{
			"id":   str("Idea id"),
			"text": str("The note"),
		}, "id", "text"),
	},
	{
		Name: "top",
		Description: "The short answer: the best k ideas on a board, with a 'settled' flag saying " +
			"whether more duels would change them. If settled is false, treat the order as provisional.",
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
		return jsonText(map[string]any{"boards": out})

	case "create_board":
		b := model.Board{
			Name:        argStr(a, "name"),
			Description: argStr(a, "description"),
			Buckets:     argStrs(a, "buckets"),
			Terminal:    argStr(a, "terminal"),
			Intake:      argStr(a, "intake"),
		}
		if err := st.CreateBoard(&b); err != nil {
			return "", err
		}
		return jsonText(b)

	case "update_board":
		b, err := st.UpdateBoard(argStr(a, "board"),
			argPtr(a, "name"), argPtr(a, "description"), argStrsPtr(a, "buckets"),
			argPtr(a, "terminal"), argPtr(a, "intake"))
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
		inbox := b.Intake
		batch := make([]model.Idea, 0, len(raw))
		for _, r := range raw {
			m, _ := r.(map[string]any)
			bucket := argStr(m, "bucket")
			if bucket == "" {
				bucket = inbox
			}
			batch = append(batch, model.Idea{
				Title: argStr(m, "title"), Note: argStr(m, "note"), Body: argStr(m, "body"),
				Bucket: bucket, Source: model.SourceAgent,
			})
		}
		out, err := st.AddIdeas(b.ID, batch)
		if err != nil {
			return "", err
		}
		return jsonText(map[string]any{"added": len(out), "ideas": out})

	case "list_ideas":
		b, err := st.Board(argStr(a, "board"))
		if err != nil {
			return "", err
		}
		ideas, err := st.ListIdeas(b.ID)
		if err != nil {
			return "", err
		}
		if bk := argStr(a, "bucket"); bk != "" {
			ideas = filter(ideas, func(i model.Idea) bool { return i.Bucket == bk })
		}
		if src := argStr(a, "source"); src != "" {
			ideas = filter(ideas, func(i model.Idea) bool { return string(i.Source) == src })
		}
		v, err := st.Verdict(b, TopK, bootstrapSamples)
		if err != nil {
			return "", err
		}
		return jsonText(map[string]any{"board": b.Slug, "buckets": b.Buckets, "verdict": v, "ideas": ideas})

	case "get_idea":
		it, err := st.Idea(argStr(a, "id"))
		if err != nil {
			return "", err
		}
		return jsonText(it)

	case "update_idea":
		it, err := st.UpdateIdea(argStr(a, "id"), store.IdeaPatch{
			Title: argPtr(a, "title"), Note: argPtr(a, "note"),
			Body: argPtr(a, "body"), Bucket: argPtr(a, "bucket"),
		})
		if err != nil {
			return "", err
		}
		return jsonText(it)

	case "comment":
		c, err := st.AddComment(argStr(a, "id"), model.SourceAgent, argStr(a, "text"))
		if err != nil {
			return "", err
		}
		return jsonText(c)

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
		live := store.Live(b, ideas)
		v, err := st.Verdict(b, k, bootstrapSamples)
		if err != nil {
			return "", err
		}
		if k > len(live) {
			k = len(live)
		}
		return jsonText(map[string]any{"board": b.Slug, "verdict": v, "top": live[:k]})
	}
	return "", fmt.Errorf("unknown tool: %s", name)
}

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

func argStrs(a map[string]any, k string) []string {
	raw, _ := a[k].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func argStrsPtr(a map[string]any, k string) *[]string {
	if _, ok := a[k]; !ok {
		return nil
	}
	v := argStrs(a, k)
	return &v
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
