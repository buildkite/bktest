package runnerplugin

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// Request is a JSON-RPC 2.0 request, one per line on the plugin's stdin.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response, one per line on the plugin's stdout.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is a JSON-RPC 2.0 error.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// JSON-RPC error codes.
const (
	CodeParseError     = -32700
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	// CodePluginError is a failure reported by the plugin, such as a dry run
	// that failed. bktest shows its message.
	CodePluginError = -32000
)

// Plugin is implemented by Go runner plugins. Serve calls one method per
// request. A plugin that doesn't support example discovery returns an error
// from Examples; bktest only calls it when Features.SplitByExample is set.
type Plugin interface {
	Initialize(InitializeParams) (InitializeResult, error)
	Discover() (DiscoverResult, error)
	Examples(ExamplesParams) (ExamplesResult, error)
	Command(CommandParams) (CommandResult, error)
	Interpret(InterpretParams) (InterpretResult, error)
}

// Serve runs the plugin on stdin and stdout until bktest closes stdin. Child
// processes the plugin starts, such as dry runs, must not write to stdout:
// send their output to stderr.
func Serve(p Plugin) {
	if os.Getenv(ProtocolEnv) == "" {
		fmt.Fprintf(os.Stderr, "This is a bktest runner plugin. Run it with `bktest run --plugin <directory>`, not directly.\n")
		os.Exit(1)
	}
	if err := ServeIO(p, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "runner plugin: %v\n", err)
		os.Exit(1)
	}
}

// ServeIO serves requests from r and writes responses to w until r ends.
func ServeIO(p Plugin, r io.Reader, w io.Writer) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	encoder := json.NewEncoder(w)
	for scanner.Scan() {
		var req Request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			if err := encoder.Encode(Response{JSONRPC: "2.0", Error: &Error{Code: CodeParseError, Message: err.Error()}}); err != nil {
				return err
			}
			continue
		}
		if err := encoder.Encode(dispatch(p, req)); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func dispatch(p Plugin, req Request) Response {
	var (
		result any
		err    error
	)
	switch req.Method {
	case MethodInitialize:
		var params InitializeParams
		if err = decodeParams(req.Params, &params); err == nil {
			result, err = p.Initialize(params)
		}
	case MethodDiscover:
		result, err = p.Discover()
	case MethodExamples:
		var params ExamplesParams
		if err = decodeParams(req.Params, &params); err == nil {
			result, err = p.Examples(params)
		}
	case MethodCommand:
		var params CommandParams
		if err = decodeParams(req.Params, &params); err == nil {
			result, err = p.Command(params)
		}
	case MethodInterpret:
		var params InterpretParams
		if err = decodeParams(req.Params, &params); err == nil {
			result, err = p.Interpret(params)
		}
	default:
		err = &Error{Code: CodeMethodNotFound, Message: fmt.Sprintf("method %q not found", req.Method)}
	}

	resp := Response{JSONRPC: "2.0", ID: req.ID}
	if err != nil {
		rpcErr, ok := err.(*Error)
		if !ok {
			rpcErr = &Error{Code: CodePluginError, Message: err.Error()}
		}
		resp.Error = rpcErr
		return resp
	}
	data, err := json.Marshal(result)
	if err != nil {
		resp.Error = &Error{Code: CodePluginError, Message: err.Error()}
		return resp
	}
	resp.Result = data
	return resp
}

func decodeParams(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return &Error{Code: CodeInvalidParams, Message: err.Error()}
	}
	return nil
}
