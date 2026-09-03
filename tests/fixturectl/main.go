// Command fixturectl is a test-only tool that seeds deterministic Activity
// rows (request_logs + request_attempts) directly into a Tiller router SQLite
// database. It exists so browser tests can exercise Activity UI at volume
// (pagination, search, detail views) without making hundreds of real proxy
// calls. It is not shipped in the router image and must never be reachable
// from a production deployment.
//
// Usage:
//
//	fixturectl activity --db /path/tiller-router.db --client <id> --rows 55
//	fixturectl activity --db ... --client <id> --rows 100 --fail-every 10 --fallback-every 7
//
// All rows for one invocation are written in a single transaction and carry
// deterministic ids/attempts so results are reproducible. Row ids are unique
// per client (the client id is folded into the id prefix), so seeds for
// different clients never collide.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/tiller-router/tiller-router/internal/database"
)

const (
	requestLogInsert = `INSERT INTO request_logs(id,client_key_id,requested_model,exposed_model,route_kind,route_model_id,route_model,resolved_provider,resolved_model,protocol,streaming,http_status,latency_ms,input_tokens,output_tokens,cache_read_input_tokens,cache_creation_input_tokens,provider_request_id,client_request_id,error_text,attempt_count,fallback_used,fallback_reason,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
	attemptInsert    = `INSERT INTO request_attempts(id,request_log_id,attempt_number,provider,model,result,http_status,failure_class,latency_ms,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: fixturectl activity --db <path> --client <id> [options]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "activity":
		if err := runActivity(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "fixturectl: "+err.Error())
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "fixturectl: unknown subcommand %q (expected activity)\n", os.Args[1])
		os.Exit(2)
	}
}

func runActivity(args []string) error {
	fs := flag.NewFlagSet("activity", flag.ExitOnError)
	dbPath := fs.String("db", "", "path to the router SQLite database (required)")
	clientID := fs.String("client", "", "client key id the rows are attributed to (required)")
	rows := fs.Int("rows", 1, "number of request_logs rows to seed")
	failEvery := fs.Int("fail-every", 0, "every Nth row is a failed request (0 disables)")
	fallbackEvery := fs.Int("fallback-every", 0, "every Nth row is an ordered-fallback request (0 disables)")
	prefix := fs.String("prefix", "activity", "row id prefix")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dbPath == "" || *clientID == "" {
		return errors.New("--db and --client are required")
	}
	if *rows < 0 {
		return errors.New("--rows must be >= 0")
	}
	if *rows == 0 {
		fmt.Println("seeded 0 rows")
		return nil
	}

	ctx := context.Background()
	db, err := database.Open(ctx, *dbPath)
	if err != nil {
		return fmt.Errorf("open database %s: %w", *dbPath, err)
	}
	defer db.Close()

	var enabled int
	if err := db.SQL.QueryRowContext(ctx, `SELECT logging_enabled FROM client_keys WHERE id=?`, *clientID).Scan(&enabled); err != nil {
		return fmt.Errorf("client %q not found or not readable: %w", *clientID, err)
	}
	if enabled == 0 {
		return fmt.Errorf("client %q has logging disabled; activity rows would be meaningless", *clientID)
	}

	short := *clientID
	if len(short) > 8 {
		short = short[:8]
	}
	base := time.Now().UTC()

	tx, err := db.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after successful Commit

	type seedAttempt struct {
		provider, model, result, failureClass string
		httpStatus                            int
	}
	for i := 0; i < *rows; i++ {
		seq := i + 1
		id := fmt.Sprintf("%s-%s-%03d", *prefix, short, seq)
		createdAt := base.Add(-time.Duration(*rows-seq) * time.Second).Add(time.Duration(seq) * time.Millisecond).Format(time.RFC3339Nano)
		latency := int64(20 + (seq % 40))

		var (
			httpStatus     int
			errorText      any
			provider, m    *string
			input, output  *int64
			fallbackUsed   bool
			fallbackReason any
			attempts       []seedAttempt
		)
		p := func(s string) *string { return &s }
		fallback := *fallbackEvery > 0 && i%*fallbackEvery == *fallbackEvery-1
		fail := !fallback && *failEvery > 0 && i%*failEvery == *failEvery-1
		switch {
		case fallback:
			httpStatus = 200
			fallbackUsed = true
			fallbackReason = "http_500"
			provider = p("fixture-provider")
			m = p("mock-model-b")
			in := int64(10)
			out := int64(5)
			input, output = &in, &out
			attempts = []seedAttempt{
				{provider: "fixture-provider", model: "mock-model-a", result: "failed", failureClass: "http_500", httpStatus: 500},
				{provider: "fixture-provider", model: "mock-model-b", result: "success", httpStatus: 200},
			}
		case fail:
			httpStatus = 500
			errorText = "upstream_error"
			attempts = []seedAttempt{
				{provider: "fixture-provider", model: "mock-model-a", result: "failed", failureClass: "http_500", httpStatus: 500},
			}
		default:
			httpStatus = 200
			provider = p("fixture-provider")
			m = p("mock-model-a")
			in := int64(10)
			out := int64(5)
			input, output = &in, &out
			attempts = []seedAttempt{
				{provider: "fixture-provider", model: "mock-model-a", result: "success", httpStatus: 200},
			}
		}

		requested := "fixture-provider/mock-model-a"
		if _, err := tx.ExecContext(ctx, requestLogInsert,
			id, *clientID, requested, nil, "real", nil, nil, provider, m, "chat", 0, httpStatus, latency,
			input, output, nil, nil, nil, id, errorText, len(attempts), boolInt(fallbackUsed), fallbackReason, createdAt,
		); err != nil {
			return fmt.Errorf("insert request_logs row %d: %w", seq, err)
		}
		for ai, a := range attempts {
			if _, err := tx.ExecContext(ctx, attemptInsert,
				fmt.Sprintf("%s-a%d", id, ai+1), id, ai+1, a.provider, a.model, a.result,
				nullInt(a.httpStatus), nullString(a.failureClass), latency, createdAt,
			); err != nil {
				return fmt.Errorf("insert request_attempts row %d.%d: %w", seq, ai+1, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit seed: %w", err)
	}
	fmt.Printf("seeded %d activity rows for client %s\n", *rows, *clientID)
	return nil
}

func nullInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}
func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
