package provider

import "strings"

// Bot Query can render reasoning as an italic Thinking header followed by a
// Markdown quote instead of a separate reasoning delta. Recognize that exact
// envelope only; ordinary quotes, code and intermediate prose stay answers.
type poeThinkingFilter struct {
	pending, header, blanks string
	mode                    int // 0 answer, 1 possible header, 2 reasoning quote
	fence                   bool
}

var poeThinkingMarkers = []string{"*Thinking...*", "*Thinking…*", "_Thinking..._", "_Thinking…_"}

func (f *poeThinkingFilter) line(line string) (answer, reasoning string) {
	trimmed := strings.TrimSpace(line)
	if f.mode == 1 {
		if trimmed == "" {
			f.header += line
			return
		}
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), ">") {
			f.mode = 2
			f.header = ""
			return "", unquoteThinking(line)
		}
		answer = f.header
		f.header = ""
		f.mode = 0
	}
	if f.mode == 2 {
		if trimmed == "" {
			f.blanks += line
			return
		}
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), ">") {
			reasoning = f.blanks + unquoteThinking(line)
			f.blanks = ""
			return
		}
		answer += f.blanks
		f.blanks = ""
		f.mode = 0
	}
	if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
		f.fence = !f.fence
		return answer + line, ""
	}
	if !f.fence {
		for _, marker := range poeThinkingMarkers {
			if at := strings.Index(line, marker); at >= 0 && strings.TrimSpace(line[at+len(marker):]) == "" {
				f.header = line[at:]
				f.mode = 1
				return answer + line[:at], ""
			}
		}
	}
	return answer + line, ""
}
func unquoteThinking(line string) string {
	line = strings.TrimLeft(line, " \t")
	line = strings.TrimPrefix(line, ">")
	return strings.TrimPrefix(line, " ")
}
func (f *poeThinkingFilter) push(text string, final bool) (string, string) {
	f.pending += text
	var answer, reasoning strings.Builder
	for {
		at := strings.IndexByte(f.pending, '\n')
		if at < 0 {
			break
		}
		a, r := f.line(f.pending[:at+1])
		answer.WriteString(a)
		reasoning.WriteString(r)
		f.pending = f.pending[at+1:]
	}
	if final {
		if f.pending != "" {
			a, r := f.line(f.pending)
			answer.WriteString(a)
			reasoning.WriteString(r)
			f.pending = ""
		}
		if f.mode == 1 {
			answer.WriteString(f.header)
			f.header = ""
			f.mode = 0
		}
		return answer.String(), reasoning.String()
	}
	// Stream normal prose immediately, retaining only a possible split header.
	if f.mode == 0 && f.pending != "" {
		keep := 0
		if !f.fence {
			for _, marker := range poeThinkingMarkers {
				if at := strings.Index(f.pending, marker); at >= 0 {
					keep = max(keep, len(f.pending)-at)
				}
				for n := 1; n < len(marker) && n <= len(f.pending); n++ {
					if strings.HasSuffix(f.pending, marker[:n]) {
						keep = max(keep, n)
					}
				}
			}
		}
		trim := strings.TrimLeft(f.pending, " \t")
		if strings.HasPrefix(trim, "`") || strings.HasPrefix(trim, "~") {
			keep = len(f.pending)
		}
		end := len(f.pending) - keep
		answer.WriteString(f.pending[:end])
		f.pending = f.pending[end:]
	}
	return answer.String(), reasoning.String()
}

// SplitPoeThinking also cleans historical Bot Query responses on read.
func SplitPoeThinking(text string) (answer, reasoning string) {
	f := poeThinkingFilter{}
	return f.push(text, true)
}
