// Package runner runs sets of test cases with a test runner.
//
// Every runner is a runner plugin (see pkg/runnerplugin): one of bktest's
// built-in runners in internal/runners, called in this process, or an
// external plugin loaded with --runner-plugin. PluginRunner adapts a plugin
// to the TestRunner interface the commands use.
package runner
