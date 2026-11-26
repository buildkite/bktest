// Package pluginhost calls runner plugins: built-in handlers in this process,
// and external plugins loaded from a directory, as child processes.
package pluginhost

import (
	"errors"

	"github.com/buildkite/bktest/v3/pkg/runnerplugin"
)

// Plugin is a runner plugin bktest can call. A plugin answers an optional
// operation with ErrUnsupported unless its Configure response lists it.
type Plugin interface {
	Configure(runnerplugin.ConfigureRequest) (runnerplugin.ConfigureResponse, error)
	PrepareAttempt(runnerplugin.PrepareAttemptRequest) (runnerplugin.PrepareAttemptResponse, error)
	DiscoverTargets(runnerplugin.DiscoverTargetsRequest) (runnerplugin.DiscoverTargetsResponse, error)
	DiscoverExamples(runnerplugin.DiscoverExamplesRequest) (runnerplugin.DiscoverExamplesResponse, error)
	InterpretAttempt(runnerplugin.InterpretAttemptRequest) (runnerplugin.InterpretAttemptResponse, error)
	ParseBatchReport(runnerplugin.ParseBatchReportRequest) (runnerplugin.ParseBatchReportResponse, error)
}

// ErrUnsupported is returned for an optional operation a plugin doesn't
// implement.
var ErrUnsupported = errors.New("the runner plugin does not implement this operation")

// InProcess calls a handler in this process, as bktest does for its built-in
// runners. Like an external plugin, which is a new process for each
// operation, each operation gets a new handler, so a built-in runner can't
// come to depend on memory that an external plugin wouldn't have.
type InProcess struct {
	New func() runnerplugin.Handler
}

var _ Plugin = InProcess{}

func (p InProcess) Configure(request runnerplugin.ConfigureRequest) (runnerplugin.ConfigureResponse, error) {
	handler := p.New()
	response, err := handler.Configure(request)
	response.ProtocolVersion = runnerplugin.ProtocolVersion
	response.Operations = runnerplugin.Operations(handler)
	return response, err
}

func (p InProcess) PrepareAttempt(request runnerplugin.PrepareAttemptRequest) (runnerplugin.PrepareAttemptResponse, error) {
	return p.New().PrepareAttempt(request)
}

func (p InProcess) DiscoverTargets(request runnerplugin.DiscoverTargetsRequest) (runnerplugin.DiscoverTargetsResponse, error) {
	if handler, ok := p.New().(runnerplugin.TargetDiscoverer); ok {
		return handler.DiscoverTargets(request)
	}
	return runnerplugin.DiscoverTargetsResponse{}, ErrUnsupported
}

func (p InProcess) DiscoverExamples(request runnerplugin.DiscoverExamplesRequest) (runnerplugin.DiscoverExamplesResponse, error) {
	if handler, ok := p.New().(runnerplugin.ExampleDiscoverer); ok {
		return handler.DiscoverExamples(request)
	}
	return runnerplugin.DiscoverExamplesResponse{}, ErrUnsupported
}

func (p InProcess) InterpretAttempt(request runnerplugin.InterpretAttemptRequest) (runnerplugin.InterpretAttemptResponse, error) {
	if handler, ok := p.New().(runnerplugin.AttemptInterpreter); ok {
		return handler.InterpretAttempt(request)
	}
	return runnerplugin.InterpretAttemptResponse{}, ErrUnsupported
}

func (p InProcess) ParseBatchReport(request runnerplugin.ParseBatchReportRequest) (runnerplugin.ParseBatchReportResponse, error) {
	if handler, ok := p.New().(runnerplugin.BatchReportParser); ok {
		return handler.ParseBatchReport(request)
	}
	return runnerplugin.ParseBatchReportResponse{}, ErrUnsupported
}
