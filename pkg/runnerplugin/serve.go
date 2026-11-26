package runnerplugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// Handler is a runner plugin. Every plugin implements these two operations;
// the others are optional interfaces.
type Handler interface {
	// Configure applies the plugin's defaults to the configuration and
	// describes the plugin. An error rejects the configuration.
	Configure(ConfigureRequest) (ConfigureResponse, error)
	// PrepareAttempt returns the command that runs the test cases.
	PrepareAttempt(PrepareAttemptRequest) (PrepareAttemptResponse, error)
}

// TargetDiscoverer is implemented by plugins that find their own targets,
// such as Go packages, instead of files that match the file patterns.
type TargetDiscoverer interface {
	DiscoverTargets(DiscoverTargetsRequest) (DiscoverTargetsResponse, error)
}

// ExampleDiscoverer is implemented by plugins that support split by example.
type ExampleDiscoverer interface {
	DiscoverExamples(DiscoverExamplesRequest) (DiscoverExamplesResponse, error)
}

// AttemptInterpreter is implemented by plugins that read a report. Without
// it, only the command's exit status counts and failed tests aren't retried.
type AttemptInterpreter interface {
	InterpretAttempt(InterpretAttemptRequest) (InterpretAttemptResponse, error)
}

// BatchReportParser is implemented by plugins that support Test Scheduler
// pools.
type BatchReportParser interface {
	ParseBatchReport(ParseBatchReportRequest) (ParseBatchReportResponse, error)
}

// Operations returns the optional operations h implements.
func Operations(h Handler) []string {
	var ops []string
	if _, ok := h.(TargetDiscoverer); ok {
		ops = append(ops, OpDiscoverTargets)
	}
	if _, ok := h.(ExampleDiscoverer); ok {
		ops = append(ops, OpDiscoverExamples)
	}
	if _, ok := h.(AttemptInterpreter); ok {
		ops = append(ops, OpInterpretAttempt)
	}
	if _, ok := h.(BatchReportParser); ok {
		ops = append(ops, OpParseBatchReport)
	}
	return ops
}

// ErrorResponse is what an external plugin writes to stdout, with a non-zero
// exit status, when an operation fails. bktest shows the message.
type ErrorResponse struct {
	Error string `json:"error"`
}

// Serve runs the operation named by the program's first argument: it reads
// the request from stdin and writes the response to stdout. A plugin's main
// function calls it with a new handler:
//
//	func main() { runnerplugin.Serve(rspec.New()) }
//
// stdout carries only the response. Anything else the handler runs, such as a
// dry run, must write elsewhere; logs go to stderr.
func Serve(h Handler) {
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "usage: %s <operation>\n\nThis is a bktest runner plugin. Operations read a JSON request from stdin and write a JSON response to stdout.\n", os.Args[0])
		os.Exit(2)
	}
	if err := ServeOperation(h, os.Args[1], os.Stdin, os.Stdout); err != nil {
		os.Exit(1)
	}
}

// ServeOperation runs one operation of h, reading its request from r and
// writing its response to w. When the operation fails it writes an
// ErrorResponse and returns the error.
func ServeOperation(h Handler, operation string, r io.Reader, w io.Writer) error {
	response, err := call(h, operation, r)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	if err != nil {
		_ = encoder.Encode(ErrorResponse{Error: err.Error()})
		return err
	}
	return encoder.Encode(response)
}

func call(h Handler, operation string, r io.Reader) (any, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("reading the %s request: %w", operation, err)
	}
	decode := func(request any) error {
		if err := json.Unmarshal(data, request); err != nil {
			return fmt.Errorf("invalid %s request: %w", operation, err)
		}
		return nil
	}
	unsupported := fmt.Errorf("this plugin does not implement %s", operation)

	switch operation {
	case OpConfigure:
		var request ConfigureRequest
		if err := decode(&request); err != nil {
			return nil, err
		}
		response, err := h.Configure(request)
		response.ProtocolVersion = ProtocolVersion
		response.Operations = Operations(h)
		return response, err
	case OpPrepareAttempt:
		var request PrepareAttemptRequest
		if err := decode(&request); err != nil {
			return nil, err
		}
		response, err := h.PrepareAttempt(request)
		response.ProtocolVersion = ProtocolVersion
		return response, err
	case OpDiscoverTargets:
		discoverer, ok := h.(TargetDiscoverer)
		if !ok {
			return nil, unsupported
		}
		var request DiscoverTargetsRequest
		if err := decode(&request); err != nil {
			return nil, err
		}
		response, err := discoverer.DiscoverTargets(request)
		response.ProtocolVersion = ProtocolVersion
		return response, err
	case OpDiscoverExamples:
		discoverer, ok := h.(ExampleDiscoverer)
		if !ok {
			return nil, unsupported
		}
		var request DiscoverExamplesRequest
		if err := decode(&request); err != nil {
			return nil, err
		}
		response, err := discoverer.DiscoverExamples(request)
		response.ProtocolVersion = ProtocolVersion
		return response, err
	case OpInterpretAttempt:
		interpreter, ok := h.(AttemptInterpreter)
		if !ok {
			return nil, unsupported
		}
		var request InterpretAttemptRequest
		if err := decode(&request); err != nil {
			return nil, err
		}
		response, err := interpreter.InterpretAttempt(request)
		response.ProtocolVersion = ProtocolVersion
		return response, err
	case OpParseBatchReport:
		parser, ok := h.(BatchReportParser)
		if !ok {
			return nil, unsupported
		}
		var request ParseBatchReportRequest
		if err := decode(&request); err != nil {
			return nil, err
		}
		response, err := parser.ParseBatchReport(request)
		response.ProtocolVersion = ProtocolVersion
		return response, err
	default:
		return nil, errors.New("unknown operation " + operation)
	}
}
