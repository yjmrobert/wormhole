package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

func isTerminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

// confirm asks a yes/no question on the terminal.
func (a *App) confirm(prompt string) (bool, error) {
	fmt.Fprint(a.Stdout, prompt+" [y/N] ")
	r := bufio.NewReader(a.Stdin)
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes", nil
}

// readSecret reads a password without echo when stdin is a terminal.
func (a *App) readSecret(prompt string) (string, error) {
	fmt.Fprint(a.Stdout, prompt)
	if f, ok := a.Stdin.(*os.File); ok && isTerminal(f) {
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(a.Stdout)
		return strings.TrimSpace(string(b)), err
	}
	r := bufio.NewReader(a.Stdin)
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
