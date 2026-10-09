package actions

import (
	"strings"
	"unicode/utf8"
)

const (
	displayNameBytes    = 100
	displayCommandBytes = 512
)

// StepDisplay returns the single-line name and command that identify a step
// in job definitions and results. The full script stays in the plan.
func StepDisplay(step Step) (name, command string) {
	command = step.Uses
	if step.Uses == "" {
		lines := strings.FieldsFunc(step.Run, func(r rune) bool { return r == '\n' || r == '\r' })
		for index, line := range lines {
			if line = strings.TrimSpace(line); line != "" {
				command = line
				if strings.TrimSpace(strings.Join(lines[index+1:], "")) != "" {
					command += " …"
				}
				break
			}
		}
		if command == "" {
			command = "run"
		}
	}
	command = displayCut(command, displayCommandBytes)
	name = strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(step.Name))
	if name == "" {
		name = "Run " + command
		if step.Uses != "" {
			name = command
		}
	}
	return displayCut(name, displayNameBytes), command
}

func displayCut(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	text = text[:limit-len(" …")]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text + " …"
}
