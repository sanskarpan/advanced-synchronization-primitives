package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sanskar/syncprimitives/internal/auth"
)

const (
	defaultServerURL = "ws://localhost:8085/ws"
	defaultTimeout   = 30 * time.Second
	version          = "dev"
)

type config struct {
	server   string
	apiKey   string
	asJSON   bool
	timeout  time.Duration
	insecure bool
}

type wsMessage struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type primitiveInfo struct {
	ID           string                 `json:"ID"`
	Type         string                 `json:"Type"`
	Name         string                 `json:"Name"`
	CreatedAt    time.Time              `json:"CreatedAt"`
	BlockedCount int32                  `json:"BlockedCount"`
	Stats        map[string]interface{} `json:"Stats"`
}

type statePayload struct {
	Primitives map[string]primitiveInfo `json:"primitives"`
}

type commandError struct {
	Code int
	Msg  string
}

func (e *commandError) Error() string { return e.Msg }

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	code, err := runE(args, getenv, stdout)
	if err == nil {
		return code
	}
	var ce *commandError
	if errors.As(err, &ce) {
		fmt.Fprintln(stderr, ce.Msg)
		return ce.Code
	}
	fmt.Fprintln(stderr, err.Error())
	return 1
}

func runE(args []string, getenv func(string) string, stdout io.Writer) (int, error) {
	cfg, cmd, cmdArgs, err := parseArgs(args, getenv)
	if err != nil {
		return 2, &commandError{Code: 2, Msg: err.Error()}
	}

	switch cmd {
	case "help", "":
		printUsage(stdout)
		return 0, nil
	case "version":
		fmt.Fprintf(stdout, "syncctl %s\n", version)
		return 0, nil
	case "list":
		return runList(cfg, stdout)
	case "create":
		return runCreate(cfg, cmdArgs, stdout)
	case "op":
		return runOp(cfg, cmdArgs, stdout)
	case "delete":
		return runDelete(cfg, cmdArgs, stdout)
	case "stats":
		return runStats(cfg, cmdArgs, stdout)
	case "token":
		return runToken(cmdArgs, stdout)
	default:
		return 2, &commandError{Code: 2, Msg: fmt.Sprintf("unknown command: %s", cmd)}
	}
}

func parseArgs(args []string, getenv func(string) string) (config, string, []string, error) {
	cfg := config{
		server:  getenv("SYNCPRIM_SERVER"),
		apiKey:  getenv("SYNCPRIM_API_KEY"),
		timeout: defaultTimeout,
	}
	if cfg.server == "" {
		cfg.server = defaultServerURL
	}

	globalArgs, command, commandArgs, err := splitGlobalArgs(args)
	if err != nil {
		return cfg, "", nil, err
	}
	fs := flag.NewFlagSet("syncctl", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.server, "server", cfg.server, "WebSocket server URL")
	fs.StringVar(&cfg.apiKey, "api-key", cfg.apiKey, "Bearer API key")
	fs.BoolVar(&cfg.asJSON, "json", false, "Emit JSON output")
	fs.DurationVar(&cfg.timeout, "timeout", cfg.timeout, "Operation timeout")
	fs.BoolVar(&cfg.insecure, "insecure", false, "Allow insecure TLS certificates")

	if err := fs.Parse(globalArgs); err != nil {
		return cfg, "", nil, err
	}
	if cfg.timeout <= 0 {
		return cfg, "", nil, errors.New("--timeout must be positive")
	}
	return cfg, command, commandArgs, nil
}

func splitGlobalArgs(args []string) ([]string, string, []string, error) {
	globalArgs := make([]string, 0, len(args))
	i := 0
	for i < len(args) {
		a := args[i]
		if a == "--" {
			if i+1 >= len(args) {
				return nil, "", nil, errors.New("missing command after --")
			}
			return globalArgs, args[i+1], args[i+2:], nil
		}
		if !strings.HasPrefix(a, "-") {
			return globalArgs, a, args[i+1:], nil
		}
		globalArgs = append(globalArgs, a)
		if hasInlineValue(a) || !flagNeedsValue(a) {
			i++
			continue
		}
		if i+1 >= len(args) {
			return nil, "", nil, fmt.Errorf("flag requires value: %s", a)
		}
		globalArgs = append(globalArgs, args[i+1])
		i += 2
	}
	return globalArgs, "help", nil, nil
}

func hasInlineValue(arg string) bool {
	return strings.Contains(arg, "=")
}

func flagNeedsValue(arg string) bool {
	name := arg
	if idx := strings.IndexByte(name, '='); idx >= 0 {
		name = name[:idx]
	}
	switch name {
	case "--server", "--api-key", "--timeout":
		return true
	default:
		return false
	}
}

func runList(cfg config, stdout io.Writer) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()

	conn, err := dial(ctx, cfg)
	if err != nil {
		return 1, &commandError{Code: 1, Msg: err.Error()}
	}
	defer closeConn(conn)

	prims, err := readPrimitivesState(ctx, conn)
	if err != nil {
		return 1, &commandError{Code: 1, Msg: err.Error()}
	}

	if cfg.asJSON {
		out := make([]map[string]interface{}, 0, len(prims))
		for _, p := range sortPrimitives(prims) {
			out = append(out, map[string]interface{}{
				"id":    p.ID,
				"type":  p.Type,
				"name":  p.Name,
				"state": summarizeState(p),
			})
		}
		return 0, writeJSON(stdout, out)
	}

	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTYPE\tNAME\tSTATE")
	for _, p := range sortPrimitives(prims) {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", p.ID, p.Type, p.Name, summarizeState(p))
	}
	_ = tw.Flush()
	return 0, nil
}

func runCreate(cfg config, args []string, stdout io.Writer) (int, error) {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	name := fs.String("name", "", "Primitive name")
	capacity := fs.Int("capacity", 0, "Semaphore capacity")
	parties := fs.Int("parties", 0, "Barrier parties")
	if err := fs.Parse(args); err != nil {
		return 2, &commandError{Code: 2, Msg: err.Error()}
	}
	pos := fs.Args()
	if len(pos) < 2 {
		return 2, &commandError{Code: 2, Msg: "usage: syncctl create <type> <id> [--name <name>] [--capacity N] [--parties N]"}
	}

	ptype := strings.ToLower(pos[0])
	id := pos[1]
	primName := *name
	if primName == "" {
		primName = id
	}

	msgType, payload, err := buildCreateMessage(ptype, id, primName, *capacity, *parties)
	if err != nil {
		return 2, &commandError{Code: 2, Msg: err.Error()}
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()
	conn, err := dial(ctx, cfg)
	if err != nil {
		return 1, &commandError{Code: 1, Msg: err.Error()}
	}
	defer closeConn(conn)

	if _, err := readPrimitivesState(ctx, conn); err != nil {
		return 1, &commandError{Code: 1, Msg: err.Error()}
	}
	if err := sendMessage(conn, msgType, payload); err != nil {
		return 1, &commandError{Code: 1, Msg: err.Error()}
	}
	respType, msg, err := waitForAck(ctx, conn)
	if err != nil {
		return 1, &commandError{Code: 1, Msg: err.Error()}
	}
	if respType == "error" {
		return 1, &commandError{Code: 1, Msg: msg}
	}

	if cfg.asJSON {
		return 0, writeJSON(stdout, map[string]interface{}{
			"result":  "ok",
			"id":      id,
			"type":    canonicalTypeName(ptype),
			"message": msg,
		})
	}
	fmt.Fprintln(stdout, msg)
	return 0, nil
}

func runOp(cfg config, args []string, stdout io.Writer) (int, error) {
	fs := flag.NewFlagSet("op", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	hold := fs.Int("hold", 0, "hold duration in milliseconds")
	holdMS := fs.Int("hold-ms", 0, "hold duration in milliseconds")
	delta := fs.Int("delta", 0, "waitgroup add delta (maps to holdMs)")
	if err := fs.Parse(args); err != nil {
		return 2, &commandError{Code: 2, Msg: err.Error()}
	}
	pos := fs.Args()
	if len(pos) < 2 {
		return 2, &commandError{Code: 2, Msg: "usage: syncctl op <id> <operation> [--hold <ms>] [--delta N]"}
	}
	id := pos[0]
	op := pos[1]

	finalHold := *hold
	if *holdMS != 0 {
		finalHold = *holdMS
	}
	if op == "add" && *delta != 0 {
		finalHold = *delta
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()
	conn, err := dial(ctx, cfg)
	if err != nil {
		return 1, &commandError{Code: 1, Msg: err.Error()}
	}
	defer closeConn(conn)

	if _, err := readPrimitivesState(ctx, conn); err != nil {
		return 1, &commandError{Code: 1, Msg: err.Error()}
	}
	if err := sendMessage(conn, "primitiveOp", map[string]interface{}{"id": id, "op": op, "holdMs": finalHold}); err != nil {
		return 1, &commandError{Code: 1, Msg: err.Error()}
	}
	respType, msg, err := waitForAck(ctx, conn)
	if err != nil {
		return 1, &commandError{Code: 1, Msg: err.Error()}
	}
	if respType == "error" {
		return 1, &commandError{Code: 1, Msg: msg}
	}

	if cfg.asJSON {
		return 0, writeJSON(stdout, map[string]interface{}{
			"result":    "ok",
			"id":        id,
			"operation": op,
			"message":   msg,
		})
	}
	fmt.Fprintln(stdout, msg)
	return 0, nil
}

func runDelete(cfg config, args []string, stdout io.Writer) (int, error) {
	if len(args) != 1 {
		return 2, &commandError{Code: 2, Msg: "usage: syncctl delete <id>"}
	}
	id := args[0]

	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()
	conn, err := dial(ctx, cfg)
	if err != nil {
		return 1, &commandError{Code: 1, Msg: err.Error()}
	}
	defer closeConn(conn)

	if _, err := readPrimitivesState(ctx, conn); err != nil {
		return 1, &commandError{Code: 1, Msg: err.Error()}
	}
	if err := sendMessage(conn, "deletePrimitive", map[string]interface{}{"id": id}); err != nil {
		return 1, &commandError{Code: 1, Msg: err.Error()}
	}
	respType, msg, err := waitForAck(ctx, conn)
	if err != nil {
		return 1, &commandError{Code: 1, Msg: err.Error()}
	}
	if respType == "error" {
		return 1, &commandError{Code: 1, Msg: msg}
	}

	if cfg.asJSON {
		return 0, writeJSON(stdout, map[string]interface{}{
			"result":  "ok",
			"id":      id,
			"message": msg,
		})
	}
	fmt.Fprintln(stdout, msg)
	return 0, nil
}

func runStats(cfg config, args []string, stdout io.Writer) (int, error) {
	if len(args) != 1 {
		return 2, &commandError{Code: 2, Msg: "usage: syncctl stats <id>"}
	}
	id := args[0]

	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()
	conn, err := dial(ctx, cfg)
	if err != nil {
		return 1, &commandError{Code: 1, Msg: err.Error()}
	}
	defer closeConn(conn)

	prims, err := readPrimitivesState(ctx, conn)
	if err != nil {
		return 1, &commandError{Code: 1, Msg: err.Error()}
	}

	p, ok := prims[id]
	if !ok {
		return 1, &commandError{Code: 1, Msg: fmt.Sprintf("primitive not found: %s", id)}
	}

	if cfg.asJSON {
		return 0, writeJSON(stdout, p)
	}

	fmt.Fprintf(stdout, "Primitive: %s\n", p.ID)
	fmt.Fprintf(stdout, "Type:      %s\n", p.Type)
	fmt.Fprintf(stdout, "Name:      %s\n", p.Name)
	fmt.Fprintf(stdout, "State:     %s\n", summarizeState(p))
	if !p.CreatedAt.IsZero() {
		fmt.Fprintf(stdout, "Created:   %s\n", p.CreatedAt.Format(time.RFC3339))
	}
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Stats:")
	keys := make([]string, 0, len(p.Stats))
	for k := range p.Stats {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(stdout, "  %s: %v\n", k, p.Stats[k])
	}
	return 0, nil
}

func runToken(args []string, stdout io.Writer) (int, error) {
	if len(args) == 0 || args[0] != "generate" {
		return 2, &commandError{Code: 2, Msg: "usage: syncctl token generate --secret <secret> --sub <subject> [--role <role>] [--ttl <dur>]"}
	}

	fs := flag.NewFlagSet("token generate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	secret := fs.String("secret", "", "HS256 signing secret")
	sub := fs.String("sub", "", "JWT subject")
	role := fs.String("role", "", "JWT role claim")
	ttl := fs.Duration("ttl", time.Hour, "Token lifetime")
	if err := fs.Parse(args[1:]); err != nil {
		return 2, &commandError{Code: 2, Msg: err.Error()}
	}
	if *secret == "" {
		return 2, &commandError{Code: 2, Msg: "--secret is required"}
	}
	if *sub == "" {
		return 2, &commandError{Code: 2, Msg: "--sub is required"}
	}
	if *ttl <= 0 {
		return 2, &commandError{Code: 2, Msg: "--ttl must be positive"}
	}

	now := time.Now()
	token, err := auth.GenerateJWT(auth.Claims{
		Sub:  *sub,
		Role: *role,
		Iat:  now.Unix(),
		Exp:  now.Add(*ttl).Unix(),
	}, *secret)
	if err != nil {
		return 1, &commandError{Code: 1, Msg: err.Error()}
	}
	fmt.Fprintln(stdout, token)
	return 0, nil
}

func dial(ctx context.Context, cfg config) (*websocket.Conn, error) {
	dialer := websocket.Dialer{}
	if cfg.timeout > 0 {
		dialer.HandshakeTimeout = cfg.timeout
	}
	if cfg.insecure {
		// #nosec G402 -- this is only enabled by the explicit --insecure CLI flag.
		dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	headers := http.Header{}
	if cfg.apiKey != "" {
		headers.Set("Authorization", "Bearer "+cfg.apiKey)
	}

	conn, resp, err := dialer.DialContext(ctx, cfg.server, headers)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("syncctl: failed to connect to %s: %v (http %d)", cfg.server, err, resp.StatusCode)
		}
		return nil, fmt.Errorf("syncctl: failed to connect to %s: %v", cfg.server, err)
	}
	return conn, nil
}

func readPrimitivesState(ctx context.Context, conn *websocket.Conn) (map[string]primitiveInfo, error) {
	for {
		msg, err := readMessage(ctx, conn)
		if err != nil {
			return nil, err
		}
		if msg.Type != "initialState" && msg.Type != "state" {
			continue
		}

		var state statePayload
		if err := json.Unmarshal(msg.Payload, &state); err != nil {
			return nil, fmt.Errorf("syncctl: invalid state payload: %w", err)
		}
		if state.Primitives == nil {
			state.Primitives = make(map[string]primitiveInfo)
		}
		for id, p := range state.Primitives {
			if p.ID == "" {
				p.ID = id
			}
			state.Primitives[id] = p
		}
		return state.Primitives, nil
	}
}

func waitForAck(ctx context.Context, conn *websocket.Conn) (string, string, error) {
	for {
		msg, err := readMessage(ctx, conn)
		if err != nil {
			return "", "", err
		}
		if msg.Type != "success" && msg.Type != "error" {
			continue
		}
		var payload map[string]interface{}
		if err := json.Unmarshal(msg.Payload, &payload); err != nil {
			return "", "", fmt.Errorf("syncctl: invalid response payload: %w", err)
		}
		text, _ := payload["message"].(string)
		if text == "" {
			text = msg.Type
		}
		return msg.Type, text, nil
	}
}

func readMessage(ctx context.Context, conn *websocket.Conn) (wsMessage, error) {
	deadline := time.Now().Add(defaultTimeout)
	if d, ok := ctx.Deadline(); ok {
		deadline = d
	}
	if err := conn.SetReadDeadline(deadline); err != nil {
		return wsMessage{}, fmt.Errorf("syncctl: set read deadline: %w", err)
	}
	var msg wsMessage
	if err := conn.ReadJSON(&msg); err != nil {
		if ctx.Err() != nil {
			return wsMessage{}, fmt.Errorf("syncctl: operation timed out")
		}
		return wsMessage{}, fmt.Errorf("syncctl: read failed: %w", err)
	}
	return msg, nil
}

func sendMessage(conn *websocket.Conn, msgType string, payload interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("syncctl: marshal payload: %w", err)
	}
	msg := wsMessage{Type: msgType, Payload: data}
	if err := conn.WriteJSON(msg); err != nil {
		return fmt.Errorf("syncctl: send %s failed: %w", msgType, err)
	}
	return nil
}

func buildCreateMessage(ptype, id, name string, capacity, parties int) (string, interface{}, error) {
	base := map[string]interface{}{"id": id, "name": name}
	switch ptype {
	case "rwlock":
		return "createRWLock", base, nil
	case "semaphore":
		if capacity <= 0 {
			return "", nil, errors.New("semaphore requires --capacity > 0")
		}
		base["capacity"] = capacity
		return "createSemaphore", base, nil
	case "mutex":
		return "createMutex", base, nil
	case "condvar":
		return "createCondVar", base, nil
	case "barrier":
		if parties <= 0 {
			return "", nil, errors.New("barrier requires --parties > 0")
		}
		base["parties"] = parties
		return "createBarrier", base, nil
	case "waitgroup":
		return "createWaitGroup", base, nil
	case "once":
		return "createOnce", base, nil
	case "singleflight":
		return "createSingleflight", base, nil
	default:
		return "", nil, fmt.Errorf("unsupported primitive type: %s", ptype)
	}
}

func canonicalTypeName(ptype string) string {
	switch strings.ToLower(ptype) {
	case "rwlock":
		return "RWLock"
	case "semaphore":
		return "Semaphore"
	case "mutex":
		return "Mutex"
	case "condvar":
		return "CondVar"
	case "barrier":
		return "Barrier"
	case "waitgroup":
		return "WaitGroup"
	case "once":
		return "Once"
	case "singleflight":
		return "Singleflight"
	default:
		return ptype
	}
}

func summarizeState(p primitiveInfo) string {
	s := p.Stats
	switch p.Type {
	case "Mutex":
		if b, ok := s["IsLocked"].(bool); ok && b {
			return "locked"
		}
		return "unlocked"
	case "RWLock":
		writerHeld, _ := s["WriterHeld"].(bool)
		readers := asInt64(s["CurrentReaders"])
		if writerHeld {
			return "writer-held"
		}
		if readers > 0 {
			return fmt.Sprintf("%d readers", readers)
		}
		return "idle"
	case "Semaphore":
		cur := asInt64(s["CurrentCount"])
		cap := asInt64(s["Capacity"])
		if cap > 0 {
			return fmt.Sprintf("%d/%d available", cur, cap)
		}
		return "active"
	case "Barrier":
		arrived := asInt64(s["Arrived"])
		parties := asInt64(s["Parties"])
		if parties > 0 {
			return fmt.Sprintf("%d/%d arrived", arrived, parties)
		}
		return "active"
	case "WaitGroup":
		count := asInt64(s["Count"])
		return fmt.Sprintf("count=%d", count)
	case "Once":
		done, _ := s["Done"].(bool)
		if done {
			return "done"
		}
		return "pending"
	default:
		return "active"
	}
}

func asInt64(v interface{}) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case float32:
		return int64(n)
	case int:
		return int64(n)
	case int32:
		return int64(n)
	case int64:
		return n
	case json.Number:
		i, _ := n.Int64()
		return i
	default:
		return 0
	}
}

func sortPrimitives(prims map[string]primitiveInfo) []primitiveInfo {
	list := make([]primitiveInfo, 0, len(prims))
	for id, p := range prims {
		if p.ID == "" {
			p.ID = id
		}
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return list
}

func writeJSON(w io.Writer, v interface{}) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func closeConn(conn *websocket.Conn) {
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(250*time.Millisecond))
	_ = conn.Close()
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "syncctl [--server <url>] [--api-key <key>] [--json] [--timeout <dur>] [--insecure] <command>")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  list                          List primitives")
	fmt.Fprintln(w, "  create <type> <id>            Create primitive")
	fmt.Fprintln(w, "  op <id> <operation>           Execute primitive operation")
	fmt.Fprintln(w, "  delete <id>                   Delete primitive")
	fmt.Fprintln(w, "  stats <id>                    Show primitive statistics")
	fmt.Fprintln(w, "  token generate                Generate an HS256 JWT")
	fmt.Fprintln(w, "  version                       Print syncctl version")
	fmt.Fprintln(w, "  help                          Show help")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Environment:")
	fmt.Fprintln(w, "  SYNCPRIM_SERVER               Default server URL")
	fmt.Fprintln(w, "  SYNCPRIM_API_KEY              Bearer API key")
}
