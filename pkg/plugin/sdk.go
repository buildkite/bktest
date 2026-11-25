package plugin

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// Handler is the minimum interface every Go runner plugin implements.
type Handler interface {
	Configure(req ConfigureRequest) (ConfigureResponse, error)
	DiscoverTargets(req DiscoverTargetsRequest) (DiscoverTargetsResponse, error)
	PrepareAttempt(req PrepareAttemptRequest) (PrepareAttemptResponse, error)
}

// ExampleDiscoverer is optionally implemented by plugins that support
// example-level splitting or tag filtering.
type ExampleDiscoverer interface {
	DiscoverExamples(req DiscoverExamplesRequest) (DiscoverExamplesResponse, error)
}

// AttemptInterpreter is optionally implemented by plugins that parse test
// reports or interpret execution outcomes.
type AttemptInterpreter interface {
	InterpretAttempt(req InterpretAttemptRequest) (InterpretAttemptResponse, error)
}

// BatchReportParser is optionally implemented by plugins that support Test
// Scheduler pool batch report parsing.
type BatchReportParser interface {
	ParseBatchReport(req ParseBatchReportRequest) (ParseBatchReportResponse, error)
}

// Serve runs the plugin CLI entrypoint, reading a JSON request for os.Args[1]
// from stdin and writing the JSON response to stdout.
func Serve(handler Handler) {
	if err := Run(handler, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// Run executes a single plugin operation against the given streams.
func Run(handler Handler, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: <plugin> <operation>")
	}
	op := args[0]

	raw, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("read request: %w", err)
	}

	writeResp := func(v any) error {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		return enc.Encode(v)
	}

	writeErr := func(err error) error {
		_ = writeResp(struct {
			ProtocolVersion int    `json:"protocol_version"`
			Error           string `json:"error"`
		}{
			ProtocolVersion: ProtocolVersion,
			Error:           err.Error(),
		})
		return err
	}

	switch op {
	case OpConfigure:
		var req ConfigureRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return writeErr(fmt.Errorf("decode configure request: %w", err))
		}
		if err := chdirIfSet(req.WorkingDir); err != nil {
			return writeErr(err)
		}
		resp, err := handler.Configure(req)
		if err != nil {
			return writeErr(err)
		}
		resp.ProtocolVersion = ProtocolVersion
		return writeResp(resp)

	case OpDiscoverTargets:
		var req DiscoverTargetsRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return writeErr(fmt.Errorf("decode discover-targets request: %w", err))
		}
		if err := chdirIfSet(req.WorkingDir); err != nil {
			return writeErr(err)
		}
		resp, err := handler.DiscoverTargets(req)
		if err != nil {
			return writeErr(err)
		}
		resp.ProtocolVersion = ProtocolVersion
		return writeResp(resp)

	case OpDiscoverExamples:
		discoverer, ok := handler.(ExampleDiscoverer)
		if !ok {
			return writeErr(fmt.Errorf("operation %q is not supported by this plugin", op))
		}
		var req DiscoverExamplesRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return writeErr(fmt.Errorf("decode discover-examples request: %w", err))
		}
		if err := chdirIfSet(req.WorkingDir); err != nil {
			return writeErr(err)
		}
		resp, err := discoverer.DiscoverExamples(req)
		if err != nil {
			return writeErr(err)
		}
		resp.ProtocolVersion = ProtocolVersion
		return writeResp(resp)

	case OpPrepareAttempt:
		var req PrepareAttemptRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return writeErr(fmt.Errorf("decode prepare-attempt request: %w", err))
		}
		if err := chdirIfSet(req.WorkingDir); err != nil {
			return writeErr(err)
		}
		resp, err := handler.PrepareAttempt(req)
		if err != nil {
			return writeErr(err)
		}
		resp.ProtocolVersion = ProtocolVersion
		return writeResp(resp)

	case OpInterpretAttempt:
		interpreter, ok := handler.(AttemptInterpreter)
		if !ok {
			return writeResp(InterpretAttemptResponse{ProtocolVersion: ProtocolVersion})
		}
		var req InterpretAttemptRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return writeErr(fmt.Errorf("decode interpret-attempt request: %w", err))
		}
		if err := chdirIfSet(req.WorkingDir); err != nil {
			return writeErr(err)
		}
		resp, err := interpreter.InterpretAttempt(req)
		if err != nil {
			return writeErr(err)
		}
		resp.ProtocolVersion = ProtocolVersion
		return writeResp(resp)

	case OpParseBatchReport:
		parser, ok := handler.(BatchReportParser)
		if !ok {
			return writeErr(fmt.Errorf("operation %q is not supported by this plugin", op))
		}
		var req ParseBatchReportRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return writeErr(fmt.Errorf("decode parse-batch-report request: %w", err))
		}
		if err := chdirIfSet(req.WorkingDir); err != nil {
			return writeErr(err)
		}
		resp, err := parser.ParseBatchReport(req)
		if err != nil {
			return writeErr(err)
		}
		resp.ProtocolVersion = ProtocolVersion
		return writeResp(resp)

	default:
		_ = stderr
		return writeErr(fmt.Errorf("unknown plugin operation %q", op))
	}
}

func chdirIfSet(dir string) error {
	if dir == "" {
		return nil
	}
	return os.Chdir(dir)
}
