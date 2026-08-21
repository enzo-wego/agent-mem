// export_test.go exposes internal symbols to the external test package
// (package llmgateway_test) for white-box testing.
package llmgateway

// CallerNameForTesting calls callerName() so the external test package can
// verify it returns a non-llmgateway frame when called from outside.
var CallerNameForTesting = callerName
