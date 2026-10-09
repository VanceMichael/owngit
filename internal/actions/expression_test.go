package actions

import (
	"reflect"
	"strings"
	"testing"
)

func TestExpression(t *testing.T) {
	ctx := EvalContext{Success: true, Values: map[string]any{
		"github":  map[string]any{"ref": "refs/heads/main", "sha": "abc", "workspace": "/synthetic/workspace", "event": map[string]any{"pull_request": map[string]any{"head": map[string]any{"sha": "def"}}}},
		"inputs":  map[string]any{"number": float64(7), "list": []any{"One", "Two"}, "empty": []any{}, "object": map[string]any{"key": "value"}, "strings": map[string]string{"key": "value"}, "index": "number"},
		"env":     map[string]any{"LONG": strings.Repeat("x", MaxValueBytes), "ESCAPED": strings.Repeat("\x01", 12000), "LARGE": strings.Repeat("x", MaxValueBytes+1)},
		"secrets": map[string]any{"TOKEN": "synthetic-value"},
		"steps":   map[string]any{"cache": map[string]any{"outputs": map[string]any{"cache-hit": "false"}}},
		"needs":   map[string]any{"build": map[string]any{"result": "success"}},
	}}
	failed := ctx
	failed.Success, failed.Failure = false, true
	cancelled := ctx
	cancelled.Success, cancelled.Cancelled = false, true
	posix := ctx
	posix.Values = cloneMap(ctx.Values)
	posix.Values["env"] = map[string]string{"MODE": "upper"}
	posix.Values["runner"] = map[string]any{"os": "Linux"}
	windows := posix
	windows.Values = cloneMap(posix.Values)
	windows.Values["runner"] = map[string]any{"os": "Windows"}
	cases := []struct {
		name, operation, text, key string
		context                    *EvalContext
		want                       any
		code                       string
	}{
		{name: "literal null", text: "null", want: nil},
		{name: "case insensitive literal", text: "TRUE", want: true},
		{name: "hex number", text: "0xff", want: float64(255)},
		{name: "exponent", text: "-2.5e2", want: float64(-250)},
		{name: "quoted apostrophe", text: "'It''s'", want: "It's"},
		{name: "property", text: "github.event.pull_request.head.sha", want: "def"},
		{name: "context case", text: "GITHUB.REF", want: "refs/heads/main"},
		{name: "index", text: "inputs[inputs.index]", want: float64(7)},
		{name: "array index", text: "inputs.list[1]", want: "Two"},
		{name: "missing property", text: "inputs.absent", want: nil},
		{name: "array missing index", text: "inputs.list[9]", want: nil},
		{name: "string equality", text: "'HELLO' == 'hello'", want: true},
		{name: "loose number equality", text: "'7' == inputs.number", want: true},
		{name: "null zero", text: "null == false", want: true},
		{name: "array NaN", text: "inputs.list > 0", want: false},
		{name: "non numeric NaN", text: "'no' < 0", want: false},
		{name: "string order", text: "'A' < 'b'", want: true},
		{name: "inequality", text: "inputs.number != 8", want: true},
		{name: "precedence", text: "!false && 1 < 2 || false", want: true},
		{name: "and returns operand", text: "true && 'chosen'", want: "chosen"},
		{name: "or returns operand", text: "'' || 'fallback'", want: "fallback"},
		{name: "short circuit", text: "true || fromJSON('bad')", want: true},
		{name: "array identity", text: "inputs.list == inputs.list", want: true},
		{name: "empty array identity", text: "inputs.empty == inputs.empty", want: true},
		{name: "distinct empty arrays", text: "fromJSON('[]') == fromJSON('[]')", want: false},
		{name: "object identity", text: "inputs.object == inputs.object", want: true},
		{name: "string-map object", text: "inputs.strings.KEY == 'value' && inputs.strings.absent == null && inputs.strings == inputs.strings", want: true},
		{name: "distinct objects", text: "fromJSON('{}') == fromJSON('{}')", want: false},
		{name: "contains array", text: "contains(inputs.list, 'one')", want: true},
		{name: "contains string", text: "contains('abcd', 'BC')", want: true},
		{name: "starts with", text: "startsWith('Hello', 'HE')", want: true},
		{name: "ends with", text: "endsWith('Hello', 'LO')", want: true},
		{name: "join", text: "join(inputs.list, ':')", want: "One:Two"},
		{name: "join default", text: "join(inputs.list)", want: "One,Two"},
		{name: "format escapes", text: "format('{{{0}}} {1}', 'x', 2)", want: "{x} 2"},
		{name: "format does not rescan", text: "format('{0}', '{1}')", want: "{1}"},
		{name: "from JSON typed", text: "fromJSON('{\"enabled\":true,\"n\":2}').n", want: float64(2)},
		{name: "to JSON", text: "toJSON(inputs.list)", want: "[\n  \"One\",\n  \"Two\"\n]"},
		{name: "invalid JSON", text: "fromJSON('bad')", code: "workflow.wrong_type"},
		{name: "invalid JSON secret stays private", text: "fromJSON(secrets.TOKEN)", code: "workflow.wrong_type"},
		{name: "invalid placeholder", text: "format('{2}', 'x')", code: "workflow.wrong_type"},
		{name: "wrong argument count", text: "contains('x')", code: "workflow.wrong_type"},
		{name: "unknown function checked in dead branch", text: "true || hashFiles('x')", code: "workflow.function"},
		{name: "unknown new function", text: "case(true, 'x', 'y')", code: "workflow.function"},
		{name: "vars", text: "vars.NAME", code: "workflow.context"},
		{name: "unsupported event field", text: "github.event.sender.login", code: "workflow.context"},
		{name: "token", text: "github.token", code: "workflow.token"},
		{name: "aliased token", text: "(github || inputs).token", code: "workflow.token"},
		{name: "token in fallback operand", text: "(inputs || github).token", code: "workflow.token"},
		{name: "and aliased token", text: "(github && inputs).token", code: "workflow.token"},
		{name: "nested aliased event", text: "((github.event || inputs) && inputs).sender.login", code: "workflow.context"},
		{name: "aliased unsupported event", text: "(github || inputs).event.sender.login", code: "workflow.context"},
		{name: "aliased supported field", text: "(github || inputs).ref", want: "refs/heads/main"},
		{name: "and aliased supported field", text: "(true && github).ref", want: "refs/heads/main"},
		{name: "aliased event field", text: "(github.event || inputs).pull_request.head.sha", want: "def"},
		{name: "aliased string index", text: "(github || inputs)['ref']", want: "refs/heads/main"},
		{name: "aliased dynamic closed index", text: "(github || inputs)[inputs.index]", code: "workflow.context"},
		{name: "independent JSON logical object", text: "(fromJSON('{\"token\":\"own-value\"}') || inputs).token", want: "own-value"},
		{name: "secret token", text: "secrets.GITHUB_TOKEN", code: "workflow.token"},
		{name: "secret direct", text: "secrets.TOKEN", want: "synthetic-value"},
		{name: "secret dynamic", text: "secrets[inputs.index]", code: "workflow.secret_ref"},
		{name: "aliased secret name", text: "(secrets || inputs).TOKEN", code: "workflow.secret_ref"},
		{name: "secret whole object", text: "toJSON(secrets)", code: "workflow.secret_ref"},
		{name: "secret in if", operation: "if", key: "step.if", text: "secrets.TOKEN != ''", code: "workflow.secret_ref"},
		{name: "outputs between jobs", text: "needs.build.outputs.sha", code: "workflow.context"},
		{name: "aliased outputs between jobs", text: "(needs || inputs).build.outputs.sha", code: "workflow.context"},
		{name: "aliased needs result", text: "(needs || inputs).build.result", want: "success"},
		{name: "aliased runner field", text: "(runner || inputs).container", code: "workflow.context"},
		{name: "aliased job field", text: "(job || inputs).services", code: "workflow.context"},
		{name: "aliased step field", text: "(steps || inputs).cache.secret", code: "workflow.context"},
		{name: "matrix unavailable for job if", operation: "if", key: "job.if", text: "matrix.experimental", code: "workflow.context"},
		{name: "needs unavailable for strategy", key: "job.strategy", text: "needs.build.result", code: "workflow.context"},
		{name: "runner unavailable in job name", key: "job.name", text: "runner.os", code: "workflow.context"},
		{name: "workspace only at step start", key: "job.name", text: "github.workspace", code: "workflow.context"},
		{name: "workspace in workflow env", key: "workflow.env", text: "github.workspace", want: "/synthetic/workspace"},
		{name: "workspace in job env", key: "job.env", text: "github.workspace", want: "/synthetic/workspace"},
		{name: "workspace in job defaults", key: "job.defaults.run", text: "github.workspace", want: "/synthetic/workspace"},
		{name: "step output", text: "steps.cache.outputs.cache-hit", want: "false"},
		{name: "env case exact on POSIX", text: "env.MODE", context: &posix, want: "upper"},
		{name: "env case differs on POSIX", text: "env.mode", context: &posix, want: nil},
		{name: "env case ignored on Windows", text: "env.mode", context: &windows, want: "upper"},
		{name: "env alias keeps case", text: "(env || fromJSON('{}')).mode", context: &posix, want: nil},
		{name: "env dictionary identity", text: "env == env", context: &posix, want: true},
		{name: "object filter", text: "inputs.*", code: "workflow.wrong_type"},
		{name: "double quote string", text: "\"text\"", code: "workflow.wrong_type"},
		{name: "trailing token", text: "true false", code: "workflow.wrong_type"},
		{name: "implicit success after failure", operation: "if", key: "step.if", text: "github.ref == 'refs/heads/main'", context: &failed, want: false},
		{name: "failure bypasses default", operation: "if", key: "step.if", text: "failure() && github.ref == 'refs/heads/main'", context: &failed, want: true},
		{name: "always after cancellation", operation: "if", key: "step.if", text: "always()", context: &cancelled, want: true},
		{name: "cancelled function", operation: "if", key: "step.if", text: "cancelled()", context: &cancelled, want: true},
		{name: "status only in if", text: "success()", code: "workflow.context"},
		{name: "bare bool if", operation: "if", key: "job.if", text: "true", want: true},
		{name: "template", operation: "template", text: "build-${{ inputs.number }}", want: "build-7"},
		{name: "sole typed template", operation: "template", text: "${{ fromJSON('[1,2]') }}", want: []any{float64(1), float64(2)}},
		{name: "quoted closing delimiter", operation: "template", text: "${{ '}}' }}", want: "}}"},
		{name: "array is not interpolated", operation: "template", text: "list-${{ inputs.list }}", code: "workflow.wrong_type"},
		{name: "unclosed template", operation: "template", text: "${{ inputs.number", code: "workflow.wrong_type"},
		{name: "secret name collection", operation: "validate", key: "step.env", text: "${{ secrets.token }}-${{ secrets.OTHER }}", want: []string{"OTHER", "TOKEN"}},
		{name: "text bound", text: "'" + strings.Repeat("x", MaxExpressionBytes) + "'", code: "workflow.limit"},
		{name: "nesting bound", text: strings.Repeat("(", 33) + "true" + strings.Repeat(")", 33), code: "workflow.limit"},
		{name: "JSON depth bound", text: "fromJSON('" + strings.Repeat("[", 33) + "0" + strings.Repeat("]", 33) + "')", code: "workflow.limit"},
		{name: "value exact bound", text: "env.LONG", want: strings.Repeat("x", MaxValueBytes)},
		{name: "value over bound", text: "env.LARGE", code: "workflow.limit"},
		{name: "format allocation bound", text: "format('{0}{0}', env.LONG)", code: "workflow.limit"},
		{name: "JSON escaped output bound", text: "toJSON(env.ESCAPED)", code: "workflow.limit"},
		{name: "JSON input bound", text: "fromJSON(env.LARGE)", code: "workflow.limit"},
		{name: "evaluation budget", text: "inputs.number", context: &EvalContext{Success: true, Values: map[string]any{"inputs": map[string]any{"list": make([]any, MaxEvaluationSteps)}}}, code: "workflow.limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, context := tc.key, ctx
			if key == "" {
				key = "step.run"
			}
			if tc.context != nil {
				context = *tc.context
			}
			var got any
			var err error
			switch tc.operation {
			case "template":
				got, err = EvalTemplate(tc.text, key, context)
			case "if":
				got, err = EvalIf(tc.text, key, context)
			case "validate":
				got, err = ValidateTemplate(tc.text, key)
			default:
				got, err = Eval(tc.text, key, context)
			}
			if errorCode(err) != tc.code {
				t.Fatalf("code = %s, want %s: %v", errorCode(err), tc.code, err)
			}
			if err == nil && !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
			if err != nil && strings.Contains(err.Error(), "synthetic-value") {
				t.Fatal("runtime value leaked into expression error")
			}
		})
	}
}
