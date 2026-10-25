package deploy

import (
	"errors"
	"path"
	"strings"
)

type shellCommandShape struct {
	script        string
	shell         bool
	commandString bool
}

// inspectShellCommand recognizes a sh or bash entrypoint, possibly behind env,
// and the command string it executes with -c. shell is true for any such
// wrapper, including one whose command string cannot be read.
func inspectShellCommand(entrypoint, argv []string) shellCommandShape {
	if len(entrypoint) == 0 {
		return shellCommandShape{}
	}
	args := append([]string(nil), entrypoint...)
	args = append(args, argv...)
	if path.Base(args[0]) == "env" {
		unwrapped := unwrapEnvCommand(args[1:])
		if len(unwrapped) == 0 {
			return shellCommandShape{shell: true}
		}
		args = unwrapped
	}
	if shell := path.Base(args[0]); shell != "sh" && shell != "bash" {
		return shellCommandShape{}
	}
	script, commandString := shellCommandString(args[1:])
	return shellCommandShape{script: script, shell: true, commandString: commandString}
}

func unwrapEnvCommand(args []string) []string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			if i+1 < len(args) {
				return args[i+1:]
			}
			return nil
		case isEnvAssignment(arg):
			continue
		case arg == "-i" || arg == "--ignore-environment":
			continue
		case arg == "-u" || arg == "--unset" || arg == "-C" || arg == "--chdir":
			if i+1 >= len(args) {
				return nil
			}
			i++
			continue
		case strings.HasPrefix(arg, "--unset=") || strings.HasPrefix(arg, "--chdir="):
			continue
		case strings.HasPrefix(arg, "-u") && len(arg) > 2:
			continue
		case strings.HasPrefix(arg, "-C") && len(arg) > 2:
			continue
		case arg == "-S" || arg == "--split-string" || strings.HasPrefix(arg, "--split-string="):
			var split string
			if arg == "-S" || arg == "--split-string" {
				if i+1 >= len(args) {
					return nil
				}
				i++
				split = args[i]
			} else {
				split = strings.TrimPrefix(arg, "--split-string=")
			}
			words, err := staticShellWords(split)
			if err != nil {
				return nil
			}
			return unwrapEnvCommand(words)
		case strings.HasPrefix(arg, "-"):
			return nil
		default:
			return args[i:]
		}
	}
	return nil
}

func isEnvAssignment(arg string) bool {
	name, _, found := strings.Cut(arg, "=")
	if !found || name == "" || !shellNameStart(name[0]) {
		return false
	}
	for i := 1; i < len(name); i++ {
		if !shellNamePart(name[i]) {
			return false
		}
	}
	return true
}

func shellNameStart(char byte) bool {
	return char == '_' || char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z'
}

func shellNamePart(char byte) bool {
	return shellNameStart(char) || char >= '0' && char <= '9'
}

func shellCommandString(args []string) (string, bool) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return "", false
		}
		if arg == "-c" {
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", false
		}
		if strings.HasPrefix(arg, "--") {
			switch arg {
			case "--login", "--noprofile", "--norc", "--posix":
				continue
			case "--rcfile", "--init-file":
				if i+1 >= len(args) {
					return "", false
				}
				i++
				continue
			default:
				return "", false
			}
		}
		if len(arg) > 1 && arg[0] == '-' {
			cluster := arg[1:]
			if cluster == "O" {
				if i+1 >= len(args) {
					return "", false
				}
				i++
				continue
			}
			command := strings.Contains(cluster, "c")
			for _, option := range cluster {
				if !shellShortOption(byte(option)) {
					return "", false
				}
			}
			if !command {
				continue
			}
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", false
		}
		return "", false
	}
	return "", false
}

func shellShortOption(option byte) bool {
	return strings.ContainsRune("abcefhilmnprsuvxCEHIPT", rune(option))
}

var errDynamicShell = errors.New("shell command is not statically inspectable")

// staticShellWords resolves shell quoting and adjacent literal fragments into
// the words a script spells out. Newlines, ;, |, & and parentheses only end a
// word. It rejects constructs whose runtime expansion could introduce engine
// arguments ($, backticks, globs, brace and bracket expressions), because the
// words would then not be what the shell runs.
func staticShellWords(script string) ([]string, error) {
	var words []string
	var word strings.Builder
	inWord := false
	var quote byte
	flush := func() {
		if inWord {
			words = append(words, word.String())
			word.Reset()
			inWord = false
		}
	}
	for i := 0; i < len(script); i++ {
		char := script[i]
		if quote == '\'' {
			if char == '\'' {
				quote = 0
			} else {
				word.WriteByte(char)
			}
			continue
		}
		if quote == '"' {
			switch char {
			case '"':
				quote = 0
			case '$', '`':
				return nil, errDynamicShell
			case '\\':
				if i+1 >= len(script) {
					return nil, errDynamicShell
				}
				next := script[i+1]
				if next == '\n' {
					i++
					continue
				}
				if strings.ContainsRune("$`\"\\", rune(next)) {
					word.WriteByte(next)
					i++
				} else {
					word.WriteByte(char)
				}
			default:
				word.WriteByte(char)
			}
			continue
		}
		if char == '<' && i+1 < len(script) && script[i+1] == '<' {
			flush()
			next, ok := skipShellHeredoc(script, i)
			if !ok {
				return nil, errDynamicShell
			}
			i = next - 1
			continue
		}
		if char == '<' || char == '>' || char == '&' && i+1 < len(script) && script[i+1] == '>' {
			if inWord && shellFileDescriptor(word.String()) {
				word.Reset()
				inWord = false
			} else {
				flush()
			}
			next, ok := skipShellRedirection(script, i)
			if !ok {
				return nil, errDynamicShell
			}
			i = next - 1
			continue
		}
		switch char {
		case '\'', '"':
			quote = char
			inWord = true
		case ' ', '\t', '\r', '\n', '<', '>', ';', '|', '&', '(', ')':
			flush()
		case '#':
			if inWord {
				word.WriteByte(char)
				continue
			}
			for i+1 < len(script) && script[i+1] != '\n' {
				i++
			}
		case '\\':
			if i+1 >= len(script) {
				return nil, errDynamicShell
			}
			next := script[i+1]
			i++
			if next != '\n' {
				word.WriteByte(next)
				inWord = true
			}
		case '$', '`', '{', '}', '[', ']', '*', '?':
			return nil, errDynamicShell
		default:
			word.WriteByte(char)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, errDynamicShell
	}
	flush()
	return words, nil
}

func skipShellHeredoc(script string, start int) (int, bool) {
	i := start + 2
	stripTabs := false
	if i < len(script) && script[i] == '-' {
		stripTabs = true
		i++
	}
	for i < len(script) && (script[i] == ' ' || script[i] == '\t' || script[i] == '\r') {
		i++
	}
	if i >= len(script) {
		return i, false
	}
	var delimiter string
	if quote := script[i]; quote == '\'' || quote == '"' {
		i++
		begin := i
		for i < len(script) && script[i] != quote {
			i++
		}
		if i >= len(script) {
			return i, false
		}
		delimiter = script[begin:i]
		i++
	} else {
		begin := i
		for i < len(script) && (shellNamePart(script[i]) || script[i] == '-') {
			i++
		}
		delimiter = script[begin:i]
	}
	if delimiter == "" {
		return i, false
	}
	for i < len(script) && (script[i] == ' ' || script[i] == '\t' || script[i] == '\r') {
		i++
	}
	if i >= len(script) || script[i] != '\n' {
		return i, false
	}
	for i < len(script) {
		i++
		end := i
		for end < len(script) && script[end] != '\n' {
			end++
		}
		line := script[i:end]
		if stripTabs {
			line = strings.TrimLeft(line, "\t")
		}
		if line == delimiter {
			return end, true
		}
		i = end
	}
	return i, false
}

func skipShellRedirection(script string, start int) (int, bool) {
	i := start
	if script[i] == '&' {
		i++
	}
	if i >= len(script) || script[i] != '<' && script[i] != '>' {
		return i, false
	}
	direction := script[i]
	i++
	if i < len(script) && script[i] == direction {
		i++
	}
	for i < len(script) && (script[i] == '|' || script[i] == '!' || script[i] == '&') {
		i++
	}
	for i < len(script) && (script[i] == ' ' || script[i] == '\t' || script[i] == '\r') {
		i++
	}
	if i >= len(script) || strings.ContainsRune("\n;|&<>", rune(script[i])) {
		return i, false
	}
	for i < len(script) {
		char := script[i]
		if char == ' ' || char == '\t' || char == '\r' || char == '\n' {
			break
		}
		if strings.ContainsRune("();`|&<>", rune(char)) || char == '$' && i+1 < len(script) && script[i+1] == '(' {
			return i, false
		}
		if char == '\\' {
			if i+1 >= len(script) {
				return i, false
			}
			i += 2
			continue
		}
		if char == '\'' || char == '"' {
			quote := char
			i++
			for i < len(script) && script[i] != quote {
				if quote == '"' && (script[i] == '`' || script[i] == '$' && i+1 < len(script) && script[i+1] == '(') {
					return i, false
				}
				if quote == '"' && script[i] == '\\' {
					if i+1 >= len(script) {
						return i, false
					}
					i += 2
				} else {
					i++
				}
			}
			if i >= len(script) {
				return i, false
			}
			i++
			continue
		}
		i++
	}
	return i, true
}

func shellFileDescriptor(word string) bool {
	if word == "" {
		return false
	}
	for i := range len(word) {
		if word[i] < '0' || word[i] > '9' {
			return false
		}
	}
	return true
}
