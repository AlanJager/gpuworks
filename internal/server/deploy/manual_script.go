package deploy

import (
	"path/filepath"
	"strings"
)

// manualScriptEngineCommand is the one command of a manual command's shell
// script that starts engine, as the words the engine would read; ok is false
// for every launch that is not such a script, for a script that cannot be
// read, and for one that starts the engine not exactly once.
//
// The script is the deployer's and may do anything: start a helper with flags
// of the same name, start one engine per card, print the flags, carry them in
// a heredoc. Only the words of the engine's own command line say what the
// engine will use, so nothing else is read, and a script that starts the
// engine twice (two processes, or two branches) states no single world.
func manualScriptEngineCommand(engine string, launch ResolvedLaunch) (words []string, ok bool) {
	if launch.CommandMode != string(CommandManual) || len(launch.Entrypoint) != 1 || launch.Entrypoint[0] != "/bin/sh" ||
		len(launch.Argv) != 2 || launch.Argv[0] != "-c" {
		return nil, false
	}
	commands, readable := shellSimpleCommands(launch.Argv[1])
	if !readable {
		return nil, false
	}
	for _, command := range commands {
		invocation := engineInvocation(engine, command)
		if invocation == nil {
			continue
		}
		if words != nil {
			return nil, false
		}
		words = invocation
	}
	return words, words != nil
}

// shellCommandPrefixes are the words a shell allows before the program of a
// simple command without changing which program runs.
var shellCommandPrefixes = map[string]bool{
	"exec": true, "nohup": true, "command": true, "time": true, "!": true,
	"then": true, "else": true, "do": true, "{": true,
}

// engineInvocation is command from the engine's program on when command runs
// engine's server, and nil otherwise. Leading variable assignments and
// shellCommandPrefixes are not the program.
func engineInvocation(engine string, command []string) []string {
	for len(command) > 0 {
		word := command[0]
		if shellCommandPrefixes[word] || isShellAssignment(word) {
			command = command[1:]
			continue
		}
		break
	}
	if !isEngineServerCommand(engine, command) {
		return nil
	}
	return command
}

func isShellAssignment(word string) bool {
	name, _, found := strings.Cut(word, "=")
	if !found || name == "" || strings.HasPrefix(name, "-") {
		return false
	}
	for i, char := range name {
		letter := char == '_' || (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z')
		if !letter && (i == 0 || char < '0' || char > '9') {
			return false
		}
	}
	return true
}

// isEngineServerCommand reports whether words start engine's own server, by
// the entrypoints the platform itself launches it with.
func isEngineServerCommand(engine string, words []string) bool {
	switch engine {
	case EngineSGLang:
		return isSGLangServeEntrypoint(words) || isSGLangLLMEntrypoint(words)
	case EngineVLLM:
		return isVLLMServeEntrypoint(words) || isVLLMAPIServerEntrypoint(words)
	}
	return false
}

func isVLLMServeEntrypoint(entrypoint []string) bool {
	return len(entrypoint) >= 2 && filepath.Base(entrypoint[0]) == EngineVLLM && entrypoint[1] == "serve"
}

func isVLLMAPIServerEntrypoint(entrypoint []string) bool {
	return len(entrypoint) >= 3 && strings.HasPrefix(filepath.Base(entrypoint[0]), "python") && entrypoint[1] == "-m" &&
		entrypoint[2] == "vllm.entrypoints.openai.api_server"
}

// shellSimpleCommands splits a POSIX shell script into its simple commands,
// each as its words with quoting removed. A command ends at an unquoted
// newline, `;`, `&`, `|` (and so `&&`, `||`) or parenthesis; a backslash
// before a newline joins the lines; `#` at the start of a word comments out
// the rest of the line; heredoc bodies are not commands. Expansions are not
// performed: a word keeps its `$` and backquotes. ok is false for a script
// with an unterminated quote or heredoc.
func shellSimpleCommands(script string) (commands [][]string, ok bool) {
	var (
		current  []string
		word     strings.Builder
		inWord   bool
		heredocs []heredoc
	)
	endWord := func() {
		if inWord {
			current = append(current, word.String())
			word.Reset()
			inWord = false
		}
	}
	endCommand := func() {
		endWord()
		if len(current) > 0 {
			commands = append(commands, current)
			current = nil
		}
	}
	text := []rune(script)
	for i := 0; i < len(text); i++ {
		char := text[i]
		switch {
		case char == '\\':
			if i+1 >= len(text) {
				return nil, false
			}
			i++
			if text[i] != '\n' { // an escaped newline joins the lines
				word.WriteRune(text[i])
				inWord = true
			}
		case char == '\'':
			end := indexRune(text, i+1, '\'')
			if end < 0 {
				return nil, false
			}
			word.WriteString(string(text[i+1 : end]))
			inWord, i = true, end
		case char == '"':
			inWord = true
			closed := false
			for i++; i < len(text); i++ {
				if text[i] == '"' {
					closed = true
					break
				}
				if text[i] == '\\' && i+1 < len(text) && strings.ContainsRune("\"\\$`\n", text[i+1]) {
					i++
					if text[i] == '\n' {
						continue
					}
				}
				word.WriteRune(text[i])
			}
			if !closed {
				return nil, false
			}
		case char == '#' && !inWord:
			for i+1 < len(text) && text[i+1] != '\n' {
				i++
			}
		case char == '<' && i+1 < len(text) && text[i+1] == '<' && !(i+2 < len(text) && text[i+2] == '<'):
			endWord()
			i += 2
			stripTabs := i < len(text) && text[i] == '-'
			if stripTabs {
				i++
			}
			for i < len(text) && (text[i] == ' ' || text[i] == '\t') {
				i++
			}
			var delimiter strings.Builder
			for ; i < len(text) && !strings.ContainsRune(" \t\n;&|()<>", text[i]); i++ {
				if text[i] != '\'' && text[i] != '"' && text[i] != '\\' {
					delimiter.WriteRune(text[i])
				}
			}
			i--
			if delimiter.Len() == 0 {
				return nil, false
			}
			heredocs = append(heredocs, heredoc{delimiter: delimiter.String(), stripTabs: stripTabs})
		case char == '\n':
			endCommand()
			for _, doc := range heredocs {
				next, found := doc.skip(text, i+1)
				if !found {
					return nil, false
				}
				i = next - 1
			}
			heredocs = nil
		case char == ';' || char == '&' || char == '|' || char == '(' || char == ')':
			endCommand()
		case char == ' ' || char == '\t' || char == '\r':
			endWord()
		default:
			word.WriteRune(char)
			inWord = true
		}
	}
	if len(heredocs) > 0 {
		return nil, false
	}
	endCommand()
	return commands, true
}

type heredoc struct {
	delimiter string
	stripTabs bool
}

// skip returns the index after the heredoc body that starts at from: the
// start of the line following the one that is the delimiter alone.
func (h heredoc) skip(text []rune, from int) (next int, found bool) {
	for from <= len(text) {
		end := indexRune(text, from, '\n')
		last := end < 0
		if last {
			end = len(text)
		}
		line := string(text[from:end])
		if h.stripTabs {
			line = strings.TrimLeft(line, "\t")
		}
		if line == h.delimiter {
			if last {
				return len(text), true
			}
			return end + 1, true
		}
		if last {
			return 0, false
		}
		from = end + 1
	}
	return 0, false
}

func indexRune(text []rune, from int, want rune) int {
	for i := from; i < len(text); i++ {
		if text[i] == want {
			return i
		}
	}
	return -1
}
