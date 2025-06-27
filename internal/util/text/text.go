package text

import (
	"strings"
)

// Dedent removes common leading whitespace from every line in text.
// It also replaces tabs with 4 spaces.
func Dedent(text string) string {
	// replace tabs with 4 spaces
	text = strings.ReplaceAll(text, "\t", "    ")

	lines := strings.Split(text, "\n")

	// remove leading/trailing empty lines
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}

	if len(lines) == 0 {
		return ""
	}

	// find minimum indentation
	minIndent := -1
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}

		indent := 0
		for _, r := range line {
			if r == ' ' {
				indent++
			} else {
				break
			}
		}

		if minIndent == -1 || indent < minIndent {
			minIndent = indent
		}
	}

	if minIndent <= 0 {
		return strings.Join(lines, "\n")
	}

	// remove indentation
	var b strings.Builder
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			b.WriteString(line)
		} else {
			if len(line) > minIndent {
				b.WriteString(line[minIndent:])
			} else {
				b.WriteString("")
			}
		}

		if i < len(lines)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}
