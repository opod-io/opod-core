package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/opod-io/opod/internal/batch"
)

// opod batch run — a generic OpenAI batch client (internal/batch). It reads an
// input file in OpenAI's batch line format, sends each line to an
// OpenAI-compatible server at bounded concurrency, and writes OpenAI's output
// and error files into a work directory that is also its progress record:
// run it again on the same directory and it resumes, never sending a line
// twice (a line in flight at a kill is written to the error file as
// "interrupted" instead).
//
// A stop is a cancel only when it was asked for (internal/batch, "Cancel, and
// a stop that is not one"): SIGINT at a terminal, or a SIGTERM while the batch
// object at --status-url says the batch is no longer in progress. Then nothing
// new is sent, what is in flight gets --drain, the partial results are
// uploaded and the process exits 0 — a cancelled batch is an outcome. Any
// other SIGTERM (an eviction, a node drain, a deleted pod) is an interruption:
// the same drain and upload, then exit 75 (batch.ExitInterrupted) when lines
// remain unsent, so whatever restarts the process — a Job — runs it again on
// the same --dir and it continues where it stopped, sending no line twice.
//
// The result files stay in --dir; --output-url and --error-url also PUT them,
// when the run ends, to URLs that carry their own authorization (an object
// store's presigned PUTs). --input-url reads the input the same way. Each of
// the three defaults to an environment variable, so a URL that is a credential
// need not be on the command line.
//
// stdout carries one JSON object per line: {"progress":{…}} every
// --progress-every, and a last {"batch":{…}} with the counts, the state
// (completed | cancelled | interrupted) and which result files were written.
func cmdBatch(args []string) {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		batchUsage()
		if len(args) == 0 {
			os.Exit(2)
		}
		return
	}
	switch args[0] {
	case "run":
		cmdBatchRun(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "%s unknown batch command %q\n", red("opod:"), args[0])
		batchUsage()
		os.Exit(2)
	}
}

func batchUsage() {
	fmt.Fprintln(os.Stderr, `usage: opod batch run (--input <file|url> | --input-url <presigned url>) --base-url <url> --dir <work dir> [options]

Send an OpenAI batch input (JSONL: custom_id, method, url, body) to an
OpenAI-compatible server and write output.jsonl + errors.jsonl in --dir.
Run it again on the same --dir to resume. SIGINT cancels; SIGTERM cancels only
when the batch object at --status-url is no longer in progress, and otherwise
stops the run with exit status 75 so it is resumed.`)
}

func cmdBatchRun(args []string) {
	fs := flag.NewFlagSet("batch run", flag.ExitOnError)
	input := fs.String("input", "", "the batch input: a JSONL path, or an http(s) URL read with the key (an OpenAI files content URL)")
	inputURL := fs.String("input-url", os.Getenv("OPOD_BATCH_INPUT_URL"), "the batch input as an http(s) URL that carries its own authorization (a presigned GET), read WITHOUT the key (default $OPOD_BATCH_INPUT_URL)")
	base := fs.String("base-url", os.Getenv("OPOD_BASE_URL"), "the OpenAI-compatible server, with or without /v1 (default $OPOD_BASE_URL)")
	dir := fs.String("dir", "", "work directory: the progress record and the result files (required)")
	conc := fs.Int("concurrency", 8, "requests in flight at once")
	models := fs.String("model-map", "", "rewrite a line's model: alias=id[,alias=id]; other models are sent as written")
	caFile := fs.String("ca", os.Getenv("OPOD_LEADER_CA"), "PEM certificate an https --base-url must be signed by (default $OPOD_LEADER_CA)")
	timeout := fs.Duration("timeout", 10*time.Minute, "per-request timeout")
	ready := fs.Duration("ready-timeout", 0, "wait up to this long for GET /v1/models to accept the key before the first line (0 = do not wait)")
	drain := fs.Duration("drain", 20*time.Second, "after a stop, how long in-flight requests may finish")
	status := fs.String("status-url", "", "an OpenAI batch object (…/v1/batches/<id>) read with the key on SIGTERM: the stop is a cancel only when its status is no longer validating or in_progress")
	outputURL := fs.String("output-url", os.Getenv("OPOD_BATCH_OUTPUT_URL"), "PUT output.jsonl here when the run ends: a URL that carries its own authorization, such as a presigned PUT (default $OPOD_BATCH_OUTPUT_URL)")
	errorURL := fs.String("error-url", os.Getenv("OPOD_BATCH_ERROR_URL"), "PUT errors.jsonl here when the run ends, as --output-url (default $OPOD_BATCH_ERROR_URL)")
	every := fs.Duration("progress-every", 5*time.Second, "how often a progress line is printed (0 = never)")
	fs.Usage = func() { batchUsage(); fs.PrintDefaults() }
	_ = fs.Parse(args)
	if (*input == "") == (*inputURL == "") || *dir == "" || *base == "" {
		if *input != "" && *inputURL != "" {
			fmt.Fprintln(os.Stderr, "error: give --input or --input-url, not both")
		}
		fs.Usage()
		os.Exit(2)
	}
	in, presigned := *input, false
	if *inputURL != "" {
		in, presigned = *inputURL, true
	}
	key := os.Getenv("OPOD_API_KEY")
	if key == "" {
		key = os.Getenv("OPENAI_API_KEY")
	}
	mm, err := parseModelMap(*models)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
	var ca []byte
	if *caFile != "" {
		if ca, err = os.ReadFile(*caFile); err != nil {
			fmt.Fprintln(os.Stderr, "error: --ca:", err)
			os.Exit(2)
		}
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	enc := json.NewEncoder(os.Stdout)
	ctx, judge, stop := batch.Signals(context.Background(), func(c context.Context) bool {
		return batch.CancelRequested(c, nil, *status, key)
	})
	defer stop()
	res, err := batch.Run(ctx, batch.Options{
		Input: in, InputPresigned: presigned, BaseURL: *base, Key: key, Dir: *dir, Concurrency: *conc, ModelMap: mm, CA: ca,
		RequestTimeout: *timeout, ReadyTimeout: *ready, Drain: *drain, Log: log, StopIsCancel: judge,
		Progress:      func(c batch.Counts) { _ = enc.Encode(map[string]any{"progress": c}) },
		ProgressEvery: *every,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	state := "completed"
	switch {
	case res.Resumable:
		state = "interrupted"
	case res.Cancelled:
		state = "cancelled"
	}
	out := map[string]any{"total": res.Total, "completed": res.Completed, "failed": res.Failed,
		"interrupted": res.Interrupted, "state": state, "output": res.OutputPath, "errors": res.ErrorPath}
	if *outputURL != "" || *errorURL != "" {
		// Writing the results is the run's last act and must outlive a stop: a
		// batch stopped at its deadline still hands back what it finished, and
		// an interrupted one that is never resumed is not left with nothing (a
		// later write replaces this one).
		upCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
		written, err := batch.PutResults(upCtx, nil, *outputURL, *errorURL, res)
		cancel()
		if err != nil {
			_ = enc.Encode(map[string]any{"batch": out})
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		out["written"] = written
	}
	_ = enc.Encode(map[string]any{"batch": out})
	stop()
	os.Exit(batch.ExitCode(res))
}

// parseModelMap reads "alias=id[,alias=id]".
func parseModelMap(s string) (map[string]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	m := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		from, to, ok := strings.Cut(strings.TrimSpace(pair), "=")
		from, to = strings.TrimSpace(from), strings.TrimSpace(to)
		if !ok || from == "" || to == "" {
			return nil, fmt.Errorf("--model-map %q: want alias=id[,alias=id]", s)
		}
		m[from] = to
	}
	return m, nil
}
