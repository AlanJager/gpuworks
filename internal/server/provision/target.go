package provision

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// ParseTarget resolves an SSH target to user, host and port. The target is
// either an address — user@host[:port], host[:port] with fallbackUser, or
// ssh://[user@]host[:port] — or a pasted OpenSSH command line such as
// `ssh -p 51223 root@203.0.113.7 -L 8080:localhost:8080`. IPv6 literals in an
// address must use [addr]:port.
func ParseTarget(target, fallbackUser string, fallbackPort int) (user, host string, port int, err error) {
	target = strings.TrimSpace(target)
	user = strings.TrimSpace(fallbackUser)
	port = fallbackPort
	if port == 0 {
		port = 22
	}
	if target == "" {
		return "", "", 0, errors.New("target is required")
	}
	destination := target
	var cmd sshCommand
	if isSSHCommand(target) {
		cmd, err = parseSSHCommand(target)
		if err != nil {
			return "", "", 0, err
		}
		destination = cmd.destination
	}
	destUser, host, destPort, err := splitDestination(destination)
	if err != nil {
		return "", "", 0, err
	}
	// Explicit -l/-p (or -o User/Port) take precedence over the destination,
	// matching OpenSSH.
	if cmd.user != "" {
		user = cmd.user
	} else if destUser != "" {
		user = destUser
	}
	if cmd.port != 0 {
		port = cmd.port
	} else if destPort != 0 {
		port = destPort
	}
	if user == "" {
		return "", "", 0, errors.New("ssh user is required")
	}
	if host == "" {
		return "", "", 0, errors.New("ssh host is required")
	}
	return user, host, port, nil
}

// splitDestination parses [ssh://][user@]host[:port]. Port 0 means absent.
func splitDestination(dest string) (user, host string, port int, err error) {
	dest = strings.TrimPrefix(strings.TrimSpace(dest), "ssh://")
	dest = strings.TrimSuffix(dest, "/")
	if at := strings.LastIndex(dest, "@"); at >= 0 {
		user = dest[:at]
		dest = dest[at+1:]
	}
	host = dest
	if h, p, splitErr := net.SplitHostPort(dest); splitErr == nil {
		host = h
		if port, err = parsePort(p); err != nil {
			return "", "", 0, err
		}
	} else if strings.Count(dest, ":") == 1 {
		parts := strings.SplitN(dest, ":", 2)
		if n, convErr := strconv.Atoi(parts[1]); convErr == nil {
			host = parts[0]
			port = n
		}
	}
	return user, host, port, nil
}

func parsePort(value string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("invalid port %q", value)
	}
	return n, nil
}

type sshCommand struct {
	destination string
	user        string
	port        int
}

func isSSHCommand(target string) bool {
	fields := strings.Fields(target)
	return len(fields) > 0 && (fields[0] == "ssh" || strings.HasSuffix(fields[0], "/ssh"))
}

// sshArgOptions are the OpenSSH client flags that consume an argument
// (ssh.c getopt string); every other letter is a plain switch.
const sshArgOptions = "BbcDEeFIiJLlmOoPpQRSWw"

// parseSSHCommand extracts the destination, -l user and -p port from an
// OpenSSH command line. Like OpenSSH, options may follow the destination; the
// first non-option word after it starts the remote command, which is ignored,
// as are forwarding and identity options that do not apply to the server's own
// connection. A shell separator ends the command.
func parseSSHCommand(line string) (sshCommand, error) {
	words, err := splitShellWords(line)
	if err != nil {
		return sshCommand{}, err
	}
	var cmd sshCommand
	args := words[1:]
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == ";" || arg == "&&" || arg == "||" || arg == "|" {
			break
		}
		if arg == "--" {
			if cmd.destination == "" && i+1 < len(args) {
				cmd.destination = args[i+1]
			}
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			if cmd.destination != "" {
				break // remote command
			}
			cmd.destination = arg
			continue
		}
		for j := 1; j < len(arg); j++ {
			flag := arg[j]
			if !strings.ContainsRune(sshArgOptions, rune(flag)) {
				continue
			}
			value := arg[j+1:]
			if value == "" {
				if i+1 >= len(args) {
					return sshCommand{}, fmt.Errorf("ssh option -%c needs a value", flag)
				}
				i++
				value = args[i]
			}
			if err := cmd.apply(flag, value); err != nil {
				return sshCommand{}, err
			}
			break
		}
	}
	if cmd.destination == "" {
		return sshCommand{}, errors.New("ssh command has no destination")
	}
	return cmd, nil
}

func (c *sshCommand) apply(flag byte, value string) error {
	switch flag {
	case 'l':
		c.user = value
	case 'p':
		port, err := parsePort(value)
		if err != nil {
			return err
		}
		c.port = port
	case 'J':
		return errors.New("ssh jump hosts (-J) are not supported")
	case 'o':
		key, val, ok := strings.Cut(value, "=")
		if !ok {
			key, val, _ = strings.Cut(strings.TrimSpace(value), " ")
		}
		key, val = strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(val)
		switch key {
		case "user":
			c.user = val
		case "port":
			port, err := parsePort(val)
			if err != nil {
				return err
			}
			c.port = port
		case "hostname":
			return errors.New("ssh option HostName is not supported; put the host in the destination")
		case "proxyjump", "proxycommand":
			return fmt.Errorf("ssh option %s is not supported", key)
		}
	}
	return nil
}

// splitShellWords splits a command line on blanks, honouring single quotes,
// double quotes and backslash escapes. A bare ';' glued to a word is split
// off so `ssh host; ssh other` ends at the first command.
func splitShellWords(line string) ([]string, error) {
	var (
		words   []string
		current strings.Builder
		inWord  bool
		quote   rune
	)
	flush := func() {
		if inWord {
			words = append(words, current.String())
			current.Reset()
			inWord = false
		}
	}
	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				current.WriteRune(r)
			}
		case quote == '"':
			if r == '"' {
				quote = 0
			} else if r == '\\' && i+1 < len(runes) {
				i++
				current.WriteRune(runes[i])
			} else {
				current.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == '\\' && i+1 < len(runes):
			i++
			current.WriteRune(runes[i])
			inWord = true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush()
		case r == ';':
			flush()
			words = append(words, ";")
		default:
			current.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, errors.New("ssh command has an unterminated quote")
	}
	flush()
	return words, nil
}
