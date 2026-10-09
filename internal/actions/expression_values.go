package actions

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Arrays carry identity during evaluation, including empty arrays. JSON slice
// headers alone cannot distinguish two independently created empty arrays.
type expressionArray struct{ values []any }

type expressionEnvironment struct {
	values      map[string]any
	insensitive bool
}

func expressionValues(value any, cache map[string]any, depth int, steps *int) (any, error) {
	*steps++
	if depth > MaxExpressionDepth || *steps > MaxEvaluationSteps {
		return nil, limitError("expression context depth or evaluation steps", MaxEvaluationSteps)
	}
	switch value := value.(type) {
	case map[string]any:
		identity := fmt.Sprintf("map:%p", value)
		if old, ok := cache[identity]; ok {
			return old, nil
		}
		out := make(map[string]any, len(value))
		cache[identity] = out
		for key, item := range value {
			converted, err := expressionValues(item, cache, depth+1, steps)
			if err != nil {
				return nil, err
			}
			out[key] = converted
		}
		return out, nil
	case map[string]string:
		identity := fmt.Sprintf("map:%p", value)
		if old, ok := cache[identity]; ok {
			return old, nil
		}
		out := make(map[string]any, len(value))
		cache[identity] = out
		for key, item := range value {
			converted, err := expressionValues(item, cache, depth+1, steps)
			if err != nil {
				return nil, err
			}
			out[key] = converted
		}
		return out, nil
	case []any:
		identity := ""
		if len(value) > 0 {
			identity = fmt.Sprintf("array:%p:%d", value, len(value))
			if old, ok := cache[identity]; ok {
				return old, nil
			}
		}
		out := &expressionArray{values: make([]any, len(value))}
		if identity != "" {
			cache[identity] = out
		}
		for i, item := range value {
			converted, err := expressionValues(item, cache, depth+1, steps)
			if err != nil {
				return nil, err
			}
			out.values[i] = converted
		}
		return out, nil
	}
	return value, nil
}

func prepareContext(ctx EvalContext, steps *int) (EvalContext, error) {
	values, err := expressionValues(ctx.Values, map[string]any{}, 0, steps)
	if err != nil {
		return ctx, err
	}
	ctx.Values = values.(map[string]any)
	if env, ok := ctx.Values["env"].(map[string]any); ok {
		runner, _ := ctx.Values["runner"].(map[string]any)
		os, _ := runner["os"].(string)
		ctx.Values["env"] = &expressionEnvironment{values: env, insensitive: strings.EqualFold(os, "Windows")}
	}
	return ctx, nil
}

func arrayValues(value any) ([]any, bool) {
	switch value := value.(type) {
	case *expressionArray:
		return value.values, true
	case []any:
		return value, true
	}
	return nil, false
}

func nativeValue(value any) any {
	switch value := value.(type) {
	case *expressionEnvironment:
		return nativeValue(value.values)
	case *expressionArray:
		out := make([]any, len(value.values))
		for i, item := range value.values {
			out[i] = nativeValue(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, item := range value {
			out[key] = nativeValue(item)
		}
		return out
	}
	return value
}

func propertyValue(value, property any) any {
	if env, ok := value.(*expressionEnvironment); ok {
		if name, ok := property.(string); ok {
			if env.insensitive {
				return objectProperty(env.values, name)
			}
			return env.values[name]
		}
		return nil
	}
	if object, ok := value.(map[string]any); ok {
		if name, ok := property.(string); ok {
			return objectProperty(object, name)
		}
	} else if array, ok := arrayValues(value); ok {
		n := number(property)
		if n >= 0 && n < float64(len(array)) && n == float64(int(n)) {
			return array[int(n)]
		}
	}
	return nil
}

func preflightJSONSize(value any, depth int) (int, error) {
	if env, ok := value.(*expressionEnvironment); ok {
		return preflightJSONSize(env.values, depth)
	}
	if depth > MaxExpressionDepth {
		return 0, limitError("JSON value depth", MaxExpressionDepth)
	}
	if array, ok := arrayValues(value); ok {
		n := 2
		for _, item := range array {
			size, err := preflightJSONSize(item, depth+1)
			if err != nil {
				return 0, err
			}
			n += size + 2*(depth+1) + 2
			if n > MaxValueBytes {
				return 0, limitError("toJSON output", MaxValueBytes)
			}
		}
		if len(array) > 0 {
			n += 2 * depth
		}
		return n, nil
	}
	if object, ok := value.(map[string]any); ok {
		n := 2
		for key, item := range object {
			size, err := preflightJSONSize(item, depth+1)
			if err != nil {
				return 0, err
			}
			n += jsonStringSize(key) + size + 2*(depth+1) + 4
			if n > MaxValueBytes {
				return 0, limitError("toJSON output", MaxValueBytes)
			}
		}
		if len(object) > 0 {
			n += 2 * depth
		}
		return n, nil
	}
	if text, ok := value.(string); ok {
		return jsonStringSize(text), nil
	}
	if value == nil {
		return 4, nil
	}
	text, err := ScalarString(value)
	return len(text), err
}

func jsonStringSize(text string) int {
	n := 2
	for len(text) > 0 {
		c, size := utf8.DecodeRuneInString(text)
		text = text[size:]
		switch {
		case c == '\\' || c == '"' || strings.ContainsRune("\b\f\n\r\t", c):
			n += 2
		case c < 32 || c == '\u2028' || c == '\u2029':
			n += 6
		case c == utf8.RuneError && size == 1:
			n += 6
		default:
			n += size
		}
	}
	return n
}

func collectionEqual(a, b any) (bool, bool) {
	if array, ok := a.(*expressionArray); ok {
		other, ok := b.(*expressionArray)
		return ok && array == other, true
	}
	if object, ok := objectValues(a); ok {
		other, ok := objectValues(b)
		return ok && fmt.Sprintf("%p", object) == fmt.Sprintf("%p", other), true
	}
	return false, false
}

func objectValues(value any) (map[string]any, bool) {
	if env, ok := value.(*expressionEnvironment); ok {
		return env.values, true
	}
	object, ok := value.(map[string]any)
	return object, ok
}
