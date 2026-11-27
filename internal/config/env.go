package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
)

func LoadEnvFile(path string, required bool) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !required {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("failed to read env file %q: %w", path, err)
	}
	defer f.Close()

	values := make(map[string]string)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if lineNum == 1 {
			line = strings.TrimPrefix(line, "\xef\xbb\xbf")
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}

		key, rawVal, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		values[key] = parseEnvValue(rawVal)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to parse env file %q: %w", path, err)
	}

	return values, nil
}

func parseEnvValue(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if trimmed[0] == '\'' {
		if end := strings.IndexByte(trimmed[1:], '\''); end >= 0 {
			closeIdx := end + 1
			rest := strings.TrimSpace(trimmed[closeIdx+1:])
			if rest == "" || strings.HasPrefix(rest, "#") {
				return trimmed[1:closeIdx]
			}
		}
	}
	if trimmed[0] == '"' {
		for i := 1; i < len(trimmed); i++ {
			if trimmed[i] == '\\' && i+1 < len(trimmed) {
				i++
				continue
			}
			if trimmed[i] == '"' {
				rest := strings.TrimSpace(trimmed[i+1:])
				if rest == "" || strings.HasPrefix(rest, "#") {
					return unescapeDoubleQuoted(trimmed[1:i])
				}
				break
			}
		}
	}
	if idx := strings.IndexByte(trimmed, '#'); idx >= 0 {
		trimmed = strings.TrimSpace(trimmed[:idx])
	}
	return trimmed
}

func unescapeDoubleQuoted(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			default:
				b.WriteByte('\\')
				b.WriteByte(s[i])
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func buildEnvLookup(dotenv map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		if val, ok := os.LookupEnv(key); ok {
			return val, true
		}
		val, ok := dotenv[key]
		return val, ok
	}
}

func Interpolate(input string, lookup func(string) (string, bool)) (string, error) {
	var b strings.Builder
	b.Grow(len(input))

	for i := 0; i < len(input); i++ {
		if input[i] != '$' {
			b.WriteByte(input[i])
			continue
		}

		if i+1 >= len(input) {
			b.WriteByte('$')
			break
		}

		next := input[i+1]
		if next == '$' {
			b.WriteByte('$')
			i++
			continue
		}

		if next == '{' {
			end, ok := findClosingBrace(input, i+2)
			if !ok {
				return "", fmt.Errorf("invalid interpolation syntax: unclosed '${' expression")
			}
			expr := input[i+2 : end]
			val, err := evalBraceExpr(expr, lookup)
			if err != nil {
				return "", err
			}
			b.WriteString(val)
			i = end
			continue
		}

		if isVarStart(next) {
			j := i + 2
			for j < len(input) && isVarPart(input[j]) {
				j++
			}
			name := input[i+1 : j]
			if val, ok := lookup(name); ok {
				b.WriteString(val)
			}
			i = j - 1
			continue
		}

		b.WriteByte('$')
	}

	return b.String(), nil
}

func findClosingBrace(s string, start int) (int, bool) {
	depth := 1
	for i := start; i < len(s); i++ {
		if s[i] == '$' && i+1 < len(s) && s[i+1] == '$' {
			i++
			continue
		}
		if s[i] == '$' && i+1 < len(s) && s[i+1] == '{' {
			depth++
			i++
			continue
		}
		if s[i] == '}' {
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return -1, false
}

func evalBraceExpr(expr string, lookup func(string) (string, bool)) (string, error) {
	if expr == "" || !isVarStart(expr[0]) {
		return "", fmt.Errorf("invalid variable name in interpolation %q", "${"+expr+"}")
	}

	i := 1
	for i < len(expr) && isVarPart(expr[i]) {
		i++
	}
	name := expr[:i]
	rest := expr[i:]

	val, exists := lookup(name)
	if rest == "" {
		if exists {
			return val, nil
		}
		return "", nil
	}

	var op string
	var operand string
	switch {
	case strings.HasPrefix(rest, ":-"):
		op, operand = ":-", rest[2:]
	case strings.HasPrefix(rest, "-"):
		op, operand = "-", rest[1:]
	case strings.HasPrefix(rest, ":?"):
		op, operand = ":?", rest[2:]
	case strings.HasPrefix(rest, "?"):
		op, operand = "?", rest[1:]
	case strings.HasPrefix(rest, ":+"):
		op, operand = ":+", rest[2:]
	case strings.HasPrefix(rest, "+"):
		op, operand = "+", rest[1:]
	default:
		return "", fmt.Errorf("unsupported interpolation expression %q", "${"+expr+"}")
	}

	switch op {
	case ":-":
		if !exists || val == "" {
			return Interpolate(operand, lookup)
		}
		return val, nil
	case "-":
		if !exists {
			return Interpolate(operand, lookup)
		}
		return val, nil
	case ":?":
		if !exists || val == "" {
			msg, err := Interpolate(operand, lookup)
			if err != nil {
				return "", err
			}
			if msg == "" {
				return "", fmt.Errorf("required variable %s is missing a value", name)
			}
			return "", fmt.Errorf("required variable %s: %s", name, msg)
		}
		return val, nil
	case "?":
		if !exists {
			msg, err := Interpolate(operand, lookup)
			if err != nil {
				return "", err
			}
			if msg == "" {
				return "", fmt.Errorf("required variable %s is not set", name)
			}
			return "", fmt.Errorf("required variable %s: %s", name, msg)
		}
		return val, nil
	case ":+":
		if exists && val != "" {
			return Interpolate(operand, lookup)
		}
		return "", nil
	case "+":
		if exists {
			return Interpolate(operand, lookup)
		}
		return "", nil
	}

	return "", nil
}

func isVarStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

func isVarPart(c byte) bool {
	return isVarStart(c) || (c >= '0' && c <= '9')
}
