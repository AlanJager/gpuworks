package main

import (
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// The two install banners: a UTF-8 terminal gets the block art, everything else
// — a pipe, a log file, a C locale — gets the plain one, because mojibake in an
// installation log outlives the installation. Trailing spaces are trimmed here;
// they are invisible in a terminal and only give text editors something to eat.
const bannerASCII = `  ____   ____    _   _  __        __   ___    ____    _  __  ____
 / ___| |  _ \  | | | | \ \      / /  / _ \  |  _ \  | |/ / / ___|
| |  _  | |_) | | | | |  \ \ /\ / /  | | | | | |_) | | ' /  \___ \
| |_| | |  __/  | |_| |   \ V  V /   | |_| | |  _ <  | . \   ___) |
 \____| |_|      \___/     \_/\_/     \___/  |_| \_\ |_|\_\ |____/`

const bannerShadow = ` ██████╗  ██████╗  ██╗   ██╗ ██╗    ██╗  ██████╗  ██████╗  ██╗  ██╗ ███████╗
██╔════╝  ██╔══██╗ ██║   ██║ ██║    ██║ ██╔═══██╗ ██╔══██╗ ██║ ██╔╝ ██╔════╝
██║  ███╗ ██████╔╝ ██║   ██║ ██║ █╗ ██║ ██║   ██║ ██████╔╝ █████╔╝  ███████╗
██║   ██║ ██╔═══╝  ██║   ██║ ██║███╗██║ ██║   ██║ ██╔══██╗ ██╔═██╗  ╚════██║
╚██████╔╝ ██║      ╚██████╔╝ ╚███╔███╔╝ ╚██████╔╝ ██║  ██║ ██║  ██╗ ███████║
 ╚═════╝  ╚═╝       ╚═════╝   ╚══╝╚══╝   ╚═════╝  ╚═╝  ╚═╝ ╚═╝  ╚═╝ ╚══════╝`

// installBanner returns the art `gpuworks install` should print.
func installBanner(w io.Writer) string {
	return pickBanner(stdoutIsTerminal(w), localeIsUTF8())
}

// pickBanner holds the whole decision. Its two inputs are observed separately
// so that both branches are testable without a real terminal.
func pickBanner(terminal, utf8Locale bool) string {
	if terminal && utf8Locale {
		return bannerShadow
	}
	return bannerASCII
}

// stdoutIsTerminal reports whether w is a terminal. Only an actual file can be
// one: a buffer, a pipe or an io.MultiWriter is by definition not.
func stdoutIsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// localeIsUTF8 reports whether the effective locale is UTF-8. LC_ALL beats
// LC_CTYPE, which beats LANG; with none of them set the C locale is in effect,
// which is not UTF-8.
func localeIsUTF8() bool {
	for _, key := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if value := os.Getenv(key); value != "" {
			value = strings.ToLower(value)
			return strings.Contains(value, "utf-8") || strings.Contains(value, "utf8")
		}
	}
	return false
}
